//go:build linux

package alsa

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctlFunc is the ioctl seam. Production wires the package ioctl; tests inject
// a fake that fills HwParams/Xferi and returns canned errnos, so negotiate and
// recovery are covered without hardware.
type ioctlFunc func(fd int, req uintptr, arg unsafe.Pointer) error

// PCM is an open capture PCM device.
//
// The fd's lifetime is guarded because Close may run on a different goroutine to
// unblock a parked ReadI (the documented unblock-a-parked-reader path). An
// atomic on the fd alone would stop the data race but not the use-after-close:
// ReadI could read the fd, be preempted, and issue its ioctl after Close had
// already closed that fd and the number was reused elsewhere. Instead each ioctl
// on the normal API paths registers as in-flight (acquire/release) and Close
// waits for in-flight to drain before unix.Close, so the fd is never closed
// under a live syscall. (Close's own DROP is the one ioctl outside the guard: it
// runs on the closing goroutine before unix.Close, so it cannot overlap it
// either.) A mutex held across the blocking READI_FRAMES ioctl is not an option:
// Close could then never run to issue the DROP that unblocks the reader, so the
// mutex is only ever held around the bookkeeping, never around a syscall.
type PCM struct {
	// fd is written once by newPCM before the *PCM is published and never
	// mutated after, so plain reads are race-free; its lifetime (not its value)
	// is what the guard below protects.
	fd    int
	ioctl ioctlFunc
	// xferi is reused across ReadI calls so the per-read transfer descriptor
	// never heap-allocates. ReadI is single-consumer (Stream.Read drives it),
	// and Recover never touches this field, so reuse is race-free.
	xferi Xferi

	mu       sync.Mutex // guards closed and inflight; never held across a syscall
	cond     sync.Cond  // L == &mu; Close waits on it until inflight drains to 0
	closed   bool       // set once by the winning Close; blocks new acquires
	inflight int        // ioctls currently between acquire and release
}

// Negotiated reports the configuration the hardware actually accepted. With no
// hidden conversion layer, these values are exactly what Read delivers.
type Negotiated struct {
	Rate         int
	Channels     int
	Format       uint32
	PeriodFrames int
	Periods      int
	BufferFrames int
}

// BadRateError reports that the hardware does not support the exact requested
// sample rate. Min and Max bound the supported range discovered by HW_REFINE.
// This is returned instead of silently negotiating a different rate.
type BadRateError struct {
	Requested int
	Min       int
	Max       int
}

func (e *BadRateError) Error() string {
	return fmt.Sprintf("alsa: sample rate %d Hz not supported (hardware range %d..%d Hz)", e.Requested, e.Min, e.Max)
}

// BadFormatError reports that the hardware does not support the requested
// access/format/channel combination: the initial HW_REFINE, which pins those
// and leaves the rate open, was rejected. It is distinct from BadRateError (an
// otherwise-supported format at an unsupported rate) so the public layer can
// surface the right typed error instead of leaking the raw ioctl string. Format
// is the SNDRV_PCM_FORMAT_* id, not the public capture.Format.
type BadFormatError struct {
	Channels int
	Format   uint32
}

func (e *BadFormatError) Error() string {
	return fmt.Sprintf("alsa: %d-channel format id %d not supported", e.Channels, e.Format)
}

// ioctlError wraps an errno with the name of the ioctl that failed, so callers
// never see a bare "invalid argument".
type ioctlError struct {
	Op  string
	Err error
}

func (e *ioctlError) Error() string { return "alsa: " + e.Op + ": " + e.Err.Error() }
func (e *ioctlError) Unwrap() error { return e.Err }

// OpenPCM opens the capture device /dev/snd/pcmC{card}D{device}c for streaming.
// It tries O_RDWR first (what alsa-lib uses) and falls back to O_RDONLY on a
// permission error, since capture needs only reads.
func OpenPCM(card, device int) (*PCM, error) {
	return openPCM(card, device, 0)
}

// OpenPCMForQuery opens the capture device for a capability query only, adding
// O_NONBLOCK so the open cannot block waiting on the device. Some drivers block
// a plain capture open until the stream is ready (snd-aloop, for one, blocks
// until a playback client attaches); a query must never hang on that. HW_REFINE,
// the only ioctl a query issues, is synchronous and unaffected by O_NONBLOCK.
func OpenPCMForQuery(card, device int) (*PCM, error) {
	return openPCM(card, device, unix.O_NONBLOCK)
}

