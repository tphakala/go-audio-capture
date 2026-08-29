//go:build windows

package wasapi

import (
	"errors"
	"fmt"
)

// Sentinel errors for the distinct exclusive-mode failure modes. The public
// capture layer maps these to its own exported errors so callers never import
// internal/wasapi. They are surfaced through hresultError.Unwrap, so
// errors.Is(err, ErrExclusiveNotAllowed) works on a wrapped HRESULT error.
var (
	// ErrExclusiveNotAllowed reports that "Allow applications to take exclusive
	// control of this device" is disabled for the endpoint.
	ErrExclusiveNotAllowed = errors.New("wasapi: exclusive access disabled for this endpoint")
	// ErrDeviceInUse reports that another application holds the endpoint in
	// exclusive mode.
	ErrDeviceInUse = errors.New("wasapi: endpoint is in use by another application")
	// ErrDeviceGone reports that the endpoint was invalidated (unplugged,
	// disabled, or the device was reconfigured).
	ErrDeviceGone = errors.New("wasapi: endpoint has been invalidated")
)

// BadRateError reports that the endpoint does not support the exact requested
// sample rate in exclusive mode. Min and Max bound the supported range when it
// can be determined from the endpoint; when it cannot, both are 0 and the error
// text notes the exclusive-mode rejection. Returned instead of resampling.
type BadRateError struct {
	Requested int
	Min       int
	Max       int
}

func (e *BadRateError) Error() string {
	if e.Min == 0 && e.Max == 0 {
		return fmt.Sprintf("wasapi: sample rate %d Hz not supported in exclusive mode", e.Requested)
	}
	return fmt.Sprintf("wasapi: sample rate %d Hz not supported (endpoint range %d..%d Hz)", e.Requested, e.Min, e.Max)
}

// BadFormatError reports that the endpoint does not support the requested
// channel count / sample format combination in exclusive mode (a distinct
// failure from an unsupported rate: exclusive endpoints commonly accept only
// specific channel counts and bit depths, e.g. stereo S16 but not mono). It is
// returned instead of up/down-mixing or converting the sample format.
type BadFormatError struct {
	Rate     int
	Channels int
	Bits     int
}

func (e *BadFormatError) Error() string {
	return fmt.Sprintf("wasapi: format %d ch / %d-bit @ %d Hz not supported in exclusive mode", e.Channels, e.Bits, e.Rate)
}

// hresultError wraps a failing COM call with the call name and its HRESULT, so
// callers never see a bare "unspecified error". It unwraps to a sentinel for the
// classifiable exclusive-mode failures.
type hresultError struct {
	Op string
	HR hresult
}

func (e *hresultError) Error() string {
	return fmt.Sprintf("wasapi: %s: %s (0x%08X)", e.Op, e.HR.name(), uint32(e.HR))
}

func (e *hresultError) Unwrap() error {
	switch e.HR {
	case hrExclusiveNotAllowed:
		return ErrExclusiveNotAllowed
	case hrDeviceInUse:
		return ErrDeviceInUse
	case hrDeviceInvalidated:
		return ErrDeviceGone
	default:
		return nil
	}
}

// name returns a short symbolic name for a known HRESULT, or "HRESULT".
func (h hresult) name() string {
	switch h {
	case sOK:
		return "S_OK"
	case sFALSE:
		return "S_FALSE"
	case hrUnsupportedFormat:
		return "AUDCLNT_E_UNSUPPORTED_FORMAT"
	case hrExclusiveNotAllowed:
		return "AUDCLNT_E_EXCLUSIVE_MODE_NOT_ALLOWED"
	case hrDeviceInUse:
		return "AUDCLNT_E_DEVICE_IN_USE"
	case hrBufferSizeNotAligned:
		return "AUDCLNT_E_BUFFER_SIZE_NOT_ALIGNED"
	case hrDeviceInvalidated:
		return "AUDCLNT_E_DEVICE_INVALIDATED"
	case hrNotInitialized:
		return "AUDCLNT_E_NOT_INITIALIZED"
	case hrRPCChangedMode:
		return "RPC_E_CHANGED_MODE"
	default:
		return "HRESULT"
	}
}
