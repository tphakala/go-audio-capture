//go:build windows && (amd64 || arm64)

package wasapi

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrClosed is returned by Client.Read after Close has been called (including
// when Close unblocks a parked Read). The public layer maps it to capture.ErrClosed.
var ErrClosed = errors.New("wasapi: stream closed")

// standardRates is the ladder probed to classify an unsupported format as a
// rate problem versus a channel/format problem, and to bound a *BadRateError.
// It spans the usual PCM rates plus the ultrasonic rates used for bat audio.
var standardRates = []int{8000, 11025, 16000, 22050, 32000, 44100, 48000, 88200, 96000, 176400, 192000, 250000, 256000, 384000}

// Client is an exclusive-mode WASAPI capture client over one endpoint. Read is
// single-consumer; Close may be called from another goroutine to unblock a
// parked Read.
type Client struct {
	device  unsafe.Pointer // IMMDevice (kept for the buffer-alignment re-Activate)
	client  unsafe.Pointer // IAudioClient
	capture unsafe.Pointer // IAudioCaptureClient (nil until Negotiate)

	audioEvent windows.Handle // auto-reset: signaled when a buffer is ready
	closeEvent windows.Handle // manual-reset: signaled by Close

	neg Negotiated
	// carryBuf retains whole-frame overflow from a packet larger than the caller's
	// buffer; the still-unread bytes are carryBuf[carryOff:]. It is reset with
	// carryBuf[:0] (not resliced past carryOff) so a fresh overflow appends into
	// the retained capacity instead of allocating — making Read amortized zero-alloc
	// even when the caller under-sizes its buffer (only the first overflow, and any
	// growth past the retained capacity, allocates).
	carryBuf []byte
	carryOff int

	nextPos uint64 // monotonic high-water mark of delivered device frames, for overlap/gap detection
	havePos bool   // whether nextPos has been seeded by the first packet
	posLive bool   // whether devicePosition has demonstrated a forward advance (arms overlap-skip)

	// getPacketFn/releaseFn are the IAudioCaptureClient seam fill() drives;
	// production wires the real GetBuffer/ReleaseBuffer, tests inject fakes.
	getPacketFn func() (capturePacket, error)
	releaseFn   func(frames uint32)

	mu       sync.Mutex // serializes buffer access against Close teardown
	closed   atomic.Bool
	readerWG sync.WaitGroup // tracks an in-flight Read so Close waits before closing event handles

	diag captureDiag // device-position evidence, dumped by Close under GAC_WASAPI_DIAG (updated under mu)
}

// capturePacket is one IAudioCaptureClient::GetBuffer result. data aliases the
// WASAPI buffer (valid until releaseFn) and is nil for a SILENT or empty packet;
// empty reports AUDCLNT_S_BUFFER_EMPTY (no packet was returned).
type capturePacket struct {
	data   []byte
	frames uint32
	flags  uint32
	devPos uint64
	empty  bool
}

// Open resolves an endpoint (id, or "" / "default" for the default capture
// endpoint) and activates its IAudioClient. The returned Client is not yet
// negotiated; call Negotiate, then Start, then Read.
func Open(id string) (*Client, error) {
	enum, err := createEnumerator()
	if err != nil {
		return nil, err
	}
	defer release(enum)

	dev, err := resolveDevice(enum, id)
	if err != nil {
		return nil, err
	}
	client, err := activateAudioClient(dev)
	if err != nil {
		release(dev)
		return nil, err
	}
	c := &Client{device: dev, client: client}
	c.getPacketFn = c.getPacket
	c.releaseFn = c.releaseBuffer
	return c, nil
}

// Negotiate configures exclusive-mode capture at the exact rate, channel count,
// and bit depth, or fails with a typed error (*BadRateError for an unsupported
// rate, *BadFormatError for an unsupported channel/format combo, or a sentinel-
// wrapped HRESULT for exclusive-disallowed / device-in-use / device-gone). On
// success the endpoint is initialized event-driven and prepared; call Start. On
// error the Client may hold partially initialized state; the caller must still
// call Close (idempotent) to release it. The sole caller (the public Stream)
// does this.
func (c *Client) Negotiate(rate, channels, bits int) (Negotiated, error) {
	format := pcmFormat(rate, channels, bits)
	if hr := c.isFormatSupported(format); hr != sOK {
		if hr == hrUnsupportedFormat {
			return Negotiated{}, c.classifyUnsupported(rate, channels, bits)
		}
		return Negotiated{}, &hresultError{Op: "IsFormatSupported", HR: hr}
	}

	dur := c.capturePeriod()
	hr := c.initialize(format, dur)
	if hr == hrBufferSizeNotAligned {
		// Documented retry: re-Activate a fresh client and re-Initialize with a
		// period aligned to the endpoint's buffer size.
		aligned := c.bufferFrames()
		dur = int64(10000.0*1000.0/float64(rate)*float64(aligned) + 0.5)
		if err := c.reactivate(); err != nil {
			return Negotiated{}, err
		}
		format = pcmFormat(rate, channels, bits)
		hr = c.initialize(format, dur)
	}
	if hr.failed() {
		return Negotiated{}, &hresultError{Op: "IAudioClient::Initialize", HR: hr}
	}

	evt, err := windows.CreateEvent(nil, 0, 0, nil) // auto-reset
	if err != nil {
		return Negotiated{}, err
	}
	closeEvt, err := windows.CreateEvent(nil, 1, 0, nil) // manual-reset
	if err != nil {
		_ = windows.CloseHandle(evt)
		return Negotiated{}, err
	}
	c.audioEvent, c.closeEvent = evt, closeEvt

	if hr := c.setEventHandle(evt); hr.failed() {
		return Negotiated{}, &hresultError{Op: "SetEventHandle", HR: hr}
	}
	capClient, hr := c.getCaptureClient()
	if hr.failed() {
		return Negotiated{}, &hresultError{Op: "GetService(IAudioCaptureClient)", HR: hr}
	}
	c.capture = capClient

	bufFrames := int(c.bufferFrames())
	c.neg = Negotiated{
		Rate: rate, Channels: channels, Bits: bits,
		PeriodFrames: bufFrames, Periods: 1, BufferFrames: bufFrames,
	}
	return c.neg, nil
}

// classifyUnsupported turns an AUDCLNT_E_UNSUPPORTED_FORMAT into a *BadRateError
// or *BadFormatError by probing the same channel/bit combo across the standard
// rate ladder: if some rate works, the requested rate was the problem (and the
// working rates bound the range); if none works, the channel/format combo is
// unsupported.
func (c *Client) classifyUnsupported(rate, channels, bits int) error {
	lo, hi := 0, 0
	for _, r := range standardRates {
		if c.isFormatSupported(pcmFormat(r, channels, bits)) == sOK {
			if lo == 0 || r < lo {
				lo = r
			}
			if r > hi {
				hi = r
			}
		}
	}
	if hi == 0 {
		return &BadFormatError{Rate: rate, Channels: channels, Bits: bits}
	}
	return &BadRateError{Requested: rate, Min: lo, Max: hi}
}

// Start begins capture (IAudioClient::Start). It holds c.mu so it cannot race a
// concurrent Close that nils the COM pointers.
func (c *Client) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.client == nil {
		return ErrClosed
	}
	r, _, _ := syscall.SyscallN(methodAddr(c.client, mClientStart), uintptr(c.client))
	if h := hresult(uint32(r)); h.failed() {
		return &hresultError{Op: "IAudioClient::Start", HR: h}
	}
	return nil
}

