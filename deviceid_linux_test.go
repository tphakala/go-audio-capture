//go:build linux

package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostLayout is the machine this was developed against: an snd-aloop platform
// card, a USB microphone that reports a serial, and a USB interface that does
// not.
func hostLayout() []fakeCard {
	return []fakeCard{
		loopbackCard(0),
		portCard(1, "3"),
		serialCard(2, audiomothSerial, "2"),
	}
}

const (
	wantLoopbackID = "hw:CARD=Loopback,DEV=0"
	wantPortID     = "usb:1686:067f:p=0000:00:14.0-3:if=0,0"
	wantSerialID   = "usb:16d0:06f3:s=0384_2474750763FA81C9:if=0,0"
	// Fixture identity values that recur across the identity tests, named so the
	// assertions and the fixtures cannot drift apart (and to satisfy goconst).
	// audiomothVID/PID/Serial describe the serial-reporting AudioMoth mic;
	// compositeVID/PID the composite test device; dupSerial the shared serial of
	// the same-serial twins; twinPort4ID the port-form id of the twin on devpath 4.
	audiomothVID    = "16d0"
	audiomothPID    = "06f3"
	audiomothSerial = "0384_2474750763FA81C9"
	compositeVID    = "1234"
	compositePID    = "5678"
	dupSerial       = "DUPLICATE"
	twinPort4ID     = "usb:16d0:06f3:p=0000:00:14.0-4:if=0,0"
)

func TestDevicesStableIDs(t *testing.T) {
	proc, sys := buildFixture(t, hostLayout())
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	// Loopback exposes two capture PCMs, so four capture devices in total.
	if len(devs) != 4 {
		t.Fatalf("got %d devices, want 4: %+v", len(devs), devs)
	}

	tests := []struct {
		hwaddr, id, portID, cardID string
		usb                        bool
	}{
		{"hw:0,0", wantLoopbackID, "", loopbackName, false},
		{"hw:0,1", "hw:CARD=Loopback,DEV=1", "", loopbackName, false},
		{hwAddrCard1, wantPortID, wantPortID, "AMS24", true},
		{hwAddrCard2, wantSerialID, "usb:16d0:06f3:p=0000:00:14.0-2:if=0,0", "Microphone", true},
	}
	for _, tt := range tests {
		d := findDevice(t, devs, tt.hwaddr)
		if d.ID != tt.id {
			t.Errorf("%s ID = %q, want %q", tt.hwaddr, d.ID, tt.id)
		}
		if !d.IDStable {
			t.Errorf("%s IDStable = false, want true", tt.hwaddr)
		}
		if d.PortID != tt.portID {
			t.Errorf("%s PortID = %q, want %q", tt.hwaddr, d.PortID, tt.portID)
		}
		if d.CardID != tt.cardID {
			t.Errorf("%s CardID = %q, want %q", tt.hwaddr, d.CardID, tt.cardID)
		}
		if d.IsUSB() != tt.usb {
			t.Errorf("%s IsUSB = %v, want %v", tt.hwaddr, d.IsUSB(), tt.usb)
		}
	}

	// The serialled card carries its full USB identity.
	d := findDevice(t, devs, hwAddrCard2)
	want := USBInfo{VendorID: audiomothVID, ProductID: audiomothPID, Serial: audiomothSerial, Port: "0000:00:14.0-2", Interface: 0}
	if d.USB != want {
		t.Errorf("USB = %+v, want %+v", d.USB, want)
	}
}

