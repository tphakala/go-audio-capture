package capture

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrClosed is returned by Stream.Read once the stream has been closed, and by
// every method of a Controls handle once it has been closed.
var ErrClosed = errors.New("capture: closed")

// ErrExclusiveNotAllowed reports that the device cannot be opened for exclusive
// capture because exclusive access is disabled for it (Windows: "Allow
// applications to take exclusive control of this device" is unchecked).
var ErrExclusiveNotAllowed = errors.New("capture: exclusive access disabled for this device")

// ErrDeviceInUse reports that the device is held exclusively by another
// application. On Linux, a busy card that is no longer the unit a stable id
// resolved to is reported as ErrDeviceGone instead.
var ErrDeviceInUse = errors.New("capture: device is in use by another application")

// ErrDeviceGone reports that the device disappeared (unplugged, disabled, or
// otherwise invalidated). Open, Start, and Read all return it when the device is
// missing or removed. On Linux, Read also returns it at the recovery cap when
// one PVERSION probe finds the device gone, and SupportedRates and
// SupportedRatesVerified return it for a query against a device that is gone or
// was removed mid-query. Resolve returns it wrapped in a *DeviceNotFoundError
// (which unwraps to it) when an id matches nothing present. A caller can
// therefore retire the device with errors.Is(err, ErrDeviceGone) at any point
// from resolution through the stream lifecycle. On Linux, OpenControls and the
// Controls methods return it when the card behind the control device is gone or
// is no longer the unit the DeviceInfo named. On Windows a Read parked while
// the endpoint is invalidated is not woken yet, so it may not return until
// Close.
var ErrDeviceGone = errors.New("capture: device is gone")

// ErrDeviceStalled reports that the device stopped delivering audio while a
// PVERSION probe did not find it gone (a device the probe finds gone is
// reported as ErrDeviceGone instead; an answered probe does not prove the
// device is healthy): Stream.Read on Linux returns it (wrapped in a
// *StallError) when a read stall repeats after a restart, or when recovery
// repeats without any frames being delivered, and (wrapped in a *ShortfallError)
// when the device delivers markedly fewer frames than its rate over a window of
// wall-clock time. The stream is unusable; Close it and Open a new one. Windows
// does not return it yet.
var ErrDeviceStalled = errors.New("capture: device stopped delivering audio")

// ErrCapabilitiesUnsupported reports that a device capability feature is not
// implemented on this platform (currently Linux/ALSA only): capability queries
// such as SupportedRates, and hardware controls (OpenControls and the Controls
// methods). Callers should fall back to a static rate list, or to leaving the
// gain alone. It is about the platform: on Linux a device that merely lacks a
// control reports ErrControlNotFound instead.
var ErrCapabilitiesUnsupported = errors.New("capture: not supported on this platform")

// ErrControlNotFound reports that a hardware control element does not exist on
// the device: no element has the requested id, or no element matches the capture
// volume rule. It is matched with errors.Is on *ControlNotFoundError.
var ErrControlNotFound = errors.New("capture: no such control")

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
// Min/Max, BadFormatError has Rate/Channels/Format and the accepted channel
// range, GeometryError has the period geometry and the driver's error,
// AmbiguousDeviceError has Matches). Err is nil only for a value built without a reason.
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

// StallError is the concrete error behind ErrDeviceStalled. Recoveries is the
// number of recoveries attempted in the failing Read, none of which delivered
// frames; Err is the last READI_FRAMES errno (EIO for a timeout, EPIPE for an
// overrun, ESTRPIPE for a suspend). It unwraps to both ErrDeviceStalled and Err.
type StallError struct {
	Recoveries int
	Err        error
}

func (e *StallError) Error() string {
	return fmt.Sprintf("capture: device stalled: no audio after %d recovery attempt(s) (READI_FRAMES: %v)", e.Recoveries, e.Err)
}

// Unwrap reports ErrDeviceStalled and the last errno, so errors.Is matches both.
func (e *StallError) Unwrap() []error { return []error{ErrDeviceStalled, e.Err} }

// ShortfallError is returned by Stream.Read on Linux when the device delivered
// markedly fewer frames than its sample rate implies over a window of wall-clock
// time, with no overrun or other error to show for it. Some period geometries
// make a driver's hardware pointer itself advance slower than real time, so the
// reader keeps up with a pointer that is already short and the kernel reports
// nothing. Rate is the negotiated sample rate. Window is the span of wall-clock
// time that was measured (at least 2 s, and 20 times the buffer duration for a
// large buffer). Expected is the number of frames Window and Rate imply, and
// Delivered is the number of frames Read returned in it. The check allows for the
// frames that sit in the buffer and a 10% tolerance, so a stream is flagged only
// when it fell short by more than the tolerance plus one buffer.
//
// It unwraps to ErrDeviceStalled, so a caller that closes and reopens on a stall
// handles it unchanged. Reopening with a larger PeriodFrames or Periods is the
// remedy when the device keeps producing it.
type ShortfallError struct {
	Rate      int
	Window    time.Duration
	Expected  int64
	Delivered int64
}

func (e *ShortfallError) Error() string {
	return fmt.Sprintf("capture: device delivered %d of %d expected frames at %d Hz over %s", e.Delivered, e.Expected, e.Rate, e.Window)
}

