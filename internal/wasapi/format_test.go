//go:build windows

package wasapi

import (
	"errors"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The WAVEFORMATEXTENSIBLE layout is load-bearing: a wrong offset produces a
// malformed format the driver silently rejects (AUDCLNT_E_UNSUPPORTED_FORMAT),
// which is exactly the bug the flat-struct design avoids. These assertions pin
// the C ABI (40 bytes; validBits@18, channelMask@20, subFormat@24).
func TestWaveFormatExtensibleLayout(t *testing.T) {
	if got := unsafe.Sizeof(waveFormatExtensible{}); got != 40 {
		t.Fatalf("sizeof(waveFormatExtensible) = %d, want 40", got)
	}
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"wValidBitsPerSample", unsafe.Offsetof(waveFormatExtensible{}.wValidBitsPerSample), 18},
		{"dwChannelMask", unsafe.Offsetof(waveFormatExtensible{}.dwChannelMask), 20},
		{"subFormat", unsafe.Offsetof(waveFormatExtensible{}.subFormat), 24},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("offsetof(waveFormatExtensible.%s) = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestPropVariantSize(t *testing.T) {
	if got := unsafe.Sizeof(propVariant{}); got != 24 {
		t.Errorf("sizeof(propVariant) = %d, want 24", got)
	}
}

func TestChannelMask(t *testing.T) {
	tests := []struct {
		ch   int
		want uint32
	}{
		{1, 0x4}, // FRONT_CENTER
		{2, 0x3}, // FRONT_LEFT | FRONT_RIGHT
		{4, 0x0}, // endpoint default
	}
	for _, tt := range tests {
		if got := channelMask(tt.ch); got != tt.want {
			t.Errorf("channelMask(%d) = %#x, want %#x", tt.ch, got, tt.want)
		}
	}
}

func TestPCMFormat(t *testing.T) {
	f := pcmFormat(48000, 2, 16)
	if f.wFormatTag != 0xFFFE { // WAVE_FORMAT_EXTENSIBLE, pinned by literal
		t.Errorf("wFormatTag = %#x, want 0xFFFE", f.wFormatTag)
	}
	if f.nChannels != 2 || f.nSamplesPerSec != 48000 || f.wBitsPerSample != 16 {
		t.Errorf("basic fields = %d ch, %d Hz, %d bits", f.nChannels, f.nSamplesPerSec, f.wBitsPerSample)
	}
	if f.nBlockAlign != 4 { // 2 ch * 2 bytes
		t.Errorf("nBlockAlign = %d, want 4", f.nBlockAlign)
	}
	if f.nAvgBytesPerSec != 192000 { // 48000 Hz * 4 bytes/frame, pinned by literal
		t.Errorf("nAvgBytesPerSec = %d, want 192000", f.nAvgBytesPerSec)
	}
	if f.cbSize != 22 {
		t.Errorf("cbSize = %d, want 22", f.cbSize)
	}
	if f.wValidBitsPerSample != 16 || f.dwChannelMask != 0x3 {
		t.Errorf("extension = validBits %d, mask %#x", f.wValidBitsPerSample, f.dwChannelMask)
	}
	// KSDATAFORMAT_SUBTYPE_PCM {00000001-0000-0010-8000-00AA00389B71}, pinned by a
	// local literal so a typo in the package-level ksSubtypePCM cannot move both
	// sides of the comparison together (which would defeat the check).
	wantPCM := windows.GUID{Data1: 0x00000001, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}
	if f.subFormat != wantPCM {
		t.Errorf("subFormat = %+v, want KSDATAFORMAT_SUBTYPE_PCM", f.subFormat)
	}
}

func TestPCMFormatUltrasonic(t *testing.T) {
	// 256 kHz mono S16: the ultrasonic bat-audio case. No field overflows a
	// uint32 (nAvgBytesPerSec = 256000*2 = 512000).
	f := pcmFormat(256000, 1, 16)
	if f.nSamplesPerSec != 256000 || f.nChannels != 1 || f.nAvgBytesPerSec != 512000 {
		t.Errorf("256k mono = %d Hz, %d ch, %d avg bytes", f.nSamplesPerSec, f.nChannels, f.nAvgBytesPerSec)
	}
	if f.dwChannelMask != 0x4 {
		t.Errorf("mono mask = %#x, want 0x4", f.dwChannelMask)
	}
}

func TestNegotiatedBlockAlign(t *testing.T) {
	if got := (Negotiated{Channels: 2, Bits: 16}).blockAlign(); got != 4 {
		t.Errorf("blockAlign(2ch/16) = %d, want 4", got)
	}
	if got := (Negotiated{Channels: 1, Bits: 32}).blockAlign(); got != 4 {
		t.Errorf("blockAlign(1ch/32) = %d, want 4", got)
	}
}

func TestBadRateErrorMessage(t *testing.T) {
	withRange := (&BadRateError{Requested: 48000, Min: 44100, Max: 96000}).Error()
	if !strings.Contains(withRange, "44100..96000") {
		t.Errorf("BadRateError with range = %q, want a range", withRange)
	}
	noRange := (&BadRateError{Requested: 256000}).Error()
	if strings.Contains(noRange, "..") {
		t.Errorf("BadRateError without range = %q, should omit range", noRange)
	}
}

func TestHResultErrorClassifies(t *testing.T) {
	tests := []struct {
		hr       hresult
		sentinel error
	}{
		{hrExclusiveNotAllowed, ErrExclusiveNotAllowed},
		{hrDeviceInUse, ErrDeviceInUse},
		{hrDeviceInvalidated, ErrDeviceGone},
	}
	for _, tt := range tests {
		err := error(&hresultError{Op: "Initialize", HR: tt.hr})
		if !errors.Is(err, tt.sentinel) {
			t.Errorf("hresultError(%s) not Is %v", tt.hr.name(), tt.sentinel)
		}
	}
	// An unclassified HRESULT unwraps to nil (matches no sentinel).
	other := error(&hresultError{Op: "Initialize", HR: hrUnsupportedFormat})
	if errors.Is(other, ErrExclusiveNotAllowed) || errors.Is(other, ErrDeviceInUse) || errors.Is(other, ErrDeviceGone) {
		t.Errorf("unclassified HRESULT matched a sentinel")
	}
}

func TestHResultFailed(t *testing.T) {
	if sOK.failed() || sFALSE.failed() || hrBufferEmpty.failed() {
		t.Error("success HRESULTs reported as failed")
	}
	if !hrUnsupportedFormat.failed() || !hrDeviceInvalidated.failed() {
		t.Error("failure HRESULTs reported as success")
	}
}

// TestHResultConstantValues pins the AUDCLNT_E_* codes to their audioclient.h
// values numerically. AUDCLNT_E_* = MAKE_HRESULT(SEVERITY_ERROR,
// FACILITY_AUDCLNT=0x889, n), so the low byte is the ordinal n. A wrong value is
// a syntactically valid uint32 that vet and lint cannot catch; it silently
// misclassifies runtime errors (verified on hardware: a rate change on an
// interface invalidated the endpoint and IMMDevice::Activate returned
// AUDCLNT_E_DEVICE_INVALIDATED = 0x88890004).
func TestHResultConstantValues(t *testing.T) {
	tests := []struct {
		name string
		got  hresult
		want uint32
	}{
		{"AUDCLNT_E_UNSUPPORTED_FORMAT", hrUnsupportedFormat, 0x88890008},
		{"AUDCLNT_E_EXCLUSIVE_MODE_NOT_ALLOWED", hrExclusiveNotAllowed, 0x8889000E},
		{"AUDCLNT_E_DEVICE_IN_USE", hrDeviceInUse, 0x8889000A},
		{"AUDCLNT_E_BUFFER_SIZE_NOT_ALIGNED", hrBufferSizeNotAligned, 0x88890019},
		{"AUDCLNT_E_DEVICE_INVALIDATED", hrDeviceInvalidated, 0x88890004},
		{"AUDCLNT_E_NOT_INITIALIZED", hrNotInitialized, 0x88890001},
	}
	for _, tt := range tests {
		if uint32(tt.got) != tt.want {
			t.Errorf("%s = 0x%08X, want 0x%08X", tt.name, uint32(tt.got), tt.want)
		}
	}
}
