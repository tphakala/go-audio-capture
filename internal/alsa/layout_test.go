//go:build linux

package alsa

import (
	"testing"
	"unsafe"
)

// The struct sizes, field offsets, and ioctl request numbers below are the
// kernel's own, verified 2026-08-29 by compiling an offsetof/sizeof probe
// against /usr/include/sound/asound.h on amd64 (identical on arm64: both are
// LP64, and every field here is fixed-width or an 8-byte unsigned long):
//
//	HWPARAMS_SIZE=608 SWPARAMS_SIZE=136 XFERI_SIZE=24
//	hw: masks@4 mres@100 intervals@260 ires@404 rmask@512 fifo@536 sync@544 reserved@560
//	sw: avail_min@16 start_threshold@32 boundary@64 proto@72 reserved@80
//	PVERSION=0x80044100 HW_REFINE=0xc2604110 HW_PARAMS=0xc2604111 SW_PARAMS=0xc0884113
//	PREPARE=0x4140 START=0x4142 DROP=0x4143 RESUME=0x4147 READI=0x80184151
//
// A mismatch means the Go mirror has drifted from the kernel ABI and every
// capture would silently corrupt, so these are hard assertions.

func TestMaskAndIntervalSize(t *testing.T) {
	if got := unsafe.Sizeof(Mask{}); got != 32 {
		t.Errorf("sizeof(Mask) = %d, want 32", got)
	}
	if got := unsafe.Sizeof(Interval{}); got != 12 {
		t.Errorf("sizeof(Interval) = %d, want 12", got)
	}
}

func TestHwParamsSize(t *testing.T) {
	if got := unsafe.Sizeof(HwParams{}); got != 608 {
		t.Errorf("sizeof(HwParams) = %d, want 608", got)
	}
}

func TestHwParamsOffsets(t *testing.T) {
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Masks", unsafe.Offsetof(HwParams{}.Masks), 4},
		{"Mres", unsafe.Offsetof(HwParams{}.Mres), 100},
		{"Intervals", unsafe.Offsetof(HwParams{}.Intervals), 260},
		{"Ires", unsafe.Offsetof(HwParams{}.Ires), 404},
		{"Rmask", unsafe.Offsetof(HwParams{}.Rmask), 512},
		{"FifoSize", unsafe.Offsetof(HwParams{}.FifoSize), 536},
		{"Sync", unsafe.Offsetof(HwParams{}.Sync), 544},
		{"Reserved", unsafe.Offsetof(HwParams{}.Reserved), 560},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("offsetof(HwParams.%s) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestSwParamsLayout(t *testing.T) {
	if got := unsafe.Sizeof(SwParams{}); got != 136 {
		t.Errorf("sizeof(SwParams) = %d, want 136", got)
	}
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"AvailMin", unsafe.Offsetof(SwParams{}.AvailMin), 16},
		{"StartThreshold", unsafe.Offsetof(SwParams{}.StartThreshold), 32},
		{"Boundary", unsafe.Offsetof(SwParams{}.Boundary), 64},
		{"Proto", unsafe.Offsetof(SwParams{}.Proto), 72},
		{"Reserved", unsafe.Offsetof(SwParams{}.Reserved), 80},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("offsetof(SwParams.%s) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestXferiSize(t *testing.T) {
	if got := unsafe.Sizeof(Xferi{}); got != 24 {
		t.Errorf("sizeof(Xferi) = %d, want 24", got)
	}
}

func TestIoctlNumbers(t *testing.T) {
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"PVersion", iocPVersion, 0x80044100},
		{"HwRefine", iocHwRefine, 0xc2604110},
		{"HwParams", iocHwParams, 0xc2604111},
		{"SwParams", iocSwParams, 0xc0884113},
		{"Prepare", iocPrepare, 0x4140},
		{"Start", iocStart, 0x4142},
		{"Drop", iocDrop, 0x4143},
		{"Resume", iocResume, 0x4147},
		{"ReadIFrames", iocReadIFrames, 0x80184151},
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
