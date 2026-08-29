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
	defer s.Close()

	buf := make([]byte, 256*s.frameBytes)
	allocs := testing.AllocsPerRun(1000, func() {
		if _, err := s.Read(buf); err != nil {
			t.Fatalf("Read: %v", err)
		}
	})
	if allocs != 0 {
		t.Errorf("Stream.Read allocs/op = %v, want 0", allocs)
	}
}
