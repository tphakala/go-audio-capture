// Package wasapi is the Windows capture backend for the capture library. It
// talks directly to WASAPI through hand-rolled COM over golang.org/x/sys/windows
// (no cgo, no third-party COM or audio dependency), the Windows analog of the
// internal/alsa package.
//
// It captures in EXCLUSIVE mode only (AUDCLNT_SHAREMODE_EXCLUSIVE), the WASAPI
// equivalent of ALSA hw: access: the requested format is negotiated directly
// with the endpoint, exactly, or the call fails with a typed error. Shared mode
// is deliberately unsupported, because the OS mixer resamples to the engine mix
// rate and converts sample formats behind the caller's back, which is precisely
// the silent conversion this library exists to avoid. AUDCLNT_STREAMFLAGS_
// AUTOCONVERTPCM is never used.
//
// Design policy, mirroring internal/alsa:
//
//   - No silent resampling or format conversion. What the endpoint negotiates is
//     what Read delivers; Negotiated reports it honestly.
//   - Requested rate, channel count, and sample format are sacred: an
//     unsupported rate yields *BadRateError, an unsupported channel/format combo
//     yields *BadFormatError, each carrying what the endpoint reported.
//   - Typed errors name the failing COM call and its HRESULT, never an opaque
//     "unspecified error".
//
// COM is initialized multithreaded (MTA) once per process; a host that already
// initialized STA is tolerated (RPC_E_CHANGED_MODE). Capture is event-driven:
// Read blocks on the audio-ready event and a close event, so Close unblocks a
// parked Read. The whole COM surface is six interfaces: IMMDeviceEnumerator,
// IMMDeviceCollection, IMMDevice, IPropertyStore, IAudioClient, and
// IAudioCaptureClient.
package wasapi
