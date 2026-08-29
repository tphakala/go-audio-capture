// Package capture is a pure-Go, cgo-free audio capture library.
//
// Phase 1 targets Linux only and talks directly to the ALSA PCM character
// devices under /dev/snd via kernel ioctls, with no dependency on libasound.
// This gives hw:-level access (no plug, dsnoop, dmix, or default plugins,
// which are alsa-lib userspace features): a deliberate choice, because
// dsnoop's silent resampling is exactly the failure this library exists to
// avoid, and sysdefault already fails inside containers.
//
// Design policy:
//
//   - No silent resampling or format conversion. What the hardware negotiates
//     is what the caller receives; Stream.Negotiated reports it honestly.
//   - Requested rate is sacred: if the exact rate is unsupported, Open fails
//     with a typed error carrying the supported range rather than quietly
//     substituting a different rate.
//   - Typed errors wrap errno and name the failing ioctl, never an opaque
//     "invalid argument".
//
// The public API (Devices, Open, Stream) is platform-neutral; the Linux ALSA
// implementation lives in the *_linux.go files and internal/alsa. Windows
// (WASAPI) and macOS (CoreAudio) backends are planned for later phases.
package capture
