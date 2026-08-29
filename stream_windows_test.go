//go:build windows

package capture

import (
	"errors"
	"testing"
	"time"

	"github.com/tphakala/go-audio-capture/internal/wasapi"
)

// errUnclassifiedProbe stands in for an internal error that translateWASAPIError
// does not classify, to assert the default branch passes it through unchanged.
var errUnclassifiedProbe = errors.New("wasapi: unclassified probe error")

// fakeDevice implements the wasapiDevice seam so Open/Read/Close are testable
// with no audio hardware, mirroring fakePCM on the Linux side.
type fakeDevice struct {
	neg     wasapi.Negotiated
	negErr  error
	readFn  func() (int, bool, error)
	block   chan struct{} // closed by Close to unblock a parked Read
	parked  chan struct{} // closed by readFn once it is about to park, if non-nil
	started bool
}

func (f *fakeDevice) Negotiate(rate, channels, bits int) (wasapi.Negotiated, error) {
	if f.negErr != nil {
		return wasapi.Negotiated{}, f.negErr
	}
	n := f.neg
	if n.Rate == 0 {
		n = wasapi.Negotiated{Rate: rate, Channels: channels, Bits: bits, PeriodFrames: 480, Periods: 1, BufferFrames: 480}
	}
	return n, nil
}
func (f *fakeDevice) Start() error { f.started = true; return nil }
func (f *fakeDevice) Read(_ []byte) (frames int, discontinuity bool, err error) {
	return f.readFn()
}
func (f *fakeDevice) Close() error {
	if f.block != nil {
		close(f.block)
	}
	return nil
}

func swapOpenDevice(d wasapiDevice) func() {
	prev := openDevice
	openDevice = func(_ string) (wasapiDevice, error) { return d, nil }
	return func() { openDevice = prev }
}

func TestOpenReportsNegotiated(t *testing.T) {
	defer swapOpenDevice(&fakeDevice{readFn: func() (int, bool, error) { return 0, false, nil }})()
	s, err := Open(Config{Device: "default", Rate: 48000, Channels: 2, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	got := s.Negotiated()
	if got.Rate != 48000 || got.Channels != 2 || got.PeriodFrames != 480 || got.Periods != 1 {
		t.Errorf("Negotiated = %+v, want rate 48000, ch 2, period 480, periods 1", got)
	}
	if got.Format != FormatS16LE {
		t.Errorf("Negotiated.Format = %v, want s16", got.Format)
	}
}

func TestReadCountsDiscontinuity(t *testing.T) {
	calls := 0
	f := &fakeDevice{readFn: func() (int, bool, error) {
		calls++
		if calls == 1 {
			return 480, true, nil // discontinuity on the first read
		}
		return 480, false, nil
	}}
	defer swapOpenDevice(f)()
	s, err := Open(Config{Device: "", Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if n, err := s.Read(make([]byte, 480*2)); err != nil || n != 480 {
		t.Fatalf("Read = %d, %v; want 480, nil", n, err)
	}
	if s.Xruns() != 1 {
		t.Errorf("Xruns = %d, want 1", s.Xruns())
	}
	// A clean read does not bump the counter.
	if _, err := s.Read(make([]byte, 480*2)); err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if s.Xruns() != 1 {
		t.Errorf("Xruns after clean read = %d, want 1", s.Xruns())
	}
}

func TestCloseUnblocksRead(t *testing.T) {
	f := &fakeDevice{block: make(chan struct{}), parked: make(chan struct{})}
	f.readFn = func() (int, bool, error) {
		close(f.parked) // signal that Read has reached the park
		<-f.block       // park until Close unblocks us
		return 0, false, wasapi.ErrClosed
	}
	defer swapOpenDevice(f)()
	s, err := Open(Config{Device: "", Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, e := s.Read(make([]byte, 96)); done <- e }()
	<-f.parked // deterministically wait until Read has parked, no timing guess
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case e := <-done:
		if !errors.Is(e, ErrClosed) {
			t.Fatalf("Read after Close = %v, want ErrClosed", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return after Close")
	}
}

func TestReadAfterCloseReturnsErrClosed(t *testing.T) {
	defer swapOpenDevice(&fakeDevice{readFn: func() (int, bool, error) { return 0, false, nil }})()
	s, err := Open(Config{Device: "", Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Read(make([]byte, 96)); !errors.Is(err, ErrClosed) {
		t.Errorf("Read after Close = %v, want ErrClosed", err)
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	defer swapOpenDevice(&fakeDevice{readFn: func() (int, bool, error) { return 0, false, nil }})()
	tests := []Config{
		{Device: "", Rate: 0, Channels: 1, Format: FormatS16LE},
		{Device: "", Rate: 48000, Channels: 0, Format: FormatS16LE},
		{Device: "", Rate: 48000, Channels: 1, Format: Format(99)},
	}
	for _, cfg := range tests {
		if _, err := Open(cfg); err == nil {
			t.Errorf("Open(%+v) = nil error, want failure", cfg)
		}
	}
}

func TestOpenTranslatesNegotiateErrors(t *testing.T) {
	tests := []struct {
		name   string
		negErr error
		check  func(error) bool
	}{
		{
			"bad rate",
			&wasapi.BadRateError{Requested: 256000, Min: 44100, Max: 96000},
			func(e error) bool {
				var b *BadRateError
				return errors.As(e, &b) && b.Requested == 256000 && b.Min == 44100 && b.Max == 96000
			},
		},
		{
			"bad format",
			&wasapi.BadFormatError{Rate: 48000, Channels: 1, Bits: 16},
			func(e error) bool {
				var b *BadFormatError
				return errors.As(e, &b) && b.Rate == 48000 && b.Channels == 1 && b.Format == FormatS16LE
			},
		},
		{
			"exclusive not allowed",
			wasapi.ErrExclusiveNotAllowed,
			func(e error) bool { return errors.Is(e, ErrExclusiveNotAllowed) },
		},
		{
			"device in use",
			wasapi.ErrDeviceInUse,
			func(e error) bool { return errors.Is(e, ErrDeviceInUse) },
		},
		{
			"device gone",
			wasapi.ErrDeviceGone,
			func(e error) bool { return errors.Is(e, ErrDeviceGone) },
		},
		{
			"unclassified error passes through",
			errUnclassifiedProbe,
			func(e error) bool { return errors.Is(e, errUnclassifiedProbe) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer swapOpenDevice(&fakeDevice{negErr: tt.negErr})()
			_, err := Open(Config{Device: "", Rate: 48000, Channels: 1, Format: FormatS16LE})
			if err == nil || !tt.check(err) {
				t.Errorf("Open with %v = %v, want translated public error", tt.negErr, err)
			}
		})
	}
}