// TestDevicesIDSurvivesCardSwap is the bug this whole change exists for: two
// USB cards trade kernel indices across a reboot, and the persisted id must
// still name the same physical hardware.
func TestDevicesIDSurvivesCardSwap(t *testing.T) {
	before, sysBefore := buildFixture(t, hostLayout())
	afterCards := []fakeCard{
		loopbackCard(0),
		serialCard(1, audiomothSerial, "2"),
		portCard(2, "3"),
	}
	after, sysAfter := buildFixture(t, afterCards)

	devsBefore, err := devicesFrom(before, sysBefore)
	if err != nil {
		t.Fatalf("devicesFrom before: %v", err)
	}
	devsAfter, err := devicesFrom(after, sysAfter)
	if err != nil {
		t.Fatalf("devicesFrom after: %v", err)
	}

	serialBefore := findDevice(t, devsBefore, hwAddrCard2)
	serialAfter := findDevice(t, devsAfter, hwAddrCard1)
	if serialBefore.ID != serialAfter.ID {
		t.Errorf("serialled card ID changed across swap: %q -> %q", serialBefore.ID, serialAfter.ID)
	}
	if serialBefore.HWAddr == serialAfter.HWAddr {
		t.Errorf("HWAddr should track the card index, but both are %q", serialBefore.HWAddr)
	}

	portBefore := findDevice(t, devsBefore, hwAddrCard1)
	portAfter := findDevice(t, devsAfter, hwAddrCard2)
	if portBefore.ID != portAfter.ID {
		t.Errorf("serial-less card ID changed across swap: %q -> %q", portBefore.ID, portAfter.ID)
	}
}

// TestDevicesSerialledCardMovedPort pins the choice to key on the serial: the
// same unit in a different socket keeps its id.
func TestDevicesSerialledCardMovedPort(t *testing.T) {
	procA, sysA := buildFixture(t, []fakeCard{serialCard(1, "SN123", "2")})
	procB, sysB := buildFixture(t, []fakeCard{serialCard(1, "SN123", "4")})

	a, err := devicesFrom(procA, sysA)
	if err != nil {
		t.Fatalf("devicesFrom a: %v", err)
	}
	b, err := devicesFrom(procB, sysB)
	if err != nil {
		t.Fatalf("devicesFrom b: %v", err)
	}
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("got %d and %d devices, want 1 each: %+v %+v", len(a), len(b), a, b)
	}
	if a[0].ID != b[0].ID {
		t.Errorf("ID changed when the device moved port: %q -> %q", a[0].ID, b[0].ID)
	}
	if a[0].PortID == b[0].PortID {
		t.Errorf("PortID should follow the port, but both are %q", a[0].PortID)
	}
}

// TestDevicesSeriallessTwinsDistinctIDs covers two identical units with no
// serial: only the port tells them apart, and it must.
func TestDevicesSeriallessTwinsDistinctIDs(t *testing.T) {
	proc, sys := buildFixture(t, []fakeCard{portCard(1, "3"), portCard(2, "4")})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devs), devs)
	}
	if devs[0].ID == devs[1].ID {
		t.Fatalf("serial-less twins share id %q", devs[0].ID)
	}
	for i := range devs {
		if !devs[i].IDStable {
			t.Errorf("%s IDStable = false, want true", devs[i].HWAddr)
		}
		if !strings.Contains(devs[i].ID, ":p=") {
			t.Errorf("%s ID = %q, want the port form", devs[i].HWAddr, devs[i].ID)
		}
	}
}

// TestDevicesCompositeUSBFunctions covers a composite device exposing more than
// one audio function: bInterfaceNumber must keep them apart.
func TestDevicesCompositeUSBFunctions(t *testing.T) {
	mk := func(card int, ifnum string) fakeCard {
		return fakeCard{
			Card: card, CardID: "Composite" + ifnum, LongName: "Composite Audio",
			Capture: []int{0},
			USB: &fakeUSB{
				Vendor: compositeVID, Product: compositePID, Serial: "SHARED",
				Controller: testController, DevPath: "5", Interface: ifnum,
			},
		}
	}
	proc, sys := buildFixture(t, []fakeCard{mk(1, "00"), mk(2, "02")})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devs), devs)
	}
	if devs[0].ID == devs[1].ID {
		t.Fatalf("composite functions share id %q", devs[0].ID)
	}
	if got, want := devs[0].ID, "usb:1234:5678:s=SHARED:if=0,0"; got != want {
		t.Errorf("interface 0 ID = %q, want %q", got, want)
	}
	if got, want := devs[1].ID, "usb:1234:5678:s=SHARED:if=2,0"; got != want {
		t.Errorf("interface 2 ID = %q, want %q", got, want)
	}
}

