//go:build linux

package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This file builds throwaway /proc/asound and /sys trees under t.TempDir() so
// the device-identity code can be exercised over layouts this machine does not
// have: swapped card indices, duplicate serials, composite USB functions, a
// two-digit card index. The sysfs trees are built with os.Symlink at run time
// rather than committed, because a git checkout cannot carry the symlink web
// that sysfs identity derivation walks.

// Fixture constants shared by the identity tests.
const (
	loopbackName   = "Loopback"
	testController = "0000:00:14.0"
)

// fakeUSB describes the USB attachment of a fixture card. Controller and
// ControllerBus name the host controller the port walk terminates on.
type fakeUSB struct {
	Vendor        string
	Product       string
	Serial        string
	Controller    string // e.g. "0000:00:14.0" or "xhci-hcd.0"
	ControllerBus string // "pci" or "platform"; defaults to "pci"
	DevPath       string // USB devpath, e.g. "3" or "1.4"
	Interface     string // bInterfaceNumber as sysfs spells it, e.g. "00" (hex)
	HubName       string // root-hub directory name; defaults to "usb3"

	// Knobs that omit or reshape parts of the tree, to reach production branches
	// the fully-populated single-hub layout cannot. Each defaults to the normal,
	// complete shape.
	NoVendorProduct    bool // omit idVendor and idProduct (no usable vid:pid)
	NoInterfaceNum     bool // omit bInterfaceNumber on the interface node
	NoDevPath          bool // omit the devpath file (port not derivable)
	NoSubsystem        bool // omit the subsystem link on the node cardN/device points at
	DeviceLinkToUSBDev bool // point cardN/device straight at the usb_device, not the interface
	// UnreadableSerial makes the serial attribute exist but refuse to be read
	// (mode 0000), which is the case sysfs absence must NOT be confused with: a
	// device that HAS a serial whose value we failed to obtain.
	UnreadableSerial bool
}

// fakeCard describes one card in a fixture: its kernel id, its capture and
// playback PCM nodes, and its bus attachment. NoSysfs omits the sysfs node
// entirely, which is the container / partial-/sys case.
type fakeCard struct {
	Card     int
	CardID   string // kernel short id, e.g. "Loopback"
	LongName string // /proc/asound/cards longname
	Capture  []int  // capture PCM device numbers
	Playback []int  // playback-only PCM device numbers
	USB      *fakeUSB
	NoSysfs  bool
	// NoDeviceLink omits cardN/device, as a purely virtual card does.
	NoDeviceLink bool
}

// buildFixture writes both kernel trees and returns their roots.
func buildFixture(t *testing.T, cards []fakeCard) (proc, sys string) {
	t.Helper()
	root := t.TempDir()
	proc = filepath.Join(root, "proc")
	sys = filepath.Join(root, "sys")
	writeProcTree(t, proc, cards)
	writeSysTree(t, sys, cards)
	return proc, sys
}

// useFixture points Devices and the post-open identity check at a fixture for
// the duration of one test.
func useFixture(t *testing.T, cards []fakeCard) (proc, sys string) {
	t.Helper()
	proc, sys = buildFixture(t, cards)
	setRoots(t, proc, sys)
	return proc, sys
}

// setRoots points the package's procRoot and sysRoot at a fixture for the
// duration of one test, restoring them on cleanup. It mutates package vars, so a
// test that reaches it (directly or via useFixture) must NOT call t.Parallel:
// parallel tests would race these globals and see each other's fixtures.
func setRoots(t *testing.T, proc, sys string) {
	t.Helper()
	oldProc, oldSys := procRoot, sysRoot
	procRoot, sysRoot = proc, sys
	t.Cleanup(func() { procRoot, sysRoot = oldProc, oldSys })
}

func writeProcTree(t *testing.T, proc string, cards []fakeCard) {
	t.Helper()
	mkdirAll(t, proc)
	var listing string
	for _, c := range cards {
		long := c.LongName
		if long == "" {
			long = c.CardID
		}
		listing += fmt.Sprintf("%2d [%-14s]: Fixture - %s\n", c.Card, c.CardID, long)
		for _, d := range c.Capture {
			writeFile(t, filepath.Join(proc, fmt.Sprintf("card%d", c.Card), fmt.Sprintf("pcm%dc", d), "info"), "")
		}
		for _, d := range c.Playback {
			writeFile(t, filepath.Join(proc, fmt.Sprintf("card%d", c.Card), fmt.Sprintf("pcm%dp", d), "info"), "")
		}
	}
	writeFile(t, filepath.Join(proc, "cards"), listing)
}

