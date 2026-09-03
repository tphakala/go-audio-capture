// Package capture is a pure-Go, cgo-free audio capture library for Linux and
// Windows (a macOS backend is planned).
//
// On Linux it talks directly to the ALSA PCM character devices under /dev/snd
// via kernel ioctls, with no dependency on libasound. This gives hw:-level
// access (no plug, dsnoop, dmix, or default plugins, which are alsa-lib
// userspace features): a deliberate choice, because dsnoop's silent resampling
// is exactly the failure this library exists to avoid, and sysdefault already
// fails inside containers. The ALSA backend supports 64-bit and 32-bit targets
// (amd64, arm64, riscv64, loong64 and 386, arm). On Windows it uses WASAPI in
// exclusive mode via hand-rolled COM, also cgo-free.
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
// SupportedRates queries which sample rates a device accepts for a given
// channel count and format, using the ALSA HW_REFINE ioctl only (no state
// transition, so it does not disturb a device another process holds). It is
// Linux-only and returns ErrCapabilitiesUnsupported on other platforms.
//
// The public API (Devices, Open, Stream, SupportedRates) is platform-neutral;
// the Linux ALSA implementation lives in the *_linux.go files and internal/alsa,
// and the Windows WASAPI implementation in the *_windows.go files and
// internal/wasapi. A macOS CoreAudio backend is planned.
package capture
