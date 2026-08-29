//go:build windows && (amd64 || arm64)

package wasapi

import (
	"bytes"
	"errors"
	"slices"
	"testing"
)

// fakeCapture scripts the getPacketFn/releaseFn seam so fill()'s frame
// accounting is testable without audio hardware. It records every releaseFn
// argument so tests can assert IAudioCaptureClient::ReleaseBuffer is always
// called with the full packet frame count (even when frames are skipped).
type fakeCapture struct {
	pkts     []capturePacket
	err      error // if set, getPacketFn returns it once the errAt'th packet is reached
	errAt    int
	i        int
	released []uint32
}

// fillClient builds a Client wired to the fake. channels/bits set blockAlign.
func fillClient(channels, bits int, pkts []capturePacket) (*Client, *fakeCapture) {
	fc := &fakeCapture{pkts: pkts}
	c := &Client{neg: Negotiated{Channels: channels, Bits: bits}}
	c.getPacketFn = func() (capturePacket, error) {
		if fc.err != nil && fc.i == fc.errAt {
			fc.i++
			return capturePacket{}, fc.err
		}
		if fc.i >= len(fc.pkts) {
			return capturePacket{empty: true}, nil
		}
		p := fc.pkts[fc.i]
		fc.i++
		return p, nil
	}
	c.releaseFn = func(frames uint32) { fc.released = append(fc.released, frames) }
	return c, fc
}

// pcm16 makes a mono S16 buffer of `frames` frames all set to byte v.
func pcm16(frames int, v byte) []byte {
	b := make([]byte, frames*2)
	for i := range b {
		b[i] = v
	}
	return b
}

// pcm16seq makes a mono S16 buffer whose frame i is two bytes of value base+i, so
// a delivered region's exact offset is pinned (a wrong offset changes the bytes).
func pcm16seq(frames int, base byte) []byte {
	b := make([]byte, frames*2)
	for f := 0; f < frames; f++ {
		b[2*f] = base + byte(f)
		b[2*f+1] = base + byte(f)
	}
	return b
}

func TestFillDeliversWholePacket(t *testing.T) {
	c, fc := fillClient(1, 16, []capturePacket{{data: pcm16(4, 0xAA), frames: 4, devPos: 0}})
	buf := make([]byte, 4*2)
	n, disc, err := c.fill(buf)
	if err != nil || disc || n != 4 {
		t.Fatalf("fill = (%d, %v, %v), want (4, false, nil)", n, disc, err)
	}
	if !bytes.Equal(buf, pcm16(4, 0xAA)) {
		t.Errorf("buf = %v, want 4 frames of 0xAA", buf)
	}
	if !slices.Equal(fc.released, []uint32{4}) {
		t.Errorf("releaseFn calls = %v, want [4]", fc.released)
	}
}

// TestFillSkipsOverlap is the ZxR bug: after a forward advance arms position
// tracking, a driver re-presents a buffer overlapping already-delivered frames
// (backward devicePosition). The overlapping prefix must be skipped, delivering
// only the new tail, and ReleaseBuffer must still get the FULL packet frames.
func TestFillSkipsOverlap(t *testing.T) {
	c, fc := fillClient(1, 16, []capturePacket{
		{data: pcm16seq(4, 0x10), frames: 4, devPos: 0}, // frames 0..4
		{data: pcm16seq(4, 0x20), frames: 4, devPos: 6}, // advance to arm posLive
		{data: pcm16seq(4, 0x40), frames: 4, devPos: 8}, // 8..12: overlaps by 2 (nextPos=10)
	})
	if n, _, _ := c.fill(make([]byte, 4*2)); n != 4 {
		t.Fatalf("first fill n = %d, want 4", n)
	}
	if n, _, _ := c.fill(make([]byte, 4*2)); n != 4 {
		t.Fatalf("second fill (advance) n = %d, want 4", n)
	}
	// Third: devPos 8 < nextPos 10, overlap of 2 frames skipped; deliver the last
	// 2 frames of 0x40 (bytes for frames 2 and 3: 0x42, 0x43), not the head.
	buf := make([]byte, 4*2)
	n, _, _ := c.fill(buf)
	if n != 2 {
		t.Fatalf("third fill n = %d, want 2 (2-frame overlap skipped)", n)
	}
	wantTail := pcm16seq(4, 0x40)[4:8] // frames 2,3 => 0x42,0x42,0x43,0x43
	if !bytes.Equal(buf[:4], wantTail) {
		t.Errorf("delivered = %v, want the non-overlapping tail %v", buf[:4], wantTail)
	}
	// ReleaseBuffer must get the full frame count of every packet, incl. the skip.
	if !slices.Equal(fc.released, []uint32{4, 4, 4}) {
		t.Errorf("releaseFn calls = %v, want [4 4 4] (full frames even on the skipped overlap)", fc.released)
	}
}