// Read fills buf with whole interleaved frames and returns the number of frames
// read plus whether a DATA_DISCONTINUITY was seen (which the caller counts as an
// xrun). It blocks until at least one frame is available. It returns ErrClosed
// when Close is called (unblocking a parked Read) and a device-gone error when
// the endpoint is invalidated. buf must be a whole number of frames.
func (c *Client) Read(buf []byte) (frames int, discontinuity bool, err error) {
	frameBytes := c.neg.blockAlign()
	if frameBytes == 0 || len(buf) < frameBytes {
		return 0, false, nil
	}
	// Register as the active reader under c.mu so Close waits for this Read to
	// exit before it closes the event handles passed to WaitForMultipleObjects.
	// Setting c.closed under c.mu in Close makes every Add happen-before Close's
	// Wait, so there is no Add-after-Wait race.
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return 0, false, ErrClosed
	}
	c.readerWG.Add(1)
	c.mu.Unlock()
	defer c.readerWG.Done()

	for {
		if c.closed.Load() {
			return 0, false, ErrClosed
		}
		c.mu.Lock()
		if c.closed.Load() {
			c.mu.Unlock()
			return 0, false, ErrClosed
		}
		n, disc, err := c.fill(buf)
		c.mu.Unlock()
		if err != nil {
			return 0, false, err
		}
		if n > 0 {
			return n, disc, nil
		}
		// Nothing available yet: wait for the next buffer or for Close.
		w, werr := windows.WaitForMultipleObjects(
			[]windows.Handle{c.audioEvent, c.closeEvent}, false, windows.INFINITE)
		if c.closed.Load() {
			return 0, false, ErrClosed
		}
		if werr != nil {
			return 0, false, werr
		}
		if w != 0 { // index 1 = closeEvent: treat as closed
			return 0, false, ErrClosed
		}
	}
}