// openPCM opens the capture PCM node with the given extra open flags, applying
// the shared O_RDWR-then-O_RDONLY fallback so both entry points behave alike.
func openPCM(card, device, extraFlags int) (*PCM, error) {
	path := fmt.Sprintf("/dev/snd/pcmC%dD%dc", card, device)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|extraFlags, 0)
	if err != nil && errors.Is(err, unix.EACCES) {
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|extraFlags, 0)
	}
	if err != nil {
		return nil, &ioctlError{Op: "open " + path, Err: err}
	}
	return newPCM(fd, ioctl), nil
}

// newPCM builds a PCM and wires the condition variable to the mutex. It is the
// single construction point (production and tests) so the sync.Cond is always
// wired before the *PCM is published.
func newPCM(fd int, ioctl ioctlFunc) *PCM {
	p := &PCM{fd: fd, ioctl: ioctl}
	p.cond.L = &p.mu
	return p
}

// acquire registers an in-flight ioctl and returns the fd to use. It returns
// unix.EBADF (which Stream.Read maps to ErrClosed) if the PCM is already closed,
// so no ioctl is ever issued on a closed or about-to-be-closed fd. Pair every
// successful acquire with a release.
func (p *PCM) acquire() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return -1, unix.EBADF
	}
	p.inflight++
	return p.fd, nil
}

// release marks an in-flight ioctl done and wakes a Close waiting to drain.
func (p *PCM) release() {
	p.mu.Lock()
	p.inflight--
	if p.inflight == 0 && p.closed {
		p.cond.Broadcast()
	}
	p.mu.Unlock()
}

// guardedIoctl runs a control ioctl inside the in-flight guard so Close cannot
// close the fd under it. The ioctl runs outside the mutex. ReadI does not use
// this helper: it must keep buf alive across the syscall (runtime.KeepAlive) and
// read p.xferi.Result back afterward, so it inlines the same acquire/release.
func (p *PCM) guardedIoctl(req uintptr, arg unsafe.Pointer) error {
	fd, err := p.acquire()
	if err != nil {
		return err
	}
	defer p.release()
	return p.ioctl(fd, req, arg)
}

// Negotiate configures the hardware for the requested format via HW_REFINE then
// HW_PARAMS, then sets the software params. periodFrames and periods must be
// concrete positive values (the public layer computes defaults before calling).
// The requested rate is honored exactly or the call fails with *BadRateError.
func (p *PCM) Negotiate(rate, channels int, format uint32, periodFrames, periods int) (Negotiated, error) {
	var hw HwParams
	hw.FillAny()
	hw.SetMask(ParamAccess, AccessRWInterleaved)
	hw.SetMask(ParamFormat, uint(format))
	hw.SetMask(ParamSubformat, SubformatSTD)
	hw.SetIntervalExact(ParamChannels, uint32(channels))

	// Discover the supported rate range for this format/channel/access combo.
	// The refine pins access, format, subformat, and channels and leaves rate
	// open, so an EINVAL here means the hardware rejects that combination
	// outright (not a rate): report it as a typed format error rather than
	// leaking the raw ioctl string. Device-gone errnos (ENODEV/ENXIO/ENOENT) are
	// disjoint from EINVAL and pass through unchanged for the caller to classify.
	if err := p.refine(&hw); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return Negotiated{}, &BadFormatError{Channels: channels, Format: format}
		}
		return Negotiated{}, err
	}
	rlo, rhi := hw.Interval(ParamRate)
	if uint32(rate) < rlo || uint32(rate) > rhi {
		return Negotiated{}, &BadRateError{Requested: rate, Min: int(rlo), Max: int(rhi)}
	}

	// Pin the exact rate and a sane period geometry clamped into the supported
	// ranges, then commit. A commit failure here (e.g. a discrete-rate gap
	// inside the range, or an unsupported period size) surfaces honestly rather
	// than as a substituted configuration. VerifyRate shares pinRateGeometry so the
	// rate probe and the real open commit identical geometry.
	hw.pinRateGeometry(rate, periodFrames, periods)
	// The rate already passed the [rlo, rhi] window check, so an EINVAL at commit
	// means the exact rate falls in a discrete gap inside that window (or, rarely,
	// the clamped period geometry was refused). The library surfaces no
	// period-geometry error and a discrete-rate gap is the dominant cause, so
	// report a bad rate rather than leaking the raw HW_PARAMS errno.
	if err := p.hwParams(&hw); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return Negotiated{}, &BadRateError{Requested: rate, Min: int(rlo), Max: int(rhi)}
		}
		return Negotiated{}, err
	}

	// After HW_PARAMS every interval is resolved to a single value.
	gotRate, _ := hw.Interval(ParamRate)
	gotPeriod, _ := hw.Interval(ParamPeriodSize)
	gotPeriods, _ := hw.Interval(ParamPeriods)
	gotBuffer, _ := hw.Interval(ParamBufferSize)
	n := Negotiated{
		Rate:         int(gotRate),
		Channels:     channels,
		Format:       format,
		PeriodFrames: int(gotPeriod),
		Periods:      int(gotPeriods),
		BufferFrames: int(gotBuffer),
	}

	// Software params: wake once per period, and set the start threshold above
	// the buffer so capture starts only on an explicit Start, never implicitly.
	if err := p.setSwParams(n); err != nil {
		return Negotiated{}, err
	}
	// Move the stream from SETUP to PREPARED so a later Start (SETUP -> START
	// is EBADFD) is valid: Open leaves the device prepared but not running.
	if err := p.Prepare(); err != nil {
		return Negotiated{}, err
	}
	return n, nil
}

