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
// rate yields *BadFormatError. In the ErrDeviceInUse and ErrDeviceGone cases the
// caller should fall back to a static rate list rather than treating the query
// as authoritative.
func SupportedRates(device string, channels int, format Format) (RateSupport, error) {
	card, dev, err := parseDeviceID(device)
	if err != nil {
		return RateSupport{}, err
	}
	if channels < 1 {
		return RateSupport{}, &ConfigError{Field: "channels", Reason: "must be at least 1"}
	}
	af, err := alsaFormat(format)
	if err != nil {
		return RateSupport{}, err
	}

	p, err := openRatePCM(card, dev)
	if err != nil {
		return RateSupport{}, translateQueryError(err)
	}
	defer func() { _ = p.Close() }()

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
// actually COMMIT, not merely advertise. It runs SupportedRates first (a fast
// HW_REFINE pass that yields the advertised window and a candidate filter), then
// re-opens the device once per candidate and issues a full HW_PARAMS commit to
// confirm the hardware truly delivers that rate.
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
	rs, err := SupportedRates(device, channels, format)
	if err != nil {
		return RateSupport{}, err
	}
	if len(rs.Rates) == 0 {
		return rs, nil // nothing advertised: nothing to verify
	}

	card, dev, err := parseDeviceID(device)
	if err != nil {
		return RateSupport{}, err
	}
	af, err := alsaFormat(format)
	if err != nil {
		return RateSupport{}, err
	}

	verified := make([]int, 0, len(rs.Rates))
	for _, r := range rs.Rates {
		// Scope the open in a closure so its Close is deferred: a panic in
		// VerifyRate then still releases the fd rather than leaking it.
		ok, verr := func() (bool, error) {
			p, err := openRatePCM(card, dev)
			if err != nil {
				return false, err
			}
			defer func() { _ = p.Close() }()
			return p.VerifyRate(channels, af, r)
		}()
		if verr != nil {
			return RateSupport{}, translateQueryError(verr)
		}
		if ok {
			verified = append(verified, r)
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
