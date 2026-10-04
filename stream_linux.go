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

// Recovery budgets for one Read call. Read returns as soon as frames arrive, so
// these count recoveries within one gap in the data and a stream with periodic
// xruns between good reads never reaches them. Real sequences inside one gap are
// 2-3 recoveries long, so 8 leaves margin. One stall restart is tried; a second
// EIO in the same gap means the restart did not help and the caller must reopen.
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
		// A device unplugged between Open and Start surfaces as ErrDeviceGone
		// (ENODEV, or EBADFD confirmed by the probe), as it does at Open and Read.
		return s.terminalError(err)
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
		// Only errnos Recover can act on count against the budget; anything
		// else (a Close, an unplug, a state error) is classified at once.
		if s.closed.Load() || !alsa.IsRecoverable(err) {
			return 0, s.terminalError(err)
		}
		stall := errors.Is(err, unix.EIO)
		if recoveries == maxRecoveriesWithoutData || (stall && stalls == maxStallRestarts) {
			return 0, &StallError{Recoveries: recoveries, Err: err}
		}
		if stall {
			stalls++
		}
		recoveries++
		if rerr := s.pcm.Recover(err); rerr != nil {
			// Recover returns an unrecoverable errno unchanged, and a concurrent
			// Close can fail its own ioctls with EBADF.
			return 0, s.terminalError(rerr)
		}
		s.xruns.Add(1)
	}
}

// terminalError maps an error that ends Start or Read onto the public errors.
// A Close always wins: Close sets s.closed before pcm.Close, which covers the
// EBADFD that Close's DROP gives a parked read, and EBADF is a closed PCM
// refusing the ioctl. Any other EBADFD is either an unplug (the kernel wakes a
// parked reader with it once the PCM is DISCONNECTED) or an ordinary state
// error, so one PVERSION probe decides; s.closed is checked again after it
// because a Close racing the probe fails it with EBADF. Device-gone errnos
// become ErrDeviceGone; anything else is returned unchanged.
func (s *Stream) terminalError(err error) error {
	if s.closed.Load() || errors.Is(err, unix.EBADF) {
		return ErrClosed
	}
	if errors.Is(err, unix.EBADFD) {
		gone := deviceDisconnected(s.pcm)
		if s.closed.Load() {
			return ErrClosed
		}
		if gone {
			return ErrDeviceGone
		}
		return err
	}
	if alsa.IsDeviceGone(err) {
		return ErrDeviceGone
	}
	return err
}

// deviceDisconnected reports whether a PVERSION probe shows the device gone:
// ENODEV/ENXIO/ENOENT (the card's file operations were shut down) or EBADFD (the
// PCM alone is DISCONNECTED). EBADF (closed) and success are not disconnects.
func deviceDisconnected(p pcm) bool {
	perr := p.Probe()
	return perr != nil && (alsa.IsDeviceGone(perr) || errors.Is(perr, unix.EBADFD))
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
	if alsa.IsDeviceGone(err) {
		return ErrDeviceGone
	}
	return err
}