// TestDevicesCompositeHexInterface pins bInterfaceNumber as HEX. sysfs prints it
// %02x, so "0a" is interface 10. The "00"/"02" cases elsewhere parse identically
// in base 10 and 16, so only a value with a hex letter proves the base: read as
// decimal, "0a" would fail to parse and the card would lose its id entirely.
func TestDevicesCompositeHexInterface(t *testing.T) {
	proc, sys := buildFixture(t, []fakeCard{{
		Card: 1, CardID: "Comp0a", LongName: "Composite Audio",
		Capture: []int{0},
		USB: &fakeUSB{
			Vendor: compositeVID, Product: compositePID, Serial: "SHARED",
			Controller: testController, DevPath: "5", Interface: "0a",
		},
	}})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(devs), devs)
	}
	if got, want := devs[0].ID, "usb:1234:5678:s=SHARED:if=10,0"; got != want {
		t.Errorf("ID = %q, want %q", got, want)
	}
	if devs[0].USB.Interface != 10 {
		t.Errorf("USB.Interface = %d, want 10", devs[0].USB.Interface)
	}
}

// TestDevicesUnparseableInterfaceUnidentifiable covers a present but unparseable
// bInterfaceNumber: the card must become unidentifiable (fall back to the
// unstable hw address), not silently become interface 0, since a spurious 0
// would collide a composite device's functions.
func TestDevicesUnparseableInterfaceUnidentifiable(t *testing.T) {
	proc, sys := buildFixture(t, []fakeCard{{
		Card: 1, CardID: "Weird", LongName: "Weird USB",
		Capture: []int{0},
		USB: &fakeUSB{
			Vendor: compositeVID, Product: compositePID, Serial: "SN",
			Controller: testController, DevPath: "5", Interface: "zz",
		},
	}})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(devs), devs)
	}
	d := devs[0]
	if d.IDStable {
		t.Errorf("IDStable = true, want false for an unparseable interface: %+v", d)
	}
	if d.ID != d.HWAddr || d.ID != hwAddrCard1 {
		t.Errorf("ID = %q, want the hw-address fallback hw:1,0", d.ID)
	}
	if d.IsUSB() {
		t.Errorf("IsUSB = true, want false when the card is unidentifiable")
	}
}

// TestDevicesLowercasesVidPid pins the ToLower on idVendor/idProduct: sysfs may
// print them in either case, and the id must be canonical lowercase so a
// persisted id compares equal regardless of what the kernel printed.
func TestDevicesLowercasesVidPid(t *testing.T) {
	c := serialCard(1, "SN9", "3")
	c.USB.Vendor = "16D0"
	c.USB.Product = "06F3"
	proc, sys := buildFixture(t, []fakeCard{c})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(devs), devs)
	}
	if got, want := devs[0].ID, "usb:16d0:06f3:s=SN9:if=0,0"; got != want {
		t.Errorf("ID = %q, want lowercased %q", got, want)
	}
	if devs[0].USB.VendorID != audiomothVID || devs[0].USB.ProductID != audiomothPID {
		t.Errorf("USB vid/pid = %q/%q, want lowercase", devs[0].USB.VendorID, devs[0].USB.ProductID)
	}
}

// TestDevicesUSBWithoutSerialOrPortFallsBack is the "do not persist this id"
// contract: a USB card that reports neither a serial nor a derivable port has
// nothing stable to key on, so it falls back to the current-boot hw address with
// IDStable=false, even though it is genuinely a USB device.
func TestDevicesUSBWithoutSerialOrPortFallsBack(t *testing.T) {
	c := portCard(1, "3")  // vid/pid, no serial
	c.USB.NoDevPath = true // and no derivable port
	proc, sys := buildFixture(t, []fakeCard{c})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(devs), devs)
	}
	d := devs[0]
	if d.IDStable {
		t.Errorf("IDStable = true, want false: a serial-less, port-less USB card must not be persisted (%+v)", d)
	}
	if d.ID != d.HWAddr || d.ID != hwAddrCard1 {
		t.Errorf("ID = %q, want the hw fallback hw:1,0", d.ID)
	}
	if !d.IsUSB() {
		t.Errorf("IsUSB = false, want true: it is still a USB device, just unpersistable")
	}
	if d.PortID != "" {
		t.Errorf("PortID = %q, want empty when the port is not derivable", d.PortID)
	}
}

