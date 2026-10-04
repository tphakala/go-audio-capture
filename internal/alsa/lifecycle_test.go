//go:build linux

package alsa

import (
	"errors"
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// errLoopGuard is returned by fakeKernel after loopGuardCalls ioctls, so a
// regression that loops forever fails with a clear error instead of hanging.
var errLoopGuard = errors.New("fakeKernel: loop guard tripped")

const loopGuardCalls = 1000

type pcmState int

const (
	stateSetup pcmState = iota
	statePrepared
	stateRunning
	stateSuspended
	stateDisconnected
)

// fakeKernel is a small model of the PCM state machine in sound/core/pcm_native.c
// for the transitions Recover and Probe depend on. It derives errnos from state
// where the kernel does, so a test cannot claim a recovery the kernel refuses.
type fakeKernel struct {
	state     pcmState
	resumeErr func(call int) error // consulted on RESUME while SUSPENDED; nil result resumes
	calls     []uintptr
	resumes   int
}

func (k *fakeKernel) ioctl(_ int, req uintptr, arg unsafe.Pointer) error {
	k.calls = append(k.calls, req)
	if len(k.calls) > loopGuardCalls {
		return errLoopGuard
	}
	switch req {
	case iocPrepare:
		if k.state == stateRunning {
			return unix.EBUSY
		}
		if k.state == stateDisconnected {
			return unix.EBADFD
		}
		k.state = statePrepared
	case iocStart:
		if k.state != statePrepared {
			return unix.EBADFD
		}
		k.state = stateRunning
	case iocDrop:
		if k.state == stateDisconnected {
			return unix.EBADFD
		}
		k.state = stateSetup
	case iocResume:
		k.resumes++
		if k.state != stateSuspended { // includes DISCONNECTED
			return unix.EBADFD
		}
		if k.resumeErr != nil {
			if err := k.resumeErr(k.resumes); err != nil {
				return err
			}
		}
		k.state = stateRunning
	case iocPVersion:
		if k.state == stateDisconnected {
			return unix.EBADFD
		}
		*(*int32)(arg) = 0x020010
	}
	return nil
}

func (k *fakeKernel) count(req uintptr) int {
	n := 0
	for _, c := range k.calls {
		if c == req {
			n++
		}
	}
	return n
}

// skipResumeSleep replaces the RESUME retry wait with a no-op for one test and
// returns a pointer to the number of waits taken.
func skipResumeSleep(t *testing.T) *int {
	t.Helper()
	var n int
	prev := resumeSleep
	resumeSleep = func() { n++ }
	t.Cleanup(func() { resumeSleep = prev })
	return &n
}

func TestRecoverResumeSuccessKeepsRunning(t *testing.T) {
	k := &fakeKernel{state: stateSuspended}
	p := newPCM(-1, k.ioctl)
	if err := p.Recover(unix.ESTRPIPE); err != nil {
		t.Fatalf("Recover(ESTRPIPE) = %v, want nil", err)
	}
	if want := []uintptr{iocResume}; !slices.Equal(k.calls, want) {
		t.Errorf("ioctl log = %v, want only RESUME (a successful resume leaves the stream RUNNING)", k.calls)
	}
	if k.state != stateRunning {
		t.Errorf("state = %v, want running", k.state)
	}
}

func TestRecoverResumeFailureReprepares(t *testing.T) {
	for _, errno := range []unix.Errno{unix.ENOSYS, unix.EINVAL, unix.EIO} {
		t.Run(errno.Error(), func(t *testing.T) {
			k := &fakeKernel{state: stateSuspended, resumeErr: func(int) error { return errno }}
			p := newPCM(-1, k.ioctl)
			if err := p.Recover(unix.ESTRPIPE); err != nil {
				t.Fatalf("Recover(ESTRPIPE) with RESUME %v = %v, want nil", errno, err)
			}
			want := []uintptr{iocResume, iocPrepare, iocStart}
			if !slices.Equal(k.calls, want) {
				t.Errorf("ioctl log = %v, want RESUME, PREPARE, START", k.calls)
			}
			if k.state != stateRunning {
				t.Errorf("state = %v, want running", k.state)
			}
		})
	}
}

func TestRecoverResumeOnDisconnectedReturnsEBADFD(t *testing.T) {
	k := &fakeKernel{state: stateDisconnected}
	p := newPCM(-1, k.ioctl)
	err := p.Recover(unix.ESTRPIPE)
	if !errors.Is(err, unix.EBADFD) {
		t.Fatalf("Recover on a disconnected stream = %v, want EBADFD", err)
	}
	if k.count(iocStart) != 0 {
		t.Errorf("START was issued after a failed PREPARE: %v", k.calls)
	}
}

func TestRecoverResumeStopsOnClosedOrGone(t *testing.T) {
	for _, errno := range []unix.Errno{unix.EBADF, unix.ENODEV, unix.ENXIO, unix.ENOENT} {
		t.Run(errno.Error(), func(t *testing.T) {
			k := &fakeKernel{state: stateSuspended, resumeErr: func(int) error { return errno }}
			p := newPCM(-1, k.ioctl)
			err := p.Recover(unix.ESTRPIPE)
			if !errors.Is(err, errno) {
				t.Fatalf("Recover = %v, want an error wrapping %v", err, errno)
			}
			if k.count(iocPrepare) != 0 {
				t.Errorf("PREPARE was issued after RESUME %v: %v", errno, k.calls)
			}
		})
	}
}

func TestRecoverResumeEAGAINIsBounded(t *testing.T) {
	skipResumeSleep(t)
	k := &fakeKernel{state: stateSuspended, resumeErr: func(int) error { return unix.EAGAIN }}
	p := newPCM(-1, k.ioctl)
	if err := p.Recover(unix.ESTRPIPE); err != nil {
		t.Fatalf("Recover = %v, want nil after the fallback re-prepare", err)
	}
	if k.resumes != resumeRetries+1 {
		t.Errorf("RESUME calls = %d, want %d", k.resumes, resumeRetries+1)
	}
	if k.count(iocPrepare) != 1 || k.count(iocStart) != 1 {
		t.Errorf("ioctl log tail = PREPARE x%d, START x%d, want 1 each", k.count(iocPrepare), k.count(iocStart))
	}
}

func TestRecoverResumeEAGAINStopsOnClose(t *testing.T) {
	k := &fakeKernel{state: stateSuspended, resumeErr: func(int) error { return unix.EAGAIN }}
	p := newPCM(openDevNull(t), k.ioctl)
	sleeps := 0
	prev := resumeSleep
	resumeSleep = func() {
		// Close between retries, as a Close from another goroutine lands while
		// Recover waits; no ioctl is in flight, so Close returns at once.
		sleeps++
		if sleeps == 3 {
			_ = p.Close()
		}
	}
	t.Cleanup(func() { resumeSleep = prev })
	err := p.Recover(unix.ESTRPIPE)
	// The RESUME after Close fails acquire with EBADF and must end the loop
	// there: not fall through to PREPARE, and not keep retrying.
	var ie *ioctlError
	if !errors.As(err, &ie) || ie.Op != "RESUME" || !errors.Is(err, unix.EBADF) {
		t.Fatalf("Recover = %v, want the RESUME ioctl failing with EBADF", err)
	}
	if k.resumes != 3 || sleeps != 3 {
		t.Errorf("RESUME calls = %d, waits = %d, want 3 each", k.resumes, sleeps)
	}
}

func TestRecoverRestartsStalledStream(t *testing.T) {
	k := &fakeKernel{state: stateRunning}
	p := newPCM(-1, k.ioctl)
	if err := p.Recover(unix.EIO); err != nil {
		t.Fatalf("Recover(EIO) = %v, want nil", err)
	}
	want := []uintptr{iocDrop, iocPrepare, iocStart}
	if !slices.Equal(k.calls, want) {
		t.Errorf("ioctl log = %v, want DROP, PREPARE, START", k.calls)
	}
	if k.state != stateRunning {
		t.Errorf("state = %v, want running", k.state)
	}
}

func TestRecoverStallOnDisconnectedFails(t *testing.T) {
	k := &fakeKernel{state: stateDisconnected}
	p := newPCM(-1, k.ioctl)
	if err := p.Recover(unix.EIO); !errors.Is(err, unix.EBADFD) {
		t.Fatalf("Recover(EIO) on a disconnected stream = %v, want EBADFD", err)
	}
}

func TestProbe(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		k := &fakeKernel{state: stateRunning}
		if err := newPCM(-1, k.ioctl).Probe(); err != nil {
			t.Fatalf("Probe = %v, want nil", err)
		}
		if want := []uintptr{iocPVersion}; !slices.Equal(k.calls, want) {
			t.Errorf("ioctl log = %v, want PVERSION only", k.calls)
		}
	})
	t.Run("disconnected", func(t *testing.T) {
		k := &fakeKernel{state: stateDisconnected}
		if err := newPCM(-1, k.ioctl).Probe(); !errors.Is(err, unix.EBADFD) {
			t.Fatalf("Probe = %v, want EBADFD", err)
		}
	})
	t.Run("shutdown fops", func(t *testing.T) {
		p := newPCM(-1, func(int, uintptr, unsafe.Pointer) error { return unix.ENODEV })
		if err := p.Probe(); !errors.Is(err, unix.ENODEV) {
			t.Fatalf("Probe = %v, want ENODEV", err)
		}
	})
	t.Run("closed", func(t *testing.T) {
		var calls int
		p := newPCM(openDevNull(t), func(int, uintptr, unsafe.Pointer) error { calls++; return nil })
		_ = p.Close() // best-effort DROP goes through the fake
		calls = 0
		if err := p.Probe(); !errors.Is(err, unix.EBADF) {
			t.Fatalf("Probe after Close = %v, want EBADF", err)
		}
		if calls != 0 {
			t.Errorf("Probe issued %d ioctls after Close, want 0", calls)
		}
	})
}

