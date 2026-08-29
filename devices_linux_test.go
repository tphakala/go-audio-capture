//go:build linux

package capture

import (
	"path/filepath"
	"testing"
)

func TestDevicesFromFixture(t *testing.T) {
	got, err := devicesFrom(filepath.Join("testdata", "proc_asound"))
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	// card2 is playback-only (pcm0p), so it must not appear. Results are
	// ordered by card then device, with the card longname as the Name.
	want := []DeviceInfo{
		{ID: "hw:0,0", Card: 0, Device: 0, Name: "HDA Intel PCH"},
		{ID: "hw:1,0", Card: 1, Device: 0, Name: "C-Media USB Audio Device"},
	}
	if len(got) != len(want) {
		t.Fatalf("devicesFrom returned %d devices, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("device[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseCards(t *testing.T) {
	data := []byte(" 0 [PCH            ]: HDA-Intel - HDA Intel PCH\n" +
		"                      HDA Intel PCH at 0xf7e40000 irq 145\n" +
		" 1 [Device         ]: USB-Audio - C-Media USB Audio Device\n")
	names := parseCards(data)
	if names[0] != "HDA Intel PCH" {
		t.Errorf("card 0 name = %q, want %q", names[0], "HDA Intel PCH")
	}
	if names[1] != "C-Media USB Audio Device" {
		t.Errorf("card 1 name = %q, want %q", names[1], "C-Media USB Audio Device")
	}
	if len(names) != 2 {
		t.Errorf("parseCards found %d cards, want 2", len(names))
	}
}
