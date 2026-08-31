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
	negErr    error // when set, Negotiate returns it
	startErr  error // when set, Start returns it
	readFn    func() (int, error)
	recoverFn func(error) error // overrides Recover when set
	recovered int
	block     chan struct{} // closed by Close to unblock a parked ReadI
}

func (f *fakePCM) Negotiate(rate, channels int, format uint32, periodFrames, periods int) (alsa.Negotiated, error) {
	if f.negErr != nil {
		return alsa.Negotiated{}, f.negErr
	}
	return alsa.Negotiated{
		Rate: rate, Channels: channels, Format: format,
		PeriodFrames: periodFrames, Periods: periods, BufferFrames: periodFrames * periods,
	}, nil
}
func (f *fakePCM) Start() error                       { return f.startErr }
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

// swapOpenPCMError makes openPCM itself fail (device absent, removed, or busy at
// open time), exercising the open-failure arm of Open that a fake pcm cannot.
func swapOpenPCMError(err error) func() {
	prev := openPCM
	openPCM = func(_, _ int) (pcm, error) { return nil, err }
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

func TestAlsaFormat(t *testing.T) {
	tests := []struct {
		f       Format
		want    uint32
		wantErr bool
	}{
		{FormatS16LE, alsa.FormatS16LE, false},
		{FormatS32LE, alsa.FormatS32LE, false},
		{FormatF32LE, alsa.FormatFloatLE, false},
		{Format(99), 0, true},
	}
	for _, tt := range tests {
		got, err := alsaFormat(tt.f)
		if tt.wantErr {
			if err == nil {
				t.Errorf("alsaFormat(%v) err = nil, want error", tt.f)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("alsaFormat(%v) = (%d, %v), want (%d, nil)", tt.f, got, err, tt.want)
		}
	}
}

// TestOpenFloat32Negotiated confirms Open accepts FormatF32LE end to end: it must
// not error (which exercises alsaFormat's F32LE arm; a missing arm fails Open),
// and the negotiated config echoes f32 at 4 bytes per sample.
func TestOpenFloat32Negotiated(t *testing.T) {
	defer swapOpenPCM(&fakePCM{readFn: func() (int, error) { return 0, nil }})()
	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 1, Format: FormatF32LE})
	if err != nil {
		t.Fatalf("Open f32: %v", err)
	}
	defer func() { _ = s.Close() }()
	if got := s.Negotiated().Format; got != FormatF32LE {
		t.Errorf("Negotiated.Format = %v, want f32", got)
	}
	if got := s.Negotiated().Format.BytesPerSample(); got != 4 {
		t.Errorf("f32 BytesPerSample = %d, want 4", got)
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

// TestOpenMapsUnsupportedFormat covers the ZOOM AMS-24 symptom from issue #9:
// when Negotiate rejects the channel/format combination (its initial HW_REFINE
// EINVALs), Open must return a typed *BadFormatError carrying the requested
// channels and format, not leak the raw "alsa: HW_REFINE: invalid argument".
func TestOpenMapsUnsupportedFormat(t *testing.T) {
	fp := &fakePCM{negErr: &alsa.BadFormatError{Channels: 1, Format: alsa.FormatS16LE}}
	defer swapOpenPCM(fp)()
	_, err := Open(Config{Device: devID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	var bfe *BadFormatError
	if !errors.As(err, &bfe) {
		t.Fatalf("Open with unsupported format = %v, want *BadFormatError", err)
	}
	if bfe.Channels != 1 || bfe.Format != FormatS16LE || bfe.Rate != 0 {
		t.Errorf("BadFormatError = %+v, want {Rate:0, Channels:1, Format:s16}", bfe)
	}
}

// TestOpenMapsBadRate confirms the existing rate path still yields a public
// *BadRateError after the translate function was broadened.
func TestOpenMapsBadRate(t *testing.T) {
	fp := &fakePCM{negErr: &alsa.BadRateError{Requested: 256000, Min: 44100, Max: 96000}}
	defer swapOpenPCM(fp)()
	_, err := Open(Config{Device: devID, Rate: 256000, Channels: 2, Format: FormatS32LE})
	var bre *BadRateError
	if !errors.As(err, &bre) {
		t.Fatalf("Open with bad rate = %v, want *BadRateError", err)
	}
	if bre.Requested != 256000 || bre.Min != 44100 || bre.Max != 96000 {
		t.Errorf("BadRateError = %+v, want {256000, 44100, 96000}", bre)
	}
}

// TestOpenMapsDeviceGone covers a device removed between enumeration and Open:
// a device-gone errno from Negotiate must surface as ErrDeviceGone (issue #10),
// matching what Read and SupportedRates already do. Each errno is wrapped like
// alsa's ioctlError to prove translation unwraps through the driver error.
func TestOpenMapsDeviceGone(t *testing.T) {
	for _, errno := range []unix.Errno{unix.ENODEV, unix.ENXIO, unix.ENOENT} {
		t.Run(errno.Error(), func(t *testing.T) {
			fp := &fakePCM{negErr: &recoverError{errno}}
			defer swapOpenPCM(fp)()
			_, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS32LE})
			if !errors.Is(err, ErrDeviceGone) {
				t.Errorf("Open with %v = %v, want ErrDeviceGone", errno, err)
			}
		})
	}
}

// TestOpenPassesThroughOtherError guards the other half of translateOpenError:
// an error that is neither a bad rate, nor a bad format, nor device-gone (EIO is
// a genuine fault on a present device) must surface unchanged, never relabelled.
func TestOpenPassesThroughOtherError(t *testing.T) {
	fp := &fakePCM{negErr: &recoverError{unix.EIO}}
	defer swapOpenPCM(fp)()
	_, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS32LE})
	if errors.Is(err, ErrDeviceGone) {
		t.Errorf("Open with EIO = %v, want it passed through, not ErrDeviceGone", err)
	}
	if !errors.Is(err, unix.EIO) {
		t.Errorf("Open with EIO = %v, want it to unwrap to EIO", err)
	}
}

