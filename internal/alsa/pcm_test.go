//go:build linux

package alsa

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openDevNull returns a real fd (backed by /dev/null) whose lifetime the test's
// PCM.Close will manage, or skips if it cannot be opened.
func openDevNull(t *testing.T) int {
	t.Helper()
	fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("cannot open /dev/null: %v", err)
	}
	return fd
}

// setInterval is a tiny helper for the fake kernel to narrow a parameter.
func setInterval(hw *HwParams, param int, lo, hi uint32) {
	hw.Intervals[param-paramFirstInterval] = Interval{Min: lo, Max: hi}
}

func TestNegotiateRefusesBadRate(t *testing.T) {
	// Fake hardware locked at 48 kHz: HW_REFINE narrows the rate interval to
	// [48000, 48000], so a 256 kHz request must be refused, not substituted.
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req == iocHwRefine {
			hw := (*HwParams)(arg)
			setInterval(hw, ParamRate, 48000, 48000)
			setInterval(hw, ParamPeriodSize, 16, 65536)
			setInterval(hw, ParamPeriods, 2, 32)
		}
		return nil
	}
	p := newPCM(-1, fake)
	_, err := p.Negotiate(256000, 1, FormatS16LE, 960, 4)
	var bre *BadRateError
	if !errors.As(err, &bre) {
		t.Fatalf("Negotiate(256000) err = %v, want *BadRateError", err)
	}
	if bre.Requested != 256000 || bre.Min != 48000 || bre.Max != 48000 {
		t.Errorf("BadRateError = %+v, want {256000, 48000, 48000}", bre)
	}
}

func TestNegotiateRefusesBadFormat(t *testing.T) {
	// The initial unconstrained HW_REFINE (which pins access/format/channels and
	// leaves rate open) is rejected with EINVAL, meaning the device does not
	// support this channel/format combo. Negotiate must surface a typed
	// *BadFormatError, not the raw ioctl error, so the public layer can report
	// it cleanly (this is the ZOOM AMS-24 symptom from issue #9: an unsupported
	// combo leaked "alsa: HW_REFINE: invalid argument").
	fake := func(_ int, req uintptr, _ unsafe.Pointer) error {
		if req == iocHwRefine {
			return unix.EINVAL
		}
		return nil
	}
	p := newPCM(-1, fake)
	_, err := p.Negotiate(48000, 1, FormatS16LE, 960, 4)
	var bfe *BadFormatError
	if !errors.As(err, &bfe) {
		t.Fatalf("Negotiate with refine EINVAL err = %v, want *BadFormatError", err)
	}
	if bfe.Channels != 1 || bfe.Format != FormatS16LE {
		t.Errorf("BadFormatError = %+v, want {Channels:1, Format:%d}", bfe, FormatS16LE)
	}
}

func TestNegotiateRefinePassesThroughDeviceGone(t *testing.T) {
	// A device-gone errno at the initial refine (ENODEV/ENXIO/ENOENT) must NOT be
	// relabelled a format error: it is disjoint from EINVAL and must pass through
	// so the public layer can map it to ErrDeviceGone.
	for _, errno := range []unix.Errno{unix.ENODEV, unix.ENXIO, unix.ENOENT} {
		t.Run(errno.Error(), func(t *testing.T) {
			fake := func(_ int, req uintptr, _ unsafe.Pointer) error {
				if req == iocHwRefine {
					return errno
				}
				return nil
			}
			p := newPCM(-1, fake)
			_, err := p.Negotiate(48000, 1, FormatS16LE, 960, 4)
			var bfe *BadFormatError
			if errors.As(err, &bfe) {
				t.Fatalf("Negotiate with refine %v = %v, want it NOT a *BadFormatError", errno, err)
			}
			if !errors.Is(err, errno) {
				t.Errorf("Negotiate with refine %v = %v, want it to unwrap to %v", errno, err, errno)
			}
		})
	}
}

func TestNegotiateBadRateOnCommit(t *testing.T) {
	// The rate is inside the reported [rlo, rhi] window so it passes the range
	// check, but the exact HW_PARAMS commit is rejected with EINVAL (a discrete
	// gap inside the window). Negotiate must report *BadRateError carrying the
	// window bounds, not leak the raw HW_PARAMS errno.
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		switch req {
		case iocHwRefine:
			hw := (*HwParams)(arg)
			setInterval(hw, ParamRate, 44100, 96000)
			setInterval(hw, ParamPeriodSize, 16, 65536)
			setInterval(hw, ParamPeriods, 2, 32)
		case iocHwParams:
			return unix.EINVAL
		}
		return nil
	}
	p := newPCM(-1, fake)
	_, err := p.Negotiate(44101, 2, FormatS32LE, 882, 4)
	var bre *BadRateError
	if !errors.As(err, &bre) {
		t.Fatalf("Negotiate with HW_PARAMS EINVAL err = %v, want *BadRateError", err)
	}
	if bre.Requested != 44101 || bre.Min != 44100 || bre.Max != 96000 {
		t.Errorf("BadRateError = %+v, want {44101, 44100, 96000}", bre)
	}
}

