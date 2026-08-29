//go:build windows && (amd64 || arm64)

package wasapi

import (
	"runtime"
	"testing"
)

// benchClient wires a Client to a getPacketFn that re-serves one aliased,
// contiguous packet forever (like the real driver aliasing its ring buffer),
// mirroring steady-state capture. releaseFn is a no-op. frames is the packet
// size; the returned buf holds bufFrames frames.
func benchClient(frames uint32, bufFrames int) (c *Client, buf []byte) {
	const blk = 2 // mono S16
	pkt := capturePacket{data: make([]byte, int(frames)*blk), frames: frames}
	var pos uint64
	c = &Client{neg: Negotiated{Channels: 1, Bits: 16}}
	c.getPacketFn = func() (capturePacket, error) {
		pkt.devPos = pos
		pos += uint64(frames)
		return pkt, nil
	}
	c.releaseFn = func(uint32) {}
	return c, make([]byte, bufFrames*blk)
}

// benchClientSilent is benchClient for SILENT packets (data nil, SILENT flag),
// so the carry path runs its slices.Grow+clear branch instead of the append
// branch. frames is the packet size; buf holds bufFrames frames.
func benchClientSilent(frames uint32, bufFrames int) (c *Client, buf []byte) {
	const blk = 2 // mono S16
	var pos uint64
	c = &Client{neg: Negotiated{Channels: 1, Bits: 16}}
	c.getPacketFn = func() (capturePacket, error) {
		p := capturePacket{frames: frames, flags: bufferFlagsSilent, devPos: pos}
		pos += uint64(frames)
		return p, nil
	}
	c.releaseFn = func(uint32) {}
	return c, make([]byte, bufFrames*blk)
}

// Zero-alloc guard sizing. The carry path is bimodal: it either reuses its
// backing buffer (≈0 heap allocations across a window) or, if that reuse
// regresses, reallocates on roughly every other read (≈iters/2 allocations). At
// allocWindowIters that regression signal is ~64k allocations per window; the
// clean path is ~0. allocSlack sits two orders of magnitude below the regression
// yet well above the few stray allocations a single window may pick up from
// background runtime goroutines, so a relapse fails loudly while a clean run does
// not flake.
const (
	allocWindows     = 8       // measurement windows; the MINIMUM across them is the verdict
	allocWindowIters = 128_000 // fill() calls per window
	allocSlack       = 1000    // pass ceiling for the minimum window
)

// minMallocsPerWindow reports the FEWEST heap allocations fill(buf) performs
// across any one of allocWindows measurement windows. Taking the minimum makes
// the guard immune to sporadic background allocation (GC workers, sysmon): a
// truly alloc-free path yields at least one clean window, whereas a per-read
// regression allocates in every window, so its minimum is still ~iters/2. It
// pins the runtime to a single P (as testing.AllocsPerRun does) and reads
// runtime.MemStats directly rather than using testing.AllocsPerRun, whose integer
// division truncates a sub-1.0 average (e.g. the ~0.5/op pre-fix carry regression)
// to zero and would hide it. A positive-control fill first proves the measured
// path actually delivers audio, so a zero-alloc pass cannot be a vacuous early
// return, and every fill's error is checked so a path that bailed out early cannot
// masquerade as allocation-free.
func minMallocsPerWindow(tb testing.TB, c *Client, buf []byte) uint64 {
	tb.Helper()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	if frames, _, err := c.fill(buf); err != nil || frames == 0 {
		tb.Fatalf("positive-control fill = (%d frames, %v), want frames>0 and no error", frames, err)
	}
	best := ^uint64(0)
	for w := 0; w < allocWindows; w++ {
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		for i := 0; i < allocWindowIters; i++ {
			if _, _, err := c.fill(buf); err != nil {
				tb.Fatalf("fill: %v", err)
			}
		}
		runtime.ReadMemStats(&m1)
		if d := m1.Mallocs - m0.Mallocs; d < best {
			best = d
		}
	}
	return best
}

// TestFillZeroAllocSteadyState pins the invariant that a caller buffer sized to
// (at least) one packet — the documented sizing the reference consumer uses —
// runs the capture data path with no heap allocation.
func TestFillZeroAllocSteadyState(t *testing.T) {
	c, buf := benchClient(480, 480)
	if got := minMallocsPerWindow(t, c, buf); got > allocSlack {
		t.Errorf("steady-state fill allocated %d times in its cleanest window (ceiling %d), want ~0", got, allocSlack)
	}
}

// TestFillZeroAllocOverflow pins the invariant that even an under-sized caller
// buffer (which forces the carry path every read) is amortized zero-alloc: the
// reusable carry backing is grown at most a bounded number of times, not once
// per read. A pre-fix carry (resliced past its read offset) allocated ~0.5
// times/op; the reusable buffer settles to no allocations after warmup.
func TestFillZeroAllocOverflow(t *testing.T) {
	c, buf := benchClient(480, 240) // buf holds half a packet: carry every read
	warmCarry(t, c, buf)
	if got := minMallocsPerWindow(t, c, buf); got > allocSlack {
		t.Errorf("overflow fill allocated %d times in its cleanest window (ceiling %d), want ~0 (carry buffer must be reused)", got, allocSlack)
	}
}

// TestFillZeroAllocSilentOverflow is TestFillZeroAllocOverflow for the SILENT
// carry branch (slices.Grow+clear): a silent packet larger than buf must also
// reuse the carry backing rather than allocate a fresh zeroed block per read.
func TestFillZeroAllocSilentOverflow(t *testing.T) {
	c, buf := benchClientSilent(480, 240)
	warmCarry(t, c, buf)
	if got := minMallocsPerWindow(t, c, buf); got > allocSlack {
		t.Errorf("silent-overflow fill allocated %d times in its cleanest window (ceiling %d), want ~0", got, allocSlack)
	}
}

// warmCarry drives the carry backing to its steady capacity so the subsequent
// measurement windows observe only the reuse (not the one-time growth). An even
// count leaves the carry fully drained.
func warmCarry(tb testing.TB, c *Client, buf []byte) {
	tb.Helper()
	for i := 0; i < 1000; i++ {
		if _, _, err := c.fill(buf); err != nil {
			tb.Fatal(err)
		}
	}
}

// BenchmarkFillSteadyState measures the capture data path at the documented
// buffer sizing (buf >= one packet). Expect 0 B/op, 0 allocs/op.
func BenchmarkFillSteadyState(b *testing.B) {
	c, buf := benchClient(480, 480)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := c.fill(buf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFillOverflow measures the carry path (caller buffer smaller than a
// packet). Expect 0 allocs/op once the reusable carry backing has warmed.
func BenchmarkFillOverflow(b *testing.B) {
	c, buf := benchClient(480, 240)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := c.fill(buf); err != nil {
			b.Fatal(err)
		}
	}
}