// fill serves any carry-over, then reads exactly ONE GetBuffer packet into buf.
// It does not block and the caller holds c.mu.
//
// Exclusive event-driven capture presents at most one buffer per audio-ready
// event, so fill reads a single packet rather than looping until
// AUDCLNT_S_BUFFER_EMPTY: that shared-mode drain idiom makes some drivers
// (notably Creative's) re-present an already-delivered buffer, which the loop
// then delivered twice (a backward devicePosition jump). fill instead uses the
// packet's devicePosition as ground truth: a packet behind the expected position
// is an overlap whose already-delivered prefix is skipped, and a packet ahead is
// a gap (the device dropped frames on an overrun) reported as a discontinuity.
// This keeps delivered frames equal to the device's own advance on every driver.
func (c *Client) fill(buf []byte) (frames int, discontinuity bool, err error) {
	frameBytes := c.neg.blockAlign()
	whole := (len(buf) / frameBytes) * frameBytes
	n := 0

	if c.carryOff < len(c.carryBuf) {
		m := copy(buf[:whole], c.carryBuf[c.carryOff:])
		c.carryOff += m
		n += m
		if c.carryOff >= len(c.carryBuf) {
			// Fully drained: reset to length 0 (keeping capacity) so the next
			// overflow append reuses this backing array rather than allocating.
			c.carryBuf = c.carryBuf[:0]
			c.carryOff = 0
		}
		if n == whole {
			return n / frameBytes, false, nil
		}
	}

	pkt, err := c.getPacketFn()
	if err != nil {
		return n / frameBytes, false, err
	}
	if pkt.empty || pkt.frames == 0 {
		if !pkt.empty {
			c.releaseFn(0)
		}
		return n / frameBytes, false, nil
	}
	c.diag.record(pkt.devPos, pkt.frames, pkt.flags)

	disc := pkt.flags&bufferFlagsDataDiscontinuity != 0
	end := pkt.devPos + uint64(pkt.frames)
	// A packet that extends past the high-water mark is a forward advance, which
	// proves devicePosition is live and (stickily) arms overlap-skipping. This is
	// evaluated BEFORE the skip so a packet that both advances and overlaps still
	// gets its overlapping prefix skipped. Some drivers report a constant position
	// (e.g. always 0): they never advance, posLive never arms, and every packet is
	// delivered unconditionally as the pre-devicePosition drain loop did, rather
	// than being dropped as a false overlap (which would silence the stream).
	if c.havePos && end > c.nextPos {
		c.posLive = true
	}
	skip := uint32(0)
	if c.havePos && c.posLive {
		switch {
		case pkt.devPos < c.nextPos: // overlap: skip frames already delivered
			if over := c.nextPos - pkt.devPos; over >= uint64(pkt.frames) {
				skip = pkt.frames // whole packet already delivered
			} else {
				skip = uint32(over)
			}
		case pkt.devPos > c.nextPos: // gap: device dropped frames (overrun)
			disc = true
		}
	}
	// nextPos is a monotonic high-water mark; never let a re-presented earlier
	// packet rewind it, which would re-admit already-delivered frames as a fresh
	// region or misread the next contiguous packet as a gap.
	if !c.havePos || end > c.nextPos {
		c.nextPos = end
	}
	c.havePos = true

	if use := pkt.frames - skip; use > 0 {
		off := int(skip) * frameBytes
		pktBytes := int(use) * frameBytes
		copied := min(pktBytes, whole-n)
		if pkt.flags&bufferFlagsSilent != 0 || pkt.data == nil {
			// SILENT: packet memory is undefined; deliver zeros.
			clear(buf[n : n+copied])
		} else {
			copy(buf[n:n+copied], pkt.data[off:off+pktBytes])
		}
		if copied < pktBytes {
			// Overflow beyond the caller's buffer: keep the remainder for the next
			// Read (SILENT contributes zeros). carryBuf is empty here (any prior
			// carry was fully drained above), so these appends into carryBuf[:0]
			// reuse its retained capacity instead of allocating.
			c.carryOff = 0
			if pkt.flags&bufferFlagsSilent != 0 || pkt.data == nil {
				need := pktBytes - copied
				c.carryBuf = slices.Grow(c.carryBuf[:0], need)[:need]
				clear(c.carryBuf)
			} else {
				c.carryBuf = append(c.carryBuf[:0], pkt.data[off+copied:off+pktBytes]...)
			}
		}
		n += copied
	}
	c.releaseFn(pkt.frames)
	return n / frameBytes, disc, nil
}

