package capture

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrClosed is returned by Stream.Read once the stream has been closed.
var ErrClosed = errors.New("capture: stream is closed")

// ErrExclusiveNotAllowed reports that the device cannot be opened for exclusive
// capture because exclusive access is disabled for it (Windows: "Allow
// applications to take exclusive control of this device" is unchecked).
var ErrExclusiveNotAllowed = errors.New("capture: exclusive access disabled for this device")

// ErrDeviceInUse reports that the device is held exclusively by another
// application.
var ErrDeviceInUse = errors.New("capture: device is in use by another application")

// ErrDeviceGone reports that the device disappeared (unplugged, disabled, or
// otherwise invalidated). Open, Start, and Read all return it when the device is
// missing or removed; SupportedRates and SupportedRatesVerified return it for a
// query against a device that is gone; and Resolve returns it wrapped in a
// *DeviceNotFoundError (which unwraps to it) when an id matches nothing present.
// A caller can therefore retire the device with errors.Is(err, ErrDeviceGone) at
// any point from resolution through the stream lifecycle.
var ErrDeviceGone = errors.New("capture: device is gone")

// ErrCapabilitiesUnsupported reports that device capability queries such as
// SupportedRates are not implemented on this platform (currently Linux/ALSA
// only). Callers should fall back to a static rate list.
var ErrCapabilitiesUnsupported = errors.New("capture: capability query not supported on this platform")

// BadDeviceError reports a device id that is not in any accepted form. On Linux
// those are the stable forms "usb:vid:pid:s=serial:if=n,dev",
// "usb:vid:pid:p=port:if=n,dev" and "hw:CARD=name,DEV=dev", plus the
// current-boot "hw:card,device" (or "card,device", or "hw:card"). The id is
// malformed, as opposed to well-formed but not currently present, which is
// *DeviceNotFoundError.
//
// Err names the specific reason the id was rejected (which separator was
// missing, which field was not a number, a malformed percent-escape, and so
// on); BadDeviceError unwraps to it. It matches the other typed errors in this
// file, each of which carries the detail that produced it (BadRateError has
// Min/Max, BadFormatError has Rate/Channels/Format, AmbiguousDeviceError has
// Matches). Err is nil only for a value built without a reason.
type BadDeviceError struct {
	Value string
	Err   error
}

func (e *BadDeviceError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("capture: invalid device id %q: %v (want hw:card,device, hw:CARD=name,DEV=dev, or usb:vid:pid:...)", e.Value, e.Err)
	}
	return fmt.Sprintf("capture: invalid device id %q (want hw:card,device, hw:CARD=name,DEV=dev, or usb:vid:pid:...)", e.Value)
}

// Unwrap reports the specific parse failure so errors.Is and errors.As can
// reach it.
func (e *BadDeviceError) Unwrap() error { return e.Err }

// DeviceNotFoundError reports a well-formed device id that matches no device
// currently present: the hardware it names is unplugged, powered off, or was
// never attached to this machine. It unwraps to ErrDeviceGone, so a caller that
// already retires a device with errors.Is(err, ErrDeviceGone) needs no change,
// while one that wants to tell a resolve-time absence (an id that resolved to
// nothing) from a runtime invalidation (a device lost mid-stream) can use
// errors.As.
type DeviceNotFoundError struct {
	ID string
}

func (e *DeviceNotFoundError) Error() string {
	return fmt.Sprintf("capture: no device matches id %q", e.ID)
}

// Unwrap reports ErrDeviceGone so errors.Is(err, ErrDeviceGone) holds.
func (e *DeviceNotFoundError) Unwrap() error { return ErrDeviceGone }

// AmbiguousDeviceError reports a device id that matches more than one device
// present right now, which happens when two identical units report the same
// serial. Matches carries the PortID of each matching device (falling back to
// its current-boot "hw:card,device" address for a match that has no PortID), so
// the caller can act on the remedy directly. The entries follow the order
// Devices returns, which is by ascending card then device number, not lexical
// order. The library never picks one: opening a coin-flip device is the very
// failure a stable id exists to prevent. Resolve the ambiguity by passing one
// of the listed ids as Config.Device: a PortID pins the unit in that physical
// port and stays correct across reboots, while a fallback hw address identifies
// the unit only for the current boot and should not be persisted.
type AmbiguousDeviceError struct {
	ID      string
	Matches []string
}

func (e *AmbiguousDeviceError) Error() string {
	if len(e.Matches) == 0 {
		return fmt.Sprintf("capture: device id %q matches multiple devices; pin one of them", e.ID)
	}
	// Each match is quoted because every id form carries a comma before the
	// device number, so a bare comma-joined list cannot be split back apart by
	// eye or by a script.
	quoted := make([]string, 0, len(e.Matches))
	for _, m := range e.Matches {
		quoted = append(quoted, strconv.Quote(m))
	}
	// "a listed id" rather than "its PortID": a match whose port could not be
	// derived is listed by its hw address instead, which is not a PortID.
	return fmt.Sprintf("capture: device id %q matches %d devices (%s); pin one by a listed id", e.ID, len(e.Matches), strings.Join(quoted, ", "))
}

// BadRateError reports that the hardware does not support the exact requested
// sample rate; Min and Max bound the supported range when it can be determined.
// When it cannot (both are 0, e.g. a Windows exclusive-mode rejection), Error()
// omits the range. It is returned instead of silently negotiating a different rate.
type BadRateError struct {
	Requested int
	Min       int
	Max       int
}

func (e *BadRateError) Error() string {
	if e.Min == 0 && e.Max == 0 {
		return fmt.Sprintf("capture: sample rate %d Hz not supported", e.Requested)
	}
	return fmt.Sprintf("capture: sample rate %d Hz not supported (hardware range %d..%d Hz)", e.Requested, e.Min, e.Max)
}

// BadFormatError reports that the device does not support the requested channel
// count / sample-format combination, distinct from an unsupported rate. Some
// devices (notably in Windows exclusive mode) accept only specific channel
// counts and bit depths; the library returns this rather than up/down-mixing or
// converting the sample format. Rate is the rate in play when the combination
// was rejected, or 0 when the rejection is rate-independent (e.g. a capability
// query that found the channel/format unsupported at any rate), in which case
// Error() omits the rate.
type BadFormatError struct {
	Rate     int
	Channels int
	Format   Format
}

func (e *BadFormatError) Error() string {
	if e.Rate == 0 {
		return fmt.Sprintf("capture: format %d ch / %s not supported", e.Channels, e.Format)
	}
	return fmt.Sprintf("capture: format %d ch / %s @ %d Hz not supported", e.Channels, e.Format, e.Rate)
}

// ConfigError reports an invalid field in a Config passed to Open.
type ConfigError struct {
	Field  string
	Reason string
}

// fieldFormat is the ConfigError.Field token for the sample format. It is named
// once because it is the only field reported from both the shared code and the
// per-platform Open paths, so the token cannot drift and stays a single literal.
const fieldFormat = "format"

func (e *ConfigError) Error() string {
	return fmt.Sprintf("capture: invalid config: %s: %s", e.Field, e.Reason)
}
