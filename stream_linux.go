//go:build linux

package capture

import (
	"errors"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/tphakala/go-audio-capture/internal/alsa"
)

// pcm is the ALSA device seam that Stream drives; *alsa.PCM satisfies it, and
// tests inject a fake. openPCM is a package var so tests can substitute a
// hardware-free implementation.
type pcm interface {
	Negotiate(rate, channels int, format uint32, periodFrames, periods int) (alsa.Negotiated, error)
	Start() error
	ReadI(buf []byte, frames int) (int, error)
	Recover(err error) error
	Probe() error
	Close() error
}

// Recovery budgets for one Read call. Read returns as soon as ReadI delivers
// frames, so "recoveries inside one call" equals "consecutive recoveries with no
// delivered frames" and the counters can be plain locals: a long-running stream
// with periodic xruns between successful reads never reaches either cap.
//
//   - maxRecoveriesWithoutData bounds a device that overruns or fails again right
//     after every restart. The legitimate sequences inside one data gap are short
//     (a resume then an overrun, an overrun after a stall restart: 2-3), so 8
//     leaves margin while capping the ioctl churn.
//   - maxStallRestarts bounds EIO, the READI_FRAMES timeout (wait_for_avail:
//     about 100 ms at the default geometry on kernels from 6.6, 10 s up to 6.1).
//     One DROP+PREPARE+START restart is tried; a second EIO in the same gap means
//     the restart did not help and the caller must reopen.
const (
	maxRecoveriesWithoutData = 8
	maxStallRestarts         = 1
)

