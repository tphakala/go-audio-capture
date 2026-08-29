//go:build windows

package wasapi

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// ksSubtypePCM is KSDATAFORMAT_SUBTYPE_PCM. Only integer PCM is used; float and
// other subtypes are deliberately never requested (no conversion policy).
var ksSubtypePCM = windows.GUID{Data1: 0x00000001, Data2: 0x0000, Data3: 0x0010, Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}}

const (
	wfTagExtensible = 0xFFFE // WAVE_FORMAT_EXTENSIBLE

	speakerFrontLeft   = 0x1
	speakerFrontRight  = 0x2
	speakerFrontCenter = 0x4

	bufferFlagsDataDiscontinuity = 0x1 // AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY
	bufferFlagsSilent            = 0x2 // AUDCLNT_BUFFERFLAGS_SILENT
)

// waveFormatEx is the base WAVEFORMATEX header, used to read the tag/channels/
// rate/bits of a format returned by GetMixFormat.
type waveFormatEx struct {
	wFormatTag      uint16
	nChannels       uint16
	nSamplesPerSec  uint32
	nAvgBytesPerSec uint32
	nBlockAlign     uint16
	wBitsPerSample  uint16
	cbSize          uint16
}

// waveFormatExtensible is the FLAT WAVEFORMATEXTENSIBLE layout (exactly 40
// bytes, matching the C ABI). It must NOT embed waveFormatEx: that struct's
// uint32 fields give it 4-byte alignment, so Go would pad it 18->20 bytes and
// shift wValidBitsPerSample/dwChannelMask/subFormat by two, producing a
// malformed format the driver rejects with AUDCLNT_E_UNSUPPORTED_FORMAT (and a
// garbage read of GetMixFormat). Flattening keeps the exact C offsets:
// validBits@18, channelMask@20, subFormat@24.
type waveFormatExtensible struct {
	wFormatTag          uint16
	nChannels           uint16
	nSamplesPerSec      uint32
	nAvgBytesPerSec     uint32
	nBlockAlign         uint16
	wBitsPerSample      uint16
	cbSize              uint16
	wValidBitsPerSample uint16
	dwChannelMask       uint32
	subFormat           windows.GUID
}

// Compile-time guard: the two struct sizes are load-bearing for the ABI. If
// either drifts, one of these declarations becomes a negative array length or a
// non-constant index and the package fails to compile.
const (
	_ = 40 - unsafe.Sizeof(waveFormatExtensible{})
	_ = unsafe.Sizeof(waveFormatExtensible{}) - 40
)

// channelMask returns a KSAUDIO channel mask for the given channel count. Mono
// maps to FRONT_CENTER, stereo to FRONT_LEFT|FRONT_RIGHT; other counts use 0,
// which lets the endpoint apply its default speaker assignment.
func channelMask(channels int) uint32 {
	switch channels {
	case 1:
		return speakerFrontCenter
	case 2:
		return speakerFrontLeft | speakerFrontRight
	default:
		return 0
	}
}

// pcmFormat builds an integer-PCM WAVEFORMATEXTENSIBLE for the exact rate,
// channel count, and bit depth. No float, no conversion (phase-1 policy).
func pcmFormat(rate, channels, bits int) *waveFormatExtensible {
	block := channels * (bits / 8)
	return &waveFormatExtensible{
		wFormatTag:          wfTagExtensible,
		nChannels:           uint16(channels),
		nSamplesPerSec:      uint32(rate),
		nAvgBytesPerSec:     uint32(rate * block),
		nBlockAlign:         uint16(block),
		wBitsPerSample:      uint16(bits),
		cbSize:              22, // sizeof(WAVEFORMATEXTENSIBLE) - sizeof(WAVEFORMATEX)
		wValidBitsPerSample: uint16(bits),
		dwChannelMask:       channelMask(channels),
		subFormat:           ksSubtypePCM,
	}
}

// Negotiated reports the configuration the endpoint accepted. With no hidden
// conversion layer these values are exactly what Read delivers. It mirrors
// internal/alsa.Negotiated.
type Negotiated struct {
	Rate         int
	Channels     int
	Bits         int
	PeriodFrames int
	Periods      int
	BufferFrames int
}

// blockAlign is the size of one interleaved frame in bytes.
func (n Negotiated) blockAlign() int { return n.Channels * (n.Bits / 8) }
