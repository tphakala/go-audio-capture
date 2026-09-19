//go:build linux

package capture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cardIdent is one card's stable identity as read from sysfs. HasSysfs is false
// when the card's sysfs node could not be read at all, which is the signal to
// fall back to the unstable "hw:N,D" id. USB is nil for a non-USB card (a
// platform codec, HDA, snd-aloop, a virtual card), which is identified by its
// kernel card id instead.
type cardIdent struct {
	HasSysfs bool
	CardID   string
	USB      *USBInfo
}

// readCardIdent reads card's stable identity from sysfs. It never returns an
// error: an unreadable or unexpected layout yields a zero cardIdent (HasSysfs
// false), because a card that cannot be identified must still be listed and
// openable by its current-boot address rather than disappearing from Devices.
func readCardIdent(sys string, card int) cardIdent {
	base := filepath.Join(sys, "class", "sound", fmt.Sprintf("card%d", card))
	id, ok := readSysAttr(base, "id")
	if !ok {
		// A real read error on the kernel card id (not mere absence) means this
		// card cannot be trusted to identify itself; fall back to the unstable
		// address rather than build a confident id from partial data.
		return cardIdent{}
	}

	// cardN/device is a symlink to the bus device that owns the card: the USB
	// interface for a USB Audio Class card, the platform/PCI node otherwise.
	devLink := filepath.Join(base, "device")
	devDir, err := filepath.EvalSymlinks(devLink)
	if err != nil {
		// Absence and unresolvability are different answers here, exactly as
		// they are for an attribute. A card with no device link at all is a
		// virtual card, and its kernel card id is a usable identity. A link that
		// exists but does not resolve leaves us unable to tell whether the card
		// is behind USB, so keying it on the card id would hand out a confident
		// hw:CARD= id for what may be a USB device that owns a usb: one.
		//
		// The error from EvalSymlinks cannot make that distinction on its own: it
		// walks the whole chain, so a DANGLING link (present, target missing, the
		// shape a partially masked /sys produces) fails with ENOENT just as an
		// absent link does. Ask about the link itself with Lstat, which does not
		// follow it.
		if _, lerr := os.Lstat(devLink); !errors.Is(lerr, fs.ErrNotExist) {
			return cardIdent{}
		}
		if id == "" {
			return cardIdent{}
		}
		return cardIdent{HasSysfs: true, CardID: id}
	}

	usb, ok := readUSBIdent(sys, devDir)
	if !ok {
		// Behind USB but its identity could not be read (a real read error, or an
		// unparseable interface number). Falling back to the kernel card id here
		// would hand out a confident but DIFFERENT stable id (hw:CARD=...), so a
		// persisted usb: id would silently stop matching a device that is present.
		// Refuse to identify the card instead.
		return cardIdent{}
	}
	if usb == nil && id == "" {
		return cardIdent{}
	}
	return cardIdent{HasSysfs: true, CardID: id, USB: usb}
}