func TestNegotiateCommitPassesThroughDeviceGone(t *testing.T) {
	// A device-gone errno at the HW_PARAMS commit (the rate cleared the window
	// check, then the device vanished) must NOT be relabelled a bad rate: only
	// EINVAL maps to *BadRateError there, so a device-gone errno passes through
	// for the public layer to map to ErrDeviceGone. Mirrors the refine-time case.
	for _, errno := range []unix.Errno{unix.ENODEV, unix.ENXIO, unix.ENOENT} {
		t.Run(errno.Error(), func(t *testing.T) {
			fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
				switch req {
				case iocHwRefine:
					hw := (*HwParams)(arg)
					setInterval(hw, ParamRate, 44100, 96000)
					setInterval(hw, ParamPeriodSize, 16, 65536)
					setInterval(hw, ParamPeriods, 2, 32)
				case iocHwParams:
					return errno
				}
				return nil
			}
			p := newPCM(-1, fake)
			_, err := p.Negotiate(48000, 2, FormatS32LE, 960, 4)
			var bre *BadRateError
			if errors.As(err, &bre) {
				t.Fatalf("Negotiate with HW_PARAMS %v = %v, want it NOT a *BadRateError", errno, err)
			}
			if !errors.Is(err, errno) {
				t.Errorf("Negotiate with HW_PARAMS %v = %v, want it to unwrap to %v", errno, err, errno)
			}
		})
	}
}

func TestNegotiateSucceeds(t *testing.T) {
	var swParamsCalled bool
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		switch req {
		case iocHwRefine:
			hw := (*HwParams)(arg)
			setInterval(hw, ParamRate, 8000, 384000)
			setInterval(hw, ParamPeriodSize, 16, 65536)
			setInterval(hw, ParamPeriods, 2, 32)
		case iocHwParams:
			// Kernel resolves every param to a single value on commit.
			hw := (*HwParams)(arg)
			setInterval(hw, ParamRate, 256000, 256000)
			setInterval(hw, ParamPeriodSize, 5120, 5120)
			setInterval(hw, ParamPeriods, 4, 4)
			setInterval(hw, ParamBufferSize, 20480, 20480)
		case iocSwParams:
			swParamsCalled = true
		}
		return nil
	}
	p := newPCM(-1, fake)
	n, err := p.Negotiate(256000, 1, FormatS16LE, 5120, 4)
	if err != nil {
		t.Fatalf("Negotiate = %v", err)
	}
	want := Negotiated{Rate: 256000, Channels: 1, Format: FormatS16LE, PeriodFrames: 5120, Periods: 4, BufferFrames: 20480}
	if n != want {
		t.Errorf("Negotiated = %+v, want %+v", n, want)
	}
	if !swParamsCalled {
		t.Error("Negotiate did not issue SW_PARAMS")
	}
}

func TestReadIReturnsFrameCount(t *testing.T) {
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req == iocReadIFrames {
			x := (*Xferi)(arg)
			x.Result = sframes(x.Frames) // pretend every requested frame arrived
		}
		return nil
	}
	p := newPCM(-1, fake)
	buf := make([]byte, 960*2)
	n, err := p.ReadI(buf, 960)
	if err != nil || n != 960 {
		t.Fatalf("ReadI = %d, %v; want 960, nil", n, err)
	}
}

func TestReadIPropagatesErrno(t *testing.T) {
	fake := func(_ int, req uintptr, _ unsafe.Pointer) error {
		if req == iocReadIFrames {
			return unix.EPIPE
		}
		return nil
	}
	p := newPCM(-1, fake)
	if _, err := p.ReadI(make([]byte, 4), 1); !errors.Is(err, unix.EPIPE) {
		t.Fatalf("ReadI err = %v, want EPIPE", err)
	}
}

func TestReadIRejectsEmpty(t *testing.T) {
	p := newPCM(-1, func(int, uintptr, unsafe.Pointer) error { return nil })
	if n, err := p.ReadI(nil, 0); n != 0 || err != nil {
		t.Fatalf("ReadI(nil,0) = %d, %v; want 0, nil", n, err)
	}
}

