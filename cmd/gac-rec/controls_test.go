//go:build linux || windows

package main

import (
	"slices"
	"testing"

	capture "github.com/tphakala/go-audio-capture"
)

func TestRangeTextEnumerated(t *testing.T) {
	withNames := &capture.ControlInfo{Type: capture.ControlEnumerated, Max: 1, Items: []string{"Off", "On"}}
	if got := rangeText(withNames); got != "Off/On" {
		t.Errorf("rangeText with names = %q, want Off/On", got)
	}
	noNames := &capture.ControlInfo{Type: capture.ControlEnumerated, Max: 2000}
	if got := rangeText(noNames); got != "0..2000" {
		t.Errorf("rangeText without names = %q, want 0..2000", got)
	}
}

func TestParseSet(t *testing.T) {
	const vol, gain = "Mic Capture Volume", "Gain"
	tests := []struct {
		spec    string
		name    string
		index   int
		values  []int64
		wantErr bool
	}{
		{spec: vol + "=20", name: vol, values: []int64{20}},
		{spec: vol + "=20,30", name: vol, values: []int64{20, 30}},
		{spec: vol + "#1=5,6", name: vol, index: 1, values: []int64{5, 6}},
		{spec: gain + "=-5", name: gain, values: []int64{-5}},
		{spec: gain + "= 7 , 8", name: gain, values: []int64{7, 8}},
		{spec: "Gain", wantErr: true},
		{spec: "=5", wantErr: true},
		{spec: "Gain=", wantErr: true},
		{spec: "Gain#=5", wantErr: true},
		{spec: "Gain#x=5", wantErr: true},
		{spec: "Gain#-1=5", wantErr: true},
		{spec: "Gain=1,", wantErr: true},
		{spec: "Gain=1,x", wantErr: true},
	}
	for _, tt := range tests {
		name, index, values, err := parseSet(tt.spec)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseSet(%q) error = %v, wantErr %v", tt.spec, err, tt.wantErr)
			continue
		}
		if tt.wantErr {
			continue
		}
		if name != tt.name || index != tt.index || !slices.Equal(values, tt.values) {
			t.Errorf("parseSet(%q) = %q, %d, %v, want %q, %d, %v", tt.spec, name, index, values, tt.name, tt.index, tt.values)
		}
	}
}