// readUSBIdent derives the USB identity of the card whose owning bus device is
// devDir, or nil when the card is not behind USB. devDir is the USB *interface*
// node (subsystem "usb", carrying bInterfaceNumber); its parent is the
// usb_device node carrying idVendor, idProduct, serial, and devpath.
// The bool result is "trustworthy": true means the card was read cleanly (a nil
// *USBInfo then means it is simply not a USB card, or a USB card with no usable
// vid:pid, and the caller may key it on the kernel card id). false means the
// card IS behind USB but a real read error left its identity unknowable, so the
// caller must refuse to identify it rather than fall back to a different id form.
func readUSBIdent(root, devDir string) (*USBInfo, bool) {
	if sysSubsystem(devDir) != "usb" {
		return nil, true
	}
	// Walk up to the usb_device. An interface node has bInterfaceNumber and its
	// parent has idVendor; be tolerant of a card whose device link already
	// points at the usb_device by checking both.
	ifNum := -1
	usbDev := devDir
	s, ok := readSysAttr(devDir, "bInterfaceNumber")
	if !ok {
		// A real read error on a USB interface node: refuse to identify the card
		// rather than key it on partial data.
		return nil, false
	}
	if s != "" {
		// bInterfaceNumber is zero-padded hexadecimal in sysfs ("00", "02"). A
		// value that is present but unparseable must not silently become
		// interface 0: that would give a composite device's second audio function
		// the same id as its first. Treat present-but-unparseable as
		// unidentifiable; only a genuinely absent value defaults to 0 below.
		n, err := strconv.ParseInt(s, 16, 32)
		if err != nil {
			return nil, false
		}
		ifNum = int(n)
		usbDev = filepath.Dir(devDir)
	}
	vid, vok := readSysAttr(usbDev, "idVendor")
	pid, pok := readSysAttr(usbDev, "idProduct")
	if !vok || !pok {
		// A real read error on vid/pid on a USB device: fail closed, otherwise a
		// device would silently switch from its usb: id to the card-id form.
		return nil, false
	}
	if vid == "" || pid == "" {
		// Behind USB but without a usable vid:pid there is nothing stable to
		// key on; fall back to the card id (or to hw:N,D).
		return nil, true
	}
	serial, sok := readSysAttr(usbDev, "serial")
	if !sok {
		// A real read error on the serial must not silently drop the device to
		// the port form (or, via the card id, to a different confident id).
		return nil, false
	}
	if ifNum < 0 {
		ifNum = 0
	}
	return &USBInfo{
		VendorID:  strings.ToLower(vid),
		ProductID: strings.ToLower(pid),
		Serial:    serial,
		Port:      usbPort(root, usbDev),
		Interface: ifNum,
	}, true
}

// usbPort names the physical attachment point of the usb_device at usbDev as
// "<controller>-<devpath>", e.g. "0000:00:14.0-3" or "xhci-hcd.0-1.4". The
// controller is found by walking parents until one is not on the USB bus, which
// lands on the PCI or platform node of the host controller. The USB bus number
// is deliberately not used: bus numbers follow controller probe order and so
// carry the very instability this id exists to avoid.
func usbPort(root, usbDev string) string {
	// Resolve the sysfs root and the device path to their symlink-free forms
	// before the walk. readCardIdent hands us an EvalSymlinks-resolved device
	// path but the raw root, so a symlinked root (or a symlinked ancestor of a
	// test or container mount) would never equal a walk step, letting the walk
	// step over the dir == root boundary below. Resolving both keeps them
	// comparable; a resolve error (a path that does not exist) falls back to the
	// raw value, which still compares correctly in the common case.
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if d, err := filepath.EvalSymlinks(usbDev); err == nil {
		usbDev = d
	}
	// A real read error on devpath is deliberately treated like its absence:
	// both yield an empty port, which routes a serial-less card to the unstable
	// hw:N,D fallback (IDStable=false) rather than to a confident but wrong id. A
	// card that reports a serial does not use the port at all, so its stable id
	// is unaffected by a devpath read hiccup; failing the whole card here would
	// discard that valid serial id.
	devpath, _ := readSysAttr(usbDev, "devpath")
	controller := ""
	for dir := filepath.Dir(usbDev); ; dir = filepath.Dir(dir) {
		// Bound the walk by the sysfs root it was handed, not the filesystem
		// root, so a fixture tree (or a container mount) can never make it climb
		// out of the tree being read. Under a real /sys the host controller is
		// found long before this, so the extra guard costs nothing there.
		if dir == root || dir == "/" || dir == "." || dir == filepath.Dir(dir) {
			break
		}
		if sysSubsystem(dir) != "usb" {
			controller = filepath.Base(dir)
			break
		}
	}
	if controller == "" || devpath == "" {
		return ""
	}
	return controller + "-" + devpath
}