func TestRecoverTurnsXrunIntoRestart(t *testing.T) {
	var prepared, started int
	fake := func(_ int, req uintptr, _ unsafe.Pointer) error {
		switch req {
		case iocPrepare:
			prepared++
		case iocStart:
			started++
		}
		return nil
	}
	p := newPCM(-1, fake)
	if err := p.Recover(unix.EPIPE); err != nil {
		t.Fatalf("Recover(EPIPE) = %v, want nil", err)
	}
	if prepared != 1 || started != 1 {
		t.Errorf("after Recover(EPIPE): prepared=%d started=%d, want 1,1", prepared, started)
	}
}

func TestRecoverPassesThroughUnrecoverable(t *testing.T) {
	p := newPCM(-1, func(int, uintptr, unsafe.Pointer) error { return nil })
	sentinel := errors.New("boom")
	if err := p.Recover(sentinel); !errors.Is(err, sentinel) {
		t.Fatalf("Recover(sentinel) = %v, want the sentinel unchanged", err)
	}
}

// TestReadICloseRace exercises the documented path where Close runs on another
// goroutine to unblock a parked ReadI. Beyond -race cleanliness on the shared
// bookkeeping, it asserts the TOCTOU property the in-flight guard provides: no
// READI_FRAMES ioctl is ever issued after Close has returned (which implies the
// fd was closed), so the fd cannot be closed under a live syscall. Run under
// -race. A real /dev/null fd is used so Close runs its full DROP + unix.Close
// path; the fake ioctl replaces the real syscall.
func TestReadICloseRace(t *testing.T) {
	fd := openDevNull(t)
	var closeReturned atomic.Bool
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req == iocReadIFrames {
			if closeReturned.Load() {
				t.Error("READI_FRAMES ioctl issued after Close returned")
			}
			(*Xferi)(arg).Result = 0
		}
		return nil
	}
	p := newPCM(fd, fake)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 8)
		for i := 0; i < 2000; i++ {
			if _, rerr := p.ReadI(buf, 1); rerr != nil {
				return // stops once Close makes acquire return EBADF
			}
		}
	}()

	if cerr := p.Close(); cerr != nil {
		t.Errorf("Close: %v", cerr)
	}
	closeReturned.Store(true)
	wg.Wait()

	// Close is idempotent: a second call claims nothing and is a no-op.
	if cerr := p.Close(); cerr != nil {
		t.Errorf("second Close: %v", cerr)
	}
}

// TestCloseWaitsForInFlightReadI proves Close blocks until an in-flight ReadI
// ioctl returns before it closes the fd (the core of the TOCTOU fix). The fake
// parks inside READI_FRAMES until the test releases it; DROP is a no-op so the
// test, not the kernel, controls when the read returns.
func TestCloseWaitsForInFlightReadI(t *testing.T) {
	fd := openDevNull(t)
	entered := make(chan struct{})
	releaseRead := make(chan struct{})
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req == iocReadIFrames {
			close(entered)
			<-releaseRead
			(*Xferi)(arg).Result = 0
		}
		return nil
	}
	p := newPCM(fd, fake)

	var readReturned atomic.Bool
	go func() {
		buf := make([]byte, 8)
		_, _ = p.ReadI(buf, 1)
		readReturned.Store(true)
	}()

	<-entered // the read is now parked inside the ioctl

	closeDone := make(chan struct{})
	go func() {
		_ = p.Close()
		close(closeDone)
	}()

	// Close must not complete while the read is still in flight.
	select {
	case <-closeDone:
		t.Fatal("Close returned before the in-flight ReadI finished")
	case <-time.After(50 * time.Millisecond):
	}
	if readReturned.Load() {
		t.Fatal("ReadI returned before it was released")
	}

	close(releaseRead)
	<-closeDone
	if !readReturned.Load() {
		t.Fatal("ReadI did not return after release")
	}
}

