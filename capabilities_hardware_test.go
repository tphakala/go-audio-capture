//go:build linux

package capture

import (
	"os"
	"testing"
)

// TestHardwareSupportedRates is a manual, hardware-touching validation of the
// HW_REFINE-only rate enumeration against a real ALSA device. It is gated on the
// GAC_HW_TEST environment variable so it never runs in CI or a normal `go test`;
// the fake-ioctl tests in rates_test.go and capabilities_linux_test.go cover the
// logic without hardware.
//
// Run against a specific device, e.g. a USB interface at card 1:
//
//	GAC_HW_TEST=hw:1,0 go test -run TestHardwareSupportedRates -v
//
// It prints the enumerated rate set and raw window for each channel/format
// combination; it asserts nothing, since the supported set is device-specific.
func TestHardwareSupportedRates(t *testing.T) {
	dev := os.Getenv("GAC_HW_TEST")
	if dev == "" {
		t.Skip("set GAC_HW_TEST=hw:card,device to run")
	}
	for _, ch := range []int{1, 2} {
		for _, f := range []Format{FormatS16LE, FormatS32LE} {
			rs, err := SupportedRates(dev, ch, f)
			t.Logf("%s ch=%d %s -> rates=%v range=[%d,%d] err=%v", dev, ch, f, rs.Rates, rs.Min, rs.Max, err)
		}
	}
}