func writeSysTree(t *testing.T, sys string, cards []fakeCard) {
	t.Helper()
	// Bus directories are the symlink targets that identify a device's
	// subsystem, exactly as /sys/bus/{usb,pci,platform} do.
	for _, bus := range []string{"usb", "pci", "platform"} {
		mkdirAll(t, filepath.Join(sys, "bus", bus))
	}
	for i := range cards {
		if cards[i].NoSysfs {
			continue
		}
		writeSysCard(t, sys, &cards[i])
	}
}

func writeSysCard(t *testing.T, sys string, c *fakeCard) {
	t.Helper()
	cardDir := filepath.Join(sys, "class", "sound", fmt.Sprintf("card%d", c.Card))
	mkdirAll(t, cardDir)
	writeFile(t, filepath.Join(cardDir, "id"), c.CardID+"\n")
	if c.NoDeviceLink {
		return
	}
	if c.USB == nil {
		// A platform card: cardN/device points at a platform node.
		devDir := filepath.Join(sys, "devices", "platform", c.CardID+".0")
		mkdirAll(t, devDir)
		linkSubsystem(t, sys, devDir, "platform")
		symlink(t, devDir, filepath.Join(cardDir, "device"))
		return
	}
	u := c.USB
	bus := u.ControllerBus
	if bus == "" {
		bus = "pci"
	}
	hub := u.HubName
	if hub == "" {
		hub = "usb3"
	}
	// The root-hub bus number is the digits after "usb" (usb3 -> 3, usb10 ->
	// 10), i.e. the whole numeric tail and not just the last byte, so a
	// two-digit bus names its children "10-..." rather than "0-...".
	busNum := strings.TrimPrefix(hub, "usb")

	// .../<bus>/<controller>/<hub>/<usbdev...>/<interface>, mirroring the real
	// sysfs chain the port walk climbs. Each devpath segment becomes its own usb
	// node, so a multi-level chain (devpath "1.4") nests the device below more
	// than one usb level, exactly as a device behind an external hub does.
	ctlDir := filepath.Join(sys, "devices", bus+"0000", u.Controller)
	hubDir := filepath.Join(ctlDir, hub)
	linkSubsystem(t, sys, ctlDir, bus)
	linkSubsystem(t, sys, hubDir, "usb")

	usbDir := hubDir
	parent := hubDir
	var segs []string
	if u.DevPath != "" {
		segs = strings.Split(u.DevPath, ".")
	}
	for i := range segs {
		node := filepath.Join(parent, busNum+"-"+strings.Join(segs[:i+1], "."))
		mkdirAll(t, node)
		linkSubsystem(t, sys, node, "usb")
		parent = node
		usbDir = node
	}
	if usbDir == hubDir {
		// A devpath-less device still needs a usb_device node distinct from the
		// root hub to hang its attributes on.
		usbDir = filepath.Join(hubDir, busNum+"-0")
		mkdirAll(t, usbDir)
		linkSubsystem(t, sys, usbDir, "usb")
	}

	if !u.NoVendorProduct {
		writeFile(t, filepath.Join(usbDir, "idVendor"), u.Vendor+"\n")
		writeFile(t, filepath.Join(usbDir, "idProduct"), u.Product+"\n")
	}
	if !u.NoDevPath && u.DevPath != "" {
		writeFile(t, filepath.Join(usbDir, "devpath"), u.DevPath+"\n")
	}
	if u.Serial != "" {
		serialPath := filepath.Join(usbDir, "serial")
		writeFile(t, serialPath, u.Serial+"\n")
		if u.UnreadableSerial {
			// Present but unreadable. t.TempDir's cleanup needs the tree
			// traversable, so restore the mode when the test ends.
			if err := os.Chmod(serialPath, 0o000); err != nil {
				t.Fatalf("chmod %s: %v", serialPath, err)
			}
			t.Cleanup(func() { _ = os.Chmod(serialPath, 0o644) })
		}
	}

	// The interface node carries bInterfaceNumber. Its directory suffix is the
	// interface number in DECIMAL and unpadded ("3-3:1.0", not "3-3:1.00"), even
	// though the attribute file itself is zero-padded hex ("00").
	ifDir := filepath.Join(usbDir, filepath.Base(usbDir)+":1."+ifaceDirSuffix(u.Interface))
	mkdirAll(t, ifDir)
	linkSubsystem(t, sys, ifDir, "usb")
	if !u.NoInterfaceNum {
		writeFile(t, filepath.Join(ifDir, "bInterfaceNumber"), u.Interface+"\n")
	}

	// The card's device link normally points at the interface node; the
	// tolerated quirk layout points it straight at the usb_device.
	deviceTarget := ifDir
	if u.DeviceLinkToUSBDev {
		deviceTarget = usbDir
	}
	if u.NoSubsystem {
		// Drop the subsystem link on the node cardN/device points at, so its bus
		// cannot be determined and it reads as non-USB.
		removeSubsystem(t, deviceTarget)
	}
	symlink(t, deviceTarget, filepath.Join(cardDir, "device"))
}

