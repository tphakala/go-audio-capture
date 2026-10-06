//go:build !linux

package capture

import (
	"errors"
	"testing"
)

// TestControlsStubsReportUnsupported pins that every platform without a
// control backend answers OpenControls and each Controls method with
// ErrCapabilitiesUnsupported, which callers branch on to leave the gain alone.
func TestControlsStubsReportUnsupported(t *testing.T) {
	c, err := OpenControls(DeviceInfo{ID: "x"})
	if c != nil || !errors.Is(err, ErrCapabilitiesUnsupported) {
		t.Fatalf("OpenControls = %v, %v, want nil and ErrCapabilitiesUnsupported", c, err)
	}
	c = &Controls{}
	id := ControlID{Name: micVol}
	_, e1 := c.List()
	_, e2 := c.Info(id)
	_, e3 := c.Get(id)
	e4 := c.Set(id, []int64{1})
	_, e5 := c.CaptureVolume()
	_, e6 := c.SetCaptureVolumePercent(50)
	for i, err := range []error{e1, e2, e3, e4, e5, e6} {
		if !errors.Is(err, ErrCapabilitiesUnsupported) {
			t.Errorf("method %d = %v, want ErrCapabilitiesUnsupported", i, err)
		}
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}
