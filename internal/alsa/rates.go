//go:build linux

package alsa

import (
	"errors"
	"slices"
	"unsafe"

	"golang.org/x/sys/unix"
)

// SupportedRates probes which of the candidate sample rates the device accepts
// for the given channel count and format. It uses HW_REFINE only: no HW_PARAMS,
// no PREPARE, no state transition, so it never moves the device out of its
// current state and does not disturb a stream another process may hold.
//
// A single unconstrained refine yields only the continuous [lo, hi] rate window
// the hardware reports; it cannot reveal discrete gaps (e.g. a device that does
// 44100 and 48000 but nothing between). So each candidate inside that window is
// probed with its own refine, pinning the rate exact: the kernel returns EINVAL
// (or empties the interval) for a rate the hardware cannot produce, and keeps
// [r, r] for one it can. Every probe reuses the one open fd.
//
// A channel/format combination the device rejects at any rate returns
// *BadFormatError, carrying the channel range the device accepts for the format
// (0..0 when the format is unsupported at any channel count). Any other errno
// from a per-rate probe (the device vanished, the fd was closed) aborts the
// query and is returned wrapped with the HW_REFINE name, never a truncated list.
//
// The returned rates slice is ascending and de-duplicated (candidates need not
// be sorted or unique). lo and hi are the raw window bounds, useful when the
// device supports continuous rates and the caller wants a value not in the
// candidate list.
func (p *PCM) SupportedRates(channels int, format uint32, candidates []int) (rates []int, lo, hi int, err error) {
	base := func() HwParams {
		var hw HwParams
		hw.FillAny()
		hw.SetMask(ParamAccess, AccessRWInterleaved)
		hw.SetMask(ParamFormat, uint(format))
		hw.SetMask(ParamSubformat, SubformatSTD)
		hw.SetIntervalExact(ParamChannels, uint32(channels))
		return hw
	}

	// Unconstrained refine: learn the supported [lo, hi] window in one call. A
	// failure here is a real device error (bad fd, no such ioctl), not a mere
	// "rate unsupported", so it is wrapped and returned.
	window := base()
	if rerr := p.refine(&window); rerr != nil {
		// EINVAL means the hardware rejects this channel/format combo outright:
		// report the channel range it does accept as a typed *BadFormatError.
		if errors.Is(rerr, unix.EINVAL) {
			return nil, 0, 0, p.badFormat(channels, format)
		}
		return nil, 0, 0, rerr
	}
	// A driver may signal an unsatisfiable channel/format combo by emptying the
	// rate interval on a successful refine rather than returning EINVAL. Treat
	// that the same, so the caller still gets *BadFormatError instead of an
	// empty, healthy-looking result.
	if window.IntervalEmpty(ParamRate) {
		return nil, 0, 0, p.badFormat(channels, format)
	}
	rlo, rhi := window.Interval(ParamRate)
	lo, hi = int(rlo), int(rhi)

	for _, r := range candidates {
		if r <= 0 || uint32(r) < rlo || uint32(r) > rhi {
			continue // outside the reported window: cannot be supported
		}
		probe := base()
		probe.SetIntervalExact(ParamRate, uint32(r))
		// refineProbe returns the raw ioctl error. An unsupported pin fails with
		// EINVAL, which here just means "skip this rate". Any other errno (the
		// device vanished mid-probe, the fd was closed) is a real failure and is
		// returned wrapped with the ioctl name, so the caller never gets a
		// silently truncated rate list or an opaque errno.
		if perr := p.refineProbe(&probe); perr != nil {
			if errors.Is(perr, unix.EINVAL) {
				continue
			}
			return nil, lo, hi, &ioctlError{Op: opHwRefine, Err: perr}
		}
		if probe.IntervalEmpty(ParamRate) { // defensive: some drivers empty rather than EINVAL
			continue
		}
		if plo, phi := probe.Interval(ParamRate); plo == uint32(r) && phi == uint32(r) {
			rates = append(rates, r)
		}
	}

	slices.Sort(rates)
	rates = slices.Compact(rates)
	return rates, lo, hi, nil
}

