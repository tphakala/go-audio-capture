//go:build linux

package alsa

import (
	"errors"
	"reflect"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fakeRateDevice returns an ioctl fake that models a device supporting a
// discrete rate set inside a continuous [rangeLo, rangeHi] refine window. An
// unconstrained rate refine (Min != Max) reports the raw range; a pinned probe
// (Min == Max == r) succeeds only if r is in the supported set, otherwise it
// returns EINVAL the way the kernel does for an unsupported pin.
func fakeRateDevice(rangeLo, rangeHi uint32, supported map[uint32]bool) ioctlFunc {
	return func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req != iocHwRefine {
			return nil
		}
		hw := (*HwParams)(arg)
		lo, hi := hw.Interval(ParamRate)
		if lo == hi { // a pinned probe for exactly this rate
			if !supported[lo] {
				return unix.EINVAL
			}
			return nil // keep [lo, lo]
		}
		setInterval(hw, ParamRate, rangeLo, rangeHi)
		return nil
	}
}

func TestSupportedRatesEnumeratesDiscreteSet(t *testing.T) {
	// Hardware does 44.1k/48k/96k/192k/384k, with a continuous refine window of
	// [44100, 384000]. The candidate list also includes rates inside that window
	// the device does NOT do (64000, 88200, 176400) and rates below it (8000..
	// 32000); both classes must be dropped.
	supported := map[uint32]bool{44100: true, 48000: true, 96000: true, 192000: true, 384000: true}
	p := newPCM(-1, fakeRateDevice(44100, 384000, supported))

	candidates := []int{8000, 16000, 22050, 32000, 44100, 48000, 64000, 88200, 96000, 176400, 192000, 352800, 384000}
	rates, lo, hi, err := p.SupportedRates(1, FormatS32LE, candidates)
	if err != nil {
		t.Fatalf("SupportedRates: %v", err)
	}
	want := []int{44100, 48000, 96000, 192000, 384000}
	if !reflect.DeepEqual(rates, want) {
		t.Errorf("rates = %v, want %v", rates, want)
	}
	if lo != 44100 || hi != 384000 {
		t.Errorf("range = [%d, %d], want [44100, 384000]", lo, hi)
	}
}

func TestSupportedRatesSortsAndDedups(t *testing.T) {
	supported := map[uint32]bool{48000: true, 96000: true}
	p := newPCM(-1, fakeRateDevice(48000, 96000, supported))
	// Unsorted with a duplicate; result must be ascending and unique.
	rates, _, _, err := p.SupportedRates(2, FormatS16LE, []int{96000, 48000, 96000})
	if err != nil {
		t.Fatalf("SupportedRates: %v", err)
	}
	want := []int{48000, 96000}
	if !reflect.DeepEqual(rates, want) {
		t.Errorf("rates = %v, want %v", rates, want)
	}
}

func TestSupportedRatesPropagatesRangeRefineError(t *testing.T) {
	// If the initial unconstrained refine fails, that is a real device error and
	// must surface (not be silently swallowed as "no rates").
	sentinel := unix.ENOTTY
	fake := func(_ int, req uintptr, _ unsafe.Pointer) error {
		if req == iocHwRefine {
			return sentinel
		}
		return nil
	}
	p := newPCM(-1, fake)
	_, _, _, err := p.SupportedRates(1, FormatS32LE, []int{48000})
	if !errors.Is(err, sentinel) {
		t.Fatalf("SupportedRates err = %v, want ENOTTY", err)
	}
}

func TestSupportedRatesEmptyIntervalIsFatal(t *testing.T) {
	// Some drivers signal an unsatisfiable channel/format combo by returning
	// success with the rate interval emptied rather than EINVAL. That must still
	// surface as EINVAL (which the public layer maps to *BadFormatError), not an
	// empty, healthy-looking result.
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req == iocHwRefine {
			hw := (*HwParams)(arg)
			hw.Intervals[ParamRate-paramFirstInterval] = Interval{Flags: intervalEmpty}
		}
		return nil
	}
	p := newPCM(-1, fake)
	_, _, _, err := p.SupportedRates(1, FormatS16LE, []int{48000})
	if !errors.Is(err, unix.EINVAL) {
		t.Fatalf("SupportedRates err = %v, want EINVAL", err)
	}
}