// getPacket calls IAudioCaptureClient::GetBuffer once. It returns an empty
// packet for AUDCLNT_S_BUFFER_EMPTY. The returned data aliases the WASAPI buffer
// and stays valid until releaseBuffer; it is nil for SILENT packets.
func (c *Client) getPacket() (capturePacket, error) {
	var pData unsafe.Pointer
	var numFrames, flags uint32
	var devPos, qpcPos uint64
	r, _, _ := syscall.SyscallN(methodAddr(c.capture, mCaptureGetBuffer),
		uintptr(c.capture),
		uintptr(unsafe.Pointer(&pData)), uintptr(unsafe.Pointer(&numFrames)),
		uintptr(unsafe.Pointer(&flags)),
		uintptr(unsafe.Pointer(&devPos)), uintptr(unsafe.Pointer(&qpcPos)))
	hr := hresult(uint32(r))
	if hr == hrBufferEmpty {
		return capturePacket{empty: true}, nil
	}
	if hr.failed() {
		return capturePacket{}, &hresultError{Op: "IAudioCaptureClient::GetBuffer", HR: hr}
	}
	var data []byte
	if numFrames > 0 && flags&bufferFlagsSilent == 0 && pData != nil {
		data = unsafe.Slice((*byte)(pData), int(numFrames)*c.neg.blockAlign())
	}
	return capturePacket{data: data, frames: numFrames, flags: flags, devPos: devPos}, nil
}

// Negotiated returns the accepted configuration.
func (c *Client) Negotiated() Negotiated { return c.neg }

