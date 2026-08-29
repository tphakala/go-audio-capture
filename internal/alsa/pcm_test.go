//go:build linux

package alsa

import (
	"errors"
	"sync"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

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
			x.Result = int64(x.Frames) // pretend every requested frame arrived
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

// TestReadICloseRace exercises the documented path where Close is called from
// another goroutine to unblock a parked ReadI: Close invalidates p.fd while
// ReadI reads it for the ioctl. With p.fd held atomically this is race-free;
// run under -race, where the previous plain-int field would have been flagged.
// A real /dev/null fd is used so Close's Swap claims a non-negative value and
// runs its full DROP + unix.Close path (the fake ioctl replaces the real
// syscall, so nothing depends on the fd's kernel behavior).
func TestReadICloseRace(t *testing.T) {
	fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("cannot open /dev/null: %v", err)
	}
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req == iocReadIFrames {
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
				return
			}
		}
	}()

	// Concurrently invalidate the fd, as a cross-goroutine Close would.
	if cerr := p.Close(); cerr != nil {
		t.Errorf("Close: %v", cerr)
	}
	wg.Wait()

	// Close is idempotent: a second call claims nothing and is a no-op.
	if cerr := p.Close(); cerr != nil {
		t.Errorf("second Close: %v", cerr)
	}
}
