# Review instructions

go-audio-capture is a pure-Go, cgo-free, capture-only audio library (Linux ALSA via raw kernel ioctls, Windows WASAPI exclusive mode). Its consumers are long-running, unattended capture services, so robustness and honest error reporting matter more than features. AGENTS.md in the repository root is the full reference; this file is the short version for code review.

Flag as defects:

- Any silent conversion or fallback: resampling, channel mixing, format conversion, substituting a "close enough" rate, opening `default`/`plug`/`dsnoop` on Linux, shared mode or `AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM` on Windows. The requested format is negotiated exactly or `Open` fails with a typed error.
- Any cgo import or new runtime dependency beyond `golang.org/x/sys`.
- A path where `Open`, `Start` or `Read` can block forever, or a retry or recovery loop with no bound and no wake-up on `Close`.
- An ioctl or COM call that can run after `Close` released the handle, or a `Close` that does not unblock a parked `Read`. `Close` must always win: a concurrent close is reported as `ErrClosed`, never as a device error.
- A raw errno, HRESULT or ad hoc string error where a sentinel or typed error applies (`ErrClosed`, `ErrDeviceGone`, `ErrDeviceInUse`, `ErrDeviceStalled`, `ErrExclusiveNotAllowed`, `ErrCapabilitiesUnsupported`, `*BadRateError`, `*BadFormatError`, `*ConfigError`, `*BadDeviceError`, `*DeviceNotFoundError`, `*AmbiguousDeviceError`, `*StallError`), or an error mapped to the wrong one (an unplug reported as a stall, a close reported as a device loss).
- A second copy of an errno or HRESULT classification set instead of extending `alsa.IsDeviceGone`, `alsa.IsRecoverable` or `hresultError.Unwrap`.
- An allocation on the steady-state `Read` path.
- A change to an ioctl struct or ioctl number without updating the layout tests for both 64-bit and 32-bit builds.
- A test that cannot fail on the behaviour it names, and doc text that is false on one platform (Linux-only behaviour must say "On Linux").
- Em or en dashes anywhere in code, comments, docs or messages.

Intentional, do not flag:

- `unsafe.Pointer` conversions for ioctl and COM calls (layouts are pinned by tests).
- Package-level function variables used as test seams (`openPCM`, `openRatePCM`, `openEndpoint`, `sysOpen`, `sysSetNonblock`, `resumeSleep`).
- Ignored errors from best-effort cleanup, such as `_ = p.Close()` on an error path.
- Mutexes that are never held across a blocking syscall; `Close` drains in-flight ioctls instead.
- Numeric constants in `internal/alsa` and `internal/wasapi` that mirror kernel or Windows headers.
- Root-package typed errors that duplicate internal ones; they keep callers from importing internal packages.
