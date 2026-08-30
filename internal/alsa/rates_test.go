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
