package capture

import (
	"errors"
	"math"
	"testing"
)

// micVol is shared by the platform-neutral tests and controls_linux_test.go.
const micVol = "Mic Capture Volume"

func TestControlEnumStrings(t *testing.T) {
	for _, tt := range []struct{ got, want string }{
		{ControlCard.String(), "CARD"}, {ControlHwdep.String(), "HWDEP"}, {ControlMixer.String(), "MIXER"},
		{ControlPCM.String(), "PCM"}, {ControlRawMIDI.String(), "RAWMIDI"}, {ControlTimer.String(), "TIMER"},
		{ControlSequencer.String(), "SEQUENCER"}, {ControlInterface(9).String(), "IFACE(9)"},
		{ControlBoolean.String(), "BOOLEAN"}, {ControlInteger.String(), "INTEGER"},
		{ControlEnumerated.String(), "ENUMERATED"}, {ControlBytes.String(), "BYTES"},
		{ControlIEC958.String(), "IEC958"}, {ControlInteger64.String(), "INTEGER64"}, {ControlType(0).String(), "TYPE(0)"},
		{(AccessRead | AccessWrite | AccessTLVRead).String(), "read|write|tlv"},
		{ControlAccess(0).String(), "none"},
		{AccessInactive.String(), "inactive"},
	} {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}

func TestControlIDString(t *testing.T) {
	id := ControlID{Interface: ControlMixer, Name: micVol, Index: 1}
	if got, want := id.String(), "'Mic Capture Volume',index=1,iface=MIXER"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	id = ControlID{Interface: ControlPCM, Device: 2, Subdevice: 1, Name: "X"}
	if got, want := id.String(), "'X',index=0,iface=PCM,device=2,subdevice=1"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestIsCapture(t *testing.T) {
	for name, want := range map[string]bool{
		"Mic Capture Volume": true, "Capture Volume": true, "Capture Channel Map": true,
		"Playback Volume": false, "Recapture Volume": false,
	} {
		if got := (ControlInfo{ID: ControlID{Name: name}}).IsCapture(); got != want {
			t.Errorf("IsCapture(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestValueDB(t *testing.T) {
	var scale ControlInfo
	// -12.00 dB at raw 0, 0.50 dB per step to raw 40 (+8.00 dB); raw 0 is mute.
	scale.setDB([]dbSegment{{rawMin: 0, rawMax: 40, minCdB: -1200, maxCdB: 800, mute: true}})
	var rng ControlInfo
	rng.setDB([]dbSegment{
		{rawMin: 0, rawMax: 10, minCdB: -5000, maxCdB: -4000},
		{rawMin: 11, rawMax: 20, minCdB: -4000, maxCdB: -3550},
	})
	tests := []struct {
		name   string
		info   ControlInfo
		raw    int64
		want   float64
		wantOK bool
	}{
		{"scale mid", scale, 20, -2, true},
		{"scale top", scale, 40, 8, true},
		{"scale mute step", scale, 0, math.Inf(-1), true},
		{"scale outside", scale, 41, 0, false},
		{"range first segment", rng, 5, -45, true},
		{"range second segment uses its own origin", rng, 20, -35.5, true},
		{"range second segment start", rng, 11, -40, true},
		{"range gap", ControlInfo{}, 3, 0, false},
	}
	for _, tt := range tests {
		got, ok := tt.info.ValueDB(tt.raw)
		if ok != tt.wantOK || got != tt.want {
			t.Errorf("%s: ValueDB(%d) = %v, %v, want %v, %v", tt.name, tt.raw, got, ok, tt.want, tt.wantOK)
		}
	}
	if !scale.HasDB || scale.MinDB != -12 || scale.MaxDB != 8 {
		t.Errorf("scale range = %v %v..%v, want -12..8", scale.HasDB, scale.MinDB, scale.MaxDB)
	}
	if rng.MinDB != -50 || rng.MaxDB != -35.5 {
		t.Errorf("range range = %v..%v, want -50..-35.5", rng.MinDB, rng.MaxDB)
	}
	// A single-value segment has no span to divide by.
	var one ControlInfo
	one.setDB([]dbSegment{{rawMin: 7, rawMax: 7, minCdB: -300, maxCdB: 600}})
	if got, ok := one.ValueDB(7); !ok || got != -3 {
		t.Errorf("single-value segment = %v, %v, want -3, true", got, ok)
	}
}

func TestStepOK(t *testing.T) {
	for _, tt := range []struct {
		v, step int64
		want    bool
	}{
		{7, 0, true}, {-3, 1, true}, {10, 5, true}, {7, 5, false},
		{-5, 5, false}, // uint64(-5) % 5 == 1: the kernel's rule, not (v-min)%step
		{-20, 5, false},
		{-16, 4, true}, // 2^64 mod 4 == 0, so negative multiples of 4 pass
	} {
		if got := stepOK(tt.v, tt.step); got != tt.want {
			t.Errorf("stepOK(%d, %d) = %v, want %v", tt.v, tt.step, got, tt.want)
		}
	}
}

func TestControlErrors(t *testing.T) {
	id := ControlID{Interface: ControlMixer, Name: micVol}
	nf := &ControlNotFoundError{ID: id}
	if !errors.Is(nf, ErrControlNotFound) {
		t.Error("ControlNotFoundError does not unwrap to ErrControlNotFound")
	}
	if errors.Is(nf, ErrDeviceGone) || errors.Is(nf, ErrCapabilitiesUnsupported) {
		t.Error("ControlNotFoundError matches an unrelated sentinel")
	}
	for _, e := range []error{
		nf, &ControlNotFoundError{Pattern: true},
		&AmbiguousControlError{Matches: []ControlID{id, id}},
		&ControlAccessError{ID: id, Op: accessOpWrite}, &ControlAccessError{ID: id, Op: accessOpWrite, Locked: true},
		&ControlValueError{ID: id, Values: []int64{9}, Max: 5, Reason: "out of range"},
	} {
		if msg := e.Error(); len(msg) < 10 || msg[:8] != "capture:" {
			t.Errorf("message %q lacks the capture: prefix", msg)
		}
	}
}

func TestControlValueErrorMessage(t *testing.T) {
	id := ControlID{Interface: ControlMixer, Name: micVol}
	for _, tt := range []struct {
		name string
		err  *ControlValueError
		want string
	}{
		{"no element, values or range", &ControlValueError{Reason: "percent 101 is outside 0..100"},
			"capture: invalid value: percent 101 is outside 0..100"},
		{"element only", &ControlValueError{ID: id, Reason: "reading BYTES elements is not supported"},
			"capture: control 'Mic Capture Volume',index=0,iface=MIXER: invalid value: reading BYTES elements is not supported"},
		{"element, values and range", &ControlValueError{ID: id, Values: []int64{9}, Max: 5, Reason: "9 is outside the range"},
			"capture: control 'Mic Capture Volume',index=0,iface=MIXER: invalid value [9]: 9 is outside the range (range 0..5, step 0)"},
		{"negative min only", &ControlValueError{ID: id, Values: []int64{5}, Min: -10, Reason: "5 is outside the range"},
			"capture: control 'Mic Capture Volume',index=0,iface=MIXER: invalid value [5]: 5 is outside the range (range -10..0, step 0)"},
		{"step only", &ControlValueError{ID: id, Values: []int64{7}, Step: 5, Reason: "off the step rule"},
			"capture: control 'Mic Capture Volume',index=0,iface=MIXER: invalid value [7]: off the step rule (range 0..0, step 5)"},
	} {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("%s:\n got %q\nwant %q", tt.name, got, tt.want)
		}
	}
}