// TestDevicesUSBQuirkLayouts covers sysfs shapes the fully-populated single-hub
// layout cannot: the card's device link pointing straight at the usb_device, and
// a card that reads as non-USB (no vid/pid, or no subsystem link) falling back
// to its kernel card id.
func TestDevicesUSBQuirkLayouts(t *testing.T) {
	const quirkCardFormID = "hw:CARD=Quirk,DEV=0" // kernel-card-id fallback shared by the non-USB shapes
	base := func() *fakeUSB {
		return &fakeUSB{
			Vendor: compositeVID, Product: compositePID, Serial: "SN-Q",
			Controller: testController, DevPath: "3", Interface: "00",
		}
	}
	tests := []struct {
		name     string
		mutate   func(*fakeUSB)
		wantID   string
		wantUSB  bool
		wantStab bool
	}{
		{
			name:     "device link points straight at the usb_device",
			mutate:   func(u *fakeUSB) { u.DeviceLinkToUSBDev = true },
			wantID:   "usb:1234:5678:s=SN-Q:if=0,0",
			wantUSB:  true,
			wantStab: true,
		},
		{
			name:     "no vid/pid falls back to the kernel card id",
			mutate:   func(u *fakeUSB) { u.NoVendorProduct = true },
			wantID:   quirkCardFormID,
			wantUSB:  false,
			wantStab: true,
		},
		{
			name:     "no subsystem link reads as non-USB, keyed on the card id",
			mutate:   func(u *fakeUSB) { u.NoSubsystem = true },
			wantID:   quirkCardFormID,
			wantUSB:  false,
			wantStab: true,
		},
		{
			// With bInterfaceNumber absent the walk stays on the interface node,
			// where idVendor/idProduct are not, so there is no usable vid:pid and
			// the card keys on its kernel id.
			name:     "no bInterfaceNumber leaves no usable vid:pid, keyed on the card id",
			mutate:   func(u *fakeUSB) { u.NoInterfaceNum = true },
			wantID:   quirkCardFormID,
			wantUSB:  false,
			wantStab: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := base()
			tt.mutate(u)
			proc, sys := buildFixture(t, []fakeCard{{
				Card: 1, CardID: "Quirk", LongName: "Quirk Audio", Capture: []int{0}, USB: u,
			}})
			devs, err := devicesFrom(proc, sys)
			if err != nil {
				t.Fatalf("devicesFrom: %v", err)
			}
			if len(devs) != 1 {
				t.Fatalf("got %d devices, want 1: %+v", len(devs), devs)
			}
			d := devs[0]
			if d.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", d.ID, tt.wantID)
			}
			if d.IsUSB() != tt.wantUSB {
				t.Errorf("IsUSB = %v, want %v", d.IsUSB(), tt.wantUSB)
			}
			if d.IDStable != tt.wantStab {
				t.Errorf("IDStable = %v, want %v", d.IDStable, tt.wantStab)
			}
		})
	}
}

// TestDevicesMultiLevelHubPort covers a device behind an external hub: its
// devpath has more than one segment ("1.4"), so the usb_device sits below more
// than one usb level and the port walk must climb past all of them to reach the
// controller.
func TestDevicesMultiLevelHubPort(t *testing.T) {
	proc, sys := buildFixture(t, []fakeCard{portCard(1, "1.4")})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(devs), devs)
	}
	d := devs[0]
	if want := "usb:1686:067f:p=0000:00:14.0-1.4:if=0,0"; d.ID != want {
		t.Errorf("ID = %q, want %q", d.ID, want)
	}
	if want := "0000:00:14.0-1.4"; d.USB.Port != want {
		t.Errorf("USB.Port = %q, want %q", d.USB.Port, want)
	}
}

