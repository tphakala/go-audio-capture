//go:build linux

package alsa

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
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
//
// MinChannels and MaxChannels are the HW_REFINE bounds on the channel count for
// that format; both are 0 when the format is unsupported at any channel count.
// They are bounds only: a device with a discrete channel set (1, 2 or 8) reports
// 1..8.
type BadFormatError struct {
	Channels    int
	Format      uint32
	MinChannels int
	MaxChannels int
}

func (e *BadFormatError) Error() string {
	if e.MaxChannels > 0 {
		return fmt.Sprintf("alsa: %d-channel format id %d not supported (device accepts %d..%d channels)", e.Channels, e.Format, e.MinChannels, e.MaxChannels)
	}
	return fmt.Sprintf("alsa: %d-channel format id %d not supported", e.Channels, e.Format)
}

// GeometryError reports that the device refused every period size and count
// near the requested ones at a rate, format and channel count that passed
// HW_REFINE. Rate, PeriodFrames and Periods are the values the commit was
// attempted with (or the requested ones when no nearby value could be pinned);
// Err is the driver's error (HW_PARAMS for a refused commit, HW_REFINE when no
// nearby value could be pinned).
type GeometryError struct {
	Rate         int
	PeriodFrames int
	Periods      int
	Err          error
}

func (e *GeometryError) Error() string {
	return fmt.Sprintf("alsa: device refused period geometry at %d Hz (%d frames x %d periods): %v", e.Rate, e.PeriodFrames, e.Periods, e.Err)
}

func (e *GeometryError) Unwrap() error { return e.Err }

// errRateRefused is returned by refineGeometry when the rate-pinned refine
// rejects the exact rate (a discrete gap inside the supported window). Negotiate
// maps it to *BadRateError; VerifyRate maps it to "not supported".
var errRateRefused = errors.New("alsa: rate refused at HW_REFINE")

// noNearError reports that refineNear could not pin any value near its target.
type noNearError struct{ err error }

func (e *noNearError) Error() string {
	return "alsa: no attainable value near target: " + e.err.Error()
}
func (e *noNearError) Unwrap() error { return e.err }

// ioctlError wraps an errno with the name of the ioctl that failed, so callers
// never see a bare "invalid argument".
type ioctlError struct {
	Op  string
	Err error
}

func (e *ioctlError) Error() string { return "alsa: " + e.Op + ": " + e.Err.Error() }
func (e *ioctlError) Unwrap() error { return e.Err }

// sysOpen and sysSetNonblock are the open(2) and fcntl(2) seams, so tests can
// observe the open flags without a /dev/snd node.
var (
	sysOpen        = unix.Open
	sysSetNonblock = unix.SetNonblock
)

// resumeRetries and resumeSleep bound the RESUME retry loop in Recover: up to
// 100 retries 10 ms apart. resumeSleep is a var so tests can skip and count
// the waits.
const resumeRetries = 100

var resumeSleep = func() { time.Sleep(10 * time.Millisecond) }

// OpenPCM opens the capture device /dev/snd/pcmC{card}D{device}c for streaming.
// It tries O_RDWR first (what alsa-lib uses) and falls back to O_RDONLY on a
// permission error, since capture needs only reads.
//
// Every open attempt carries O_NONBLOCK. Without it the kernel's snd_pcm_open
// sleeps on pcm->open_wait while every substream is busy; with it the same
// condition becomes EBUSY at once, which the public layer reports as
// ErrDeviceInUse. The flag is cleared again before OpenPCM returns: reads must
// block, and __snd_pcm_lib_xfer takes its blocking mode from substream->f_flags,
// which is copied from the file at attach and refreshed only by the PREPARE
// ioctl. Clearing the flag here, before Negotiate's final PREPARE, therefore
// makes every later read block.
func OpenPCM(card, device int) (*PCM, error) {
	path := fmt.Sprintf("/dev/snd/pcmC%dD%dc", card, device)
	fd, err := sysOpen(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil && errors.Is(err, unix.EACCES) {
		fd, err = sysOpen(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		return nil, &ioctlError{Op: "open " + path, Err: err}
	}
	if err := sysSetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, &ioctlError{Op: "fcntl(F_SETFL) " + path, Err: err}
	}
	return newPCM(fd, ioctl), nil
}

// IsDeviceGone reports whether err (or an error it wraps) is one of the errnos
// that mean the device is missing or was removed: ENODEV, ENXIO, or ENOENT.
// It is the single source of truth for that set, shared by Recover and the
// public layer.
func IsDeviceGone(err error) bool {
	return errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENXIO) || errors.Is(err, unix.ENOENT)
}