func TestFillWholePacketOverlapDeliversNothing(t *testing.T) {
	c, _ := fillClient(1, 16, []capturePacket{
		{data: pcm16(4, 0x11), frames: 4, devPos: 0}, // 0..4
		{data: pcm16(4, 0x22), frames: 4, devPos: 4}, // 4..8: advance, arms posLive
		{data: pcm16(4, 0x33), frames: 4, devPos: 0}, // fully re-presents 0..4
	})
	_, _, _ = c.fill(make([]byte, 4*2))
	_, _, _ = c.fill(make([]byte, 4*2))
	n, _, _ := c.fill(make([]byte, 4*2))
	if n != 0 {
		t.Errorf("re-presented packet delivered %d frames, want 0", n)
	}
}

// TestFillDevPosAlwaysZeroDeliversAll covers Agent 6's HIGH: a driver that never
// advances devicePosition (constant 0) must NOT have its packets dropped as false
// overlaps; every packet is delivered, as the pre-devicePosition loop did.
func TestFillDevPosAlwaysZeroDeliversAll(t *testing.T) {
	c, _ := fillClient(1, 16, []capturePacket{
		{data: pcm16(4, 0x01), frames: 4, devPos: 0},
		{data: pcm16(4, 0x02), frames: 4, devPos: 0},
		{data: pcm16(4, 0x03), frames: 4, devPos: 0},
	})
	total := 0
	for range 3 {
		n, disc, err := c.fill(make([]byte, 4*2))
		if err != nil || disc {
			t.Fatalf("fill = (_, %v, %v), want no disc/err", disc, err)
		}
		total += n
	}
	if total != 12 {
		t.Errorf("constant-devPos stream delivered %d frames, want 12 (all)", total)
	}
}

// TestFillFragmentedRepresentationNoDuplicate covers Agent 2's Medium: a
// fragmented re-presentation of already-delivered frames must not rewind the
// high-water mark and re-admit those frames.
func TestFillFragmentedRepresentationNoDuplicate(t *testing.T) {
	c, _ := fillClient(1, 16, []capturePacket{
		{data: pcm16seq(4, 0x10), frames: 4, devPos: 0}, // 0..4
		{data: pcm16seq(4, 0x20), frames: 4, devPos: 4}, // 4..8: advance, arms posLive; nextPos=8
		{data: pcm16(2, 0x30), frames: 2, devPos: 0},    // re-present 0..2 (whole overlap)
		{data: pcm16(2, 0x40), frames: 2, devPos: 2},    // re-present 2..4 (whole overlap)
	})
	total := 0
	for range 4 {
		n, _, _ := c.fill(make([]byte, 4*2))
		total += n
	}
	if total != 8 {
		t.Errorf("delivered %d frames, want 8 (fragmented re-presentation must not duplicate)", total)
	}
}

func TestFillForwardGapIsDiscontinuity(t *testing.T) {
	c, _ := fillClient(1, 16, []capturePacket{
		{data: pcm16(4, 0x11), frames: 4, devPos: 0},  // 0..4
		{data: pcm16(4, 0x22), frames: 4, devPos: 4},  // 4..8: advance, arms posLive
		{data: pcm16(4, 0x33), frames: 4, devPos: 20}, // jumps ahead: device dropped frames
	})
	_, _, _ = c.fill(make([]byte, 4*2))
	_, _, _ = c.fill(make([]byte, 4*2))
	n, disc, _ := c.fill(make([]byte, 4*2))
	if !disc {
		t.Error("forward devicePosition jump should report a discontinuity")
	}
	if n != 4 {
		t.Errorf("gap packet delivered %d frames, want 4 (new data still delivered)", n)
	}
}

func TestFillDataDiscontinuityFlag(t *testing.T) {
	c, _ := fillClient(1, 16, []capturePacket{
		{data: pcm16(4, 0x11), frames: 4, devPos: 0, flags: bufferFlagsDataDiscontinuity},
	})
	_, disc, _ := c.fill(make([]byte, 4*2))
	if !disc {
		t.Error("DATA_DISCONTINUITY flag should report a discontinuity")
	}
}

// TestFillGetPacketError covers the getPacketFn error path (device-gone etc.).
func TestFillGetPacketError(t *testing.T) {
	sentinel := errors.New("getbuffer failed")
	c, fc := fillClient(1, 16, []capturePacket{{data: pcm16(4, 0x11), frames: 4, devPos: 0}})
	fc.err, fc.errAt = sentinel, 0 // first getPacket returns the error
	_, _, err := c.fill(make([]byte, 4*2))
	if !errors.Is(err, sentinel) {
		t.Errorf("fill err = %v, want the getPacketFn error", err)
	}
}