// TestDevicesMixedSysfsFallback covers a mix within one enumeration: one card is
// identified from sysfs while a sibling in the SAME enumeration has no sysfs node
// at all. The identified card must stay stable and the other must fall back to
// its hw address, which is the per-card branch the idents map introduced.
func TestDevicesMixedSysfsFallback(t *testing.T) {
	proc, sys := buildFixture(t, []fakeCard{
		serialCard(1, "SN-A", "3"),
		{Card: 2, CardID: "Blind", LongName: "No Sysfs Card", Capture: []int{0}, NoSysfs: true},
	})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devs), devs)
	}
	first := findDevice(t, devs, hwAddrCard1)
	if !first.IDStable || first.ID != "usb:16d0:06f3:s=SN-A:if=0,0" {
		t.Errorf("identified card = %+v, want a stable serial id", first)
	}
	second := findDevice(t, devs, hwAddrCard2)
	if second.IDStable {
		t.Errorf("no-sysfs sibling IDStable = true, want false: %+v", second)
	}
	if second.ID != second.HWAddr || second.ID != hwAddrCard2 {
		t.Errorf("no-sysfs sibling ID = %q, want the hw fallback hw:2,0", second.ID)
	}
}

// TestDevicesTwoDigitCardIndex exercises the numeric sort, which was previously
// untested and is the classic place a string sort puts card 10 before card 2.
func TestDevicesTwoDigitCardIndex(t *testing.T) {
	proc, sys := buildFixture(t, []fakeCard{portCard(2, "1"), portCard(10, "2"), portCard(1, "3")})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	wantOrder := []int{1, 2, 10}
	if len(devs) != len(wantOrder) {
		t.Fatalf("got %d devices, want %d", len(devs), len(wantOrder))
	}
	for i, want := range wantOrder {
		if devs[i].Card != want {
			t.Errorf("devs[%d].Card = %d, want %d (order: %v)", i, devs[i].Card, want, cardOrder(devs))
		}
	}
}

func cardOrder(devs []DeviceInfo) []int {
	out := make([]int, 0, len(devs))
	for i := range devs {
		out = append(out, devs[i].Card)
	}
	return out
}

// TestDevicesFallbackWithoutSysfs is the container case: no /sys to read, so
// the id degrades to the unstable address and says so.
func TestDevicesFallbackWithoutSysfs(t *testing.T) {
	proc, _ := buildFixture(t, []fakeCard{portCard(1, "3")})
	devs, err := devicesFrom(proc, absentSysRoot(t))
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1", len(devs))
	}
	d := devs[0]
	if d.ID != hwAddrCard1 {
		t.Errorf("ID = %q, want the %s fallback", d.ID, hwAddrCard1)
	}
	if d.IDStable {
		t.Error("IDStable = true, want false when sysfs is unreadable")
	}
	if d.HWAddr != hwAddrCard1 {
		t.Errorf("HWAddr = %q, want %s", d.HWAddr, hwAddrCard1)
	}
	if d.IsUSB() || d.CardID != "" || d.PortID != "" {
		t.Errorf("expected no identity fields without sysfs, got %+v", d)
	}
}

// TestDevicesCardWithoutDeviceLink covers a virtual card whose sysfs node has
// no bus device: the kernel card id alone still gives a stable name.
func TestDevicesCardWithoutDeviceLink(t *testing.T) {
	c := loopbackCard(0)
	c.NoDeviceLink = true
	proc, sys := buildFixture(t, []fakeCard{c})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	// The loopback card exposes two capture PCMs, so it yields two devices; the
	// first (DEV=0) carries the id under test.
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2: %+v", len(devs), devs)
	}
	if devs[0].ID != wantLoopbackID || !devs[0].IDStable {
		t.Errorf("ID = %q (stable %v), want %q (stable)", devs[0].ID, devs[0].IDStable, wantLoopbackID)
	}
}

func TestDevicesSkipsPlaybackOnlyCard(t *testing.T) {
	proc, sys := buildFixture(t, []fakeCard{
		{Card: 0, CardID: "Play", LongName: "Playback Only", Playback: []int{0}},
		portCard(1, "3"),
	})
	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 1 || devs[0].Card != 1 {
		t.Fatalf("playback-only card must not be listed, got %+v", devs)
	}
}

