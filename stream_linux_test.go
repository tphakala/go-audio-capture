//go:build linux

package capture

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tphakala/go-audio-capture/internal/alsa"
)

// devID is the device string reused across the stream tests.
const devID = "hw:1,0"

func TestParseDeviceID(t *testing.T) {
	tests := []struct {
		in        string
		card, dev int
		wantErr   bool
	}{
		{"hw:1,0", 1, 0, false},
		{"1,0", 1, 0, false},
		{"hw:1", 1, 0, false},
		{"hw:0,3", 0, 3, false},
		{"  hw:2,1 ", 2, 1, false},
		{"", 0, 0, true},
		{"hw:", 0, 0, true},
		{"x,y", 0, 0, true},
		{"hw:1,x", 0, 0, true},
		{"-1,0", 0, 0, true},
	}
	for _, tt := range tests {
		card, dev, err := parseDeviceID(tt.in)
		if tt.wantErr {
			var bde *BadDeviceError
			if !errors.As(err, &bde) {
				t.Errorf("parseDeviceID(%q) err = %v, want *BadDeviceError", tt.in, err)
			}
			continue
		}
		if err != nil || card != tt.card || dev != tt.dev {
			t.Errorf("parseDeviceID(%q) = (%d,%d,%v), want (%d,%d,nil)", tt.in, card, dev, err, tt.card, tt.dev)
		}
	}
}

// fakePCM implements the pcm seam so Open/Read/Close are testable with no
// hardware.
type fakePCM struct {
	readFn    func() (int, error)
	recoverFn func(error) error // overrides Recover when set
	recovered int
	block     chan struct{} // closed by Close to unblock a parked ReadI
}

func (f *fakePCM) Negotiate(rate, channels int, format uint32, periodFrames, periods int) (alsa.Negotiated, error) {
	return alsa.Negotiated{
		Rate: rate, Channels: channels, Format: format,
		PeriodFrames: periodFrames, Periods: periods, BufferFrames: periodFrames * periods,
	}, nil
}
func (f *fakePCM) Start() error                       { return nil }
func (f *fakePCM) ReadI(_ []byte, _ int) (int, error) { return f.readFn() }
func (f *fakePCM) Recover(err error) error {
	if f.recoverFn != nil {
		return f.recoverFn(err)
	}
	if errors.Is(err, unix.EPIPE) {
		f.recovered++
		return nil
	}
	return err
}
func (f *fakePCM) Close() error {
	if f.block != nil {
		close(f.block)
	}
	return nil
}

func swapOpenPCM(p pcm) func() {
	prev := openPCM
	openPCM = func(_, _ int) (pcm, error) { return p, nil }
	return func() { openPCM = prev }
}

