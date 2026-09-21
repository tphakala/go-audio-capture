package capture

import "fmt"

// Format is a PCM sample format: signed 16-, 24-, or 32-bit little-endian
// integer, or 32-bit IEEE-754 little-endian float. As everywhere else in this
// library, the requested format is negotiated with the hardware exactly or Open
// fails; there is no silent sample-format conversion.
type Format int

const (
	// FormatS16LE is signed 16-bit little-endian integer PCM.
	FormatS16LE Format = iota + 1
	// FormatS32LE is signed 32-bit little-endian integer PCM.
	FormatS32LE
	// FormatF32LE is 32-bit IEEE-754 little-endian float PCM. It will be the
	// native capture format on the planned macOS CoreAudio backend, and is
	// accepted on Linux/Windows when the endpoint itself supports float (many do
	// not, in which case Open fails with a typed *BadFormatError rather than
	// converting, on both Linux and Windows).
	FormatF32LE
	// FormatS243LE is signed 24-bit little-endian integer PCM packed in 3 bytes
	// (ALSA SNDRV_PCM_FORMAT_S24_3LE), the native capture format of USB Audio
	// Class microphones that offer only 24-bit. It is Linux-only: the Windows
	// WASAPI backend rejects it with a *ConfigError, because exclusive WASAPI
	// exposes 24-bit as 24-in-32, not this 3-byte-packed layout. Like every
	// format here it is passthrough: Read delivers the raw 3-byte little-endian
	// samples and Negotiated reports FormatS243LE; the library never widens or
	// converts them.
	FormatS243LE
	// FormatS24LE is signed 24-bit little-endian integer PCM: 24 valid bits in the
	// low 3 bytes of a 4-byte little-endian word (ALSA SNDRV_PCM_FORMAT_S24_LE),
	// the 24-bit layout many professional USB and PCI interfaces deliver.
	// BytesPerSample is 4, not 3: the sample occupies a full 32-bit word carrying
	// 24 valid bits. Because this is passthrough, the most significant byte is
	// whatever the device wrote there (zero padding on some devices, sign
	// extension on others), so a consumer must not assume the four bytes already
	// form a sign-extended int32. It is Linux-only for now; the Windows WASAPI
	// backend rejects it with a *ConfigError, since a 24-valid-bits-in-32 endpoint
	// negotiation is not yet implemented there. Read delivers the raw 4-byte words
	// and Negotiated reports FormatS24LE; the library never masks, shifts, or
	// converts them.
	FormatS24LE
)

// BytesPerSample returns the size of one sample in bytes, or 0 for an unknown
// format.
func (f Format) BytesPerSample() int {
	switch f {
	case FormatS16LE:
		return 2
	case FormatS243LE:
		return 3
	case FormatS24LE, FormatS32LE, FormatF32LE:
		return 4
	default:
		return 0
	}
}

// IsFloat reports whether the format holds IEEE-754 floating-point samples
// rather than signed integers.
func (f Format) IsFloat() bool { return f == FormatF32LE }

// String returns the short format token (matching the gac-rec -f flag).
func (f Format) String() string {
	switch f {
	case FormatS16LE:
		return "s16"
	case FormatS24LE:
		return "s24_le"
	case FormatS243LE:
		return "s24_3le"
	case FormatS32LE:
		return "s32"
	case FormatF32LE:
		return "f32"
	default:
		return "unknown"
	}
}

// ParseFormat maps a short format token ("s16", "s24_le", "s24_3le", "s32",
// "f32") to a Format. It is the inverse of Format.String and returns a
// *ConfigError for an unknown token.
func ParseFormat(s string) (Format, error) {
	switch s {
	case "s16":
		return FormatS16LE, nil
	case "s24_le":
		return FormatS24LE, nil
	case "s24_3le":
		return FormatS243LE, nil
	case "s32":
		return FormatS32LE, nil
	case "f32":
		return FormatF32LE, nil
	default:
		return 0, &ConfigError{Field: fieldFormat, Reason: fmt.Sprintf("unknown format %q (want s16, s24_le, s24_3le, s32, or f32)", s)}
	}
}

