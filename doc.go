// Package capture is a pure-Go, cgo-free audio capture library for Linux and
// Windows (a macOS backend is planned). It is capture-only and aims to work
// with the whole range of capture hardware, from low-cost USB sound cards to
// studio interfaces and ultrasonic recorders.
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
//   - Device ids are stable. When DeviceInfo.IDStable is true, DeviceInfo.ID
//     survives a reboot and a replug, so an application can persist it and keep
//     opening the same physical hardware; on Linux it is derived from sysfs
//     rather than from the ALSA card index, which follows kernel probe order.
//     When no stable form can be derived IDStable is false and ID is only a
//     current-boot address that must not be persisted. Resolve reports what an
//     id currently names without opening it, and OpenDevice opens a DeviceInfo
//     that Resolve already returned without resolving it again (on Linux it
//     still confirms after the open that the card is the same unit).
//   - Robust failure handling for unattended capture. Overruns, system suspend
//     and driver stalls are recovered inside Stream.Read and counted by
//     Stream.Xruns; a busy, missing, unplugged or stalled device is reported as
//     ErrDeviceInUse, ErrDeviceGone or ErrDeviceStalled so the caller knows
//     whether to retry, wait for the device or reopen. Open fails at once on a
//     busy device, and Close always unblocks a parked Read. Stall detection and
//     waking a parked Read on unplug are Linux-only so far.
//
// SupportedRates queries which sample rates a device accepts for a given
// channel count and format, using the ALSA HW_REFINE ioctl only (no state
// transition, so it does not disturb a device another process holds). It is
// Linux-only and returns ErrCapabilitiesUnsupported on other platforms.
//
// The public API (Devices, Resolve, Open, OpenDevice, Stream, SupportedRates,
// SupportedRatesVerified) is platform-neutral; the Linux ALSA implementation
// lives in the *_linux.go files and internal/alsa, and the Windows WASAPI
// implementation in the *_windows.go files and internal/wasapi. A macOS
// CoreAudio backend is planned.
package capture
