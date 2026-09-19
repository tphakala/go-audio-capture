//go:build windows && (amd64 || arm64)

package capture

import (
	"errors"
	"strings"

	"github.com/tphakala/go-audio-capture/internal/wasapi"
)

// Devices enumerates active capture endpoints via WASAPI. On Windows the
// endpoint-id string is the stable identifier (DeviceInfo.ID), so it is also
// reported as HWAddr and IDStable is always true: there is no separate unstable
// address as there is on Linux. Card, Device, CardID, PortID, and USB are
// Linux-only and stay at their zero values. The ID is accepted verbatim by
// Config.Device. The list is empty (not an error) on a machine with no capture
// endpoints.
func Devices() ([]DeviceInfo, error) {
	eps, err := wasapi.Enumerate()
	if err != nil {
		return nil, err
	}
	devs := make([]DeviceInfo, 0, len(eps))
	for _, ep := range eps {
		devs = append(devs, DeviceInfo{
			ID:       ep.ID,
			Name:     ep.Name,
			HWAddr:   ep.ID,
			IDStable: true,
		})
	}
	return devs, nil
}

// Resolve reports which endpoint an id currently names, without opening it. It
// is the Windows half of the cross-platform Resolve: a caller can check that a
// persisted DeviceInfo.ID still names present hardware before committing to a
// capture, and gets *DeviceNotFoundError (which unwraps to ErrDeviceGone) when
// it does not.
//
// An empty id, or "default", resolves to the default capture endpoint, matching
// what Config.Device accepts. That endpoint is chosen by WASAPI by role, so it
// is asked for by name rather than taken from the enumerated list. Any other id
// must name an endpoint that is currently active.
func Resolve(id string) (DeviceInfo, error) {
	// Trim once and match on the trimmed value throughout, so a padded id
	// behaves the same here as it does on Linux.
	trimmed := strings.TrimSpace(id)
	if trimmed == "" || trimmed == "default" {
		ep, err := wasapi.DefaultCaptureEndpoint()
		if err != nil {
			// A not-found default endpoint (no capture device present at all) is
			// the resolve-time "device absent" case this function documents:
			// surface it as *DeviceNotFoundError, which unwraps to ErrDeviceGone,
			// so both errors.Is(err, ErrDeviceGone) and errors.As(&DeviceNotFoundError{})
			// hold as promised. Any other failure is a real COM error, returned as is.
			if errors.Is(err, wasapi.ErrDeviceGone) {
				return DeviceInfo{}, &DeviceNotFoundError{ID: id}
			}
			return DeviceInfo{}, err
		}
		return DeviceInfo{ID: ep.ID, Name: ep.Name, HWAddr: ep.ID, IDStable: true}, nil
	}
	devs, err := Devices()
	if err != nil {
		return DeviceInfo{}, err
	}
	for i := range devs {
		if devs[i].ID == trimmed {
			return devs[i], nil
		}
	}
	return DeviceInfo{}, &DeviceNotFoundError{ID: id}
}