// ifaceDirSuffix renders bInterfaceNumber (zero-padded hex in sysfs) as the
// decimal, unpadded suffix real sysfs uses for the interface directory. A value
// that is not valid hex is passed through unchanged, since the directory name is
// never parsed by production and only the attribute file's contents are.
func ifaceDirSuffix(iface string) string {
	if n, err := strconv.ParseInt(iface, 16, 32); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return iface
}

// removeSubsystem deletes the subsystem symlink under dir, tolerating its
// absence, so a fixture can model a node whose bus is undeterminable.
func removeSubsystem(t *testing.T, dir string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, "subsystem")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove subsystem %s: %v", dir, err)
	}
}

func linkSubsystem(t *testing.T, sys, dir, bus string) {
	t.Helper()
	symlink(t, filepath.Join(sys, "bus", bus), filepath.Join(dir, "subsystem"))
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// symlink creates link -> target, tolerating a link that is already there with
// the same target: two cards behind one host controller both mark that
// controller's subsystem, exactly as they do in real sysfs.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(link))
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if errors.Is(err, os.ErrExist) {
		if got, rerr := os.Readlink(link); rerr == nil && got == target {
			return
		}
	}
	t.Fatalf("symlink %s -> %s: %v", link, target, err)
}

// absentSysRoot returns a path that is guaranteed not to exist, for the
// container / partial-/sys fallback cases where sysfs cannot be read. It lives
// under the test's own t.TempDir(), so unlike a committed relative path or a
// hardcoded absolute one it cannot accidentally exist on some machine, and git
// cannot be relied on to keep it absent.
func absentSysRoot(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "no-sysfs")
}

// findDevice returns the enumerated device with the given hw address.
func findDevice(t *testing.T, devs []DeviceInfo, hwaddr string) DeviceInfo {
	t.Helper()
	for i := range devs {
		if devs[i].HWAddr == hwaddr {
			return devs[i]
		}
	}
	t.Fatalf("no device with HWAddr %q in %+v", hwaddr, devs)
	return DeviceInfo{}
}

// The fixture cards reused across tests: a platform loopback, a USB device with
// a serial, and a USB device without one. They mirror the hardware this was
// developed against (snd-aloop, an AudioMoth, and a ZOOM AMS-24).
func loopbackCard(idx int) fakeCard {
	return fakeCard{Card: idx, CardID: loopbackName, LongName: loopbackName, Capture: []int{0, 1}}
}

func serialCard(idx int, serial, devpath string) fakeCard {
	return fakeCard{
		Card: idx, CardID: "Microphone", LongName: "384kHz AudioMoth USB Microphone",
		Capture: []int{0},
		USB: &fakeUSB{
			Vendor: audiomothVID, Product: audiomothPID, Serial: serial,
			Controller: testController, DevPath: devpath, Interface: "00",
		},
	}
}

func portCard(idx int, devpath string) fakeCard {
	return fakeCard{
		Card: idx, CardID: "AMS24", LongName: "AMS-24",
		Capture: []int{0},
		USB: &fakeUSB{
			Vendor: "1686", Product: "067f",
			Controller: testController, DevPath: devpath, Interface: "00",
		},
	}
}
