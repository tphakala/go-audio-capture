//go:build linux

package capture

import (
	"errors"
	"strconv"
	"strings"
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
	Close() error
}

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
// Start before Read.
func Open(cfg Config) (*Stream, error) {
	card, device, err := parseDeviceID(cfg.Device)
	if err != nil {
		return nil, err
	}
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
	periodFrames := cfg.PeriodFrames
	if periodFrames == 0 {
		periodFrames = cfg.Rate / 50 // 20 ms
	}
	periods := cfg.Periods
	if periods == 0 {
		periods = 4
	}

	p, err := openPCM(card, device)
	if err != nil {
		return nil, err
	}
	n, err := p.Negotiate(cfg.Rate, cfg.Channels, format, periodFrames, periods)
	if err != nil {
		_ = p.Close()
		return nil, translateBadRate(err)
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
		return err
	}
	return nil
}

// Read fills buf with whole interleaved frames and returns the number of frames
// read. It blocks until at least one period is available. An overrun (xrun) is
// recovered internally (the counter is bumped and the read retried). Read
// returns ErrClosed when the stream is closed and ErrDeviceGone when the device
// disappears (e.g. a USB capture device unplugged mid-stream); any other
// unrecoverable error is returned unchanged. Any returned error leaves the
// stream unusable (a short read, fewer frames than requested, is not an error and
// returns a nil error): the caller must Close it (Read does not release the
// device fd on its own) and, to resume, Open a new stream.
func (s *Stream) Read(buf []byte) (int, error) {
	if s.closed.Load() {
		return 0, ErrClosed
	}
	frames := len(buf) / s.frameBytes
	if frames == 0 {
		return 0, nil
	}
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
		if rerr := s.pcm.Recover(err); rerr != nil {
			// A concurrent Close can fail Recover's own ioctls with EBADF;
			// surface that as a clean close rather than a raw driver error.
			if s.closed.Load() || errors.Is(rerr, unix.EBADF) {
				return 0, ErrClosed
			}
			// Unrecoverable: Recover returns the error unchanged. Map a
			// disappeared device onto ErrDeviceGone so a caller can classify a
			// surprise unplug with errors.Is instead of matching bare errnos.
			return 0, translateReadError(rerr)
		}
		s.xruns.Add(1)
	}
}

// Xruns returns the number of buffer overruns recovered so far.
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

// parseDeviceID accepts "hw:card,device", "card,device", or "hw:card" (device
// defaulting to 0) and returns the card and device numbers.
func parseDeviceID(s string) (card, device int, err error) {
	orig := s
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "hw:")
	cardStr, devStr, hasComma := strings.Cut(s, ",")
	card, err = strconv.Atoi(strings.TrimSpace(cardStr))
	if err != nil {
		return 0, 0, &BadDeviceError{Value: orig}
	}
	if hasComma {
		device, err = strconv.Atoi(strings.TrimSpace(devStr))
		if err != nil {
			return 0, 0, &BadDeviceError{Value: orig}
		}
	}
	if card < 0 || device < 0 {
		return 0, 0, &BadDeviceError{Value: orig}
	}
	return card, device, nil
}

func alsaFormat(f Format) (uint32, error) {
	switch f {
	case FormatS16LE:
		return alsa.FormatS16LE, nil
	case FormatS32LE:
		return alsa.FormatS32LE, nil
	case FormatF32LE:
		return alsa.FormatFloatLE, nil
	default:
		return 0, &ConfigError{Field: "format", Reason: "must be s16, s32, or f32"}
	}
}

// translateBadRate converts the internal ALSA bad-rate error into the public
// *BadRateError so callers never import internal/alsa.
func translateBadRate(err error) error {
	var bre *alsa.BadRateError
	if errors.As(err, &bre) {
		return &BadRateError{Requested: bre.Requested, Min: bre.Min, Max: bre.Max}
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
	switch {
	case errors.Is(err, unix.ENODEV), errors.Is(err, unix.ENXIO), errors.Is(err, unix.ENOENT):
		return ErrDeviceGone
	default:
		return err
	}
}
