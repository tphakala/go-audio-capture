//go:build linux

package alsa

import (
	"testing"
	"unsafe"
)

// TestReadIAllocFree pins the steady-state ReadI path to zero heap allocations.
func TestReadIAllocFree(t *testing.T) {
	p := &PCM{fd: -1, ioctl: func(_ int, _ uintptr, arg unsafe.Pointer) error {
		// Emulate the kernel filling in the frame count.
		x := (*Xferi)(arg)
		x.Result = 256
		return nil
	}}
	// ReadI is frame-size-agnostic: it only needs a non-empty buffer and a
	// valid &buf[0], so the length here is arbitrary (256 frames worth of a
	// 4-byte S16 stereo frame), not a size ReadI validates.
	buf := make([]byte, 256*4)
	allocs := testing.AllocsPerRun(1000, func() {
		if n, err := p.ReadI(buf, 256); err != nil || n != 256 {
			t.Fatalf("ReadI = (%d,%v)", n, err)
		}
	})
	if allocs != 0 {
		t.Errorf("ReadI allocs/op = %v, want 0", allocs)
	}
}
