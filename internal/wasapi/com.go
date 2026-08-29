//go:build windows && (amd64 || arm64)

package wasapi

import (
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---- HRESULT ---------------------------------------------------------------

// hresult is a COM HRESULT. The high bit (0x80000000) marks a failure; the same
// bit pattern is used success or failure, so hresult is unsigned and failed()
// tests the severity bit rather than sign.
type hresult uint32

func (h hresult) failed() bool { return h&0x80000000 != 0 }

// Well-known HRESULTs this backend classifies. The AUDCLNT_* codes are from
// audioclient.h; the rest are standard COM/RPC codes.
const (
	sOK    hresult = 0x00000000
	sFALSE hresult = 0x00000001

	hrRPCChangedMode hresult = 0x80010106 // RPC_E_CHANGED_MODE

	// AUDCLNT_E_* values are MAKE_HRESULT(SEVERITY_ERROR, FACILITY_AUDCLNT=0x889, n),
	// so the low byte is the audioclient.h ordinal n.
	hrUnsupportedFormat    hresult = 0x88890008 // AUDCLNT_E_UNSUPPORTED_FORMAT (n=0x08)
	hrExclusiveNotAllowed  hresult = 0x8889000E // AUDCLNT_E_EXCLUSIVE_MODE_NOT_ALLOWED (n=0x0E)
	hrDeviceInUse          hresult = 0x8889000A // AUDCLNT_E_DEVICE_IN_USE (n=0x0A)
	hrBufferSizeNotAligned hresult = 0x88890019 // AUDCLNT_E_BUFFER_SIZE_NOT_ALIGNED (n=0x19)
	hrDeviceInvalidated    hresult = 0x88890004 // AUDCLNT_E_DEVICE_INVALIDATED (n=0x04)
	hrNotInitialized       hresult = 0x88890001 // AUDCLNT_E_NOT_INITIALIZED
	hrBufferEmpty          hresult = 0x08890001 // AUDCLNT_S_BUFFER_EMPTY (success)
)

// ---- GUIDs -----------------------------------------------------------------

var (
	clsidMMDeviceEnumerator = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C, Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35, Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioClient         = windows.GUID{Data1: 0x1CB9AD4C, Data2: 0xDBFA, Data3: 0x4C32, Data4: [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient  = windows.GUID{Data1: 0xC8ADBD64, Data2: 0xE71E, Data3: 0x48A0, Data4: [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}
)

// ---- vtable method indices --------------------------------------------------

const (
	// IUnknown
	mRelease = 2

	// IMMDeviceEnumerator
	mEnumAudioEndpoints      = 3
	mGetDefaultAudioEndpoint = 4
	mGetDevice               = 5

	// IMMDeviceCollection
	mCollGetCount = 3
	mCollItem     = 4

	// IMMDevice
	mDevActivate          = 3
	mDevOpenPropertyStore = 4
	mDevGetID             = 5
	mDevGetState          = 6

	// IPropertyStore
	mPropGetValue = 5

	// IAudioClient
	mClientInitialize        = 3
	mClientGetBufferSize     = 4
	mClientIsFormatSupported = 7
	mClientGetMixFormat      = 8
	mClientGetDevicePeriod   = 9
	mClientStart             = 10
	mClientStop              = 11
	mClientSetEventHandle    = 13
	mClientGetService        = 14

	// IAudioCaptureClient
	mCaptureGetBuffer     = 3
	mCaptureReleaseBuffer = 4
	mCaptureGetNextPacket = 5
)

// ---- COM constants ----------------------------------------------------------

const (
	coinitMultithreaded = 0x0
	clsctxAll           = 0x17
	stgmRead            = 0x0

	eCapture = 1 // EDataFlow.eCapture
	eConsole = 0 // ERole.eConsole

	deviceStateActive = 0x1

	shareModeExclusive = 1

	streamFlagsEventCallback = 0x00040000
)

// ---- system procs -----------------------------------------------------------

var (
	modole32           = windows.NewLazySystemDLL("ole32.dll")
	procCoInitializeEx = modole32.NewProc("CoInitializeEx")
	procCoCreate       = modole32.NewProc("CoCreateInstance")
	procCoTaskMemFree  = modole32.NewProc("CoTaskMemFree")
	procPropVariantClr = modole32.NewProc("PropVariantClear")
)

// initOnce ensures COM is initialized exactly once for the process.
var (
	initOnce sync.Once
	errInit  error
)

// ensureCOM initializes COM as multithreaded (MTA) once. A host process that
// already initialized COM as STA yields RPC_E_CHANGED_MODE, which is tolerated:
// the enumerator and audio-client calls work in either apartment.
func ensureCOM() error {
	initOnce.Do(func() {
		r, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
		if h := hresult(uint32(r)); h.failed() && h != hrRPCChangedMode {
			errInit = &hresultError{Op: "CoInitializeEx", HR: h}
		}
	})
	return errInit
}

// ---- vtable call plumbing ---------------------------------------------------
//
// A COM interface pointer is held as unsafe.Pointer to the object, whose first
// machine word is a pointer to its vtable (an array of method addresses). We
// keep object references as unsafe.Pointer (never uintptr) so the conversions
// fed to syscall.SyscallN are the compiler-recognized, GC-safe
// uintptr(unsafe.Pointer(x)) form, and no uintptr is ever converted back to a
// pointer. methodAddr reads the i'th vtable slot without any uintptr->pointer
// round-trip. The [64] bound comfortably exceeds every interface used here.

func methodAddr(obj unsafe.Pointer, index int) uintptr {
	vtbl := *(**[64]uintptr)(obj)
	return vtbl[index]
}

// coTaskFree frees memory returned by COM (allocated with CoTaskMemAlloc).
func coTaskFree(p unsafe.Pointer) {
	if p != nil {
		_, _, _ = procCoTaskMemFree.Call(uintptr(p))
	}
}

// release calls IUnknown::Release on a COM object.
func release(obj unsafe.Pointer) {
	if obj != nil {
		_, _, _ = syscall.SyscallN(methodAddr(obj, mRelease), uintptr(obj))
	}
}

// createEnumerator instantiates the MMDeviceEnumerator singleton.
func createEnumerator() (unsafe.Pointer, error) {
	if err := ensureCOM(); err != nil {
		return nil, err
	}
	var enum unsafe.Pointer
	r, _, _ := procCoCreate.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)),
		0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&enum)),
	)
	if h := hresult(uint32(r)); h.failed() {
		return nil, &hresultError{Op: "CoCreateInstance(MMDeviceEnumerator)", HR: h}
	}
	return enum, nil
}
