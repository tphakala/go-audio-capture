//go:build !linux

package capture

// Controls is the hardware control handle. It exists only on Linux (ALSA); on
// every other platform OpenControls returns ErrCapabilitiesUnsupported.
type Controls struct{}

// OpenControls is implemented only on Linux. Elsewhere it returns
// ErrCapabilitiesUnsupported so callers can leave the device gain alone.
//
//nolint:gocritic // hugeParam: DeviceInfo is passed by value like OpenDevice.
func OpenControls(d DeviceInfo) (*Controls, error) {
	_ = d
	return nil, ErrCapabilitiesUnsupported
}

// Close does nothing and returns nil.
func (c *Controls) Close() error { return nil }

// List returns ErrCapabilitiesUnsupported.
func (c *Controls) List() ([]ControlInfo, error) { return nil, ErrCapabilitiesUnsupported }

// Info returns ErrCapabilitiesUnsupported.
func (c *Controls) Info(id ControlID) (ControlInfo, error) {
	_ = id
	return ControlInfo{}, ErrCapabilitiesUnsupported
}

// Get returns ErrCapabilitiesUnsupported.
func (c *Controls) Get(id ControlID) ([]int64, error) {
	_ = id
	return nil, ErrCapabilitiesUnsupported
}

// Set returns ErrCapabilitiesUnsupported.
func (c *Controls) Set(id ControlID, values []int64) error {
	_ = id
	_ = values
	return ErrCapabilitiesUnsupported
}

// CaptureVolume returns ErrCapabilitiesUnsupported.
func (c *Controls) CaptureVolume() (ControlInfo, error) {
	return ControlInfo{}, ErrCapabilitiesUnsupported
}

// SetCaptureVolumePercent returns ErrCapabilitiesUnsupported.
func (c *Controls) SetCaptureVolumePercent(percent float64) (raw int64, err error) {
	_ = percent
	return 0, ErrCapabilitiesUnsupported
}
