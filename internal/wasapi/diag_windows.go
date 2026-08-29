//go:build windows

package wasapi

import (
	"fmt"
	"os"
)

// captureDiag accumulates device-position evidence from each GetBuffer packet so
// a real-time capture over/under-delivery can be root-caused on hardware. It is
// updated under c.mu in fill() and dumped by Close only when GAC_WASAPI_DIAG is
// set in the environment; it has no effect on the capture path otherwise.
//
// devicePosition is the authoritative stream position (frames) of the first
// frame in a packet. For gap-free real-time capture each packet's devicePosition
// must equal the previous packet's devicePosition + its frame count. A forward
// jump means the driver dropped frames (an overrun); a backward jump means the
// driver re-presented frames the loop already delivered (a duplicate). Summing
// numFrames and comparing it to the devicePosition span (last end - first) tells
// whether the delivered frame count matches the device's own advance.
type captureDiag struct {
	calls         uint64 // GetBuffer calls that returned a non-empty packet
	deviceFrames  uint64 // raw sum of packet frame counts (a naive drain would deliver this; the fixed loop delivers ~span, skipping re-presented frames)
	firstPos      uint64
	firstSet      bool
	lastPosEnd    uint64 // devicePosition + numFrames of the most recent packet
	discontinuous uint64 // packets flagged AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY
	silent        uint64 // packets flagged AUDCLNT_BUFFERFLAGS_SILENT
	gapPkts       uint64 // packets whose devicePosition jumped forward (dropped frames)
	gapFrames     uint64 // total forward jump (frames the device dropped)
	overlapPkts   uint64 // packets whose devicePosition went backward (re-presented frames)
	overlapFrames uint64 // total backward jump (frames delivered more than once)
}

// record folds one GetBuffer packet (numFrames > 0) into the diagnostics.
func (d *captureDiag) record(devPos uint64, numFrames, flags uint32) {
	d.calls++
	if d.firstSet {
		switch delta := int64(devPos) - int64(d.lastPosEnd); {
		case delta > 0:
			d.gapPkts++
			d.gapFrames += uint64(delta)
		case delta < 0:
			d.overlapPkts++
			d.overlapFrames += uint64(-delta)
		}
	} else {
		d.firstPos = devPos
		d.firstSet = true
	}
	d.lastPosEnd = devPos + uint64(numFrames)
	d.deviceFrames += uint64(numFrames)
	if flags&bufferFlagsDataDiscontinuity != 0 {
		d.discontinuous++
	}
	if flags&bufferFlagsSilent != 0 {
		d.silent++
	}
}

// dump writes the diagnostics to stderr when GAC_WASAPI_DIAG is set. span is the
// device's own reported advance (≈ what the fixed loop delivers); deviceFrames is
// the raw packet-frame sum (what a naive drain would deliver). They diverge
// exactly by the net overlap/gap, which is the root-cause signal.
func (d *captureDiag) dump(rate int) {
	if os.Getenv("GAC_WASAPI_DIAG") == "" {
		return
	}
	span := int64(d.lastPosEnd) - int64(d.firstPos)
	fmt.Fprintf(os.Stderr,
		"wasapi diag: rate=%d getBuffer=%d deviceFrames=%d devPosSpan=%d (deviceFrames-span=%d) "+
			"discontinuities=%d silent=%d gaps=%d(+%d frames dropped) overlaps=%d(-%d frames duplicated)\n",
		rate, d.calls, d.deviceFrames, span, int64(d.deviceFrames)-span,
		d.discontinuous, d.silent, d.gapPkts, d.gapFrames, d.overlapPkts, d.overlapFrames)
}