func TestOpenReportsNegotiated(t *testing.T) {
	defer swapOpenPCM(&fakePCM{readFn: func() (int, error) { return 0, nil }})()
	s, err := Open(Config{Device: "hw:2,1", Rate: 48000, Channels: 2, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	got := s.Negotiated()
	// Defaults: PeriodFrames = Rate/50 (20 ms) = 960; Periods = 4.
	if got.Rate != 48000 || got.Channels != 2 || got.PeriodFrames != 960 || got.Periods != 4 {
		t.Errorf("Negotiated = %+v, want rate 48000, ch 2, period 960, periods 4", got)
	}
	if got.Format != FormatS16LE {
		t.Errorf("Negotiated.Format = %v, want s16", got.Format)
	}
}

func TestReadRecoversXrun(t *testing.T) {
	calls := 0
	f := &fakePCM{readFn: func() (int, error) {
		calls++
		if calls == 1 {
			return 0, unix.EPIPE // first read overruns
		}
		return 480, nil // retry succeeds
	}}
	defer swapOpenPCM(f)()
	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	n, err := s.Read(make([]byte, 480*2))
	if err != nil || n != 480 {
		t.Fatalf("Read = %d, %v; want 480, nil", n, err)
	}
	if s.Xruns() != 1 {
		t.Errorf("Xruns = %d, want 1", s.Xruns())
	}
	if f.recovered != 1 {
		t.Errorf("recovered = %d, want 1", f.recovered)
	}
}

func TestCloseUnblocksRead(t *testing.T) {
	f := &fakePCM{block: make(chan struct{})}
	f.readFn = func() (int, error) {
		<-f.block // park until Close unblocks us
		return 0, unix.EBADF
	}
	defer swapOpenPCM(f)()
	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, e := s.Read(make([]byte, 96)); done <- e }()
	time.Sleep(20 * time.Millisecond) // let Read park in ReadI
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
	defer swapOpenPCM(&fakePCM{readFn: func() (int, error) { return 0, nil }})()
	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = s.Close()
	if _, err := s.Read(make([]byte, 96)); !errors.Is(err, ErrClosed) {
		t.Errorf("Read after Close = %v, want ErrClosed", err)
	}
}

// TestReadMapsRecoverEBADFToErrClosed covers the path where a concurrent Close
// makes Recover's own ioctls fail with EBADF: Read must report ErrClosed rather
// than leaking the raw driver error.
func TestReadMapsRecoverEBADFToErrClosed(t *testing.T) {
	fp := &fakePCM{
		readFn:    func() (int, error) { return 0, unix.ESTRPIPE }, // triggers Recover
		recoverFn: func(error) error { return &recoverError{unix.EBADF} },
	}
	defer swapOpenPCM(fp)()
	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Read(make([]byte, 96)); !errors.Is(err, ErrClosed) {
		t.Errorf("Read with Recover EBADF = %v, want ErrClosed", err)
	}
}

// recoverError wraps an errno the way alsa's ioctlError does, so errors.Is unwraps
// to the underlying errno.
type recoverError struct{ err error }

func (e *recoverError) Error() string { return "recover: " + e.err.Error() }
func (e *recoverError) Unwrap() error { return e.err }

// TestReadMapsDeviceGoneToErrDeviceGone covers a surprise disconnect mid-stream:
// an unrecoverable read errno signalling the device disappeared must surface as
// ErrDeviceGone (not a raw driver errno), so a caller can retire the device with
// errors.Is(err, ErrDeviceGone). Each errno is wrapped like alsa's ioctlError to
// prove translation unwraps through the driver error, not just a bare errno.
func TestReadMapsDeviceGoneToErrDeviceGone(t *testing.T) {
	for _, errno := range []unix.Errno{unix.ENODEV, unix.ENXIO, unix.ENOENT} {
		t.Run(errno.Error(), func(t *testing.T) {
			fp := &fakePCM{readFn: func() (int, error) { return 0, &recoverError{errno} }}
			defer swapOpenPCM(fp)()
			s, err := Open(Config{Device: devID, Rate: 48000, Channels: 1, Format: FormatS16LE})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = s.Close() }()
			if _, err := s.Read(make([]byte, 96)); !errors.Is(err, ErrDeviceGone) {
				t.Errorf("Read with %v = %v, want ErrDeviceGone", errno, err)
			}
		})
	}
}

// TestReadPassesThroughNonDeviceGoneError guards the other half of
// translateReadError's contract: an unrecoverable errno that is NOT a
// device-gone code (EIO is a genuine read fault on a device still present) must
// surface unchanged, never relabelled ErrDeviceGone. Without this, widening the
// mapping (a stray default: return ErrDeviceGone) would slip through green.
func TestReadPassesThroughNonDeviceGoneError(t *testing.T) {
	fp := &fakePCM{readFn: func() (int, error) { return 0, &recoverError{unix.EIO} }}
	defer swapOpenPCM(fp)()
	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	_, err = s.Read(make([]byte, 96))
	if errors.Is(err, ErrDeviceGone) {
		t.Errorf("Read with EIO = %v, want it passed through, not ErrDeviceGone", err)
	}
	if !errors.Is(err, unix.EIO) {
		t.Errorf("Read with EIO = %v, want it to unwrap to EIO", err)
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	defer swapOpenPCM(&fakePCM{readFn: func() (int, error) { return 0, nil }})()
	tests := []Config{
		{Device: "nonsense", Rate: 48000, Channels: 1, Format: FormatS16LE},
		{Device: devID, Rate: 0, Channels: 1, Format: FormatS16LE},
		{Device: devID, Rate: 48000, Channels: 0, Format: FormatS16LE},
		{Device: devID, Rate: 48000, Channels: 1, Format: Format(99)},
	}
	for _, cfg := range tests {
		if _, err := Open(cfg); err == nil {
			t.Errorf("Open(%+v) = nil error, want failure", cfg)
		}
	}
}
