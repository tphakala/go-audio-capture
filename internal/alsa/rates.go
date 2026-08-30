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