// TestDevicesUnreadableSerialIsNotTreatedAsAbsent pins the distinction the
// whole identity path turns on: an attribute that is ABSENT is normal (not
// every device reports a serial), but one that exists and cannot be READ means
// we do not know what this device is. Confusing the two is the subtle version
// of the bug this feature exists to fix: the device would silently enumerate
// under its port form while still claiming IDStable, so a persisted serial id
// would stop matching hardware that is sitting right there.
func TestDevicesUnreadableSerialIsNotTreatedAsAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0000 does not deny reads, so the case cannot be staged")
	}
	c := serialCard(1, "SN-UNREADABLE", "3")
	c.USB.UnreadableSerial = true
	proc, sys := buildFixture(t, []fakeCard{c})

	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(devs), devs)
	}
	d := devs[0]
	if d.IDStable {
		t.Errorf("IDStable = true for a card whose serial could not be read; ID = %q", d.ID)
	}
	if d.ID != d.HWAddr {
		t.Errorf("ID = %q, want the unstable %q fallback", d.ID, d.HWAddr)
	}
	if strings.Contains(d.ID, ":p=") {
		t.Errorf("ID = %q: an unreadable serial must not silently become the port form", d.ID)
	}
}

// TestDevicesUnreadableVendorIsNotTreatedAsAbsent is the same distinction one
// attribute over, on the pair that decides whether the card is USB at all. A
// read failure here must not let the card fall through to the kernel card-id
// form, which would be a different confident stable id for the same hardware.
func TestDevicesUnreadableVendorIsNotTreatedAsAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0000 does not deny reads, so the case cannot be staged")
	}
	proc, sys := buildFixture(t, []fakeCard{serialCard(1, "SN1", "3")})
	vendor := findSysAttr(t, sys, "idVendor")
	if err := os.Chmod(vendor, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(vendor, 0o644) })

	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(devs), devs)
	}
	if devs[0].IDStable {
		t.Errorf("IDStable = true though idVendor could not be read; ID = %q", devs[0].ID)
	}
	if strings.HasPrefix(devs[0].ID, "hw:CARD=") {
		t.Errorf("ID = %q: an unreadable vid must not fall through to the card-id form", devs[0].ID)
	}
}

// findSysAttr locates a named attribute file anywhere under a fixture's sysfs
// tree, so a test can make one unreadable without restating the layout.
func findSysAttr(t *testing.T, sys, name string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(sys, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != name || found != "" {
			return nil //nolint:nilerr // a walk error just means this entry is not the one
		}
		found = path
		return nil
	})
	if err != nil || found == "" {
		t.Fatalf("no %s under %s (walk err %v)", name, sys, err)
	}
	return found
}

// TestQuoteGlobMeta covers the escaping that keeps enumeration working when the
// procfs root contains a glob metacharacter, which a fixture path can.
func TestQuoteGlobMeta(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"plain/path", "plain/path"},
		{"a*b", `a\*b`},
		{"a?b", `a\?b`},
		{"a[b]c", `a\[b]c`},
		{`a\b`, `a\\b`},
		{"", ""},
	} {
		if got := quoteGlobMeta(tt.in); got != tt.want {
			t.Errorf("quoteGlobMeta(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestDevicesWithGlobMetaInProcRoot is the reason quoteGlobMeta exists: an
// unescaped root turns the card glob into a pattern that matches nothing, and
// enumeration then reports "no capture devices" rather than failing.
func TestDevicesWithGlobMetaInProcRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root[1]")
	proc := filepath.Join(root, "proc")
	sys := filepath.Join(root, "sys")
	mkdirAll(t, proc)
	cards := []fakeCard{portCard(1, "3")}
	writeProcTree(t, proc, cards)
	writeSysTree(t, sys, cards)

	devs, err := devicesFrom(proc, sys)
	if err != nil {
		t.Fatalf("devicesFrom: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1: a glob metacharacter in the root must not hide them", len(devs))
	}
}
