//go:build windows

package capture

import "github.com/tphakala/go-audio-capture/internal/wasapi"

// Devices enumerates active capture endpoints via WASAPI. On Windows the
// endpoint-id string is the stable identifier (DeviceInfo.ID); Card and Device
// are Linux-only and remain 0. The ID is accepted verbatim by Config.Device.
// The list is empty (not an error) on a machine with no capture endpoints.
func Devices() ([]DeviceInfo, error) {
	eps, err := wasapi.Enumerate()
	if err != nil {
		return nil, err
	}
	devs := make([]DeviceInfo, 0, len(eps))
	for _, ep := range eps {
		devs = append(devs, DeviceInfo{
			ID:   ep.ID,
			Name: ep.Name,
		})
	}
	return devs, nil
}