// IsRecoverable reports whether err (or an error it wraps) is one Recover
// handles: EPIPE (overrun), ESTRPIPE (suspend) or EIO (stall). It is the single
// source of truth for that set, so Stream.Read counts against its recovery
// budget exactly the errnos Recover can act on.
func IsRecoverable(err error) bool {
	return errors.Is(err, unix.EPIPE) || errors.Is(err, unix.ESTRPIPE) || errors.Is(err, unix.EIO)
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
// The requested rate, channel count and format are honored exactly or the call
// fails with *BadRateError or *BadFormatError. The period size and count are
// buffering parameters, not audio conversion: they move to the nearest values the
// driver accepts (see refineGeometry) and the result is reported in Negotiated.
// A commit the driver refuses after every refine passed is a *GeometryError.
func (p *PCM) Negotiate(rate, channels int, format uint32, periodFrames, periods int) (Negotiated, error) {
	var hw HwParams
	hw.FillAny()
	hw.SetMask(ParamAccess, AccessRWInterleaved)
	hw.SetMask(ParamFormat, uint(format))
	hw.SetMask(ParamSubformat, SubformatSTD)
	hw.SetIntervalExact(ParamChannels, uint32(channels))

	// Discover the supported rate range for this format/channel/access combo.
	// The refine pins access, format, subformat, and channels and leaves rate
	// open, so an EINVAL here (or a rate interval the driver emptied without
	// EINVAL) means the hardware rejects that combination outright, not a rate:
	// report it as a typed format error rather than leaking the raw ioctl string.
	// Device-gone errnos (ENODEV/ENXIO/ENOENT) are disjoint from EINVAL and pass
	// through unchanged for the caller to classify.
	if err := p.refine(&hw); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return Negotiated{}, p.badFormat(channels, format)
		}
		return Negotiated{}, err
	}
	if hw.IntervalEmpty(ParamRate) {
		return Negotiated{}, p.badFormat(channels, format)
	}
	rlo, rhi := hw.Interval(ParamRate)
	if uint32(rate) < rlo || uint32(rate) > rhi {
		return Negotiated{}, &BadRateError{Requested: rate, Min: int(rlo), Max: int(rhi)}
	}

	// Pin the exact rate, then the period size and count nearest the request that
	// the driver accepts. A rate refused at the pin is a discrete gap inside the
	// window. VerifyRate shares refineGeometry so the rate probe and the real open
	// settle on identical geometry.
	if err := p.refineGeometry(&hw, rate, periodFrames, periods); err != nil {
		if errors.Is(err, errRateRefused) {
			return Negotiated{}, &BadRateError{Requested: rate, Min: int(rlo), Max: int(rhi)}
		}
		return Negotiated{}, err
	}
	// Every refine passed with the geometry fully pinned, so an EINVAL at commit
	// is not a format or a window problem. On some USB devices it is the rate
	// itself, which only the commit resolves; GeometryError's doc says so.
	chosenPeriod, _ := hw.Interval(ParamPeriodSize)
	chosenPeriods, _ := hw.Interval(ParamPeriods)
	if err := p.hwParams(&hw); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return Negotiated{}, &GeometryError{Rate: rate, PeriodFrames: int(chosenPeriod), Periods: int(chosenPeriods), Err: err}
		}
		return Negotiated{}, err
	}

	// After HW_PARAMS every interval is resolved to a single value. A driver may
	// commit yet substitute another rate; that is never accepted (no conversion).
	gotRate, _ := hw.Interval(ParamRate)
	if gotRate != uint32(rate) {
		return Negotiated{}, &BadRateError{Requested: rate, Min: int(rlo), Max: int(rhi)}
	}
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
// buffer boundary so START is always explicit. stop_threshold is the buffer
// size (the alsa-lib default): once the buffer fills, the kernel stops the
// stream in XRUN, the next read fails with EPIPE, and Recover restarts it and
// counts the overrun. A threshold at or past sw_params.Boundary (the
// pointer-wrap boundary, far larger than the buffer) never stops the stream,
// so the hardware silently overwrites unread audio and no overrun is ever
// reported.
func (p *PCM) setSwParams(n Negotiated) error {
	sw := SwParams{
		AvailMin:       uframes(n.PeriodFrames),
		StartThreshold: uframes(n.BufferFrames) + 1,
		StopThreshold:  uframes(n.BufferFrames),
		Boundary:       boundary(uframes(n.BufferFrames)),
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

// Probe issues SNDRV_PCM_IOCTL_PVERSION, the cheapest PCM ioctl: it changes no
// state, so its only failures on a live fd are the disconnect ones. After
// snd_card_disconnect swaps the file operations it returns ENODEV; if only the
// PCM is DISCONNECTED, snd_pcm_common_ioctl returns EBADFD. Once the PCM is
// closed it returns EBADF without touching the fd.
func (p *PCM) Probe() error {
	var v int32
	if err := p.guardedIoctl(iocPVersion, unsafe.Pointer(&v)); err != nil {
		return &ioctlError{Op: "PVERSION", Err: err}
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
	p.xferi.Frames = uframes(frames)
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

// Recover handles a transfer error:
//   - EPIPE (overrun): re-prepare and restart.
//   - ESTRPIPE (system suspend): RESUME. A successful RESUME leaves the stream
//     RUNNING (snd_pcm_post_resume restores the pre-suspend state), where PREPARE
//     would fail with EBUSY, so it returns at once. EBADF and device-gone errnos
//     are returned; EAGAIN is retried up to resumeRetries times; any other
//     failure (ENOSYS, a driver trigger error) falls back to PREPARE+START.
//   - EIO (stall): READI_FRAMES timed out in wait_for_avail, which leaves the
//     stream RUNNING, and PREPARE on a running stream is EBUSY, so DROP first,
//     then PREPARE+START.
//   - anything else is returned unchanged as unrecoverable.
func (p *PCM) Recover(err error) error {
	if !IsRecoverable(err) {
		return err
	}
	switch {
	case errors.Is(err, unix.ESTRPIPE):
		return p.resume()
	case errors.Is(err, unix.EIO):
		if e := p.control(iocDrop, "DROP"); e != nil {
			return e
		}
	}
	return p.restart()
}

// restart re-prepares and starts the stream.
func (p *PCM) restart() error {
	if e := p.Prepare(); e != nil {
		return e
	}
	return p.Start()
}

// resume implements the ESTRPIPE arm of Recover.
func (p *PCM) resume() error {
	for attempt := 0; ; attempt++ {
		e := p.guardedIoctl(iocResume, nil)
		if e == nil {
			return nil
		}
		if errors.Is(e, unix.EBADF) || IsDeviceGone(e) {
			return &ioctlError{Op: "RESUME", Err: e}
		}
		if errors.Is(e, unix.EAGAIN) && attempt < resumeRetries {
			resumeSleep()
			continue
		}
		// ENOSYS, EBADFD, a driver error, or EAGAIN that never cleared:
		// snd_pcm_prepare stops a SUSPENDED stream to SETUP itself, so re-prepare.
		return p.restart()
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

// hwFree releases the HW_PARAMS commit and returns the stream to OPEN
// (SNDRV_PCM_IOCTL_HW_FREE). The kernel accepts it only in SETUP and PREPARED.
func (p *PCM) hwFree() error {
	if err := p.guardedIoctl(iocHwFree, nil); err != nil {
		return &ioctlError{Op: "HW_FREE", Err: err}
	}
	return nil
}

func (p *PCM) hwParams(hw *HwParams) error {
	if err := p.guardedIoctl(iocHwParams, unsafe.Pointer(hw)); err != nil {
		return &ioctlError{Op: "HW_PARAMS", Err: err}
	}
	return nil
}

// DefaultPeriods is the streaming open's default periods-per-buffer count when a
// caller does not specify one. VerifyRate uses it too, so the rate probe commits
// with the same geometry the real open will (a target, like DefaultPeriodFrames).
const DefaultPeriods = 4

// DefaultPeriodFrames returns the streaming open's default period length in frames
// for a sample rate: about 20 ms (rate/50), at least one frame. VerifyRate uses it
// too so a probed rate is committed with the same period geometry the real open
// uses by default, never the interval's degenerate minimum (which some USB devices
// reject at high rates). It is a target: refineGeometry moves it to the nearest
// period size the driver accepts.
func DefaultPeriodFrames(rate int) int {
	if pf := rate / 50; pf > 0 {
		return pf
	}
	return 1
}

// refineAll sets Rmask to every parameter and issues HW_REFINE. The kernel clears
// rmask at the end of each refine and applies constraints only to the parameters
// it names, so every refine after the first must set it again or the kernel would
// accept an invalid pin without checking it. (HW_PARAMS sets rmask itself.)
func (p *PCM) refineAll(hw *HwParams) error {
	hw.Rmask = ^uint32(0)
	return p.refine(hw)
}

// refineGeometry pins the exact rate, then moves the period size and the period
// count to the nearest values the driver accepts. Interval bounds do not encode
// step rules (HDA wants period bytes in multiples of 128), so a value inside the
// bounds can still be refused; refineNear asks the kernel instead of guessing.
// Rate, channels and format never move (no conversion). It returns
// errRateRefused when the rate pin is rejected, and *GeometryError when no value
// near the period size or count can be pinned. Negotiate and VerifyRate share it,
// so the probe and the open agree for the default geometry. It assumes hw has
// been refined for the target access/format/channels.
func (p *PCM) refineGeometry(hw *HwParams, rate, periodFrames, periods int) error {
	hw.SetIntervalExact(ParamRate, uint32(rate))
	if err := p.refineAll(hw); err != nil {
		if errors.Is(err, unix.EINVAL) {
			return errRateRefused
		}
		return err
	}
	if !hw.pinnedTo(ParamRate, uint32(rate)) {
		return errRateRefused
	}
	// Period count rather than buffer size second, because Config.Periods is what
	// callers set.
	for _, step := range []struct {
		param  int
		target int
	}{{ParamPeriodSize, periodFrames}, {ParamPeriods, periods}} {
		if err := p.refineNear(hw, step.param, uint32(step.target)); err != nil {
			if nn, ok := errors.AsType[*noNearError](err); ok {
				return &GeometryError{Rate: rate, PeriodFrames: periodFrames, Periods: periods, Err: nn.err}
			}
			return err
		}
	}
	return nil
}

// absDiff returns |a-b| without unsigned underflow.
func absDiff(a, b uint32) uint64 {
	if a > b {
		return uint64(a - b)
	}
	return uint64(b - a)
}

// nearCandidate is a value refineNear may pin.
type nearCandidate struct {
	value uint32
	dist  uint64
}

// refineNear pins param to the accepted value nearest target, after alsa-lib's
// snd_pcm_hw_param_set_near but bounded: no loop, at most one pin refine for the
// target, two probe refines and two more pin refines. A target the driver accepts
// is pinned at once. Otherwise the up probe refines [target, hi] and takes the
// lowest value the kernel leaves, and the down probe refines [lo, target] and
// takes the highest. The nearer wins and a tie goes to the larger value (a larger
// period means fewer wakeups and more overrun headroom). A refined bound is not
// always attainable, so each candidate is pinned and re-refined, falling back to
// the other one. A non-EINVAL error returns at once; EINVAL or an empty interval
// just drops a candidate. On success hw holds the pinned value.
func (p *PCM) refineNear(hw *HwParams, param int, target uint32) error {
	old := *hw.interval(param)
	lo, hi := old.Min, old.Max

	// pin tries to pin param to v on a copy and commits the copy to hw only when
	// the refine keeps exactly [v, v]. It returns the refine error, EINVAL included.
	pin := func(v uint32) (bool, error) {
		c := *hw
		c.SetIntervalExact(param, v)
		if err := p.refineAll(&c); err != nil {
			return false, err
		}
		if !c.pinnedTo(param, v) {
			return false, nil
		}
		*hw = c
		return true, nil
	}
	probe := func(minV, maxV, flags uint32) (Interval, bool, error) {
		c := *hw
		*c.interval(param) = Interval{Min: minV, Max: maxV, Flags: flags}
		if err := p.refineAll(&c); err != nil {
			if errors.Is(err, unix.EINVAL) {
				return Interval{}, false, nil
			}
			return Interval{}, false, err
		}
		return *c.interval(param), !c.IntervalEmpty(param), nil
	}

	last := error(&ioctlError{Op: "HW_REFINE", Err: unix.EINVAL})
	try := func(v uint32) (bool, error) {
		ok, err := pin(v)
		if err != nil && errors.Is(err, unix.EINVAL) {
			last = err
			return false, nil
		}
		return ok, err
	}

	if target >= lo && target <= hi {
		if ok, err := try(target); ok || err != nil {
			return err
		}
	}

	var cands []nearCandidate
	if target <= hi {
		minV := max(target, lo)
		flags := uint32(intervalInteger) | old.Flags&intervalOpenMax
		if minV == lo {
			flags |= old.Flags & intervalOpenMin
		}
		got, ok, err := probe(minV, hi, flags)
		if err != nil {
			return err
		}
		if ok {
			v := got.Min
			if got.Flags&intervalOpenMin != 0 {
				v++
			}
			cands = append(cands, nearCandidate{value: v, dist: absDiff(v, target)})
		}
	}
	if target >= lo {
		maxV := min(target, hi)
		flags := uint32(intervalInteger) | old.Flags&intervalOpenMin
		if maxV == hi {
			flags |= old.Flags & intervalOpenMax
		}
		got, ok, err := probe(lo, maxV, flags)
		if err != nil {
			return err
		}
		if ok && (got.Flags&intervalOpenMax == 0 || got.Max != 0) {
			v := got.Max
			if got.Flags&intervalOpenMax != 0 {
				v--
			}
			cands = append(cands, nearCandidate{value: v, dist: absDiff(v, target)})
		}
	}
	// Stable, so on a tie the up candidate (appended first) stays first.
	slices.SortStableFunc(cands, func(a, b nearCandidate) int { return cmp.Compare(a.dist, b.dist) })

	for _, c := range cands {
		if ok, err := try(c.value); ok || err != nil {
			return err
		}
	}
	return &noNearError{err: last}
}

// badFormat builds the *BadFormatError for a format/channels combination the
// first refine rejected, with the channel range the device does accept for the
// format. A non-EINVAL error from the range probe (the device vanished) is
// returned instead, so a device loss is not reported as a format problem.
func (p *PCM) badFormat(channels int, format uint32) error {
	lo, hi, err := p.channelRange(format)
	if err != nil {
		return err
	}
	return &BadFormatError{Channels: channels, Format: format, MinChannels: lo, MaxChannels: hi}
}

// channelRange returns the channel counts HW_REFINE leaves open for the format
// with the channel count unpinned. It returns 0, 0 when the format is unsupported
// at any channel count (EINVAL, or an emptied channel or rate interval). The
// range is a bound, not a set: a device with discrete counts 1, 2 and 8 yields
// 1..8.
func (p *PCM) channelRange(format uint32) (lo, hi int, err error) {
	var hw HwParams
	hw.FillAny()
	hw.SetMask(ParamAccess, AccessRWInterleaved)
	hw.SetMask(ParamFormat, uint(format))
	hw.SetMask(ParamSubformat, SubformatSTD)
	if rerr := p.refine(&hw); rerr != nil {
		if errors.Is(rerr, unix.EINVAL) {
			return 0, 0, nil
		}
		return 0, 0, rerr
	}
	if hw.IntervalEmpty(ParamChannels) || hw.IntervalEmpty(ParamRate) {
		return 0, 0, nil
	}
	clo, chi := hw.Interval(ParamChannels)
	return int(min(clo, math.MaxInt32)), int(min(chi, math.MaxInt32)), nil
}

// boundary returns a pointer-wrap boundary that is a power-of-two multiple of the
// buffer size, matching alsa-lib's convention for the sw_params boundary. For a
// kernel-negotiated bufferFrames (always far below boundaryCap) it returns the
// largest such multiple not exceeding boundaryCap, which is word-size specific so
// it stays inside a uframes and below the kernel's signed hw_ptr limit (LONG_MAX)
// with doubling-loop headroom. A bufferFrames already above boundaryCap (not
// reachable for a real device) returns bufferFrames unchanged: the minimal
// boundary that still satisfies the kernel's boundary >= buffer_size rule.
// Clamping to boundaryCap instead would violate that rule.
func boundary(bufferFrames uframes) uframes {
	if bufferFrames == 0 {
		return 0
	}
	b := bufferFrames
	// Test b against boundaryCap/2, not b*2 against boundaryCap, so the loop test
	// never computes b*2. That keeps it from wrapping a 32-bit uframes (which would
	// spin forever) for an out-of-range buffer size, or if boundaryCap were ever
	// raised toward the word max; at the current caps no realistic input reaches
	// that, but the guard stays correct regardless.
	for b <= boundaryCap/2 {
		b *= 2
	}
	return b
}