// TestCloseUnblocksParkedReadI is the liveness guard: a reader parked in
// READI_FRAMES is woken by Close's DROP. It is also the regression test against
// any future design that holds a lock across the blocking ioctl, which would
// deadlock here. The fake blocks the read until it observes the DROP.
func TestCloseUnblocksParkedReadI(t *testing.T) {
	fd := openDevNull(t)
	entered := make(chan struct{})
	drop := make(chan struct{})
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		switch req {
		case iocReadIFrames:
			close(entered) // the read has acquired the guard and is now parked
			<-drop         // park inside the ioctl until Close issues DROP
			return unix.EBADFD
		case iocDrop:
			close(drop)
		}
		return nil
	}
	p := newPCM(fd, fake)

	readDone := make(chan struct{})
	go func() {
		buf := make([]byte, 8)
		_, _ = p.ReadI(buf, 1)
		close(readDone)
	}()

	// Do not start Close until the reader is genuinely parked inside the ioctl;
	// otherwise Close could win the mutex first and the reader would never park,
	// making this a vacuous test of the DROP-unblocks-a-parked-reader path (and
	// of the deadlock a lock-across-the-syscall design would cause).
	<-entered

	done := make(chan struct{})
	go func() {
		_ = p.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return: parked ReadI was not unblocked (possible deadlock)")
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("ReadI did not return after Close unblocked it")
	}
}

// TestReadIAfterCloseReturnsEBADF verifies that once Close has run, ReadI issues
// no ioctl and returns EBADF (which Stream.Read maps to ErrClosed), and that a
// guarded Recover likewise fails closed rather than touching the fd.
func TestReadIAfterCloseReturnsEBADF(t *testing.T) {
	fd := openDevNull(t)
	var issued atomic.Bool
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req == iocReadIFrames {
			issued.Store(true)
			(*Xferi)(arg).Result = 0
		}
		return nil
	}
	p := newPCM(fd, fake)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	buf := make([]byte, 8)
	if _, err := p.ReadI(buf, 1); !errors.Is(err, unix.EBADF) {
		t.Errorf("ReadI after Close = %v, want EBADF", err)
	}
	if issued.Load() {
		t.Error("ReadI issued a READI_FRAMES ioctl after Close")
	}
	// Recover routes its ioctls through the same guard, so it also fails closed.
	if err := p.Recover(unix.ESTRPIPE); !errors.Is(err, unix.EBADF) {
		t.Errorf("Recover after Close = %v, want EBADF-wrapping error", err)
	}
}

// TestCloseConcurrent runs two Close calls at once: both must return nil and the
// -race detector must stay quiet (exactly-once close via the closed flag).
func TestCloseConcurrent(t *testing.T) {
	fd := openDevNull(t)
	p := newPCM(fd, func(int, uintptr, unsafe.Pointer) error { return nil })

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = p.Close() }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Close[%d] = %v, want nil", i, err)
		}
	}
}

// TestBoundary asserts the sw_params pointer-wrap boundary directly. It runs on
// both word sizes (boundaryCap is 1<<60 on LP64 and 1<<30 on ILP32), so under
// GOARCH=386 the properties below are checked against the real 32-bit uframes
// arithmetic that boundary() performs.
func TestBoundary(t *testing.T) {
	if got := boundary(0); got != 0 {
		t.Errorf("boundary(0) = %d, want 0", got)
	}
	// Typical kernel-negotiated buffer sizes in frames, plus a non-power-of-two
	// (3) to prove the result is a power-of-two MULTIPLE of the buffer, not a bare
	// power of two.
	for _, buf := range []uframes{1, 3, 960, 7680, 16384, 30720} {
		b := boundary(buf)
		if b < buf {
			t.Errorf("boundary(%d) = %d, want >= the buffer size", buf, b)
		}
		if b > boundaryCap {
			t.Errorf("boundary(%d) = %d, exceeds boundaryCap %d", buf, b, uint64(boundaryCap))
		}
		// Maximal: one more doubling would pass boundaryCap. Tested as
		// b > boundaryCap/2 so the check never computes b*2 (which would overflow a
		// 32-bit uframes near the cap, the very hazard under test).
		if b <= boundaryCap/2 {
			t.Errorf("boundary(%d) = %d is not maximal (<= boundaryCap/2 = %d)", buf, b, uint64(boundaryCap/2))
		}
		// b must be buf * 2^k for some k >= 0.
		q := b
		for q > buf && q%2 == 0 {
			q /= 2
		}
		if q != buf {
			t.Errorf("boundary(%d) = %d is not a power-of-two multiple of the buffer", buf, b)
		}
	}
	// A buffer above boundaryCap (unreachable for a real negotiated buffer) has no
	// power-of-two multiple within the cap, so boundary returns it unchanged: the
	// minimal boundary that still satisfies the kernel's boundary >= buffer_size
	// rule. Clamping to boundaryCap would break that invariant.
	if got := boundary(boundaryCap + 1); got != boundaryCap+1 {
		t.Errorf("boundary(boundaryCap+1) = %d, want %d unchanged (boundary must stay >= buffer size)", uint64(got), uint64(boundaryCap+1))
	}
}