var openPCM = func(card, device int) (pcm, error) {
	p, err := alsa.OpenPCM(card, device)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Stream is an open capture stream. Read is single-consumer; Close may be
// called from another goroutine to unblock a parked Read.
type Stream struct {
	pcm        pcm
	cfg        Config
	frameBytes int
	xruns      atomic.Uint64
	closed     atomic.Bool
}

// Open configures and opens a capture stream. It negotiates the exact requested
// rate (failing with *BadRateError otherwise), applies the 20 ms / 4-period
// defaults, and returns a stream that is prepared but not yet started; call
// Start before Read. On failure it returns a typed error: *BadDeviceError for a
// malformed device id, *DeviceNotFoundError (which unwraps to ErrDeviceGone) when
// a well-formed stable id matches no present device, *AmbiguousDeviceError when
// it matches more than one, *BadRateError for an unsupported rate,
// *BadFormatError for an unsupported channel/format combination, ErrDeviceInUse
// when another application holds the device (Open fails at once rather than
// waiting for it to be released), and ErrDeviceGone when the device is missing or
// was removed.
func Open(cfg Config) (*Stream, error) {
	// Cheap, device-independent checks first, so an obviously invalid config is
	// rejected before a /proc + /sys enumeration resolves the id. SupportedRates
	// (prepareQuery) orders its checks the same way, so both entry points agree on
	// which error a caller sees when more than one field is bad.
	if cfg.Rate <= 0 {
		return nil, &ConfigError{Field: "rate", Reason: "must be positive"}
	}
	if cfg.Channels < 1 {
		return nil, &ConfigError{Field: "channels", Reason: "must be at least 1"}
	}
	format, err := alsaFormat(cfg.Format)
	if err != nil {
		return nil, err
	}
	r, err := resolveForOpen(cfg.Device)
	if err != nil {
		return nil, err
	}
	periodFrames := cfg.PeriodFrames
	if periodFrames == 0 {
		periodFrames = alsa.DefaultPeriodFrames(cfg.Rate) // ~20 ms
	}
	periods := cfg.Periods
	if periods == 0 {
		periods = alsa.DefaultPeriods
	}

	p, err := openPCM(r.card, r.device)
	if err != nil {
		// A device that is absent or removed at open time fails here (the PCM
		// node is missing, or the driver reports the card gone). Classify it the
		// same way as a mid-stream loss so a caller can retire it with
		// errors.Is(err, ErrDeviceGone), and a busy device as ErrDeviceInUse,
		// matching what SupportedRates and the Windows Open path already do.
		return nil, translateOpenError(err, cfg.Channels, cfg.Format)
	}
	// Close the resolve-to-open window: between matching the id to a card index
	// and opening it, that card could have been unplugged and another one taken
	// the index. Confirm the card we are now holding is still the one asked for
	// before any audio is read from it.
	if err := verifyCardIdentity(r.card, r.device, r.verifyID); err != nil {
		_ = p.Close()
		return nil, err
	}
	n, err := p.Negotiate(cfg.Rate, cfg.Channels, format, periodFrames, periods)
	if err != nil {
		// EBADFD from a negotiate ioctl means the PCM left the state the call
		// needs; probe before closing to tell a device that vanished mid-open
		// from any other cause.
		if errors.Is(err, unix.EBADFD) && deviceDisconnected(p) {
			err = ErrDeviceGone
		}
		_ = p.Close()
		return nil, translateOpenError(err, cfg.Channels, cfg.Format)
	}
	return &Stream{
		pcm: p,
		cfg: Config{
			Device:       cfg.Device,
			Rate:         n.Rate,
			Channels:     n.Channels,
			Format:       cfg.Format,
			PeriodFrames: n.PeriodFrames,
			Periods:      n.Periods,
		},
		frameBytes: cfg.Channels * cfg.Format.BytesPerSample(),
	}, nil
}

// Negotiated returns the configuration the hardware accepted, with the actual
// rate, period size, and period count filled in.
func (s *Stream) Negotiated() Config { return s.cfg }

// Start begins capture. Call it once before the first Read.
func (s *Stream) Start() error {
	if s.closed.Load() {
		return ErrClosed
	}
	if err := s.pcm.Start(); err != nil {
		// A concurrent Close races the START ioctl to EBADF; report the close.
		if s.closed.Load() || errors.Is(err, unix.EBADF) {
			return ErrClosed
		}
		// A device unplugged in the window between Open and Start surfaces as
		// ErrDeviceGone so a caller can classify a lost device with errors.Is at
		// Start exactly as it can at Open and Read.
		if isDeviceGoneErrno(err) {
			return ErrDeviceGone
		}
		// START on a disconnected stream is EBADFD, but so is a second START on
		// a running one; the probe tells them apart.
		if errors.Is(err, unix.EBADFD) {
			return s.badStateError(err)
		}
		return err
	}
	return nil
}

// Read fills buf with whole interleaved frames and returns the number of frames
// read. It blocks until at least one period is available. Recoverable failures
// are handled internally and counted (see Xruns): an overrun is restarted, a
// system suspend is resumed, and a stalled stream (EIO, the kernel's read
// timeout) gets one restart. Recovery is bounded per call: a second stall, or
// more than a handful of recoveries without any frames being delivered, returns
// a *StallError (which unwraps to ErrDeviceStalled and to the last errno). Read
// returns ErrClosed when the stream is closed and ErrDeviceGone when the device
// disappears (e.g. a USB capture device unplugged mid-stream, including while
// Read is parked in the driver); any other unrecoverable error is returned
// unchanged. Any returned error leaves the stream unusable (a short read, fewer
// frames than requested, is not an error and returns a nil error): the caller
// must Close it (Read does not release the device fd on its own) and, to resume,
// Open a new stream.
func (s *Stream) Read(buf []byte) (int, error) {
	if s.closed.Load() {
		return 0, ErrClosed
	}
	frames := len(buf) / s.frameBytes
	if frames == 0 {
		return 0, nil
	}
	var recoveries, stalls int
	for {
		n, err := s.pcm.ReadI(buf, frames)
		if err == nil {
			return n, nil
		}
		// A concurrent Close surfaces two distinct errnos: EBADF when acquire
		// short-circuits a closed PCM, or the kernel's EBADFD (a different errno)
		// when Close's DROP moved the stream to SETUP under a parked read. Close
		// sets s.closed before pcm.Close, so the s.closed check catches the
		// EBADFD case that errors.Is(EBADF) does not.
		if s.closed.Load() || errors.Is(err, unix.EBADF) {
			return 0, ErrClosed
		}
		// EBADFD was never recoverable. Besides Close it means the PCM was
		// disconnected (the unplug wakes a parked reader with it), so classify it
		// instead of returning the raw errno.
		if errors.Is(err, unix.EBADFD) {
			return 0, s.badStateError(err)
		}
		// Budget checks come before any counter changes, so once the budget is
		// spent nothing else moves.
		if recoveries == maxRecoveriesWithoutData {
			return 0, &StallError{Recoveries: recoveries, Err: err}
		}
		if errors.Is(err, unix.EIO) {
			if stalls == maxStallRestarts {
				return 0, &StallError{Recoveries: recoveries, Err: err}
			}
			stalls++
		}
		recoveries++
		if rerr := s.pcm.Recover(err); rerr != nil {
			// A concurrent Close can fail Recover's own ioctls with EBADF;
			// surface that as a clean close rather than a raw driver error.
			if s.closed.Load() || errors.Is(rerr, unix.EBADF) {
				return 0, ErrClosed
			}
			if errors.Is(rerr, unix.EBADFD) {
				return 0, s.badStateError(rerr)
			}
			// Unrecoverable: Recover returns the error unchanged. Map a
			// disappeared device onto ErrDeviceGone so a caller can classify a
			// surprise unplug with errors.Is instead of matching bare errnos.
			return 0, translateReadError(rerr)
		}
		s.xruns.Add(1)
	}
}

// badStateError classifies an EBADFD that did not come from a concurrent Close.
// The kernel returns EBADFD for a PCM in DISCONNECTED state (an unplug) but also
// for ordinary state errors, so one PVERSION probe decides: a disconnect becomes
// ErrDeviceGone, anything else is err unchanged. A Close always wins: s.closed is
// checked before the probe (no ioctl on a closing stream) and again after it,
// since a Close racing the probe makes it fail with EBADF.
func (s *Stream) badStateError(err error) error {
	if s.closed.Load() {
		return ErrClosed
	}
	gone := deviceDisconnected(s.pcm)
	if s.closed.Load() {
		return ErrClosed
	}
	if gone {
		return ErrDeviceGone
	}
	return err
}

// deviceDisconnected reports whether a PVERSION probe shows the device gone:
// ENODEV/ENXIO/ENOENT (the card's file operations were shut down) or EBADFD (the
// PCM alone is DISCONNECTED). EBADF (closed) and success are not disconnects.
func deviceDisconnected(p pcm) bool {
	perr := p.Probe()
	return perr != nil && (isDeviceGoneErrno(perr) || errors.Is(perr, unix.EBADFD))
}

// Xruns returns the number of capture discontinuities recovered so far:
// overruns, resumes after a system suspend, and restarted stalls.
func (s *Stream) Xruns() uint64 { return s.xruns.Load() }

// Close stops and closes the stream. It is idempotent and unblocks a Read
// currently parked in the driver. It blocks until that in-flight Read has
// returned, so the device fd is never closed out from under a live read.
func (s *Stream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.pcm.Close()
}

func alsaFormat(f Format) (uint32, error) {
	switch f {
	case FormatS16LE:
		return alsa.FormatS16LE, nil
	case FormatS24LE:
		return alsa.FormatS24LE, nil
	case FormatS243LE:
		return alsa.FormatS243LE, nil
	case FormatS32LE:
		return alsa.FormatS32LE, nil
	case FormatF32LE:
		return alsa.FormatFloatLE, nil
	default:
		return 0, &ConfigError{Field: fieldFormat, Reason: "must be s16, s24_le, s24_3le, s32, or f32"}
	}
}

// isDeviceGoneErrno reports whether err (or an error it wraps) is one of the
// ALSA errnos that mean the device is missing or was removed: ENODEV, ENXIO, or
// ENOENT. It is the single source of truth for that errno set, shared by Open,
// Start, Read, and the capability query so the set cannot drift between them.
func isDeviceGoneErrno(err error) bool { return alsa.IsDeviceGone(err) }

// translateOpenError converts the errors the open-and-negotiate path can return
// into the public typed errors so callers never import internal/alsa: an
// unsupported rate becomes *BadRateError, an unsupported channel/format
// combination becomes *BadFormatError, a device held by another application
// becomes ErrDeviceInUse, and a device that is missing or was removed becomes
// ErrDeviceGone. channels and format come from the requested Config so the
// public *BadFormatError carries them. The EBUSY and device-gone mapping mirrors
// translateQueryError so Open and SupportedRates classify a busy or lost device
// the same way. Anything else is returned unchanged.
func translateOpenError(err error, channels int, format Format) error {
	var bre *alsa.BadRateError
	if errors.As(err, &bre) {
		return &BadRateError{Requested: bre.Requested, Min: bre.Min, Max: bre.Max}
	}
	var bfe *alsa.BadFormatError
	if errors.As(err, &bfe) {
		return &BadFormatError{Channels: channels, Format: format}
	}
	if errors.Is(err, unix.EBUSY) {
		return ErrDeviceInUse
	}
	if isDeviceGoneErrno(err) {
		return ErrDeviceGone
	}
	return err
}

// translateReadError maps the raw errnos an unrecoverable capture read can hit
// onto the package's typed errors, so a caller never imports internal/alsa or
// matches bare errnos to notice a disconnect. A device that disappeared
// (unplugged, disabled, or otherwise invalidated) becomes ErrDeviceGone; this
// mirrors translateQueryError so Read and SupportedRates report a lost device
// the same way. Anything else is returned unchanged.
func translateReadError(err error) error {
	if isDeviceGoneErrno(err) {
		return ErrDeviceGone
	}
	return err
}