// setSwParams sets avail_min to the period size and start_threshold past the
// buffer boundary so START is always explicit; stop_threshold is maxed so an
// overrun does not auto-stop the stream (Recover handles overruns).
func (p *PCM) setSwParams(n Negotiated) error {
	sw := SwParams{
		AvailMin:       uint64(n.PeriodFrames),
		StartThreshold: uint64(n.BufferFrames) + 1,
		StopThreshold:  ^uint64(0),
		Boundary:       boundary(uint64(n.BufferFrames)),
	}
	if err := p.guardedIoctl(iocSwParams, unsafe.Pointer(&sw)); err != nil {
		return &ioctlError{Op: "SW_PARAMS", Err: err}
	}
	return nil
}

// Prepare moves the stream to the prepared state (SNDRV_PCM_IOCTL_PREPARE).
func (p *PCM) Prepare() error { return p.control(iocPrepare, "PREPARE") }

// Start begins capture (SNDRV_PCM_IOCTL_START).
func (p *PCM) Start() error { return p.control(iocStart, "START") }

func (p *PCM) control(req uintptr, op string) error {
	if err := p.guardedIoctl(req, nil); err != nil {
		return &ioctlError{Op: op, Err: err}
	}
	return nil
}

// ReadI reads up to frames interleaved frames into buf via READI_FRAMES and
// returns the number of frames actually read. It returns the raw errno (for
// Recover to classify) rather than a wrapped error. buf must hold at least
// frames whole frames; a non-positive frames count is a no-op.
func (p *PCM) ReadI(buf []byte, frames int) (int, error) {
	if frames <= 0 || len(buf) == 0 {
		return 0, nil
	}
	// Reuse the preallocated descriptor; Result is overwritten by the ioctl.
	p.xferi.Buf = unsafe.Pointer(&buf[0])
	p.xferi.Frames = uint64(frames)
	// Register as in-flight so Close cannot close the fd underneath the syscall.
	// The blocking ioctl runs outside the mutex; a concurrent Close unblocks it
	// with DROP, then waits for release below before it closes the fd.
	fd, err := p.acquire()
	if err != nil {
		return 0, err
	}
	defer p.release()
	err = p.ioctl(fd, iocReadIFrames, unsafe.Pointer(&p.xferi))
	runtime.KeepAlive(buf)
	if err != nil {
		return 0, err
	}
	if p.xferi.Result < 0 {
		return 0, unix.Errno(-p.xferi.Result)
	}
	return int(p.xferi.Result), nil
}

