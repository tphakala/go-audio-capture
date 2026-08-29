package capture

import (
	"errors"
	"fmt"
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
// otherwise invalidated) during use. Read returns it instead of hanging.
var ErrDeviceGone = errors.New("capture: device is gone")

// BadDeviceError reports a device ID that is not a valid "hw:card,device"
// (or "card,device", or "hw:card") string.
type BadDeviceError struct {
	Value string
}

func (e *BadDeviceError) Error() string {
	return fmt.Sprintf("capture: invalid device id %q (want hw:card,device)", e.Value)
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
// converting the sample format.
type BadFormatError struct {
	Rate     int
	Channels int
	Format   Format
}

func (e *BadFormatError) Error() string {
	return fmt.Sprintf("capture: format %d ch / %s @ %d Hz not supported", e.Channels, e.Format, e.Rate)
}

// ConfigError reports an invalid field in a Config passed to Open.
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("capture: invalid config: %s: %s", e.Field, e.Reason)
}
