//go:build linux || windows

package main

import (
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