// TestOpenMapsOpenTimeDeviceGone covers a device that is absent or removed at
// OPEN time (the PCM node is missing or the driver reports the card gone): the
// failure comes from openPCM, not Negotiate, and must still surface as
// ErrDeviceGone so a caller can classify it the same way as a mid-stream loss.
// This is the open-failure path that the Negotiate-injected tests cannot reach.
func TestOpenMapsOpenTimeDeviceGone(t *testing.T) {
	for _, errno := range []unix.Errno{unix.ENODEV, unix.ENXIO, unix.ENOENT} {
		t.Run(errno.Error(), func(t *testing.T) {
			defer swapOpenPCMError(&recoverError{errno})()
			_, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS32LE})
			if !errors.Is(err, ErrDeviceGone) {
				t.Errorf("Open with openPCM %v = %v, want ErrDeviceGone", errno, err)
			}
		})
	}
}

// TestOpenMapsOpenTimeBusy covers a device held exclusively by another
// application at open time: openPCM fails with EBUSY, which Open must classify
// as ErrDeviceInUse, matching what SupportedRates already returns for the same
// condition.
func TestOpenMapsOpenTimeBusy(t *testing.T) {
	defer swapOpenPCMError(&recoverError{unix.EBUSY})()
	_, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS32LE})
	if !errors.Is(err, ErrDeviceInUse) {
		t.Errorf("Open with openPCM EBUSY = %v, want ErrDeviceInUse", err)
	}
}

// TestOpenPassesThroughOpenTimeOtherError guards the open-failure arm's default:
// a non-classifiable open error (EACCES after both O_RDWR and O_RDONLY fail) must
// surface unchanged, not be relabelled device-gone or device-in-use.
func TestOpenPassesThroughOpenTimeOtherError(t *testing.T) {
	defer swapOpenPCMError(&recoverError{unix.EACCES})()
	_, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS32LE})
	if errors.Is(err, ErrDeviceGone) || errors.Is(err, ErrDeviceInUse) {
		t.Errorf("Open with openPCM EACCES = %v, want it passed through", err)
	}
	if !errors.Is(err, unix.EACCES) {
		t.Errorf("Open with openPCM EACCES = %v, want it to unwrap to EACCES", err)
	}
}