// Unwrap reports ErrDeviceStalled, so errors.Is matches it.
func (e *ShortfallError) Unwrap() error { return ErrDeviceStalled }

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
//
// MinChannels and MaxChannels are the channel counts the device accepts in
// Format, taken from the driver's HW_REFINE bounds. They are bounds only: a
// device that takes 1, 2 or 8 channels reports 1..8, and a count inside the
// range can still be refused. Both are 0 when the format is unsupported at any
// channel count, or when the backend cannot tell (Windows); Error() then omits
// the range.
type BadFormatError struct {
	Rate        int
	Channels    int
	Format      Format
	MinChannels int
	MaxChannels int
}

func (e *BadFormatError) Error() string {
	var msg string
	if e.Rate == 0 {
		msg = fmt.Sprintf("capture: format %d ch / %s not supported", e.Channels, e.Format)
	} else {
		msg = fmt.Sprintf("capture: format %d ch / %s @ %d Hz not supported", e.Channels, e.Format, e.Rate)
	}
	if e.MaxChannels > 0 {
		msg += fmt.Sprintf(" (device accepts %d..%d channels in %s)", e.MinChannels, e.MaxChannels, e.Format)
	}
	return msg
}

// GeometryError reports that the device refused every period size and count
// near the requested ones, at a rate, channel count and format that the device
// advertised. Rate, PeriodFrames and Periods are the values the commit was
// attempted with; when no nearby value could be pinned they are the requested
// ones after defaults and, on Linux, the geometry floor, except PeriodFrames,
// which keeps a period size already chosen when only the period count failed.
// Err is the driver's error (on Linux HW_PARAMS for a refused commit, HW_REFINE
// when no nearby value could be pinned).
//
// Some USB devices only reveal at the commit that they cannot deliver a rate they
// advertised, and then fail here rather than with *BadRateError. If trying other
// PeriodFrames and Periods does not help, pick a rate from SupportedRatesVerified,
// which lists the rates that commit at the default geometry.
type GeometryError struct {
	Rate         int
	PeriodFrames int
	Periods      int
	Err          error
}

func (e *GeometryError) Error() string {
	return fmt.Sprintf("capture: device refused period geometry at %d Hz (%d frames x %d periods): %v", e.Rate, e.PeriodFrames, e.Periods, e.Err)
}

// Unwrap returns the driver's error so errors.Is can match its errno.
func (e *GeometryError) Unwrap() error { return e.Err }

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

// ControlNotFoundError reports a control element lookup that found nothing. For
// a lookup by id, ID is the element asked for (the kernel reports "no element
// with that id", or the name cannot belong to any element: over 44 bytes or
// containing a NUL). With Pattern set, the capture volume helper found no element
// matching its rule (an INTEGER mixer element, readable and writable and active,
// named "Capture Volume" or ending in " Capture Volume"). It unwraps to
// ErrControlNotFound.
type ControlNotFoundError struct {
	ID      ControlID
	Pattern bool
}

func (e *ControlNotFoundError) Error() string {
	if e.Pattern {
		return "capture: no capture volume control (want an active, writable INTEGER mixer element named \"Capture Volume\" or ending in \" Capture Volume\")"
	}
	return fmt.Sprintf("capture: no such control %s", e.ID)
}

// Unwrap reports ErrControlNotFound, so errors.Is matches it.
func (e *ControlNotFoundError) Unwrap() error { return ErrControlNotFound }

// AmbiguousControlError reports that the capture volume helper found more than
// one element matching its rule (for example an HDA codec with several "Capture
// Volume" indices). The helper never picks one: pass one of Matches to Info, Get
// or Set.
type AmbiguousControlError struct {
	Matches []ControlID
}

func (e *AmbiguousControlError) Error() string {
	names := make([]string, 0, len(e.Matches))
	for _, m := range e.Matches {
		names = append(names, m.String())
	}
	return fmt.Sprintf("capture: %d controls match the capture volume rule (%s); pick one by id", len(e.Matches), strings.Join(names, ", "))
}

// Op values of ControlAccessError.
const (
	accessOpRead  = "read"
	accessOpWrite = "write"
)

// ControlAccessError reports that a control cannot be read or written: the
// element lacks the access bit or is inactive (checked before any write is
// issued), or the kernel refused with EPERM. Locked is set when a write was
// refused by the kernel although the element is writable, which means another
// application holds its write lock.
type ControlAccessError struct {
	ID     ControlID
	Op     string // "read" or "write"
	Locked bool
}

func (e *ControlAccessError) Error() string {
	if e.Locked {
		return fmt.Sprintf("capture: control %s: write lock held by another application", e.ID)
	}
	return fmt.Sprintf("capture: control %s: %s not permitted (access bits or inactive)", e.ID, e.Op)
}

// ControlValueError reports a value the library refused to write, or that the
// driver rejected. Values is what was asked for; Min, Max and Step describe the
// element where they apply. Apart from a driver rejection ("rejected by the
// driver", an EINVAL from the write) no ioctl was issued.
type ControlValueError struct {
	ID       ControlID
	Values   []int64
	Min, Max int64
	Step     int64
	Reason   string
}

func (e *ControlValueError) Error() string {
	return fmt.Sprintf("capture: control %s: invalid value %v: %s (range %d..%d, step %d)", e.ID, e.Values, e.Reason, e.Min, e.Max, e.Step)
}
