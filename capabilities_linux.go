//go:build linux

package capture

import (
	"errors"

	"golang.org/x/sys/unix"

	"github.com/tphakala/go-audio-capture/internal/alsa"
)

// ratePCM is the capability-query seam that SupportedRates drives; *alsa.PCM
// satisfies it, and tests inject a hardware-free fake. It is separate from the
// stream seam because a query opens and closes its own short-lived fd rather
// than driving a live Stream.
type ratePCM interface {
	SupportedRates(channels int, format uint32, candidates []int) ([]int, int, int, error)
	VerifyRate(channels int, format uint32, rate int) (bool, error)
	Close() error
}

// openRatePCM is a package var so tests can substitute a fake device. It opens
// with O_NONBLOCK (OpenPCMForQuery) so the probe never blocks on the open.
var openRatePCM = func(card, device int) (ratePCM, error) {
	p, err := alsa.OpenPCMForQuery(card, device)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// standardRates is the candidate set SupportedRates probes: the common CD/DAT,
// telephony, and professional/ultrasonic rates. HW_REFINE decides which of
// these a given device actually accepts.
var standardRates = []int{
	8000, 11025, 16000, 22050, 32000, 44100, 48000,
	64000, 88200, 96000, 176400, 192000, 256000, 352800, 384000,
}

// SupportedRates reports which standard sample rates the capture device accepts
// for the given channel count and format. It opens the device once and issues
// one HW_REFINE ioctl per candidate rate; it never runs HW_PARAMS, PREPARE, or
// START, so it does not move the device out of its current state.
//
// If the device is held exclusively by another process the open itself fails
// and the returned error is ErrDeviceInUse; a missing or removed device yields
// ErrDeviceGone; a channel count or format the device does not support at any
// rate yields *BadFormatError. Resolving the device id can also fail before any
// open, with *BadDeviceError for a malformed id, *DeviceNotFoundError (which
// unwraps to ErrDeviceGone) when a stable id matches nothing present, or
// *AmbiguousDeviceError when it matches more than one. In the ErrDeviceInUse and
// ErrDeviceGone cases the caller should fall back to a static rate list rather
// than treating the query as authoritative.
func SupportedRates(device string, channels int, format Format) (RateSupport, error) {
	r, af, err := resolveQuery(device, channels, format)
	if err != nil {
		return RateSupport{}, err
	}
	return supportedRatesAt(r, channels, format, af)
}

// resolveQuery resolves the device id and validates the channel count and
// format once, so that a query which runs two passes over the same device
// resolves it exactly once. Resolving per pass would let a device swapped in
// between the passes be refined as one unit and verified as another.
func resolveQuery(device string, channels int, format Format) (resolved, uint32, error) {
	r, err := resolveDevice(device)
	if err != nil {
		return resolved{}, 0, err
	}
	if channels < 1 {
		return resolved{}, 0, &ConfigError{Field: "channels", Reason: "must be at least 1"}
	}
	af, err := alsaFormat(format)
	if err != nil {
		return resolved{}, 0, err
	}
	return r, af, nil
}

// supportedRatesAt runs the HW_REFINE pass against an already-resolved device.
func supportedRatesAt(r resolved, channels int, format Format, af uint32) (RateSupport, error) {
	p, err := openRatePCM(r.card, r.device)
	if err != nil {
		return RateSupport{}, translateQueryError(err)
	}
	defer func() { _ = p.Close() }()

	// The short-lived query open races a replug exactly as a streaming open
	// does, so it gets the same post-open identity check: rates reported for
	// the wrong card are worse than no rates at all.
	if err := verifyCardIdentity(r.card, r.device, r.verifyID); err != nil {
		return RateSupport{}, err
	}

	rates, lo, hi, err := p.SupportedRates(channels, af, standardRates)
	if err != nil {
		// The initial unconstrained refine pins access/format/channels and leaves
		// rate open, so an EINVAL there means the hardware rejects this
		// channel/format combo outright (not merely a rate): report it as a typed
		// BadFormatError rather than leaking the internal ioctl string.
		if errors.Is(err, unix.EINVAL) {
			return RateSupport{}, &BadFormatError{Channels: channels, Format: format}
		}
		return RateSupport{}, translateQueryError(err)
	}
	return RateSupport{Rates: rates, Min: lo, Max: hi}, nil
}

// SupportedRatesVerified reports which standard sample rates the device can
// actually COMMIT, not merely advertise. It first runs the same HW_REFINE pass
// as SupportedRates (yielding the advertised window and a candidate filter), then
// re-opens the device once per candidate and issues a full HW_PARAMS commit to
// confirm the hardware truly delivers that rate. The device id is resolved once
// and shared between the two passes, so a device swapped in between them cannot
// be refined as one unit and verified as another.
//
// This exists because HW_REFINE over-reports on some USB Audio Class devices:
// the driver advertises a continuous rate window (e.g. [48000, 384000]) yet only
// a single firmware-fixed rate actually commits. A refine-only probe would offer
// rates the device silently rejects at open; the HW_PARAMS pass drops them.
//
// It is more expensive than SupportedRates (one device open per advertised rate)
// so it is meant for occasional capability discovery, not a hot path. The
// per-candidate opens use O_NONBLOCK (like every query here) so they never block
// on a device that gates its open on a peer, and each commit is discarded by
// closing from the SETUP state. Errors map exactly as SupportedRates: a busy or
// missing device yields ErrDeviceInUse / ErrDeviceGone and the caller should
// fall back to a static list.
func SupportedRatesVerified(device string, channels int, format Format) (RateSupport, error) {
	r, af, err := resolveQuery(device, channels, format)
	if err != nil {
		return RateSupport{}, err
	}
	rs, err := supportedRatesAt(r, channels, format, af)
	if err != nil {
		return RateSupport{}, err
	}
	if len(rs.Rates) == 0 {
		return rs, nil // nothing advertised: nothing to verify
	}

	verified := make([]int, 0, len(rs.Rates))
	for _, rate := range rs.Rates {
		// Scope the open in a closure so its Close is deferred: a panic in
		// VerifyRate then still releases the fd rather than leaking it.
		ok, verr := func() (bool, error) {
			p, err := openRatePCM(r.card, r.device)
			if err != nil {
				return false, err
			}
			defer func() { _ = p.Close() }()
			if err := verifyCardIdentity(r.card, r.device, r.verifyID); err != nil {
				return false, err
			}
			return p.VerifyRate(channels, af, rate)
		}()
		if verr != nil {
			return RateSupport{}, translateQueryError(verr)
		}
		if ok {
			verified = append(verified, rate)
		}
	}
	return RateSupport{Rates: verified, Min: rs.Min, Max: rs.Max}, nil
}

// translateQueryError maps the raw errnos a capability query can hit onto the
// package's typed errors, so callers never import internal/alsa or match bare
// errnos. Anything else is returned unchanged.
func translateQueryError(err error) error {
	switch {
	case errors.Is(err, unix.EBUSY):
		return ErrDeviceInUse
	case isDeviceGoneErrno(err):
		return ErrDeviceGone
	default:
		return err
	}
}
