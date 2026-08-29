package capture

import (
	"errors"
	"fmt"
)

// ErrClosed is returned by Stream.Read once the stream has been closed.
var ErrClosed = errors.New("capture: stream is closed")

// BadDeviceError reports a device ID that is not a valid "hw:card,device"
// (or "card,device", or "hw:card") string.
type BadDeviceError struct {
	Value string
}

func (e *BadDeviceError) Error() string {
	return fmt.Sprintf("capture: invalid device id %q (want hw:card,device)", e.Value)
}

// BadRateError reports that the hardware does not support the exact requested
// sample rate; Min and Max bound the supported range. It is returned instead of
// silently negotiating a different rate.
type BadRateError struct {
	Requested int
	Min       int
	Max       int
}

func (e *BadRateError) Error() string {
	return fmt.Sprintf("capture: sample rate %d Hz not supported (hardware range %d..%d Hz)", e.Requested, e.Min, e.Max)
}

// ConfigError reports an invalid field in a Config passed to Open.
type ConfigError struct {
	Field  string
	Reason string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("capture: invalid config: %s: %s", e.Field, e.Reason)
}