// refineProbe issues HW_REFINE and returns the raw ioctl error (unwrapped), so
// the rate-probe loop can treat an expected EINVAL as "unsupported" rather than
// a fatal device error.
func (p *PCM) refineProbe(hw *HwParams) error {
	return p.guardedIoctl(iocHwRefine, unsafe.Pointer(hw))
}

// VerifyRate reports whether the device can actually COMMIT to the exact rate,
// using HW_PARAMS rather than HW_REFINE alone. Some drivers (notably USB Audio
// Class devices that advertise a continuous rate window) accept a rate at
// HW_REFINE that HW_PARAMS then rejects, because only the commit resolves the
// true discrete or firmware-fixed rate. SupportedRates, which is refine-only,
// therefore over-reports on such devices; VerifyRate is the authoritative check
// a caller uses when it must not offer a rate the hardware cannot deliver.
//
// It refines once to learn the rate window, checks the rate falls inside it, then
// runs the SAME geometry selection the streaming open uses (refineGeometry: the
// exact rate, then the accepted period size and count nearest a ~20 ms period and
// 4 periods) and commits HW_PARAMS. Verifying with the streaming geometry, rather
// than the interval's degenerate minimum, is essential: some USB Audio Class
// devices advertise a period-size interval whose lower bound (e.g. 8 frames)
// yields a buffer the hardware refuses at high rates. Sharing refineGeometry with
// Negotiate makes the probe a faithful predictor of the real open at its DEFAULT
// geometry: a rate commits here iff the streaming open can deliver it with the
// default period/periods. A caller that opens with a non-zero custom
// period/periods is not modeled by this probe.
//
// A committed rate equal to the request means supported (true, nil). An EINVAL at
// refine (the channel/format combo is unsupported) or at commit (a discrete-rate
// gap, or a refine lie), an emptied rate interval, a geometry the driver refuses,
// or a driver that silently substitutes a different rate all mean unsupported
// (false, nil). Any other errno is a real device error and is returned.
//
// VerifyRate returns with the PCM back in the OPEN state, so it can run
// repeatedly on one fd: a failed HW_PARAMS makes the kernel reset the stream
// itself, and after a successful one VerifyRate issues HW_FREE. The release
// matters on USB, where the driver keeps the committed endpoint until hw_free and
// that endpoint can limit the next rate refine.
func (p *PCM) VerifyRate(channels int, format uint32, rate int) (bool, error) {
	var hw HwParams
	hw.FillAny()
	hw.SetMask(ParamAccess, AccessRWInterleaved)
	hw.SetMask(ParamFormat, uint(format))
	hw.SetMask(ParamSubformat, SubformatSTD)
	hw.SetIntervalExact(ParamChannels, uint32(channels))

	// First refine: learn the supported rate window for this format/channel combo.
	if err := p.refine(&hw); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return false, nil // channel/format combo unsupported: rate cannot be verified
		}
		return false, err
	}
	if hw.IntervalEmpty(ParamRate) {
		return false, nil // driver signalled an unsatisfiable combo without EINVAL
	}
	rlo, rhi := hw.Interval(ParamRate)
	if uint32(rate) < rlo || uint32(rate) > rhi {
		return false, nil // outside the reported window
	}

	if err := p.refineGeometry(&hw, rate, DefaultPeriodFrames(rate), DefaultPeriods); err != nil {
		if _, ok := errors.AsType[*GeometryError](err); ok || errors.Is(err, errRateRefused) {
			return false, nil
		}
		return false, err
	}
	if err := p.hwParams(&hw); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return false, nil // the driver accepted this rate at refine but cannot commit it
		}
		return false, err
	}
	// The commit succeeded: release it before judging the result, so the PCM is
	// back in OPEN whichever way the check below goes.
	if err := p.hwFree(); err != nil {
		return false, err
	}
	// A driver may commit successfully yet substitute a different rate; only an
	// exact match proves the hardware honors the request.
	got, _ := hw.Interval(ParamRate)
	return got == uint32(rate), nil
}