// DeviceInfo identifies a capture-capable PCM device.
//
// ID is a stable, platform-specific identifier that Config.Device accepts
// directly, and it is the field to persist. When IDStable is true it survives a
// reboot, a replug, and another device being added or removed; when IDStable is
// false it is only a current-boot address that must not be persisted (see the
// fallback below). On Windows it is the WASAPI endpoint-id string. On Linux it
// is derived from sysfs and takes one of three forms:
//
//	usb:<vid>:<pid>:s=<serial>:if=<n>,<dev>          USB card with a serial
//	usb:<vid>:<pid>:p=<controller>-<devpath>:if=<n>,<dev>  USB card without one
//	hw:CARD=<card id>,DEV=<dev>                      non-USB card (alsa-lib syntax)
//
// The serial form names the unit and follows it to any port. The port form names
// the physical port, so it changes if a serial-less device is moved to a
// different port (a move a serial-bearing device rides out on its serial form).
// Bytes outside [A-Za-z0-9._-] are percent-escaped in both the serial and the
// port value; the port keeps ':' raw so a PCI controller address stays readable.
//
// ID falls back to the current-boot "hw:card,device" string with IDStable false
// whenever no stable form can be built: sysfs cannot be read (a container with a
// partial /sys), or a USB card reports no serial and no derivable port, or a
// non-USB card has no kernel card id. Such an ID must not be persisted, because
// the card index follows kernel probe order and changes across reboots and
// replugs.
//
// HWAddr is the device's current-boot address, for display, logs, and passing
// to other tools: on Linux the "hw:card,device" string that arecord takes, on
// Windows the endpoint id (the same value as ID, since Windows has no separate
// unstable address). On Linux it is not stable across reboots or replugs and
// must not be persisted; persist ID instead.
//
// PortID is the port-form id for a USB card whose physical port could be
// derived, and empty otherwise (a non-USB card, or a USB card with no derivable
// port). It names the physical port rather than the unit, so use it to pin one
// of two units that report the same serial, which ID cannot distinguish.
//
// CardID is the kernel card id ("Loopback", "AMS24"), empty for a card with no
// kernel id or whose sysfs could not be read. USB carries the USB identity of a
// USB card and is the zero value otherwise; IsUSB reports which. It is a value
// rather than a pointer so that DeviceInfo stays comparable with ==, which
// compares two pointers by address and would report two reads of the same device
// as different.
//
// Card, Device, CardID, PortID, and USB are populated on Linux only.
type DeviceInfo struct {
	ID       string
	Card     int
	Device   int
	Name     string
	HWAddr   string
	IDStable bool
	PortID   string
	CardID   string
	USB      USBInfo
}

// IsUSB reports whether the device sits behind USB, in which case USB carries at
// least its vendor and product; USB.Serial and USB.Port are filled in when the
// device reports them, but either can be empty.
//
//nolint:gocritic // hugeParam: a value receiver keeps IsUSB callable on a non-addressable DeviceInfo, such as a map element.
func (d DeviceInfo) IsUSB() bool { return d.USB.VendorID != "" }

// USBInfo is the USB identity behind a Linux capture card, as read from sysfs.
// VendorID and ProductID are the four-digit lowercase hex idVendor and
// idProduct. Serial is the device serial, empty when the device reports none.
// Port is "<controller>-<devpath>", the physical attachment point: the host
// controller's device name (a PCI address such as "0000:00:14.0", or a platform
// name) joined to the USB devpath ("3", "1.4.2"). The USB bus number is
// deliberately not used, because bus numbers follow controller probe order.
// Interface is the bInterfaceNumber of the audio function, which distinguishes
// the functions of a composite device.
type USBInfo struct {
	VendorID  string
	ProductID string
	Serial    string
	Port      string
	Interface int
}

// RateSupport reports which sample rates a device accepts for a given channel
// count and format, as discovered by SupportedRates. Rates lists the accepted
// standard rates in ascending order. Min and Max bound the raw hardware rate
// window (from a single HW_REFINE): for a device with continuous rate support
// they describe the whole range, so a caller may pick a value not in Rates that
// still falls within [Min, Max].
type RateSupport struct {
	Rates    []int
	Min, Max int
}

// Config requests a capture configuration. Rate is honored exactly or Open
// fails with *BadRateError: there is no silent resampling. PeriodFrames and
// Periods default to a 20 ms period (Rate/50) and 4 periods when left zero on
// Linux; on Windows (WASAPI exclusive mode) the endpoint dictates the buffer
// period, so both fields are ignored and Negotiated reports the actual period.
type Config struct {
	// Device names the capture device. On Linux it accepts a DeviceInfo.ID in
	// any of its stable forms ("usb:...", "hw:CARD=name,DEV=0"), which is what a
	// caller should persist, and also the current-boot "hw:card,device" (or
	// "card,device", or "hw:card") for interactive use. A stable id is resolved
	// to a card index on every call, so it follows the hardware across reboots
	// and replugs. On Windows it is the WASAPI endpoint id, or ""/"default".
	Device       string
	Rate         int // requested sample rate in Hz
	Channels     int // 1 or 2
	Format       Format
	PeriodFrames int // frames per period; Linux: 0 => Rate/50 (20 ms); ignored on Windows
	Periods      int // periods per buffer; Linux: 0 => 4; ignored on Windows
}