// Close stops and releases the stream. It is idempotent and unblocks a Read
// parked in WaitForMultipleObjects by signaling the close event, then tears down
// the COM objects under c.mu so it cannot race a concurrent fill.
func (c *Client) Close() error {
	// Set closed under c.mu and wake a parked Read. Doing the Swap under the lock
	// orders it against Read's mu-guarded readerWG.Add, so Wait below cannot race
	// an Add.
	c.mu.Lock()
	if c.closed.Swap(true) {
		c.mu.Unlock()
		return nil
	}
	if c.closeEvent != 0 {
		_ = windows.SetEvent(c.closeEvent) // wake a parked Read
	}
	c.mu.Unlock()

	// Wait for an in-flight Read to observe closed and return before we destroy
	// the event handles it may still be waiting on (closing a handle another
	// thread waits on is Win32 UB, and the integer could be recycled).
	c.readerWG.Wait()

	c.mu.Lock()
	if c.client != nil {
		_, _, _ = syscall.SyscallN(methodAddr(c.client, mClientStop), uintptr(c.client))
	}
	release(c.capture)
	release(c.client)
	release(c.device)
	c.capture, c.client, c.device = nil, nil, nil
	c.mu.Unlock()

	if c.audioEvent != 0 {
		_ = windows.CloseHandle(c.audioEvent)
	}
	if c.closeEvent != 0 {
		_ = windows.CloseHandle(c.closeEvent)
	}
	c.diag.dump(c.neg.Rate)
	return nil
}

// ---- thin IAudioClient / IAudioCaptureClient wrappers -----------------------

func (c *Client) isFormatSupported(f *waveFormatExtensible) hresult {
	var closest unsafe.Pointer
	r, _, _ := syscall.SyscallN(methodAddr(c.client, mClientIsFormatSupported),
		uintptr(c.client), uintptr(shareModeExclusive),
		uintptr(unsafe.Pointer(f)), uintptr(unsafe.Pointer(&closest)))
	if closest != nil {
		coTaskFree(closest)
	}
	return hresult(uint32(r))
}

// capturePeriod returns the endpoint's DEFAULT device period (100-ns units), not
// its minimum. The minimum period (e.g. 3 ms) is too tight for the single-
// GetBuffer-per-event capture loop at high sample rates: the loop cannot service
// each buffer before the next arrives, so the device overruns and drops frames
// (seen as ~60% loss at 192 kHz). The default period (~10 ms) gives the read
// loop enough headroom to keep up, trading a few ms of latency for gap-free
// capture. The buffer-size-not-aligned retry still applies if the endpoint
// requires an aligned period.
func (c *Client) capturePeriod() int64 {
	var defPer, minPer int64
	_, _, _ = syscall.SyscallN(methodAddr(c.client, mClientGetDevicePeriod),
		uintptr(c.client), uintptr(unsafe.Pointer(&defPer)), uintptr(unsafe.Pointer(&minPer)))
	return defPer
}

func (c *Client) initialize(f *waveFormatExtensible, dur int64) hresult {
	r, _, _ := syscall.SyscallN(methodAddr(c.client, mClientInitialize),
		uintptr(c.client), uintptr(shareModeExclusive), uintptr(streamFlagsEventCallback),
		uintptr(uint64(dur)), uintptr(uint64(dur)), uintptr(unsafe.Pointer(f)), 0)
	return hresult(uint32(r))
}

func (c *Client) bufferFrames() uint32 {
	var frames uint32
	_, _, _ = syscall.SyscallN(methodAddr(c.client, mClientGetBufferSize),
		uintptr(c.client), uintptr(unsafe.Pointer(&frames)))
	return frames
}

func (c *Client) setEventHandle(evt windows.Handle) hresult {
	r, _, _ := syscall.SyscallN(methodAddr(c.client, mClientSetEventHandle),
		uintptr(c.client), uintptr(evt))
	return hresult(uint32(r))
}

func (c *Client) getCaptureClient() (unsafe.Pointer, hresult) {
	var capClient unsafe.Pointer
	r, _, _ := syscall.SyscallN(methodAddr(c.client, mClientGetService),
		uintptr(c.client), uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&capClient)))
	return capClient, hresult(uint32(r))
}

func (c *Client) releaseBuffer(frames uint32) {
	_, _, _ = syscall.SyscallN(methodAddr(c.capture, mCaptureReleaseBuffer), uintptr(c.capture), uintptr(frames))
}

// reactivate releases the current IAudioClient and activates a fresh one from
// the retained IMMDevice, for the buffer-alignment retry (a client that failed
// Initialize cannot be re-initialized).
func (c *Client) reactivate() error {
	release(c.client)
	client, err := activateAudioClient(c.device)
	if err != nil {
		c.client = nil
		return err
	}
	c.client = client
	return nil
}
