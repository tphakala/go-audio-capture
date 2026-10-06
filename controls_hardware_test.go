//go:build linux

package capture

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// TestHardwareControls is a manual, hardware-touching check of the control
// ioctls against a real card. It is the live proof of the struct sizes in
// internal/alsa (an ENOTTY from any ioctl means a layout is wrong for the
// running ABI). Gated on GAC_HW_CTL_TEST, so it never runs in CI:
//
//	GAC_HW_CTL_TEST=hw:1,0 go test -run TestHardwareControls -v
//
// It lists every element and reads every readable one. With GAC_HW_CTL_WRITE=1
// it also writes one BOOLEAN or INTEGER element the value it already holds and
// reads it back; without it nothing is ever written.
func TestHardwareControls(t *testing.T) {
	dev := os.Getenv("GAC_HW_CTL_TEST")
	if dev == "" {
		t.Skip("set GAC_HW_CTL_TEST=hw:card,device to run")
	}
	d, err := Resolve(dev)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", dev, err)
	}
	c, err := OpenControls(d)
	if err != nil {
		t.Fatalf("OpenControls: %v", err)
	}
	defer func() { _ = c.Close() }()

	list, err := c.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	t.Logf("%d elements", len(list))
	var writable *ControlInfo
	for i := range list {
		e := &list[i]
		t.Logf("numid=%d %s type=%s access=%s count=%d range=%d..%d step=%d items=%v db=%v %v..%v",
			e.ID.NumID, e.ID, e.Type, e.Access, e.Count, e.Min, e.Max, e.Step, e.Items, e.HasDB, e.MinDB, e.MaxDB)
		if e.Access&AccessRead == 0 || !supportedType(e.Type) {
			continue
		}
		vals, err := c.Get(e.ID)
		if errors.Is(err, unix.ENOTTY) {
			t.Fatalf("Get(%s): ENOTTY, the ioctl layout is wrong for this ABI: %v", e.ID, err)
		}
		if err != nil {
			t.Errorf("Get(%s): %v", e.ID, err)
			continue
		}
		t.Logf("  values=%v", vals)
		if writable == nil && e.Access&AccessWrite != 0 && e.Access&AccessInactive == 0 &&
			(e.Type == ControlBoolean || e.Type == ControlInteger) {
			writable = e
		}
	}
	if os.Getenv("GAC_HW_CTL_WRITE") != "1" {
		return
	}
	if writable == nil {
		t.Skip("no writable BOOLEAN or INTEGER element")
	}
	before, err := c.Get(writable.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set(writable.ID, before); err != nil {
		t.Fatalf("Set(%s) to its current value: %v", writable.ID, err)
	}
	after, err := c.Get(writable.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s back: before=%v after=%v", writable.ID, before, after)
}
