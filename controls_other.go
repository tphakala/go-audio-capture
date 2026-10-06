//go:build !linux

package capture

// Controls is the hardware control handle. It exists only on Linux (ALSA); on
// every other platform OpenControls returns ErrCapabilitiesUnsupported.
type Controls struct{}

// OpenControls is implemented only on Linux. Elsewhere it returns
// ErrCapabilitiesUnsupported so callers can leave the device gain alone.
//
//nolint:gocritic // hugeParam: DeviceInfo is passed by value like OpenDevice.
func OpenControls(_ DeviceInfo) (*Controls, error) {
	return nil, ErrCapabilitiesUnsupported
}

// Close does nothing and returns nil.
func (c *Controls) Close() error { return nil }

// List returns ErrCapabilitiesUnsupported.
func (c *Controls) List() ([]ControlInfo, error) { return nil, ErrCapabilitiesUnsupported }

// Info returns ErrCapabilitiesUnsupported.
func (c *Controls) Info(_ ControlID) (ControlInfo, error) {
	return ControlInfo{}, ErrCapabilitiesUnsupported
}

// Get returns ErrCapabilitiesUnsupported.
func (c *Controls) Get(_ ControlID) ([]int64, error) {
	return nil, ErrCapabilitiesUnsupported
}

// Set returns ErrCapabilitiesUnsupported.
func (c *Controls) Set(_ ControlID, _ []int64) error {
	return ErrCapabilitiesUnsupported
}

// CaptureVolume returns ErrCapabilitiesUnsupported.
func (c *Controls) CaptureVolume() (ControlInfo, error) {
	return ControlInfo{}, ErrCapabilitiesUnsupported
}

// SetCaptureVolumePercent returns ErrCapabilitiesUnsupported.
func (c *Controls) SetCaptureVolumePercent(_ float64) (raw int64, err error) {
	return 0, ErrCapabilitiesUnsupported
}
