//go:build linux

package capture

import (
	"errors"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// errLoopGuard is returned by a fake after loopGuardReads reads, so a Read that
// loops without bound fails with a clear error instead of hanging the test. A
// test asserts it never sees it.
var errLoopGuard = errors.New("test: loop guard tripped")

const loopGuardReads = 1000

// openLifecycleStream opens a stream on fp with a 1-channel S16 config (2 bytes
// per frame) and registers Close for cleanup.
func openLifecycleStream(t *testing.T, fp *fakePCM) *Stream {
	t.Helper()
	withOpenPCM(t, func(_, _ int) (pcm, error) { return fp, nil })
	s, err := Open(Config{Device: hwAddrCard1, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// guardedRead wraps fn so the loop guard trips after loopGuardReads calls.
func guardedRead(fn func(call int) (int, error)) func() (int, error) {
	calls := 0
	return func() (int, error) {
		calls++
		if calls > loopGuardReads {
			return 0, errLoopGuard
		}
		return fn(calls)
	}
}

// okRecover lets Recover succeed for every recoverable errno and counts calls.
func okRecover(counts *[]error) func(error) error {
	return func(err error) error {
		if errors.Is(err, errLoopGuard) {
			return err // not recoverable: let a runaway Read fail instead of spin
		}
		*counts = append(*counts, err)
		return nil
	}
}

// runReads drives successive Read calls on one stream, each preceded by the
// given error events. Only the last Read may fail; wantRec is the Recoveries of
// that failing Read, and wantXruns the total recoveries across all Reads.
func runReads(t *testing.T, reads [][]error, wantFail bool, wantRec int, wantXruns uint64) {
	t.Helper()
	var queue []error // pending error events for the current Read, then data
	var recovered []error
	fp := &fakePCM{recoverFn: okRecover(&recovered)}
	fp.readFn = guardedRead(func(int) (int, error) {
		if len(queue) == 0 {
			return 480, nil
		}
		e := queue[0]
		queue = queue[1:]
		return 0, e
	})
	s := openLifecycleStream(t, fp)
	for i, errs := range reads {
		queue = errs
		n, err := s.Read(make([]byte, 480*2))
		if errors.Is(err, errLoopGuard) {
			t.Fatalf("read %d: looped without bound", i)
		}
		if wantFail && i == len(reads)-1 {
			var se *StallError
			if !errors.As(err, &se) || !errors.Is(err, ErrDeviceStalled) {
				t.Fatalf("read %d: err = %v, want ErrDeviceStalled", i, err)
			}
			if se.Recoveries != wantRec {
				t.Errorf("Recoveries = %d, want %d", se.Recoveries, wantRec)
			}
			// The StallError carries the errno that exhausted the budget.
			if last := errs[len(errs)-1]; !errors.Is(err, last) {
				t.Errorf("err = %v, want it to unwrap to %v", err, last)
			}
			break
		}
		if err != nil || n != 480 {
			t.Fatalf("read %d (%d errs): = %d, %v; want 480, nil", i, len(errs), n, err)
		}
	}
	if s.Xruns() != wantXruns || uint64(len(recovered)) != wantXruns {
		t.Errorf("Xruns = %d, Recover calls = %d, want %d each", s.Xruns(), len(recovered), wantXruns)
	}
}

// TestReadRecoveryBudgetIsPerDataGap pins that both budgets reset whenever a
// Read delivers frames: a stream with periodic xruns or stalls between
// successful reads never hits the cap, while one data gap that exhausts either
// budget fails.
func TestReadRecoveryBudgetIsPerDataGap(t *testing.T) {
	xrun7 := slices.Repeat([]error{unix.EPIPE}, 7)
	cases := []struct {
		name      string
		reads     [][]error
		wantFail  bool
		wantRec   int
		wantXruns uint64
	}{
		{"stall then data", [][]error{{unix.EIO}}, false, 0, 1},
		{"xrun x7 then data, five times", slices.Repeat([][]error{xrun7}, 5), false, 0, 35},
		{"stall then data, ten times", slices.Repeat([][]error{{unix.EIO}}, 10), false, 0, 10},
		{"stall, xrun x6, data", [][]error{append([]error{unix.EIO}, xrun7[:6]...)}, false, 0, 7},
		{"exactly the cap", [][]error{slices.Repeat([]error{unix.EPIPE}, maxRecoveriesWithoutData)}, false, 0, 8},
		{"one over the cap", [][]error{slices.Repeat([]error{unix.EPIPE}, maxRecoveriesWithoutData+1)}, true, 8, 8},
		{"repeated stall", [][]error{{unix.EIO, unix.EIO}}, true, 1, 1},
		{"second stall in one gap", [][]error{{unix.EIO, unix.EPIPE, unix.EIO}}, true, 2, 2},
		{"suspends count as recoveries", [][]error{slices.Repeat([]error{unix.ESTRPIPE}, maxRecoveriesWithoutData+1)}, true, 8, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { runReads(t, tc.reads, tc.wantFail, tc.wantRec, tc.wantXruns) })
	}
}

// TestReadParkedUnplugIsErrDeviceGone is #17: a reader parked in READI_FRAMES
// is woken with EBADFD when the device is unplugged, and Read must classify it
// with one probe instead of returning the raw errno.
func TestReadParkedUnplugIsErrDeviceGone(t *testing.T) {
	for _, tt := range []struct {
		name     string
		probe    error
		wantGone bool
	}{
		{"ENODEV", unix.ENODEV, true},
		{"ENXIO", unix.ENXIO, true},
		{"ENOENT", unix.ENOENT, true},
		{"EBADFD", unix.EBADFD, true},
		{"probe ok", nil, false},
		{"probe EIO", unix.EIO, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var recovered []error
			probeErr := tt.probe
			fp := &fakePCM{recoverFn: okRecover(&recovered)}
			fp.readFn = guardedRead(func(int) (int, error) { return 0, unix.EBADFD })
			if probeErr != nil {
				fp.probeFn = func() error { return &recoverError{probeErr} }
			}
			s := openLifecycleStream(t, fp)
			_, err := s.Read(make([]byte, 96))
			if tt.wantGone {
				if !errors.Is(err, ErrDeviceGone) {
					t.Errorf("Read = %v, want ErrDeviceGone", err)
				}
			} else if !errors.Is(err, unix.EBADFD) || errors.Is(err, ErrDeviceGone) || errors.Is(err, ErrClosed) {
				t.Errorf("Read = %v, want the original EBADFD", err)
			}
			if fp.probes != 1 || len(recovered) != 0 {
				t.Errorf("probes = %d, Recover calls = %d, want 1 and 0", fp.probes, len(recovered))
			}
		})
	}
}

func TestReadRecoverEBADFDIsProbed(t *testing.T) {
	fp := &fakePCM{
		readFn:    func() (int, error) { return 0, unix.ESTRPIPE },
		recoverFn: func(error) error { return &recoverError{unix.EBADFD} },
		probeFn:   func() error { return &recoverError{unix.ENODEV} },
	}
	s := openLifecycleStream(t, fp)
	if _, err := s.Read(make([]byte, 96)); !errors.Is(err, ErrDeviceGone) {
		t.Errorf("Read = %v, want ErrDeviceGone", err)
	}
}

// TestReadEBADFDAfterCloseIsErrClosedWithoutProbe pins I3: Close's DROP makes
// the parked read return EBADFD, which must read as ErrClosed with no probe.
func TestReadEBADFDAfterCloseIsErrClosedWithoutProbe(t *testing.T) {
	parked := make(chan struct{})
	f := &fakePCM{block: make(chan struct{})}
	f.readFn = func() (int, error) {
		close(parked)
		<-f.block
		return 0, unix.EBADFD
	}
	s := openLifecycleStream(t, f)
	done := make(chan error, 1)
	go func() { _, e := s.Read(make([]byte, 96)); done <- e }()
	<-parked
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
	if f.probes != 0 {
		t.Errorf("probes = %d, want 0", f.probes)
	}
}

// TestReadCloseDuringProbeIsErrClosed pins I3 for a Close racing the probe: the
// probe then fails with EBADF (acquire on a closed PCM), which is not a gone
// device.
func TestReadCloseDuringProbeIsErrClosed(t *testing.T) {
	f := &fakePCM{readFn: func() (int, error) { return 0, unix.EBADFD }}
	s := openLifecycleStream(t, f)
	f.probeFn = func() error {
		_ = s.Close()
		return unix.EBADF
	}
	if _, err := s.Read(make([]byte, 96)); !errors.Is(err, ErrClosed) {
		t.Errorf("Read = %v, want ErrClosed", err)
	}
}

func TestStartEBADFDProbe(t *testing.T) {
	t.Run("device gone", func(t *testing.T) {
		fp := &fakePCM{startErr: &recoverError{unix.EBADFD}, probeFn: func() error { return unix.ENODEV }}
		s := openLifecycleStream(t, fp)
		if err := s.Start(); !errors.Is(err, ErrDeviceGone) {
			t.Errorf("Start = %v, want ErrDeviceGone", err)
		}
	})
	t.Run("device present", func(t *testing.T) {
		fp := &fakePCM{startErr: &recoverError{unix.EBADFD}}
		s := openLifecycleStream(t, fp)
		err := s.Start()
		if !errors.Is(err, unix.EBADFD) || errors.Is(err, ErrDeviceGone) {
			t.Errorf("Start = %v, want the original EBADFD", err)
		}
	})
}

func TestOpenNegotiateEBADFDProbe(t *testing.T) {
	fp := &fakePCM{negErr: &recoverError{unix.EBADFD}, probeFn: func() error { return unix.ENODEV }}
	defer swapOpenPCM(fp)()
	_, err := Open(Config{Device: hwAddrCard1, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if !errors.Is(err, ErrDeviceGone) {
		t.Errorf("Open = %v, want ErrDeviceGone", err)
	}
	if fp.closeCalls != 1 {
		t.Errorf("Close calls = %d, want 1", fp.closeCalls)
	}
}

func TestStallErrorMessage(t *testing.T) {
	err := &StallError{Recoveries: 3, Err: unix.EPIPE}
	want := "capture: device stalled: no audio after 3 recovery attempt(s) (READI_FRAMES: broken pipe)"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

// TestOpenNegotiateEBADFDPresentDevice pins the other half of the Open probe: an
// EBADFD from Negotiate on a device that still answers PVERSION is a plain state
// error and must not be relabelled ErrDeviceGone.
func TestOpenNegotiateEBADFDPresentDevice(t *testing.T) {
	fp := &fakePCM{negErr: &recoverError{unix.EBADFD}}
	defer swapOpenPCM(fp)()
	_, err := Open(Config{Device: hwAddrCard1, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if !errors.Is(err, unix.EBADFD) || errors.Is(err, ErrDeviceGone) {
		t.Errorf("Open = %v, want the original EBADFD, not ErrDeviceGone", err)
	}
	if fp.probes != 1 {
		t.Errorf("probes = %d, want 1", fp.probes)
	}
	if fp.closeCalls != 1 {
		t.Errorf("Close calls = %d, want 1", fp.closeCalls)
	}
}
