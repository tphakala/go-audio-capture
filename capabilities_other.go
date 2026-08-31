//go:build !linux

package capture

// SupportedRates is implemented only on Linux (ALSA). On every other platform it
// returns ErrCapabilitiesUnsupported so callers can fall back to a static rate
// list. The Windows WASAPI backend negotiates format at Open time and has no
// equivalent standalone rate-enumeration path yet.
func SupportedRates(device string, channels int, format Format) (RateSupport, error) {
	_ = device
	_ = channels
	_ = format
	return RateSupport{}, ErrCapabilitiesUnsupported
}

// SupportedRatesVerified is implemented only on Linux (ALSA). On every other
// platform it returns ErrCapabilitiesUnsupported so callers can fall back to a
// static rate list, matching SupportedRates.
func SupportedRatesVerified(device string, channels int, format Format) (RateSupport, error) {
	_ = device
	_ = channels
	_ = format
	return RateSupport{}, ErrCapabilitiesUnsupported
}