// Recover handles a transfer error. An overrun (EPIPE) is cleared by
// re-preparing and restarting. A suspend (ESTRPIPE) is cleared by retrying
// RESUME until the system resumes, then re-preparing. Anything else is returned
// unchanged as unrecoverable.
func (p *PCM) Recover(err error) error {
	switch {
	case errors.Is(err, unix.EPIPE):
		if e := p.Prepare(); e != nil {
			return e
		}
		return p.Start()
	case errors.Is(err, unix.ESTRPIPE):
		for {
			e := p.guardedIoctl(iocResume, nil)
			if e == nil || errors.Is(e, unix.ENOSYS) {
				break // resumed, or driver has no RESUME: fall through to prepare
			}
			if errors.Is(e, unix.EAGAIN) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return &ioctlError{Op: "RESUME", Err: e}
		}
		if e := p.Prepare(); e != nil {
			return e
		}
		return p.Start()
	default:
		return err
	}
}

// Close stops the stream and closes the fd. It is idempotent, and may be called
// from a different goroutine than the reader to unblock a parked ReadI. To avoid
// closing the fd out from under a live syscall (which would let the reused fd
// number take an ALSA ioctl meant for a since-closed device), it marks the PCM
// closed, issues a best-effort DROP to wake a reader parked in READI_FRAMES,
// waits for every in-flight ioctl to return, and only then closes the fd. Close
// therefore blocks until an in-flight ReadI returns (DROP wakes it promptly).
func (p *PCM) Close() error {
	// Claim the close exactly once and stop new ioctls from starting.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	fd := p.fd
	p.mu.Unlock()

	// Wake a reader parked in READI_FRAMES. Safe without the mutex: fd is still
	// open (only this Close closes it, below) and only this goroutine got here.
	_ = p.ioctl(fd, iocDrop, nil) // best effort

	// Wait for in-flight ioctls to finish, then close the fd. closed == true
	// guarantees no new acquire succeeds, so inflight cannot rise again.
	p.mu.Lock()
	for p.inflight > 0 {
		p.cond.Wait()
	}
	p.mu.Unlock()

	return unix.Close(fd)
}

func (p *PCM) refine(hw *HwParams) error {
	if err := p.guardedIoctl(iocHwRefine, unsafe.Pointer(hw)); err != nil {
		return &ioctlError{Op: "HW_REFINE", Err: err}
	}
	return nil
}

func (p *PCM) hwParams(hw *HwParams) error {
	if err := p.guardedIoctl(iocHwParams, unsafe.Pointer(hw)); err != nil {
		return &ioctlError{Op: "HW_PARAMS", Err: err}
	}
	return nil
}

func clampU32(v, lo, hi uint32) uint32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// DefaultPeriods is the streaming open's default periods-per-buffer count when a
// caller does not specify one. VerifyRate uses it too, so the rate probe commits
// with the same geometry the real open will.
const DefaultPeriods = 4

// DefaultPeriodFrames returns the streaming open's default period length in frames
// for a sample rate: about 20 ms (rate/50), at least one frame. VerifyRate uses it
// too so a probed rate is committed with the same period geometry the real open
// uses by default, never the interval's degenerate minimum (which some USB devices
// reject at high rates).
func DefaultPeriodFrames(rate int) int {
	if pf := rate / 50; pf > 0 {
		return pf
	}
	return 1
}

// pinRateGeometry pins the exact rate and a sane period/buffer geometry into the
// already-refined hw params: periodFrames and periods, each clamped into the
// current refined interval so a device with strict bounds still gets a valid
// choice. Negotiate and VerifyRate share this so a rate is verified with the same
// geometry the streaming open commits by default; the two paths cannot drift for
// that default geometry (a caller that passes a non-zero custom period/periods to
// Open bypasses the defaults and is not what VerifyRate models). It assumes hw has
// been refined for the target format/channel combo.
func (hw *HwParams) pinRateGeometry(rate, periodFrames, periods int) {
	hw.SetIntervalExact(ParamRate, uint32(rate))
	plo, phi := hw.Interval(ParamPeriodSize)
	hw.SetIntervalExact(ParamPeriodSize, clampU32(uint32(periodFrames), plo, phi))
	nlo, nhi := hw.Interval(ParamPeriods)
	hw.SetIntervalExact(ParamPeriods, clampU32(uint32(periods), nlo, nhi))
}

// boundary returns a pointer-wrap boundary that is a power-of-two multiple of
// the buffer size, matching alsa-lib's convention for the sw_params boundary.
func boundary(bufferFrames uint64) uint64 {
	if bufferFrames == 0 {
		return 0
	}
	b := bufferFrames
	for b*2 <= 1<<60 {
		b *= 2
	}
	return b
}
