//go:build linux

package capture

import "testing"

// TestStreamReadAllocFree pins the steady-state Stream.Read path (success, no
// xrun) to zero heap allocations.
func TestStreamReadAllocFree(t *testing.T) {
	fp := &fakePCM{readFn: func() (int, error) { return 256, nil }}
	restore := swapOpenPCM(fp)
	defer restore()

	s, err := Open(Config{Device: devID, Rate: 48000, Channels: 2, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	buf := make([]byte, 256*s.frameBytes)
	allocs := testing.AllocsPerRun(1000, func() {
		// Assert the frame count too: a regression that trips the
		// frames == 0 early return in Read would do no work and allocate
		// nothing, silently passing an n-blind check.
		if n, err := s.Read(buf); err != nil || n != 256 {
			t.Fatalf("Read = (%d, %v)", n, err)
		}
	})
	if allocs != 0 {
		t.Errorf("Stream.Read allocs/op = %v, want 0", allocs)
	}
}
