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
	Probe() error
	Close() error
}

// openRatePCM is a package var so tests can substitute a fake device. alsa.OpenPCM
// opens non-blocking like Open, so a query never waits on a busy device.
var openRatePCM = func(card, device int) (ratePCM, error) {
	p, err := alsa.OpenPCM(card, device)
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
// and the returned error is ErrDeviceInUse; a missing device, or one removed
// during the query, yields ErrDeviceGone; a channel count or format the device
// does not support at any rate yields *BadFormatError, which carries the
// channel range the device does accept for the format. Resolving the device
// id can also fail before any open, with *BadDeviceError for a malformed id,
// *DeviceNotFoundError (which unwraps to ErrDeviceGone) when a stable id
// matches nothing present, or *AmbiguousDeviceError when it matches more than
// one. In the ErrDeviceInUse and
// ErrDeviceGone cases the caller should fall back to a static rate list rather
// than treating the query as authoritative.
func SupportedRates(device string, channels int, format Format) (RateSupport, error) {
	r, err := prepareQuery(device, channels, format)
	if err != nil {
		return RateSupport{}, err
	}
	p, err := openQuery(r)
	if err != nil {
		return RateSupport{}, err
	}
	defer func() { _ = p.Close() }()
	return queryRates(p, channels, format)
}

// prepareQuery validates the query's cheap, device-independent inputs (channel
// count and sample format) and only then resolves the device id, so an obviously
// invalid call is rejected before paying for a /proc + /sys enumeration. Open
// orders its own checks the same way, so the two entry points agree on which
// error a caller sees when more than one input is bad. Resolution happens exactly
// once here and is shared across a query's passes: resolving per pass would let a
// device swapped in between them be refined as one unit and verified as another.
func prepareQuery(device string, channels int, format Format) (resolved, error) {
	if channels < 1 {
		return resolved{}, &ConfigError{Field: "channels", Reason: "must be at least 1"}
	}
	if _, err := alsaFormat(format); err != nil {
		return resolved{}, err
	}
	return resolveForOpen(device)
}

// openQuery opens the device for a capability query and runs the post-open
// identity check, returning the open PCM. The short-lived query open races a
// replug exactly as a streaming open does, so rates reported for the wrong card
// (worse than none) are prevented the same way. On error nothing is left open.
func openQuery(r resolved) (ratePCM, error) {
	p, err := openRatePCM(r.card, r.device)
	if err != nil {
		// Attribute the failure to the card only once it is shown to be the unit
		// that was asked for: a busy card that took over the index of an unplugged
		// one must read as the device being gone, not as busy (retry later).
		if verr := verifyCardIdentity(r); verr != nil {
			return nil, verr
		}
		return nil, translateQueryError(err)
	}
	if err := verifyCardIdentity(r); err != nil {
		_ = p.Close()
		return nil, err
	}
	return p, nil
}

// queryRates runs the HW_REFINE pass on an open device. It derives the ALSA
// format from format itself rather than taking a separate af argument, so the
// value fed to the ioctl and the one named in a BadFormatError cannot disagree.
// A refine error is classified by queryError, so it must run before the caller
// closes p.
func queryRates(p ratePCM, channels int, format Format) (RateSupport, error) {
	af, err := alsaFormat(format)
	if err != nil {
		return RateSupport{}, err
	}
	rates, lo, hi, err := p.SupportedRates(channels, af, standardRates)
	if err != nil {
		// The backend rejects this channel/format combo outright (not merely a
		// rate): report it as a typed BadFormatError, with the channel range it
		// accepts, rather than leaking the internal error.
		if abfe, ok := errors.AsType[*alsa.BadFormatError](err); ok {
			return RateSupport{}, &BadFormatError{Channels: channels, Format: format, MinChannels: abfe.MinChannels, MaxChannels: abfe.MaxChannels}
		}
		return RateSupport{}, queryError(p, err)
	}
	return RateSupport{Rates: rates, Min: lo, Max: hi}, nil
}

// SupportedRatesVerified reports which standard sample rates the device can
// actually COMMIT, not merely advertise. It runs the same HW_REFINE pass as
// SupportedRates (yielding the advertised window and a candidate filter), then
// issues a full HW_PARAMS commit for each advertised rate to confirm the hardware
// truly delivers it. The device is opened once and every commit runs on that one
// fd (each is released with HW_FREE before the next), under a single identity
// check, so a device swapped in between the passes cannot be refined as one unit
// and verified as another.
//
// This exists because HW_REFINE over-reports on some USB Audio Class devices:
// the driver advertises a continuous rate window (e.g. [48000, 384000]) yet only
// a single firmware-fixed rate actually commits. A refine-only probe would offer
// rates the device silently rejects at open; the HW_PARAMS pass drops them. A
// rate is verified at the default period geometry (Rate/50 frames x 4 periods,
// moved to the nearest values the device accepts), the same one Open uses when
// Config leaves PeriodFrames and Periods zero.
//
// It is more expensive than SupportedRates (one commit per advertised rate) so it
// is meant for occasional capability discovery, not a hot path. The open is
// non-blocking (alsa.OpenPCM, like every query here) so it never waits on a busy
// device. Errors map exactly as SupportedRates: a busy or missing device yields
// ErrDeviceInUse / ErrDeviceGone (a device removed during the HW_REFINE or
// HW_PARAMS passes included) and the caller should fall back to a static
// list.
func SupportedRatesVerified(device string, channels int, format Format) (RateSupport, error) {
	r, err := prepareQuery(device, channels, format)
	if err != nil {
		return RateSupport{}, err
	}
	// format was validated in prepareQuery, so this cannot fail here; deriving af
	// from format (rather than threading it) keeps the two in lockstep.
	af, err := alsaFormat(format)
	if err != nil {
		return RateSupport{}, err
	}
	p, err := openQuery(r)
	if err != nil {
		return RateSupport{}, err
	}
	// Deferred so a panic in VerifyRate still releases the fd.
	defer func() { _ = p.Close() }()

	rs, err := queryRates(p, channels, format)
	if err != nil {
		return RateSupport{}, err
	}
	if len(rs.Rates) == 0 {
		return rs, nil // nothing advertised: nothing to verify
	}

	verified := make([]int, 0, len(rs.Rates))
	for _, rate := range rs.Rates {
		ok, verr := p.VerifyRate(channels, af, rate)
		if verr != nil {
			return RateSupport{}, queryError(p, verr)
		}
		if ok {
			verified = append(verified, rate)
		}
	}
	return RateSupport{Rates: verified, Min: rs.Min, Max: rs.Max}, nil
}

// queryError classifies an error from an ioctl on an open query fd. EBADFD
// means the PCM left the state the ioctl needs; as in Open, Start and Read,
// one PVERSION probe tells a device removed mid-query (ErrDeviceGone) from an
// ordinary state error, which is returned unchanged. It must run before the
// query's deferred Close: a probe on a closed PCM fails with EBADF and would
// not read as gone.
func queryError(p prober, err error) error {
	if errors.Is(err, unix.EBADFD) && deviceDisconnected(p) {
		return ErrDeviceGone
	}
	return translateQueryError(err)
}

// translateQueryError maps the raw errnos a capability query can hit onto the
// package's typed errors, so callers never import internal/alsa or match bare
// errnos. It is the mapping for when there is no fd to probe (a failed open);
// queryError adds the EBADFD probe. Anything else is returned unchanged.
func translateQueryError(err error) error {
	switch {
	case errors.Is(err, unix.EBUSY):
		return ErrDeviceInUse
	case alsa.IsDeviceGone(err):
		return ErrDeviceGone
	default:
		return err
	}
}
