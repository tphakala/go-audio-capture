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
		return nil, 0, 0, rerr
	}
	// A driver may signal an unsatisfiable channel/format combo by emptying the
	// rate interval on a successful refine rather than returning EINVAL. Treat
	// that the same as EINVAL so the public layer still reports *BadFormatError
	// instead of an empty, healthy-looking result.
	if window.IntervalEmpty(ParamRate) {
		return nil, 0, 0, unix.EINVAL
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
		// returned, so the caller never gets a silently truncated rate list.
		if perr := p.refineProbe(&probe); perr != nil {
			if errors.Is(perr, unix.EINVAL) {
				continue
			}
			return nil, lo, hi, perr
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
// pins the exact rate together with the SAME period/buffer geometry the streaming
// open uses (a ~20 ms period and 4 periods, clamped into the refined bounds; see
// pinRateGeometry) and commits HW_PARAMS. Verifying with the streaming geometry,
// rather than the interval's degenerate minimum, is essential: some USB Audio
// Class devices advertise a period-size interval whose lower bound (e.g. 8 frames)
// yields a buffer the hardware refuses at high rates, so pinning that minimum drops
// a rate the real open delivers fine. Sharing pinRateGeometry with Negotiate makes
// the probe a faithful predictor of the real open at its DEFAULT geometry: a rate
// commits here iff the streaming open can deliver it with the default
// period/periods. A caller that opens with a non-zero custom period/periods is not
// modeled by this probe.
//
// A committed rate equal to the request means supported (true, nil). An EINVAL at
// refine (the channel/format combo is unsupported) or at commit (a discrete-rate
// gap, or a refine lie), an emptied rate interval, or a driver that silently
// substitutes a different rate all mean unsupported (false, nil). Any other errno
// is a real device error and is returned.
//
// HW_PARAMS moves the stream to the SETUP state, so a caller verifying several
// rates must use a fresh fd per rate. Closing from SETUP is safe and frees the
// kernel buffers HW_PARAMS allocated; the kernel runs hw_free on release.
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

	// Pin the exact rate and the streaming open's period/buffer geometry, then
	// commit. Using the same geometry as the real open (shared via pinRateGeometry)
	// is what makes this an honest predictor: pinning the period interval's
	// degenerate minimum instead would false-negative rates that stream fine.
	hw.pinRateGeometry(rate, DefaultPeriodFrames(rate), DefaultPeriods)
	if err := p.hwParams(&hw); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return false, nil // the driver accepted this rate at refine but cannot commit it
		}
		return false, err
	}
	// A driver may commit successfully yet substitute a different rate; only an
	// exact match proves the hardware honors the request.
	got, _ := hw.Interval(ParamRate)
	return got == uint32(rate), nil
}
