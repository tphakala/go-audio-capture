//go:build windows

package wasapi

import (
	"sort"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Endpoint identifies a capture endpoint: Name is the human-readable friendly
// name, ID is the opaque WASAPI endpoint-id string that Open accepts verbatim.
type Endpoint struct {
	ID   string
	Name string
}

// propertyKey mirrors PROPERTYKEY (a GUID plus a DWORD property id).
type propertyKey struct {
	fmtid windows.GUID
	pid   uint32
}

// pkeyDeviceFriendlyName is PKEY_Device_FriendlyName.
var pkeyDeviceFriendlyName = propertyKey{
	fmtid: windows.GUID{Data1: 0xA45C254E, Data2: 0xDF1C, Data3: 0x4EFD, Data4: [8]byte{0x80, 0x20, 0x67, 0xD1, 0x46, 0xA8, 0x50, 0xE0}},
	pid:   14,
}

// propVariant is PROPVARIANT (24 bytes on amd64); val holds the pointer union
// member (pwszVal for VT_LPWSTR).
type propVariant struct {
	vt  uint16
	_   [6]byte
	val unsafe.Pointer
	_   [8]byte
}

// vtLPWSTR is VT_LPWSTR: the only PROPVARIANT type whose val union member is a
// wide-string pointer. Any other type must not be dereferenced as one.
const vtLPWSTR = 31

// unknownEndpointName is the friendly-name fallback when the property is
// missing, unreadable, or not a string.
const unknownEndpointName = "Unknown capture endpoint"

// Enumerate returns all ACTIVE capture endpoints, sorted by friendly name. It
// returns an empty slice (not an error) when the machine has no capture
// endpoints, so callers and CI runners without audio hardware behave sanely.
func Enumerate() ([]Endpoint, error) {
	enum, err := createEnumerator()
	if err != nil {
		return nil, err
	}
	defer release(enum)

	coll, err := enumEndpoints(enum, eCapture, deviceStateActive)
	if err != nil {
		return nil, err
	}
	defer release(coll)

	n, err := collectionCount(coll)
	if err != nil {
		return nil, err
	}
	out := make([]Endpoint, 0, n)
	for i := uint32(0); i < n; i++ {
		dev, err := collectionItem(coll, i)
		if err != nil {
			continue
		}
		id, iderr := deviceID(dev)
		if iderr != nil {
			release(dev)
			continue
		}
		out = append(out, Endpoint{ID: id, Name: deviceFriendlyName(dev)})
		release(dev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// resolveDevice returns a device pointer for the given endpoint id, or the
// default capture endpoint when id is "" or "default". The caller must release
// the returned device. It is used by Open.
func resolveDevice(enum unsafe.Pointer, id string) (unsafe.Pointer, error) {
	if id == "" || id == "default" {
		var dev unsafe.Pointer
		r, _, _ := syscall.SyscallN(methodAddr(enum, mGetDefaultAudioEndpoint),
			uintptr(enum), uintptr(eCapture), uintptr(eConsole), uintptr(unsafe.Pointer(&dev)))
		if h := hresult(uint32(r)); h.failed() {
			return nil, &hresultError{Op: "GetDefaultAudioEndpoint", HR: h}
		}
		return dev, nil
	}
	idw, err := windows.UTF16PtrFromString(id)
	if err != nil {
		return nil, err
	}
	var dev unsafe.Pointer
	r, _, _ := syscall.SyscallN(methodAddr(enum, mGetDevice),
		uintptr(enum), uintptr(unsafe.Pointer(idw)), uintptr(unsafe.Pointer(&dev)))
	if h := hresult(uint32(r)); h.failed() {
		return nil, &hresultError{Op: "GetDevice", HR: h}
	}
	return dev, nil
}

// enumEndpoints calls IMMDeviceEnumerator::EnumAudioEndpoints.
func enumEndpoints(enum unsafe.Pointer, dataFlow, stateMask uint32) (unsafe.Pointer, error) {
	var coll unsafe.Pointer
	r, _, _ := syscall.SyscallN(methodAddr(enum, mEnumAudioEndpoints),
		uintptr(enum), uintptr(dataFlow), uintptr(stateMask), uintptr(unsafe.Pointer(&coll)))
	if h := hresult(uint32(r)); h.failed() {
		return nil, &hresultError{Op: "EnumAudioEndpoints", HR: h}
	}
	return coll, nil
}

// collectionCount calls IMMDeviceCollection::GetCount. A failed GetCount is
// returned as an error rather than folded into a zero count, so an enumeration
// failure is not indistinguishable from a machine with no capture endpoints.
func collectionCount(coll unsafe.Pointer) (uint32, error) {
	var n uint32
	r, _, _ := syscall.SyscallN(methodAddr(coll, mCollGetCount), uintptr(coll), uintptr(unsafe.Pointer(&n)))
	if h := hresult(uint32(r)); h.failed() {
		return 0, &hresultError{Op: "IMMDeviceCollection::GetCount", HR: h}
	}
	return n, nil
}

// collectionItem calls IMMDeviceCollection::Item.
func collectionItem(coll unsafe.Pointer, i uint32) (unsafe.Pointer, error) {
	var dev unsafe.Pointer
	r, _, _ := syscall.SyscallN(methodAddr(coll, mCollItem),
		uintptr(coll), uintptr(i), uintptr(unsafe.Pointer(&dev)))
	if h := hresult(uint32(r)); h.failed() {
		return nil, &hresultError{Op: "IMMDeviceCollection::Item", HR: h}
	}
	return dev, nil
}

// deviceID calls IMMDevice::GetId and returns the endpoint id string.
func deviceID(dev unsafe.Pointer) (string, error) {
	var pstr unsafe.Pointer
	r, _, _ := syscall.SyscallN(methodAddr(dev, mDevGetID),
		uintptr(dev), uintptr(unsafe.Pointer(&pstr)))
	if h := hresult(uint32(r)); h.failed() {
		return "", &hresultError{Op: "IMMDevice::GetId", HR: h}
	}
	s := windows.UTF16PtrToString((*uint16)(pstr))
	coTaskFree(pstr)
	return s, nil
}

// deviceFriendlyName reads PKEY_Device_FriendlyName; on any failure it falls
// back to a placeholder rather than erroring, since a missing friendly name
// should not hide an otherwise usable endpoint.
func deviceFriendlyName(dev unsafe.Pointer) string {
	var store unsafe.Pointer
	r, _, _ := syscall.SyscallN(methodAddr(dev, mDevOpenPropertyStore),
		uintptr(dev), uintptr(stgmRead), uintptr(unsafe.Pointer(&store)))
	if hresult(uint32(r)).failed() {
		return unknownEndpointName
	}
	defer release(store)

	var pv propVariant
	r, _, _ = syscall.SyscallN(methodAddr(store, mPropGetValue),
		uintptr(store), uintptr(unsafe.Pointer(&pkeyDeviceFriendlyName)), uintptr(unsafe.Pointer(&pv)))
	if hresult(uint32(r)).failed() {
		return unknownEndpointName
	}
	defer func() { _, _, _ = procPropVariantClr.Call(uintptr(unsafe.Pointer(&pv))) }()
	// Only VT_LPWSTR carries a wide-string pointer in val; a driver returning any
	// other type (VT_EMPTY, VT_I4, ...) would otherwise be dereferenced as a
	// string pointer and read arbitrary memory.
	if pv.vt != vtLPWSTR || pv.val == nil {
		return unknownEndpointName
	}
	return windows.UTF16PtrToString((*uint16)(pv.val))
}

// activateAudioClient calls IMMDevice::Activate(IID_IAudioClient).
func activateAudioClient(dev unsafe.Pointer) (unsafe.Pointer, error) {
	var client unsafe.Pointer
	r, _, _ := syscall.SyscallN(methodAddr(dev, mDevActivate),
		uintptr(dev),
		uintptr(unsafe.Pointer(&iidIAudioClient)),
		uintptr(clsctxAll), 0,
		uintptr(unsafe.Pointer(&client)))
	if h := hresult(uint32(r)); h.failed() {
		return nil, &hresultError{Op: "IMMDevice::Activate(IAudioClient)", HR: h}
	}
	return client, nil
}
