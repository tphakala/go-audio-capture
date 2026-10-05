//go:build linux

package capture

import (
	"errors"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tphakala/go-audio-capture/internal/alsa"
)

// pcm is the ALSA device seam that Stream drives; *alsa.PCM satisfies it, and
// tests inject a fake. openPCM is a package var so tests can substitute a
// hardware-free implementation.
type pcm interface {
	Negotiate(rate, channels int, format uint32, periodFrames, periods int) (alsa.Negotiated, error)
	Start() error
	ReadI(buf []byte, frames int) (int, error)
	Recover(err error) error
	Probe() error
	Close() error
}

// Recovery budgets for one Read call. Read returns as soon as frames arrive, so
// these count recoveries within one gap in the data and a stream with periodic
// xruns between good reads never reaches them. Real sequences inside one gap are
// 2-3 recoveries long, so 8 leaves margin. One stall restart is tried; a second
// EIO in the same gap means the restart did not help and the caller must reopen.
const (
	maxRecoveriesWithoutData = 8
	maxStallRestarts         = 1
)

var openPCM = func(card, device int) (pcm, error) {
	p, err := alsa.OpenPCM(card, device)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// monoBase anchors monoNow. time.Since reads the monotonic clock, so wall-clock
// steps (NTP, a manual change) cannot make the shortfall window look short or
// long.
var monoBase = time.Now()

// monoNow returns monotonic nanoseconds. It is a package var so tests drive the
// shortfall window with a fake clock instead of sleeping; the real one is a vDSO
// read and does not allocate.
var monoNow = func() int64 { return int64(time.Since(monoBase)) }

// Shortfall check parameters. The hardware pointer of some drivers advances
// slower than real time for a small period geometry, so ReadI keeps succeeding on
// a pointer that is already short and no errno ever reports it. Read therefore
// compares the frames it delivered with the wall-clock time that passed. The
// window is at least minShortfallWindow and 20 buffers long: the buffer's worth
// of frames that may sit unread is the check's slack, and at 20 buffers it is 5%
// of the window, half the 10% tolerance, so a large buffer cannot hide a loss.
const (
	minShortfallWindow  = 2 * int64(time.Second)
	shortfallWindowBufs = 20
	shortfallTolPercent = 10
)

// Stream is an open capture stream. Read is single-consumer; Close may be
// called from another goroutine to unblock a parked Read.
type Stream struct {
	pcm        pcm
	cfg        Config
	frameBytes int
	xruns      atomic.Uint64
	closed     atomic.Bool

	// Shortfall window, touched only by Read (single-consumer). See checkShortfall.
	bufferFrames int64 // negotiated buffer size, the slack the check allows for
	window       int64 // window length in ns: max(2 s, 20 x buffer duration)
	winOn        bool  // the window has started (lazily, at a successful read)
	winStart     int64 // monoNow at the window start
	winFrames    int64 // frames Read returned since winStart
}

// shortfallWindow returns the shortfall window length in nanoseconds for a
// stream that negotiated rate (always positive: validateStreamConfig) and
// bufferFrames, in int64 so 32-bit builds do not overflow.
func shortfallWindow(rate, bufferFrames int) int64 {
	bufferNs := int64(bufferFrames) * int64(time.Second) / int64(rate)
	return max(minShortfallWindow, shortfallWindowBufs*bufferNs)
}

// Open configures and opens a capture stream. It negotiates the exact requested
// rate (failing with *BadRateError otherwise), applies the 20 ms / 4-period
// defaults, and returns a stream that is prepared but not yet started; call
// Start before Read. The period size and count are buffering parameters: they
// are raised to the floor in applyGeometryFloor, the device may then move them
// to the nearest values it accepts, and Negotiated reports the result. A
// negative PeriodFrames or Periods fails with *ConfigError. On failure it
// returns a typed error: *BadDeviceError for a
// malformed device id, *DeviceNotFoundError (which unwraps to ErrDeviceGone) when
// a well-formed stable id matches no present device, *AmbiguousDeviceError when
// it matches more than one, *BadRateError for an unsupported rate,
// *BadFormatError for an unsupported channel/format combination,
// *GeometryError when the device refuses every period geometry near the
// requested one, ErrDeviceInUse when another application holds the device (Open
// fails at once rather than waiting for it to be released), and ErrDeviceGone
// when the device is missing or was removed. A busy card that is no longer the
// unit a stable id resolved to reports ErrDeviceGone, not ErrDeviceInUse. A
// caller that already holds a DeviceInfo can use OpenDevice, which usually
// skips the id resolution.
func Open(cfg Config) (*Stream, error) {
	// Cheap, device-independent checks first, so an obviously invalid config is
	// rejected before a /proc + /sys enumeration resolves the id. SupportedRates
	// (prepareQuery) orders its checks the same way, so both entry points agree on
	// which error a caller sees when more than one field is bad.
	format, err := validateStreamConfig(cfg)
	if err != nil {
		return nil, err
	}
	r, err := resolveForOpen(cfg.Device)
	if err != nil {
		return nil, err
	}
	return openResolved(r, cfg, format)
}

// OpenDevice opens a DeviceInfo that Devices or Resolve returned in this process,
// in most cases without resolving its id again. A caller that has just resolved
// a device (to show it, or to check that it is present) avoids the second /proc
// + /sys enumeration that Open performs; the exception is described below.
//
// It opens d.Card and d.Device, then re-reads the card's identity from sysfs, so
// a stable d.ID (and d.PortID, when set) must still name that card or OpenDevice
// fails with ErrDeviceGone and closes it. A DeviceInfo that has gone stale
// therefore never opens a different unit; call Resolve again and retry. A
// DeviceInfo with IDStable false names a card index, so its address is opened
// unverified, as Open does for "hw:N,D". Do not persist a DeviceInfo: its Card is
// a current-boot index. Persist the ID, or the PortID to pin one of two units
// with the same serial.
//
// A USB ID in the serial form with no PortID (the port could not be derived) is
// resolved as Open would resolve it: nothing read after the open can tell two
// units with one serial apart, so it enumerates and reports
// *AmbiguousDeviceError when more than one matches.
//
// Config.Device is ignored; d decides what opens, and Negotiated reports d.ID as
// the device. Errors are those of Open, plus *ConfigError (field "device") for an
// empty ID and *BadDeviceError when the fields disagree in a way visible before
// the open: a numeric ID naming another Card or Device, a stable ID naming
// another Device, or a PortID that does not match the ID. A stable ID cannot
// name a card index, so a wrong Card with a stable ID surfaces after the open as
// ErrDeviceGone, and in the serial-form-without-PortID case above Card is not
// used at all.
//
//nolint:gocritic // hugeParam: OpenDevice runs once per stream, and a value parameter has no nil case and takes a Resolve result or a map element directly.
func OpenDevice(d DeviceInfo, cfg Config) (*Stream, error) {
	format, err := validateStreamConfig(cfg)
	if err != nil {
		return nil, err
	}
	r, err := resolveDeviceInfo(&d)
	if err != nil {
		return nil, err
	}
	cfg.Device = d.ID
	return openResolved(r, cfg, format)
}

// validateStreamConfig runs the device-independent checks shared by Open and
// OpenDevice and returns the ALSA format for cfg.Format.
func validateStreamConfig(cfg Config) (uint32, error) {
	if cfg.Rate <= 0 {
		return 0, &ConfigError{Field: "rate", Reason: "must be positive"}
	}
	if cfg.Channels < 1 {
		return 0, &ConfigError{Field: "channels", Reason: "must be at least 1"}
	}
	// Zero means default; negative is rejected, not coerced.
	if cfg.PeriodFrames < 0 {
		return 0, &ConfigError{Field: "periodFrames", Reason: "must not be negative"}
	}
	if cfg.Periods < 0 {
		return 0, &ConfigError{Field: "periods", Reason: "must not be negative"}
	}
	return alsaFormat(cfg.Format)
}

// applyGeometryFloor raises a requested geometry to at least a 1 ms period and
// a 20 ms buffer; it never lowers either value. The kernel accepts smaller
// geometries but can then deliver fewer frames than real time without any
// overrun: snd-usb-audio sizes capture URBs to under a period, down to one
// 125 us microframe for sub-ms periods, and a host that misses microframes
// advances the hardware pointer by nothing; snd_pcm_update_hw_ptr0 sees the
// pointer only modulo the buffer and corrects at most one wrap, and its jiffies
// check runs only in xrun_debug mode, so a timer-driven driver (snd-aloop) that
// moves a whole buffer between updates loses those frames silently. The buffer
// floor is met by adding periods, not by growing the period: capture wake-up
// latency follows the period (avail_min), so more periods cost only ring memory.
// Inputs are already defaulted (all >= 1). The arithmetic never forms
// periodFrames * periods or rate + 999, so it cannot overflow a 32-bit int.
func applyGeometryFloor(rate, periodFrames, periods int) (frames, count int) {
	periodFloor := 1 + (rate-1)/1000 // ceil(rate/1000): 1 ms
	bufferFloor := 1 + (rate-1)/50   // ceil(rate/50): 20 ms
	frames = max(periodFrames, periodFloor)
	minPeriods := 1 + (bufferFloor-1)/frames
	return frames, max(periods, minPeriods)
}

// openResolved opens the card r names, confirms it is still the unit r was
// resolved from, and negotiates cfg on it. cfg.Device is only recorded in the
// stream's Negotiated config.
func openResolved(r resolved, cfg Config, format uint32) (*Stream, error) {
	periodFrames := cfg.PeriodFrames
	if periodFrames == 0 {
		periodFrames = alsa.DefaultPeriodFrames(cfg.Rate) // ~20 ms
	}
	periods := cfg.Periods
	if periods == 0 {
		periods = alsa.DefaultPeriods
	}
	periodFrames, periods = applyGeometryFloor(cfg.Rate, periodFrames, periods)

	p, err := openPCM(r.card, r.device)
	if err != nil {
		// A card that took over the index of the one we resolved can be busy or
		// absent for reasons that say nothing about our unit. Show the card is
		// still ours before attributing the failure to it, or a busy stranger reads
		// as ErrDeviceInUse and the caller retries against it forever.
		if verr := verifyCardIdentity(r); verr != nil {
			return nil, verr
		}
		// A device that is absent or removed at open time fails here (the PCM
		// node is missing, or the driver reports the card gone). Classify it the
		// same way as a mid-stream loss so a caller can retire it with
		// errors.Is(err, ErrDeviceGone), and a busy device as ErrDeviceInUse,
		// matching what SupportedRates and the Windows Open path already do.
		return nil, translateOpenError(err, cfg.Channels, cfg.Format)
	}
	// Close the resolve-to-open window: between matching the id to a card index
	// and opening it, that card could have been unplugged and another one taken
	// the index. Confirm the card we are now holding is still the one asked for
	// before any audio is read from it.
	if err := verifyCardIdentity(r); err != nil {
		_ = p.Close()
		return nil, err
	}
	n, err := p.Negotiate(cfg.Rate, cfg.Channels, format, periodFrames, periods)
	if err != nil {
		// EBADFD from a negotiate ioctl means the PCM left the state the call
		// needs; probe before closing to tell a device that vanished mid-open
		// from any other cause.
		if errors.Is(err, unix.EBADFD) && deviceDisconnected(p) {
			err = ErrDeviceGone
		}
		_ = p.Close()
		return nil, translateOpenError(err, cfg.Channels, cfg.Format)
	}
	return &Stream{
		pcm: p,
		cfg: Config{
			Device:       cfg.Device,
			Rate:         n.Rate,
			Channels:     n.Channels,
			Format:       cfg.Format,
			PeriodFrames: n.PeriodFrames,
			Periods:      n.Periods,
		},
		frameBytes:   cfg.Channels * cfg.Format.BytesPerSample(),
		bufferFrames: int64(n.BufferFrames),
		window:       shortfallWindow(n.Rate, n.BufferFrames),
	}, nil
}

// Negotiated returns the configuration the hardware accepted, with the actual
// rate, period size, and period count filled in.
func (s *Stream) Negotiated() Config { return s.cfg }

// Start begins capture. Call it once before the first Read.
func (s *Stream) Start() error {
	if s.closed.Load() {
		return ErrClosed
	}
	if err := s.pcm.Start(); err != nil {
		// A device unplugged between Open and Start surfaces as ErrDeviceGone
		// (ENODEV, or EBADFD confirmed by the probe), as it does at Open and Read.
		return s.terminalError(err)
	}
	return nil
}

// Read fills buf with whole interleaved frames and returns the number of frames
// read. It blocks until at least one period is available. Recoverable failures
// are handled internally and counted (see Xruns): an overrun is restarted, a
// system suspend is resumed, and a stalled stream (EIO, the kernel's read
// timeout) gets one restart. Recovery is bounded per call: a second stall, or
// more than a handful of recoveries without any frames being delivered, returns
// a *StallError (which unwraps to ErrDeviceStalled and to the last errno); if
// one PVERSION probe at that point finds the device gone, it returns
// ErrDeviceGone instead. Read also compares the frames it delivers with wall-clock
// time: over a window of at least 2 s (and 20 buffers) it returns a *ShortfallError,
// which also unwraps to ErrDeviceStalled, when the device delivered more than 10%
// plus one buffer fewer frames than the rate implies (a period geometry the driver
// accepts can make its hardware pointer run slower than real time without any
// overrun). The window starts at the first successful Read and restarts after every
// recovery. Read returns ErrClosed when the stream is closed and
// ErrDeviceGone when the device disappears (e.g. a USB capture device
// unplugged mid-stream, including while Read is parked in the driver); any
// other unrecoverable error is returned unchanged. Any returned error leaves
// the stream unusable (a short read, fewer frames than requested, is not an
// error and returns a nil error): the caller must Close it (Read does not
// release the device fd on its own) and, to resume, Open a new stream.
func (s *Stream) Read(buf []byte) (int, error) {
	if s.closed.Load() {
		return 0, ErrClosed
	}
	frames := len(buf) / s.frameBytes
	if frames == 0 {
		return 0, nil
	}
	var recoveries, stalls int
	for {
		n, err := s.pcm.ReadI(buf, frames)
		if err == nil {
			if serr := s.checkShortfall(n); serr != nil {
				return 0, serr
			}
			return n, nil
		}
		// Only errnos Recover can act on count against the budget; anything
		// else (a Close, an unplug, a state error) is classified at once.
		if s.closed.Load() || !alsa.IsRecoverable(err) {
			return 0, s.terminalError(err)
		}
		stall := errors.Is(err, unix.EIO)
		if recoveries == maxRecoveriesWithoutData || (stall && stalls == maxStallRestarts) {
			return 0, s.stallError(recoveries, err)
		}
		if stall {
			stalls++
		}
		recoveries++
		if rerr := s.pcm.Recover(err); rerr != nil {
			// A recovery ioctl failed: a concurrent Close fails it with EBADF,
			// an unplug with ENODEV or EBADFD, which terminalError classifies.
			return 0, s.terminalError(rerr)
		}
		// A restart or resume is a discontinuity the window must not span: frames
		// lost to an overrun, or time the caller spent away, are not the device
		// running slow.
		s.winOn = false
		s.xruns.Add(1)
	}
}

// checkShortfall accounts n frames returned by a successful ReadI and, once a
// window has elapsed, returns a *ShortfallError when the stream fell short of its
// rate by more than the buffer plus the tolerance. The window starts at the first
// successful read, not at Open or Start: a stream read without Start, or one that
// sat idle before its first Read, would otherwise look like a long stretch with
// nothing delivered. It restarts after every evaluation and after every Recover.
// Frames that arrive while the caller is away from Read stay in the ring and the
// next Read returns them, so a slow consumer does not trip it; one that is away
// longer than the buffer gets an overrun, and the Recover restarts the window.
// Everything is int64 integer arithmetic and one monotonic clock read, with no
// allocation on the passing path.
func (s *Stream) checkShortfall(n int) error {
	now := monoNow()
	if !s.winOn {
		s.winOn, s.winStart, s.winFrames = true, now, 0
		return nil
	}
	s.winFrames += int64(n)
	elapsed := now - s.winStart
	if elapsed < s.window {
		return nil
	}
	rate := int64(s.cfg.Rate)
	// Whole seconds and the remainder are scaled apart: elapsed is the gap since
	// the last evaluation, and elapsed * rate overflows int64 once that gap passes
	// a few hours at 384 kHz.
	secs, rem := elapsed/int64(time.Second), elapsed%int64(time.Second)
	expected := secs*rate + rem*rate/int64(time.Second)
	delivered := s.winFrames
	s.winStart, s.winFrames = now, 0
	if delivered+s.bufferFrames >= expected-expected*shortfallTolPercent/100 {
		return nil
	}
	// Same classification as a recovery that keeps failing: one probe separates a
	// vanished device from a slow one, and a Close racing it wins.
	gone := deviceDisconnected(s.pcm)
	if s.closed.Load() {
		return ErrClosed
	}
	if gone {
		return ErrDeviceGone
	}
	return &ShortfallError{Rate: s.cfg.Rate, Window: time.Duration(elapsed), Expected: expected, Delivered: delivered}
}

// terminalError maps an error that ends Start or Read onto the public errors.
// A Close always wins: Close sets s.closed before pcm.Close, which covers the
// EBADFD that Close's DROP gives a parked read, and EBADF is a closed PCM
// refusing the ioctl. Any other EBADFD is either an unplug (the kernel wakes a
// parked reader with it once the PCM is DISCONNECTED) or an ordinary state
// error, so one PVERSION probe decides; s.closed is checked again after it
// because a Close racing the probe fails it with EBADF. Device-gone errnos
// become ErrDeviceGone; anything else is returned unchanged.
func (s *Stream) terminalError(err error) error {
	if s.closed.Load() || errors.Is(err, unix.EBADF) {
		return ErrClosed
	}
	if errors.Is(err, unix.EBADFD) {
		gone := deviceDisconnected(s.pcm)
		if s.closed.Load() {
			return ErrClosed
		}
		if gone {
			return ErrDeviceGone
		}
		return err
	}
	if alsa.IsDeviceGone(err) {
		return ErrDeviceGone
	}
	return err
}

// stallError builds the error for a Read that exhausted its recovery budget.
// A USB device can die during a burst of overruns or stalls without
// READI_FRAMES ever returning ENODEV or EBADFD, so one PVERSION probe tells a
// vanished device (ErrDeviceGone, retire it) from one the probe did not find
// gone (*StallError, reopen it). s.closed is checked after the probe:
// a Close racing it fails the probe with EBADF, and a Close always wins.
func (s *Stream) stallError(recoveries int, err error) error {
	gone := deviceDisconnected(s.pcm)
	if s.closed.Load() {
		return ErrClosed
	}
	if gone {
		return ErrDeviceGone
	}
	return &StallError{Recoveries: recoveries, Err: err}
}

// prober is the one method deviceDisconnected needs; both the stream seam (pcm)
// and the capability-query seam (ratePCM) satisfy it.
type prober interface{ Probe() error }

// deviceDisconnected reports whether a PVERSION probe shows the device gone:
// ENODEV/ENXIO/ENOENT (the card's file operations were shut down) or EBADFD (the
// PCM alone is DISCONNECTED). EBADF (closed) and success are not disconnects.
// p is either seam (a Stream's pcm or a query's ratePCM).
func deviceDisconnected(p prober) bool {
	perr := p.Probe()
	return perr != nil && (alsa.IsDeviceGone(perr) || errors.Is(perr, unix.EBADFD))
}

// Xruns returns the number of capture discontinuities recovered so far:
// overruns, resumes after a system suspend, and restarted stalls.
func (s *Stream) Xruns() uint64 { return s.xruns.Load() }

// Close stops and closes the stream. It is idempotent and may be called from
// another goroutine: it wakes a Read parked in the driver, which then returns
// ErrClosed, and waits for any ioctl already in flight on the device to finish
// before releasing the fd, so the fd is never closed under a live ioctl. It does
// not wait for Read itself to return: a Read between ioctls (for example waiting
// between RESUME retries after a system suspend) sees the closed stream at its
// next ioctl and returns ErrClosed.
func (s *Stream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	return s.pcm.Close()
}

func alsaFormat(f Format) (uint32, error) {
	switch f {
	case FormatS16LE:
		return alsa.FormatS16LE, nil
	case FormatS24LE:
		return alsa.FormatS24LE, nil
	case FormatS243LE:
		return alsa.FormatS243LE, nil
	case FormatS32LE:
		return alsa.FormatS32LE, nil
	case FormatF32LE:
		return alsa.FormatFloatLE, nil
	default:
		return 0, &ConfigError{Field: fieldFormat, Reason: "must be s16, s24_le, s24_3le, s32, or f32"}
	}
}

// translateOpenError converts the errors the open-and-negotiate path can return
// into the public typed errors so callers never import internal/alsa: an
// unsupported rate becomes *BadRateError, an unsupported channel/format
// combination becomes *BadFormatError (with the channel range the device
// accepts), a period geometry the device refuses becomes *GeometryError, a device held by another application
// becomes ErrDeviceInUse, and a device that is missing or was removed becomes
// ErrDeviceGone. channels and format come from the requested Config so the
// public *BadFormatError carries them. The EBUSY and device-gone mapping mirrors
// translateQueryError so Open and SupportedRates classify a busy or lost device
// the same way. Anything else is returned unchanged.
func translateOpenError(err error, channels int, format Format) error {
	if bre, ok := errors.AsType[*alsa.BadRateError](err); ok {
		return &BadRateError{Requested: bre.Requested, Min: bre.Min, Max: bre.Max}
	}
	if bfe, ok := errors.AsType[*alsa.BadFormatError](err); ok {
		return &BadFormatError{Channels: channels, Format: format, MinChannels: bfe.MinChannels, MaxChannels: bfe.MaxChannels}
	}
	if ge, ok := errors.AsType[*alsa.GeometryError](err); ok {
		return &GeometryError{Rate: ge.Rate, PeriodFrames: ge.PeriodFrames, Periods: ge.Periods, Err: ge.Err}
	}
	if errors.Is(err, unix.EBUSY) {
		return ErrDeviceInUse
	}
	if alsa.IsDeviceGone(err) {
		return ErrDeviceGone
	}
	return err
}
