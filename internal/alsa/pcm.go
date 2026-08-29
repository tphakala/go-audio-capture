//go:build linux

package alsa

import (
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctlFunc is the ioctl seam. Production wires the package ioctl; tests inject
// a fake that fills HwParams/Xferi and returns canned errnos, so negotiate and
// recovery are covered without hardware.
type ioctlFunc func(fd int, req uintptr, arg unsafe.Pointer) error

// PCM is an open capture PCM device.
type PCM struct {
	fd    int
	ioctl ioctlFunc
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

// ioctlError wraps an errno with the name of the ioctl that failed, so callers
// never see a bare "invalid argument".
type ioctlError struct {
	Op  string
	Err error
}

func (e *ioctlError) Error() string { return "alsa: " + e.Op + ": " + e.Err.Error() }
func (e *ioctlError) Unwrap() error { return e.Err }

// OpenPCM opens the capture device /dev/snd/pcmC{card}D{device}c. It tries
// O_RDWR first (what alsa-lib uses) and falls back to O_RDONLY on a permission
// error, since capture needs only reads.
func OpenPCM(card, device int) (*PCM, error) {
	path := fmt.Sprintf("/dev/snd/pcmC%dD%dc", card, device)
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil && errors.Is(err, unix.EACCES) {
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, &ioctlError{Op: "open " + path, Err: err}
	}
	return &PCM{fd: fd, ioctl: ioctl}, nil
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
	if err := p.refine(&hw); err != nil {
		return Negotiated{}, err
	}
	rlo, rhi := hw.Interval(ParamRate)
	if uint32(rate) < rlo || uint32(rate) > rhi {
		return Negotiated{}, &BadRateError{Requested: rate, Min: int(rlo), Max: int(rhi)}
	}

	// Pin the exact rate and a sane period geometry clamped into the supported
	// ranges, then commit. A commit failure here (e.g. a discrete-rate gap
	// inside the range, or an unsupported period size) surfaces honestly rather
	// than as a substituted configuration.
	hw.SetIntervalExact(ParamRate, uint32(rate))
	plo, phi := hw.Interval(ParamPeriodSize)
	hw.SetIntervalExact(ParamPeriodSize, clampU32(uint32(periodFrames), plo, phi))
	nlo, nhi := hw.Interval(ParamPeriods)
	hw.SetIntervalExact(ParamPeriods, clampU32(uint32(periods), nlo, nhi))
	if err := p.hwParams(&hw); err != nil {
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
	if err := p.ioctl(p.fd, iocSwParams, unsafe.Pointer(&sw)); err != nil {
		return &ioctlError{Op: "SW_PARAMS", Err: err}
	}
	return nil
}

// Prepare moves the stream to the prepared state (SNDRV_PCM_IOCTL_PREPARE).
func (p *PCM) Prepare() error { return p.control(iocPrepare, "PREPARE") }

// Start begins capture (SNDRV_PCM_IOCTL_START).
func (p *PCM) Start() error { return p.control(iocStart, "START") }

func (p *PCM) control(req uintptr, op string) error {
	if err := p.ioctl(p.fd, req, nil); err != nil {
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
	x := Xferi{
		Buf:    unsafe.Pointer(&buf[0]),
		Frames: uint64(frames),
	}
	err := p.ioctl(p.fd, iocReadIFrames, unsafe.Pointer(&x))
	runtime.KeepAlive(buf)
	if err != nil {
		return 0, err
	}
	if x.Result < 0 {
		return 0, unix.Errno(-x.Result)
	}
	return int(x.Result), nil
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
			e := p.ioctl(p.fd, iocResume, nil)
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

// Close stops the stream and closes the fd. It is idempotent. The best-effort
// DROP unblocks any reader currently parked in READI_FRAMES so it returns and
// the read goroutine can exit.
func (p *PCM) Close() error {
	if p.fd < 0 {
		return nil
	}
	_ = p.ioctl(p.fd, iocDrop, nil) // best effort: unblock a parked reader
	err := unix.Close(p.fd)
	p.fd = -1
	return err
}

func (p *PCM) refine(hw *HwParams) error {
	if err := p.ioctl(p.fd, iocHwRefine, unsafe.Pointer(hw)); err != nil {
		return &ioctlError{Op: "HW_REFINE", Err: err}
	}
	return nil
}

func (p *PCM) hwParams(hw *HwParams) error {
	if err := p.ioctl(p.fd, iocHwParams, unsafe.Pointer(hw)); err != nil {
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