func TestSupportedRatesProbeErrorIsFatal(t *testing.T) {
	// A non-EINVAL error from a per-rate probe (here ENODEV: the device vanished
	// mid-probe) must surface, not be swallowed as "unsupported rate" leaving a
	// silently truncated list.
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req != iocHwRefine {
			return nil
		}
		hw := (*HwParams)(arg)
		lo, hi := hw.Interval(ParamRate)
		if lo != hi { // unconstrained range refine succeeds
			setInterval(hw, ParamRate, 44100, 96000)
			return nil
		}
		if lo == 96000 { // device disappears when this rate is probed
			return unix.ENODEV
		}
		return nil // 44100/48000/88200 accepted
	}
	p := newPCM(-1, fake)
	_, _, _, err := p.SupportedRates(2, FormatS32LE, []int{44100, 48000, 88200, 96000})
	if !errors.Is(err, unix.ENODEV) {
		t.Fatalf("SupportedRates err = %v, want ENODEV", err)
	}
}

func TestSupportedRatesEmptyWhenNoneMatch(t *testing.T) {
	// A device whose window excludes every candidate returns an empty (nil) slice
	// and a nil error, letting the caller fall back to a static list.
	p := newPCM(-1, fakeRateDevice(500000, 768000, map[uint32]bool{768000: true}))
	rates, lo, hi, err := p.SupportedRates(1, FormatS32LE, []int{44100, 48000, 96000})
	if err != nil {
		t.Fatalf("SupportedRates: %v", err)
	}
	if len(rates) != 0 {
		t.Errorf("rates = %v, want empty", rates)
	}
	if lo != 500000 || hi != 768000 {
		t.Errorf("range = [%d, %d], want [500000, 768000]", lo, hi)
	}
}

// fakeCommitDevice models a device that advertises a continuous rate window at
// HW_REFINE but only actually COMMITS the rates in committable at HW_PARAMS. This
// is the USB Audio Class failure mode (e.g. an AudioMoth advertising [48000,
// 384000] but delivering only 384000) that refine-only probing cannot detect.
func fakeCommitDevice(rangeLo, rangeHi uint32, committable map[uint32]bool) ioctlFunc {
	return func(_ int, req uintptr, arg unsafe.Pointer) error {
		hw := (*HwParams)(arg)
		switch req {
		case iocHwRefine:
			// A refine reports the advertised rate window when rate is open, and
			// keeps a pinned rate (a refine narrows, never widens, a pin). It also
			// resolves a valid period/buffer geometry the caller can pin to.
			if lo, hi := hw.Interval(ParamRate); lo != hi {
				setInterval(hw, ParamRate, rangeLo, rangeHi)
			}
			setInterval(hw, ParamPeriodSize, 64, 8192)
			setInterval(hw, ParamPeriods, 2, 32)
			return nil
		case iocHwParams:
			lo, hi := hw.Interval(ParamRate)
			if lo != hi { // a commit must pin an exact rate
				return unix.EINVAL
			}
			if !committable[lo] { // advertised at refine, rejected at commit
				return unix.EINVAL
			}
			return nil // keep [lo, lo]; the commit succeeds
		default:
			return nil
		}
	}
}

func TestVerifyRateRejectsRefineOnlyRate(t *testing.T) {
	// AudioMoth-like: refine advertises [48000, 384000] but only 384000 commits.
	// VerifyRate must reject 48000 (a refine lie) and accept 384000.
	committable := map[uint32]bool{384000: true}

	p := newPCM(-1, fakeCommitDevice(48000, 384000, committable))
	if ok, err := p.VerifyRate(1, FormatS32LE, 48000); err != nil || ok {
		t.Fatalf("VerifyRate(48000) = %v, %v; want false, nil", ok, err)
	}
	p2 := newPCM(-1, fakeCommitDevice(48000, 384000, committable))
	if ok, err := p2.VerifyRate(1, FormatS32LE, 384000); err != nil || !ok {
		t.Fatalf("VerifyRate(384000) = %v, %v; want true, nil", ok, err)
	}
}

func TestVerifyRateOutsideWindow(t *testing.T) {
	// A candidate below the advertised window never reaches HW_PARAMS.
	p := newPCM(-1, fakeCommitDevice(48000, 384000, map[uint32]bool{384000: true}))
	if ok, err := p.VerifyRate(1, FormatS32LE, 8000); err != nil || ok {
		t.Fatalf("VerifyRate(8000) = %v, %v; want false, nil", ok, err)
	}
}

func TestVerifyRatePropagatesRealError(t *testing.T) {
	// A non-EINVAL commit error (the device vanished mid-probe) surfaces rather
	// than being read as "unsupported".
	sentinel := unix.ENODEV
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		hw := (*HwParams)(arg)
		switch req {
		case iocHwRefine:
			setInterval(hw, ParamRate, 48000, 384000)
			return nil
		case iocHwParams:
			return sentinel
		default:
			return nil
		}
	}
	p := newPCM(-1, fake)
	if _, err := p.VerifyRate(1, FormatS32LE, 48000); !errors.Is(err, sentinel) {
		t.Fatalf("VerifyRate err = %v, want ENODEV", err)
	}
}