// TestFillZeroFrameNonEmpty covers a {frames:0, empty:false} packet: nothing
// delivered, ReleaseBuffer(0) still called, no discontinuity.
func TestFillZeroFrameNonEmpty(t *testing.T) {
	c, fc := fillClient(1, 16, []capturePacket{{frames: 0, devPos: 0}})
	n, disc, err := c.fill(make([]byte, 4*2))
	if n != 0 || disc || err != nil {
		t.Errorf("fill = (%d, %v, %v), want (0, false, nil)", n, disc, err)
	}
	if !slices.Equal(fc.released, []uint32{0}) {
		t.Errorf("releaseFn calls = %v, want [0] (zero-frame packet still released)", fc.released)
	}
}

func TestFillCarriesOverflow(t *testing.T) {
	data := append(pcm16(2, 0x33), pcm16(2, 0x44)...) // 4 frames: 2x0x33 then 2x0x44
	c, _ := fillClient(1, 16, []capturePacket{{data: data, frames: 4, devPos: 0}})

	buf := make([]byte, 2*2) // room for only 2 frames
	n, _, _ := c.fill(buf)
	if n != 2 || !bytes.Equal(buf, pcm16(2, 0x33)) {
		t.Fatalf("first fill = %d frames %v, want 2 frames of 0x33", n, buf)
	}
	buf2 := make([]byte, 2*2)
	n, _, _ = c.fill(buf2)
	if n != 2 || !bytes.Equal(buf2, pcm16(2, 0x44)) {
		t.Fatalf("carry fill = %d frames %v, want 2 frames of 0x44", n, buf2)
	}
}

func TestFillSilentDeliversZeros(t *testing.T) {
	c, _ := fillClient(1, 16, []capturePacket{{data: nil, frames: 4, devPos: 0, flags: bufferFlagsSilent}})
	buf := bytes.Repeat([]byte{0xFF}, 4*2)
	n, _, _ := c.fill(buf)
	if n != 4 {
		t.Fatalf("silent fill n = %d, want 4", n)
	}
	if !bytes.Equal(buf, make([]byte, 4*2)) {
		t.Errorf("silent packet should deliver zeros, got %v", buf)
	}
}

// TestFillSilentOverflowCarriesZeros covers the SILENT overflow-to-carry branch:
// a silent packet larger than buf carries a zeroed remainder.
func TestFillSilentOverflowCarriesZeros(t *testing.T) {
	c, _ := fillClient(1, 16, []capturePacket{{data: nil, frames: 4, devPos: 0, flags: bufferFlagsSilent}})
	buf := bytes.Repeat([]byte{0xFF}, 2*2) // room for 2 of the 4 silent frames
	n, _, _ := c.fill(buf)
	if n != 2 || !bytes.Equal(buf, make([]byte, 2*2)) {
		t.Fatalf("silent overflow first fill = %d frames %v, want 2 zeroed frames", n, buf)
	}
	buf2 := bytes.Repeat([]byte{0xFF}, 2*2)
	n, _, _ = c.fill(buf2)
	if n != 2 || !bytes.Equal(buf2, make([]byte, 2*2)) {
		t.Fatalf("silent carry fill = %d frames %v, want 2 zeroed frames", n, buf2)
	}
}

func TestFillEmptyPacket(t *testing.T) {
	c, _ := fillClient(1, 16, []capturePacket{{empty: true}})
	n, disc, err := c.fill(make([]byte, 4*2))
	if n != 0 || disc || err != nil {
		t.Errorf("empty fill = (%d, %v, %v), want (0, false, nil)", n, disc, err)
	}
}

// TestFillContiguousMatchesDeviceAdvance is the real-time invariant: across a
// contiguous packet stream, total delivered frames equal the device's advance.
func TestFillContiguousMatchesDeviceAdvance(t *testing.T) {
	pkts := make([]capturePacket, 0, 10)
	for i := 0; i < 10; i++ {
		pkts = append(pkts, capturePacket{data: pcm16(4, byte(i)), frames: 4, devPos: uint64(i * 4)})
	}
	c, _ := fillClient(1, 16, pkts)
	total := 0
	for range pkts {
		n, _, err := c.fill(make([]byte, 4*2))
		if err != nil {
			t.Fatalf("fill: %v", err)
		}
		total += n
	}
	if total != 40 {
		t.Errorf("delivered %d frames over a contiguous 40-frame stream, want 40", total)
	}
}