// sysSubsystem returns the bus name a sysfs device node belongs to ("usb",
// "pci", "platform"), or "" when it has no subsystem link.
func sysSubsystem(dir string) string {
	target, err := filepath.EvalSymlinks(filepath.Join(dir, "subsystem"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// readSysAttr reads a one-line sysfs attribute and trims it. ok is true when the
// attribute is present, and also when it is simply absent, because a missing
// attribute is normal (not every device reports a serial). ok is false ONLY for
// a real read error: any failure other than "does not exist", such as a
// permission error. The callers fail closed on ok=false, treating the card as
// unidentifiable rather than silently switching it to a different id form built
// from partial data.
func readSysAttr(dir, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", true
		}
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// stableID builds the persistable id for a card/device pair, and reports
// whether it is in fact stable. An unidentifiable card falls back to the
// current-boot "hw:N,D" address with stable false.
func stableID(ci cardIdent, card, device int) (id string, stable bool) {
	if !ci.HasSysfs {
		return hwAddr(card, device), false
	}
	if ci.USB != nil {
		if ci.USB.Serial != "" {
			return usbSerialID(ci.USB, device), true
		}
		if pid := usbPortID(ci.USB, device); pid != "" {
			return pid, true
		}
		// Behind USB, no serial and no derivable port: nothing stable is left.
		return hwAddr(card, device), false
	}
	if ci.CardID != "" {
		return cardFormID(ci.CardID, device), true
	}
	// HasSysfs with neither a USB identity nor a kernel card id. readCardIdent
	// never returns that combination: a sysfs-backed non-USB card always carries
	// its card id, and it fails closed (HasSysfs=false) otherwise. This fallback
	// exists only to keep the function total; TestStableIDFallsBackToHWAddr
	// exercises it directly so the guard cannot rot unnoticed.
	return hwAddr(card, device), false
}

// usbSerialID is the serial form: it names the unit, so it follows the device
// to any port. The serial is used whenever the device reports one, regardless
// of what else is plugged in, because choosing by "is a twin present right now"
// would make the id change when a second unit is attached.
func usbSerialID(u *USBInfo, device int) string {
	return fmt.Sprintf("usb:%s:%s:s=%s:if=%d,%d", u.VendorID, u.ProductID, escapeSerial(u.Serial), u.Interface, device)
}

// usbPortID is the port form: it names the physical port rather than the unit,
// and is empty when the port could not be derived. Keeping vid:pid in it means
// a different model moved onto that port resolves to "not found" rather than
// being opened as if it were the expected device.
func usbPortID(u *USBInfo, device int) string {
	if u == nil || u.Port == "" {
		return ""
	}
	return fmt.Sprintf("usb:%s:%s:p=%s:if=%d,%d", u.VendorID, u.ProductID, escapePort(u.Port), u.Interface, device)
}

// cardFormID builds the non-USB "hw:CARD=<id>,DEV=<dev>" form. It is the single
// grammar for that id: stableID generates it and canonicalCardID re-renders a
// parsed id through it, so the generated and canonical spellings cannot drift
// apart (a drift would make every resolve of a valid card id silently miss). The
// kernel card id is interpolated raw, not escaped: alsa-lib's own hw:CARD=
// syntax takes it verbatim, and the kernel restricts it to a safe charset, so
// escaping it here would produce ids alsa-lib does not accept.
func cardFormID(cardID string, device int) string {
	return fmt.Sprintf("hw:CARD=%s,DEV=%d", cardID, device)
}

func hwAddr(card, device int) string { return fmt.Sprintf("hw:%d,%d", card, device) }

const hexDigits = "0123456789ABCDEF"

// errTruncatedEscape reports a percent escape cut short by the end of the
// field, e.g. a persisted id truncated in storage.
var errTruncatedEscape = errors.New("truncated escape")

// escapeSerial percent-escapes every byte outside [A-Za-z0-9._-]. A serial is
// whatever bytes a vendor chose to put in its descriptor, so it gets the strict
// treatment: the escape covers the id's own delimiters, whitespace, control
// bytes, and non-ASCII, leaving a single unambiguous token.
func escapeSerial(s string) string { return escapeField(s, isSerialSafeByte) }

// escapePort escapes a port string, keeping ':' readable because a PCI host
// controller name contains colons ("0000:00:14.0") and the id grammar locates
// the ":if=" suffix from the right, so an embedded colon cannot mis-parse. The
// controller and devpath names this joins come from PCI/platform addresses and
// USB devpaths, which on the layouts seen use only [0-9a-f:.-]; the escaping is
// defensive, so an unexpected name still cannot produce an unparseable id.
func escapePort(s string) string { return escapeField(s, isPortSafeByte) }

func escapeField(s string, safe func(byte) bool) string {
	needs := false
	for i := range len(s) {
		if !safe(s[i]) {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := range len(s) {
		c := s[i]
		if safe(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[c>>4])
		b.WriteByte(hexDigits[c&0x0f])
	}
	return b.String()
}

func isSerialSafeByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '.', c == '_', c == '-':
		return true
	default:
		return false
	}
}

func isPortSafeByte(c byte) bool { return isSerialSafeByte(c) || c == ':' }

// unescapeIDField reverses the percent-escaping applied by escapeField (through
// escapeSerial and escapePort). A malformed escape is an error rather than a
// silent pass-through, so a corrupted persisted id is reported instead of
// resolving to something unintended.
func unescapeIDField(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", errTruncatedEscape
		}
		hi, ok := hexVal(s[i+1])
		if !ok {
			return "", fmt.Errorf("invalid percent-escape %q", s[i:i+3])
		}
		lo, ok := hexVal(s[i+2])
		if !ok {
			return "", fmt.Errorf("invalid percent-escape %q", s[i:i+3])
		}
		b.WriteByte(hi<<4 | lo)
		i += 2
	}
	return b.String(), nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// resolved is the outcome of turning a device id into something openable.
// verifyID is the canonical stable id the caller asked for, to be re-checked
// after the PCM opens; it is empty for a numeric "hw:N,D" id, where the card
// index IS what the caller named and there is nothing else to verify against.
type resolved struct {
	card     int
	device   int
	verifyID string
}

// resolveForOpen turns any accepted device id into a card and device number.
//
// A numeric "hw:N,D" (or "N,D", or "hw:N") passes straight through without
// enumerating, so it keeps opening exactly what it names even on a system where
// /proc/asound cannot be read. A stable id is matched against the devices
// present right now, on every call: caching the result would reintroduce the
// very staleness the stable id exists to remove.
func resolveForOpen(id string) (resolved, error) {
	trimmed := strings.TrimSpace(id)
	if !isStableIDForm(trimmed) {
		card, dev, err := parseNumericDeviceID(id)
		if err != nil {
			return resolved{}, err
		}
		return resolved{card: card, device: dev}, nil
	}
	canon, err := canonicalStableID(trimmed)
	if err != nil {
		return resolved{}, err
	}
	d, err := matchStableID(canon, id)
	if err != nil {
		return resolved{}, err
	}
	return resolved{card: d.Card, device: d.Device, verifyID: canon}, nil
}

// Resolve reports which device an id currently names, without opening it. It
// accepts the same ids as Config.Device and is the way to check that a
// persisted id still points at present hardware, and to learn its current
// HWAddr for display, before committing to a capture.
//
// It returns *DeviceNotFoundError (which unwraps to ErrDeviceGone) when nothing
// matches and *AmbiguousDeviceError when more than one device does. Unlike
// opening, a numeric "hw:N,D" is resolved against the enumerated devices too,
// so Resolve reports a card index that is not present rather than reporting
// success for a device that cannot be opened.
func Resolve(id string) (DeviceInfo, error) {
	trimmed := strings.TrimSpace(id)
	if !isStableIDForm(trimmed) {
		card, dev, err := parseNumericDeviceID(id)
		if err != nil {
			return DeviceInfo{}, err
		}
		devs, err := Devices()
		if err != nil {
			// Same reasoning as matchStableID: a caller told to retire a device
			// on ErrDeviceGone must see that classification from both branches,
			// or the numeric and stable forms behave differently for one cause.
			return DeviceInfo{}, fmt.Errorf("%w: %w", ErrDeviceGone, err)
		}
		for i := range devs {
			if devs[i].Card == card && devs[i].Device == dev {
				return devs[i], nil
			}
		}
		return DeviceInfo{}, &DeviceNotFoundError{ID: id}
	}
	canon, err := canonicalStableID(trimmed)
	if err != nil {
		return DeviceInfo{}, err
	}
	return matchStableID(canon, id)
}

// matchStableID finds the one device a canonical stable id names. orig is the
// caller's spelling, used in errors so the message quotes what they passed. A
// device matches on either its ID or its PortID, which is what lets a caller
// pin one of two same-serial twins by port.
func matchStableID(canon, orig string) (DeviceInfo, error) {
	devs, err := Devices()
	if err != nil {
		// Resolving a stable id means enumerating; if enumeration itself fails
		// (an unreadable /proc/asound), surface it under ErrDeviceGone so a caller
		// that retires a device with errors.Is(err, ErrDeviceGone) still fires for
		// the persisted stable-id form Open documents, keeping the cause via %w.
		return DeviceInfo{}, fmt.Errorf("%w: %w", ErrDeviceGone, err)
	}
	var matches []DeviceInfo
	for i := range devs {
		if devs[i].ID == canon || (devs[i].PortID != "" && devs[i].PortID == canon) {
			matches = append(matches, devs[i])
		}
	}
	switch len(matches) {
	case 0:
		return DeviceInfo{}, &DeviceNotFoundError{ID: orig}
	case 1:
		return matches[0], nil
	default:
		// The error's remedy is to pin one match by a listed id, so Matches must
		// carry PortIDs; fall back to a match's current-boot HWAddr when it has no
		// PortID, so the list never has a hole the caller cannot act on.
		pins := make([]string, 0, len(matches))
		for i := range matches {
			pin := matches[i].PortID
			if pin == "" {
				pin = matches[i].HWAddr
			}
			pins = append(pins, pin)
		}
		return DeviceInfo{}, &AmbiguousDeviceError{ID: orig, Matches: pins}
	}
}

// isStableIDForm reports whether id is written in one of the stable forms, as
// opposed to a current-boot numeric address. It decides which parser runs, so it
// keys on the cheapest thing that separates the forms: a "usb:" prefix, or a
// "hw:" prefix that also contains '=' (the alsa-lib "hw:CARD=..." form, versus a
// numeric "hw:1,0"). A malformed stable id must reach the stable parser and be
// reported as such, not fall through to the numeric one and be misreported.
func isStableIDForm(id string) bool {
	if strings.HasPrefix(id, "usb:") {
		return true
	}
	// "hw:CARD=Loopback,DEV=1" versus "hw:1,0": only the alsa-lib form has '='.
	return strings.HasPrefix(id, "hw:") && strings.Contains(id, "=")
}

// canonicalStableID parses a stable id and renders it back in canonical
// spelling, so an id that differs only cosmetically still compares equal to the
// ids Devices generates. It normalizes vid/pid to lowercase hex, the CARD and
// DEV keys to their canonical case, percent-escapes to uppercase hex digits, and
// an omitted DEV to ",DEV=0", and it re-escapes the value through the same
// builders Devices uses, so any of those variations resolves.
func canonicalStableID(id string) (string, error) {
	if strings.HasPrefix(id, "usb:") {
		return canonicalUSBID(id)
	}
	return canonicalCardID(id)
}

// canonicalUSBID parses "usb:<vid>:<pid>:{s|p}=<value>:if=<n>,<dev>". The value
// is delimited by the LAST ":if=" rather than by splitting on every ':', so a
// port value may carry the raw colons of a PCI address.
func canonicalUSBID(id string) (string, error) {
	bad := func(err error) (string, error) { return "", &BadDeviceError{Value: id, Err: err} }

	rest := strings.TrimPrefix(id, "usb:")
	vid, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return bad(errors.New("missing ':' after vendor id"))
	}
	pid, rest, ok := strings.Cut(rest, ":")
	if !ok {
		return bad(errors.New("missing ':' after product id"))
	}
	if !isHex4(vid) || !isHex4(pid) {
		return bad(fmt.Errorf("vendor and product id must each be four hex digits, got %q and %q", vid, pid))
	}
	i := strings.LastIndex(rest, ":if=")
	if i < 0 {
		return bad(errors.New(`missing ":if=" interface marker`))
	}
	kv, tail := rest[:i], rest[i+len(":if="):]
	kind, raw, ok := strings.Cut(kv, "=")
	if !ok || (kind != "s" && kind != "p") {
		return bad(fmt.Errorf(`selector must be "s=" (serial) or "p=" (port), got %q`, kv))
	}
	value, err := unescapeIDField(raw)
	if err != nil {
		return bad(err)
	}
	if value == "" {
		return bad(errors.New("empty selector value"))
	}
	ifStr, devStr, hasComma := strings.Cut(tail, ",")
	ifNum, err := atoiNonNeg(ifStr)
	if err != nil {
		return bad(fmt.Errorf("interface number: %w", err))
	}
	device := 0
	if hasComma {
		if device, err = atoiNonNeg(devStr); err != nil {
			return bad(fmt.Errorf("device number: %w", err))
		}
	}
	// Re-render through the same builders Devices uses, so the canonical spelling
	// can never drift from the generated one. The parsed value is already
	// unescaped; each builder re-applies the escaping for its own kind, which also
	// fixes the port form (the old code escaped the value as a serial and then
	// discarded that result for a port id).
	u := &USBInfo{VendorID: strings.ToLower(vid), ProductID: strings.ToLower(pid), Interface: ifNum}
	if kind == "s" {
		u.Serial = value
		return usbSerialID(u, device), nil
	}
	u.Port = value
	return usbPortID(u, device), nil
}

// canonicalCardID parses the alsa-lib "hw:CARD=<name>[,DEV=<n>]" form, with DEV
// defaulting to 0 exactly as alsa-lib does.
func canonicalCardID(id string) (string, error) {
	bad := func(err error) (string, error) { return "", &BadDeviceError{Value: id, Err: err} }

	rest := strings.TrimPrefix(id, "hw:")
	cardPart, devPart, hasComma := strings.Cut(rest, ",")
	key, name, ok := strings.Cut(cardPart, "=")
	if !ok || !strings.EqualFold(key, "CARD") || name == "" {
		return bad(errors.New(`expected "CARD=<name>"`))
	}
	device := 0
	if hasComma {
		dkey, dval, ok := strings.Cut(devPart, "=")
		if !ok || !strings.EqualFold(dkey, "DEV") {
			return bad(errors.New(`expected "DEV=<n>"`))
		}
		var err error
		if device, err = atoiNonNeg(dval); err != nil {
			return bad(fmt.Errorf("device number: %w", err))
		}
	}
	return cardFormID(name, device), nil
}

// parseNumericDeviceID accepts the current-boot forms "hw:card,device",
// "card,device", and "hw:card" (device defaulting to 0).
func parseNumericDeviceID(s string) (card, device int, err error) {
	orig := s
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "hw:")
	cardStr, devStr, hasComma := strings.Cut(s, ",")
	if card, err = atoiNonNeg(strings.TrimSpace(cardStr)); err != nil {
		return 0, 0, &BadDeviceError{Value: orig, Err: fmt.Errorf("card number: %w", err)}
	}
	if hasComma {
		if device, err = atoiNonNeg(strings.TrimSpace(devStr)); err != nil {
			return 0, 0, &BadDeviceError{Value: orig, Err: fmt.Errorf("device number: %w", err)}
		}
	}
	return card, device, nil
}

func atoiNonNeg(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("negative value %d", n)
	}
	return n, nil
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := range len(s) {
		if _, ok := hexVal(s[i]); !ok {
			return false
		}
	}
	return true
}

// verifyCardIdentity re-reads card's identity after its PCM has been opened and
// reports whether it still matches want, the stable id that resolved to it.
//
// This closes the window between resolving an id to a card index and opening
// that index: in between, the card could be unplugged and a different one take
// the index. Re-reading after the fd exists makes the result independent of
// what the kernel does with an fd held across an unplug, because a mismatch is
// detected either way.
func verifyCardIdentity(card, device int, want string) error {
	if want == "" {
		return nil
	}
	ci := readCardIdent(sysRoot, card)
	id, stable := stableID(ci, card, device)
	if !stable {
		// The card we opened can no longer be identified, so it cannot be shown
		// to be the one that was asked for. Treat that as the device being gone
		// rather than assuming the best.
		return ErrDeviceGone
	}
	if id == want {
		return nil
	}
	// The port form is an equally valid name for the same hardware, so a
	// caller who pinned a port must not be failed here.
	if pid := usbPortID(ci.USB, device); pid != "" && pid == want {
		return nil
	}
	return ErrDeviceGone
}