func TestVerifyRateRejectsSilentSubstitution(t *testing.T) {
	// A driver that commits successfully but substitutes a different rate than
	// requested must be rejected: only an exact match proves the rate is honored.
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		hw := (*HwParams)(arg)
		switch req {
		case iocHwRefine:
			if lo, hi := hw.Interval(ParamRate); lo != hi {
				setInterval(hw, ParamRate, 44100, 48000)
			}
			setInterval(hw, ParamPeriodSize, 64, 8192)
			setInterval(hw, ParamPeriods, 2, 32)
			return nil
		case iocHwParams:
			// Commit succeeds but the hardware forces 44100 regardless of request.
			setInterval(hw, ParamRate, 44100, 44100)
			return nil
		default:
			return nil
		}
	}
	p := newPCM(-1, fake)
	if ok, err := p.VerifyRate(1, FormatS16LE, 48000); err != nil || ok {
		t.Fatalf("VerifyRate(48000 substituted to 44100) = %v, %v; want false, nil", ok, err)
	}
}

// fakeGeometryDevice models a USB device whose advertised period-size interval
// has a degenerate lower bound: HW_REFINE offers periods from minAdvertised
// frames, but HW_PARAMS only COMMITS a rate when the pinned period size is at
// least minCommittable frames (a realistic buffer). Pinning the advertised
// minimum is refused with EINVAL; a period at the streaming default (rate/50)
// commits. This is the ZOOM AMS-24 failure mode at 88200/96000 that made
// VerifyRate under-report when it pinned the interval minimum instead of
// verifying with the real streaming open's geometry.
func fakeGeometryDevice(rangeLo, rangeHi, minAdvertised, minCommittable uint32) ioctlFunc {
	return func(_ int, req uintptr, arg unsafe.Pointer) error {
		hw := (*HwParams)(arg)
		switch req {
		case iocHwRefine:
			if lo, hi := hw.Interval(ParamRate); lo != hi {
				setInterval(hw, ParamRate, rangeLo, rangeHi)
			}
			setInterval(hw, ParamPeriodSize, minAdvertised, 8192)
			setInterval(hw, ParamPeriods, 2, 1024)
			return nil
		case iocHwParams:
			if lo, hi := hw.Interval(ParamRate); lo != hi {
				return unix.EINVAL // a commit must pin an exact rate
			}
			if plo, _ := hw.Interval(ParamPeriodSize); plo < minCommittable {
				return unix.EINVAL // degenerate buffer geometry refused
			}
			return nil
		default:
			return nil
		}
	}
}

func TestVerifyRateUsesStreamingGeometryNotIntervalMinimum(t *testing.T) {
	// Regression for the AMS-24 under-report: the device advertises a period floor
	// of 8 frames but only commits with a realistic period (>= 64 frames). Pinning
	// the advertised minimum (the old behavior) fails; verifying with the streaming
	// default period (rate/50, e.g. 1920 at 96 kHz) commits, matching the real open.
	const minAdvertised, minCommittable = 8, 64
	for _, rate := range []int{88200, 96000} {
		p := newPCM(-1, fakeGeometryDevice(44100, 96000, minAdvertised, minCommittable))
		ok, err := p.VerifyRate(2, FormatS32LE, rate)
		if err != nil || !ok {
			t.Fatalf("VerifyRate(%d) = %v, %v; want true, nil (streaming geometry must commit)", rate, ok, err)
		}
	}
	// Sanity: DefaultPeriodFrames clamps into the advertised interval and stays
	// above the commit floor for these rates.
	if pf := DefaultPeriodFrames(96000); pf < minCommittable {
		t.Fatalf("DefaultPeriodFrames(96000) = %d; test assumes >= %d", pf, minCommittable)
	}
}

func TestVerifyRateRejectsEmptyRateInterval(t *testing.T) {
	// A driver that returns success from the first refine but empties the rate
	// interval (an unsatisfiable combo signalled without EINVAL) is unsupported.
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req == iocHwRefine {
			hw := (*HwParams)(arg)
			hw.Intervals[ParamRate-paramFirstInterval] = Interval{Flags: intervalEmpty}
		}
		return nil
	}
	p := newPCM(-1, fake)
	if ok, err := p.VerifyRate(1, FormatS16LE, 48000); err != nil || ok {
		t.Fatalf("VerifyRate(empty interval) = %v, %v; want false, nil", ok, err)
	}
}
