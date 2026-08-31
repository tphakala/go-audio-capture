//go:build windows && (amd64 || arm64)

package capture

import (
	"errors"
	"sync/atomic"

	"github.com/tphakala/go-audio-capture/internal/wasapi"
)

// wasapiDevice is the WASAPI seam that Stream drives; *wasapi.Client satisfies
// it, and tests inject a fake. openDevice is a package var so tests can
// substitute a hardware-free implementation. It mirrors the Linux pcm seam.
type wasapiDevice interface {
	Negotiate(rate, channels int, sf wasapi.SampleFormat) (wasapi.Negotiated, error)
	Start() error
	Read(buf []byte) (frames int, discontinuity bool, err error)
	Close() error
}

var openDevice = func(id string) (wasapiDevice, error) {
	return wasapi.Open(id)
}

// Stream is an open capture stream. Read is single-consumer; Close may be
// called from another goroutine to unblock a parked Read.
type Stream struct {
	dev        wasapiDevice
	cfg        Config
	frameBytes int
	xruns      atomic.Uint64
	closed     atomic.Bool
}

// Open configures and opens an exclusive-mode capture stream. Config.Device is a
// WASAPI endpoint-id string, or "" / "default" for the default capture endpoint.
// The exact requested rate, channel count, and format are negotiated or Open
// fails with a typed error (*BadRateError, *BadFormatError, ErrExclusiveNotAllowed,
// or ErrDeviceInUse). The returned stream is prepared but not started; call Start
// before Read.
func Open(cfg Config) (*Stream, error) {
	if cfg.Rate <= 0 {
		return nil, &ConfigError{Field: "rate", Reason: "must be positive"}
	}
	if cfg.Channels < 1 {
		return nil, &ConfigError{Field: "channels", Reason: "must be at least 1"}
	}
	sf, err := waFormat(cfg.Format)
	if err != nil {
		return nil, err
	}

	dev, err := openDevice(cfg.Device)
	if err != nil {
		return nil, translateWASAPIError(err, cfg)
	}
	n, err := dev.Negotiate(cfg.Rate, cfg.Channels, sf)
	if err != nil {
		_ = dev.Close()
		return nil, translateWASAPIError(err, cfg)
	}
	return &Stream{
		dev: dev,
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

// Negotiated returns the configuration the endpoint accepted.
func (s *Stream) Negotiated() Config { return s.cfg }

// Start begins capture. Call it once before the first Read.
func (s *Stream) Start() error {
	if s.closed.Load() {
		return ErrClosed
	}
	if err := s.dev.Start(); err != nil {
		// A concurrent Close surfaces as ErrClosed; an invalidated endpoint maps
		// to ErrDeviceGone, mirroring Open and Read so a caller classifies a lost
		// device the same way across the whole lifecycle.
		if s.closed.Load() || errors.Is(err, wasapi.ErrClosed) {
			return ErrClosed
		}
		return translateWASAPIError(err, s.cfg)
	}
	return nil
}

// Read fills buf with whole interleaved frames and returns the number of frames
// read. It blocks until at least one frame is available. A DATA_DISCONTINUITY is
// counted as an xrun (Xruns is bumped) but is not an error. Read returns ErrClosed
// when the stream is closed and ErrDeviceGone when the endpoint is invalidated;
// any other device error is returned as-is.
func (s *Stream) Read(buf []byte) (int, error) {
	if s.closed.Load() {
		return 0, ErrClosed
	}
	if len(buf)/s.frameBytes == 0 {
		return 0, nil
	}
	n, discontinuity, err := s.dev.Read(buf)
	if err != nil {
		if s.closed.Load() || errors.Is(err, wasapi.ErrClosed) {
			return 0, ErrClosed
		}
		return 0, translateWASAPIError(err, s.cfg)
	}
	if discontinuity {
		s.xruns.Add(1)
	}
	return n, nil
}

// Xruns returns the number of capture discontinuities seen so far.
func (s *Stream) Xruns() uint64 { return s.xruns.Load() }

// Close stops and closes the stream. It is idempotent and unblocks a Read
// currently parked in the driver.
func (s *Stream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.dev.Close()
}

// waFormat maps the public Format to a WASAPI sample format.
func waFormat(f Format) (wasapi.SampleFormat, error) {
	switch f {
	case FormatS16LE:
		return wasapi.SampleS16, nil
	case FormatS32LE:
		return wasapi.SampleS32, nil
	case FormatF32LE:
		return wasapi.SampleF32, nil
	default:
		return 0, &ConfigError{Field: "format", Reason: "must be s16, s32, or f32"}
	}
}

// translateWASAPIError converts internal/wasapi errors into the public error
// types so callers never import internal/wasapi. cfg supplies the requested
// channel count and format for BadFormatError.
func translateWASAPIError(err error, cfg Config) error {
	var bre *wasapi.BadRateError
	if errors.As(err, &bre) {
		return &BadRateError{Requested: bre.Requested, Min: bre.Min, Max: bre.Max}
	}
	var bfe *wasapi.BadFormatError
	if errors.As(err, &bfe) {
		return &BadFormatError{Rate: bfe.Rate, Channels: bfe.Channels, Format: cfg.Format}
	}
	switch {
	case errors.Is(err, wasapi.ErrExclusiveNotAllowed):
		return ErrExclusiveNotAllowed
	case errors.Is(err, wasapi.ErrDeviceInUse):
		return ErrDeviceInUse
	case errors.Is(err, wasapi.ErrDeviceGone):
		return ErrDeviceGone
	default:
		return err
	}
}