// TestStartSucceeds covers Start's success path (no injected error): it must
// return nil and not be tripped by the error-classification arms.
func TestStartSucceeds(t *testing.T) {
	defer swapOpenPCM(&fakePCM{})()
	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS32LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Start(); err != nil {
		t.Errorf("Start = %v, want nil", err)
	}
}

// TestStartAfterCloseReturnsErrClosed covers Start's top closed-guard: starting a
// closed stream returns ErrClosed without touching the device.
func TestStartAfterCloseReturnsErrClosed(t *testing.T) {
	defer swapOpenPCM(&fakePCM{startErr: &recoverError{unix.ENODEV}})()
	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS32LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = s.Close()
	if err := s.Start(); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after Close = %v, want ErrClosed (even with a device-gone startErr)", err)
	}
}

// TestStartMapsDeviceGone covers a device unplugged between Open and Start: the
// device-gone errno from the START ioctl must surface as ErrDeviceGone (issue
// #10) so the whole lifecycle classifies a lost device the same way.
func TestStartMapsDeviceGone(t *testing.T) {
	for _, errno := range []unix.Errno{unix.ENODEV, unix.ENXIO, unix.ENOENT} {
		t.Run(errno.Error(), func(t *testing.T) {
			fp := &fakePCM{startErr: &recoverError{errno}}
			defer swapOpenPCM(fp)()
			s, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS32LE})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = s.Close() }()
			if err := s.Start(); !errors.Is(err, ErrDeviceGone) {
				t.Errorf("Start with %v = %v, want ErrDeviceGone", errno, err)
			}
		})
	}
}

// TestStartMapsEBADFToErrClosed keeps the concurrent-Close race: an EBADF from
// START (Close won the fd) is reported as ErrClosed, not a raw errno.
func TestStartMapsEBADFToErrClosed(t *testing.T) {
	fp := &fakePCM{startErr: &recoverError{unix.EBADF}}
	defer swapOpenPCM(fp)()
	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS32LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Start(); !errors.Is(err, ErrClosed) {
		t.Errorf("Start with EBADF = %v, want ErrClosed", err)
	}
}

// TestStartPassesThroughOtherError guards Start's default arm: a non-device-gone,
// non-EBADF error (EIO on a present device) must surface unchanged.
func TestStartPassesThroughOtherError(t *testing.T) {
	fp := &fakePCM{startErr: &recoverError{unix.EIO}}
	defer swapOpenPCM(fp)()
	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS32LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	err = s.Start()
	if errors.Is(err, ErrDeviceGone) || errors.Is(err, ErrClosed) {
		t.Errorf("Start with EIO = %v, want it passed through", err)
	}
	if !errors.Is(err, unix.EIO) {
		t.Errorf("Start with EIO = %v, want it to unwrap to EIO", err)
	}
}

// TestIsDeviceGoneErrno pins the shared errno set directly: the three device-gone
// codes match (bare and wrapped), and a present-device fault (EIO) does not.
func TestIsDeviceGoneErrno(t *testing.T) {
	for _, errno := range []unix.Errno{unix.ENODEV, unix.ENXIO, unix.ENOENT} {
		if !isDeviceGoneErrno(errno) {
			t.Errorf("isDeviceGoneErrno(%v) = false, want true", errno)
		}
		if !isDeviceGoneErrno(&recoverError{errno}) {
			t.Errorf("isDeviceGoneErrno(wrapped %v) = false, want true", errno)
		}
	}
	if isDeviceGoneErrno(unix.EIO) {
		t.Error("isDeviceGoneErrno(EIO) = true, want false")
	}
	if isDeviceGoneErrno(nil) {
		t.Error("isDeviceGoneErrno(nil) = true, want false")
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
