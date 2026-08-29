//go:build windows && (amd64 || arm64)

package capture

import "testing"

// TestDevicesIntegration exercises the real WASAPI enumeration path. It must not
// fail on a machine (or CI runner) with no audio subsystem or no capture
// endpoints: an enumeration error or an empty result is a skip, since that is an
// environment condition, not a code defect (GitHub's Windows runners are
// headless with the audio service off). When endpoints exist, every entry must
// carry a non-empty endpoint-id string in ID (the field Config.Device consumes).
func TestDevicesIntegration(t *testing.T) {
	devs, err := Devices()
	if err != nil {
		t.Skipf("enumeration unavailable in this environment: %v", err)
	}
	if len(devs) == 0 {
		t.Skip("no capture endpoints on this machine")
	}
	for i, d := range devs {
		if d.ID == "" {
			t.Errorf("device[%d] has empty ID: %+v", i, d)
		}
	}
}
