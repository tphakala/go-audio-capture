//go:build windows

package wasapi

import (
	"bytes"
	"testing"
)

// fillClient builds a Client wired to deliver a scripted sequence of packets
// through the getPacketFn seam, so fill()'s frame accounting is testable without
// audio hardware. channels/bits set the frame size (blockAlign).
func fillClient(channels, bits int, pkts []capturePacket) *Client {
	c := &Client{neg: Negotiated{Channels: channels, Bits: bits}}
	i := 0
	c.getPacketFn = func() (capturePacket, error) {
		if i >= len(pkts) {
			return capturePacket{empty: true}, nil
		}
		p := pkts[i]
		i++
		return p, nil
	}
	c.releaseFn = func(uint32) {}
	return c
}

// pcm16 makes a mono S16 data buffer of `frames` frames with byte value v, so a
// delivered region is identifiable.
func pcm16(frames int, v byte) []byte {
	b := make([]byte, frames*2)
	for i := range b {
		b[i] = v
	}
	return b
}

func TestFillDeliversWholePacket(t *testing.T) {
	c := fillClient(1, 16, []capturePacket{{data: pcm16(4, 0xAA), frames: 4, devPos: 0}})
	buf := make([]byte, 4*2)
	n, disc, err := c.fill(buf)
	if err != nil || disc || n != 4 {
		t.Fatalf("fill = (%d, %v, %v), want (4, false, nil)", n, disc, err)
	}
	if !bytes.Equal(buf, pcm16(4, 0xAA)) {
		t.Errorf("buf = %v, want 4 frames of 0xAA", buf)
	}
}

// TestFillSkipsOverlap is the ZxR bug: the driver re-presents a buffer that
// overlaps already-delivered frames (backward devicePosition). The overlapping
// prefix must be skipped so no frame is delivered twice.
func TestFillSkipsOverlap(t *testing.T) {
	c := fillClient(1, 16, []capturePacket{
		{data: pcm16(4, 0x11), frames: 4, devPos: 0}, // frames 0..4
		{data: pcm16(4, 0x22), frames: 4, devPos: 2}, // frames 2..6: 2 already delivered
	})
	// First fill: delivers all 4 frames, nextPos -> 4.
	if n, _, _ := c.fill(make([]byte, 4*2)); n != 4 {
		t.Fatalf("first fill n = %d, want 4", n)
	}
	// Second fill: devPos 2 < nextPos 4, overlap of 2 frames skipped; only the
	// last 2 frames (0x22) are delivered.
	buf := make([]byte, 4*2)
	n, _, _ := c.fill(buf)
	if n != 2 {
		t.Fatalf("second fill n = %d, want 2 (2-frame overlap skipped)", n)
	}
	if !bytes.Equal(buf[:4], pcm16(2, 0x22)) {
		t.Errorf("delivered = %v, want 2 frames of 0x22 (the non-overlapping tail)", buf[:4])
	}
}

func TestFillWholePacketOverlapDeliversNothing(t *testing.T) {
	c := fillClient(1, 16, []capturePacket{
		{data: pcm16(4, 0x11), frames: 4, devPos: 0}, // 0..4
		{data: pcm16(4, 0x22), frames: 4, devPos: 0}, // fully re-presented
	})
	_, _, _ = c.fill(make([]byte, 4*2)) // deliver first, nextPos -> 4
	n, _, _ := c.fill(make([]byte, 4*2))
	if n != 0 {
		t.Errorf("re-presented packet delivered %d frames, want 0", n)
	}
}

func TestFillForwardGapIsDiscontinuity(t *testing.T) {
	c := fillClient(1, 16, []capturePacket{
		{data: pcm16(4, 0x11), frames: 4, devPos: 0},  // 0..4, nextPos -> 4
		{data: pcm16(4, 0x22), frames: 4, devPos: 10}, // jumps ahead: device dropped frames
	})
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
	c := fillClient(1, 16, []capturePacket{
		{data: pcm16(4, 0x11), frames: 4, devPos: 0, flags: bufferFlagsDataDiscontinuity},
	})
	_, disc, _ := c.fill(make([]byte, 4*2))
	if !disc {
		t.Error("DATA_DISCONTINUITY flag should report a discontinuity")
	}
}

// TestFillCarriesOverflow: a packet larger than the caller's buffer keeps the
// remainder for the next Read rather than dropping it.
func TestFillCarriesOverflow(t *testing.T) {
	data := append(pcm16(2, 0x33), pcm16(2, 0x44)...) // 4 frames: 2x0x33 then 2x0x44
	c := fillClient(1, 16, []capturePacket{{data: data, frames: 4, devPos: 0}})

	buf := make([]byte, 2*2) // room for only 2 frames
	n, _, _ := c.fill(buf)
	if n != 2 || !bytes.Equal(buf, pcm16(2, 0x33)) {
		t.Fatalf("first fill = %d frames %v, want 2 frames of 0x33", n, buf)
	}
	// Next fill (packet source now empty) must serve the carried 2 frames.
	buf2 := make([]byte, 2*2)
	n, _, _ = c.fill(buf2)
	if n != 2 || !bytes.Equal(buf2, pcm16(2, 0x44)) {
		t.Fatalf("carry fill = %d frames %v, want 2 frames of 0x44", n, buf2)
	}
}

func TestFillSilentDeliversZeros(t *testing.T) {
	c := fillClient(1, 16, []capturePacket{{data: nil, frames: 4, devPos: 0, flags: bufferFlagsSilent}})
	buf := bytes.Repeat([]byte{0xFF}, 4*2)
	n, _, _ := c.fill(buf)
	if n != 4 {
		t.Fatalf("silent fill n = %d, want 4", n)
	}
	if !bytes.Equal(buf, make([]byte, 4*2)) {
		t.Errorf("silent packet should deliver zeros, got %v", buf)
	}
}

func TestFillEmptyPacket(t *testing.T) {
	c := fillClient(1, 16, []capturePacket{{empty: true}})
	n, disc, err := c.fill(make([]byte, 4*2))
	if n != 0 || disc || err != nil {
		t.Errorf("empty fill = (%d, %v, %v), want (0, false, nil)", n, disc, err)
	}
}

// TestFillContiguousMatchesDeviceAdvance is the real-time invariant: across a
// contiguous packet stream, total delivered frames equal the device's advance
// (no over- or under-delivery), which is what the hardware bug violated.
func TestFillContiguousMatchesDeviceAdvance(t *testing.T) {
	pkts := make([]capturePacket, 0, 10)
	for i := 0; i < 10; i++ {
		pkts = append(pkts, capturePacket{data: pcm16(4, byte(i)), frames: 4, devPos: uint64(i * 4)})
	}
	c := fillClient(1, 16, pkts)
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
