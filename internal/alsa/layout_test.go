//go:build linux && (amd64 || arm64 || 386 || arm || riscv64 || loong64)

package alsa

import (
	"testing"
	"unsafe"
)

// The struct sizes, field offsets, and ioctl request numbers asserted here are
// the kernel's own. The expected values are word-size specific and live in
// layout_lp64_test.go (amd64, arm64) and layout_ilp32_test.go (386, arm) as the
// want* constants, each C-verified against /usr/include/sound/asound.h with an
// offsetof/sizeof probe (see the header comment in each file).
//
// A mismatch means the Go mirror has drifted from the kernel ABI and every
// capture would silently corrupt, so these are hard assertions. On amd64/arm64
// they run natively; the ILP32 set runs under `GOARCH=386 go test` on an x86_64
// host (or GOARCH=arm on real hardware / an emulator).

func TestMaskAndIntervalSize(t *testing.T) {
	if got := unsafe.Sizeof(Mask{}); got != wantMaskSize {
		t.Errorf("sizeof(Mask) = %d, want %d", got, wantMaskSize)
	}
	if got := unsafe.Sizeof(Interval{}); got != wantIntervalSize {
		t.Errorf("sizeof(Interval) = %d, want %d", got, wantIntervalSize)
	}
}

func TestHwParamsSize(t *testing.T) {
	if got := unsafe.Sizeof(HwParams{}); got != wantHwParamsSize {
		t.Errorf("sizeof(HwParams) = %d, want %d", got, wantHwParamsSize)
	}
}

func TestHwParamsOffsets(t *testing.T) {
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Masks", unsafe.Offsetof(HwParams{}.Masks), wantHwMasks},
		{"Mres", unsafe.Offsetof(HwParams{}.Mres), wantHwMres},
		{"Intervals", unsafe.Offsetof(HwParams{}.Intervals), wantHwIntervals},
		{"Ires", unsafe.Offsetof(HwParams{}.Ires), wantHwIres},
		{"Rmask", unsafe.Offsetof(HwParams{}.Rmask), wantHwRmask},
		{"FifoSize", unsafe.Offsetof(HwParams{}.FifoSize), wantHwFifoSize},
		{"Sync", unsafe.Offsetof(HwParams{}.Sync), wantHwSync},
		{"Reserved", unsafe.Offsetof(HwParams{}.Reserved), wantHwReserved},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("offsetof(HwParams.%s) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestSwParamsLayout(t *testing.T) {
	if got := unsafe.Sizeof(SwParams{}); got != wantSwParamsSize {
		t.Errorf("sizeof(SwParams) = %d, want %d", got, wantSwParamsSize)
	}
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"AvailMin", unsafe.Offsetof(SwParams{}.AvailMin), wantSwAvailMin},
		{"StartThreshold", unsafe.Offsetof(SwParams{}.StartThreshold), wantSwStartThreshold},
		{"Boundary", unsafe.Offsetof(SwParams{}.Boundary), wantSwBoundary},
		{"Proto", unsafe.Offsetof(SwParams{}.Proto), wantSwProto},
		{"Reserved", unsafe.Offsetof(SwParams{}.Reserved), wantSwReserved},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("offsetof(SwParams.%s) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestXferiLayout(t *testing.T) {
	if got := unsafe.Sizeof(Xferi{}); got != wantXferiSize {
		t.Errorf("sizeof(Xferi) = %d, want %d", got, wantXferiSize)
	}
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Result", unsafe.Offsetof(Xferi{}.Result), 0},
		{"Buf", unsafe.Offsetof(Xferi{}.Buf), wantXferiBuf},
		{"Frames", unsafe.Offsetof(Xferi{}.Frames), wantXferiFrames},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("offsetof(Xferi.%s) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestIoctlNumbers(t *testing.T) {
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"PVersion", iocPVersion, wantIocPVersion},
		{"HwRefine", iocHwRefine, wantIocHwRefine},
		{"HwParams", iocHwParams, wantIocHwParams},
		{"SwParams", iocSwParams, wantIocSwParams},
		{"Prepare", iocPrepare, wantIocPrepare},
		{"Start", iocStart, wantIocStart},
		{"Drop", iocDrop, wantIocDrop},
		{"Resume", iocResume, wantIocResume},
		{"ReadIFrames", iocReadIFrames, wantIocReadI},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s ioctl = %#x, want %#x", tt.name, tt.got, tt.want)
		}
	}
}

func TestFillAnySetsFullRanges(t *testing.T) {
	var p HwParams
	p.FillAny()
	if p.Rmask != ^uint32(0) {
		t.Errorf("FillAny Rmask = %#x, want all ones", p.Rmask)
	}
	// Every mask bit set.
	for i := range p.Masks {
		for j, b := range p.Masks[i].Bits {
			if b != ^uint32(0) {
				t.Fatalf("FillAny Masks[%d].Bits[%d] = %#x, want all ones", i, j, b)
			}
		}
	}
	// Every interval opened to the full range.
	for i := range p.Intervals {
		if p.Intervals[i].Min != 0 || p.Intervals[i].Max != ^uint32(0) {
			t.Fatalf("FillAny Intervals[%d] = [%d,%d], want [0,max]", i, p.Intervals[i].Min, p.Intervals[i].Max)
		}
	}
}

func TestSetMaskSelectsSingleBit(t *testing.T) {
	var p HwParams
	p.FillAny()
	p.SetMask(ParamAccess, AccessRWInterleaved)
	m := p.Masks[ParamAccess-paramFirstMask]
	// Only bit 3 (RW_INTERLEAVED) set in word 0, nothing elsewhere.
	if m.Bits[0] != 1<<AccessRWInterleaved {
		t.Errorf("SetMask access word0 = %#x, want %#x", m.Bits[0], 1<<AccessRWInterleaved)
	}
	for j := 1; j < len(m.Bits); j++ {
		if m.Bits[j] != 0 {
			t.Errorf("SetMask access word%d = %#x, want 0", j, m.Bits[j])
		}
	}
}

func TestSetIntervalExactAndGetter(t *testing.T) {
	var p HwParams
	p.FillAny()
	p.SetIntervalExact(ParamRate, 256000)
	lo, hi := p.Interval(ParamRate)
	if lo != 256000 || hi != 256000 {
		t.Errorf("Interval(rate) = [%d,%d], want [256000,256000]", lo, hi)
	}
	if p.Intervals[ParamRate-paramFirstInterval].Flags&intervalInteger == 0 {
		t.Error("SetIntervalExact did not set the integer flag")
	}
}