func TestIsDeviceGone(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{unix.ENODEV, true},
		{unix.ENXIO, true},
		{unix.ENOENT, true},
		{&ioctlError{Op: "x", Err: unix.ENODEV}, true},
		{unix.EBADFD, false},
		{unix.EBADF, false},
		{unix.EIO, false},
		{nil, false},
	} {
		if got := IsDeviceGone(tt.err); got != tt.want {
			t.Errorf("IsDeviceGone(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// swapOpenSeams installs fakes for the open and fcntl syscalls for one test.
func swapOpenSeams(t *testing.T, open func(path string, mode int, perm uint32) (int, error), nonblock func(fd int, nb bool) error) {
	t.Helper()
	po, pn := sysOpen, sysSetNonblock
	sysOpen, sysSetNonblock = open, nonblock
	t.Cleanup(func() { sysOpen, sysSetNonblock = po, pn })
}

func TestOpenPCMAlwaysNonblockingThenBlocking(t *testing.T) {
	for _, tt := range []struct {
		name   string
		eacces bool
	}{
		{"rdwr", false},
		{"rdonly fallback", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var flags []int
			swapOpenSeams(t,
				func(_ string, mode int, _ uint32) (int, error) {
					flags = append(flags, mode)
					if tt.eacces && mode&unix.O_ACCMODE == unix.O_RDWR {
						return -1, unix.EACCES
					}
					return unix.Open("/dev/null", mode&^unix.O_ACCMODE|unix.O_RDONLY, 0)
				},
				unix.SetNonblock)
			p, err := OpenPCM(0, 0)
			if err != nil {
				t.Fatalf("OpenPCM: %v", err)
			}
			defer func() { _ = p.Close() }()
			wantAttempts := 1
			if tt.eacces {
				wantAttempts = 2
			}
			if len(flags) != wantAttempts {
				t.Fatalf("open attempts = %d, want %d", len(flags), wantAttempts)
			}
			for _, f := range flags {
				if f&unix.O_NONBLOCK == 0 {
					t.Errorf("open flags %#x lack O_NONBLOCK: a busy device would block the open", f)
				}
			}
			fl, err := unix.FcntlInt(uintptr(p.fd), unix.F_GETFL, 0)
			if err != nil {
				t.Fatalf("F_GETFL: %v", err)
			}
			if fl&unix.O_NONBLOCK != 0 {
				t.Error("returned fd is still O_NONBLOCK: reads would fail with EAGAIN instead of blocking")
			}
		})
	}
}

func TestOpenPCMBusyIsEBUSYImmediately(t *testing.T) {
	var cleared int
	swapOpenSeams(t,
		func(string, int, uint32) (int, error) { return -1, unix.EBUSY },
		func(int, bool) error { cleared++; return nil })
	if _, err := OpenPCM(0, 0); !errors.Is(err, unix.EBUSY) {
		t.Fatalf("OpenPCM = %v, want EBUSY", err)
	}
	if cleared != 0 {
		t.Error("O_NONBLOCK was cleared for a failed open")
	}
}

func TestOpenPCMClosesFdWhenClearFails(t *testing.T) {
	var fd int
	swapOpenSeams(t,
		func(string, int, uint32) (int, error) {
			var err error
			fd, err = unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
			return fd, err
		},
		func(int, bool) error { return unix.EIO })
	if _, err := OpenPCM(0, 0); !errors.Is(err, unix.EIO) {
		t.Fatalf("OpenPCM = %v, want an error wrapping EIO", err)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Errorf("fd %d still open after a failed clear (F_GETFD err = %v)", fd, err)
	}
}

func TestIsRecoverable(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{unix.EPIPE, true},
		{unix.ESTRPIPE, true},
		{unix.EIO, true},
		{&ioctlError{Op: "x", Err: unix.EPIPE}, true},
		{unix.ENODEV, false},
		{unix.EBADFD, false},
		{unix.EBADF, false},
		{unix.EINVAL, false},
		{nil, false},
	} {
		if got := IsRecoverable(tt.err); got != tt.want {
			t.Errorf("IsRecoverable(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
