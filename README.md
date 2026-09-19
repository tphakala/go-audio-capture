# go-audio-capture

[![CI](https://github.com/tphakala/go-audio-capture/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/go-audio-capture/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tphakala/go-audio-capture.svg)](https://pkg.go.dev/github.com/tphakala/go-audio-capture)
[![codecov](https://codecov.io/gh/tphakala/go-audio-capture/branch/main/graph/badge.svg)](https://codecov.io/gh/tphakala/go-audio-capture)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tphakala/go-audio-capture)](go.mod)
[![Latest tag](https://img.shields.io/github/v/tag/tphakala/go-audio-capture?sort=semver&label=release)](https://github.com/tphakala/go-audio-capture/tags)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/tphakala/go-audio-capture/badge)](https://scorecard.dev/viewer/?uri=github.com/tphakala/go-audio-capture)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Sponsor](https://img.shields.io/github/sponsors/tphakala?logo=githubsponsors&color=ea4aaa&label=Sponsor)](https://github.com/sponsors/tphakala)

Pure Go, cgo-free audio capture for Linux, Windows, and macOS.

Capture only: device enumeration, honest format negotiation, and PCM delivery through a blocking pull API. Planned replacement for the malgo/miniaudio capture path in BirdNET-Go.

## Why

miniaudio has served well, but the cgo boundary keeps costing debugging time: ALSA's dsnoop plugin silently resampling 256 kHz captures down to 48 kHz content, opaque "miniaudio: Invalid argument" failures in containers, hex-encoded device IDs, and negotiated formats only visible through a fork patch. Native Go backends make negotiation, buffering, and errors fully visible and debuggable.

## Scope

| Phase | Platform | Backend | Status |
|-------|----------|---------|--------|
| 1 | Linux (32/64-bit) | ALSA via kernel ioctl (/dev/snd), no libasound | implemented (see below) |
| 2 | Windows (64-bit) | WASAPI (exclusive mode) via hand-rolled COM/syscall | implemented (see below) |
| 3 | macOS | CoreAudio/AudioToolbox via purego (still cgo-free) | planned |

Non-goals: playback, mobile platforms, full miniaudio parity, pro-audio latency.

### Sample formats

`FormatS16LE` and `FormatS32LE` (signed 16- and 32-bit little-endian integer) and `FormatF32LE` (32-bit IEEE-754 little-endian float) are the supported capture formats. As with the sample rate, the requested format is negotiated with the hardware exactly or `Open` fails; there is no silent conversion. Float is the native format on macOS CoreAudio (the reason it exists here); on Linux `hw:` devices and Windows exclusive endpoints it is accepted only when the hardware itself offers float, which many do not.

## Phase 1: Linux ALSA

The Linux backend talks directly to the ALSA PCM character devices under `/dev/snd` via kernel ioctls, with no dependency on libasound. This is `hw:`-level access only (no `plug`, `dsnoop`, `dmix`, or `default`, which are alsa-lib userspace plugins): a deliberate choice, because dsnoop's silent resampling is the failure this library exists to avoid, and `sysdefault` already fails inside containers.

Policy: no silent resampling or format conversion. The requested rate is honored exactly or `Open` fails with `*BadRateError`, so what the hardware negotiates is exactly what `Read` delivers, and `Stream.Negotiated` reports it honestly.

```go
devs, err := capture.Devices()
if err != nil {
    log.Fatal(err)
}
if len(devs) == 0 {
    log.Fatal("no capture devices found")
}
// devs[0].ID: "usb:16d0:06f3:s=0384_2474750763FA81C9:if=0,0", HWAddr: "hw:2,0", ...

s, err := capture.Open(capture.Config{
    Device:   devs[0].ID, // persist this, not the hw: address
    Rate:     256000, // exact; a rate the device cannot do fails, never silently substitutes
    Channels: 1,
    Format:   capture.FormatS16LE,
})
if err != nil {
    log.Fatal(err)
}
defer s.Close()

if err := s.Start(); err != nil {
    log.Fatal(err)
}
buf := make([]byte, s.Negotiated().PeriodFrames*2) // 1 ch * 2 bytes
for {
    frames, err := s.Read(buf) // blocks a period; recovers xruns internally
    if errors.Is(err, capture.ErrClosed) {
        break
    }
    if err != nil {
        log.Fatal(err)
    }
    _ = frames // buf[:frames*frameBytes] is fresh S16LE PCM
}
```

`Read` is single-consumer and blocking; `Close` may be called from another goroutine to unblock it. Overruns (xruns) are recovered internally and counted via `Stream.Xruns()`.

### Device ids are stable

`DeviceInfo.ID` is the field to persist. ALSA card indices follow kernel probe order, so `hw:1,0` can name a different microphone after a reboot or a replug: plug two USB mics in and they can trade indices, and an application that stored `hw:1,0` then opens the wrong one. On Linux the id is therefore derived from sysfs and survives that, in one of three forms:

| Card | Id | Example |
|---|---|---|
| USB with a serial | `usb:<vid>:<pid>:s=<serial>:if=<n>,<dev>` | `usb:16d0:06f3:s=0384_2474750763FA81C9:if=0,0` |
| USB without a serial | `usb:<vid>:<pid>:p=<controller>-<devpath>:if=<n>,<dev>` | `usb:1686:067f:p=0000:00:14.0-3:if=0,0` |
| Non-USB | `hw:CARD=<card id>,DEV=<dev>` (alsa-lib syntax) | `hw:CARD=Loopback,DEV=1` |

A device with a serial is keyed on the serial, so it keeps its id when moved to another port. One without a serial is keyed on the physical port instead, which keeps two identical units apart; `vid:pid` stays in the port form so a different model moved onto that port reports not-found rather than being opened as if it were the expected device. Bytes outside `[A-Za-z0-9._-]` are percent-escaped in both the serial and the port value, with the port keeping `:` raw so a PCI controller address stays readable. The USB bus number is deliberately unused, because bus numbers follow controller probe order.

The `hw:CARD=` form is only as unique as the kernel card id it carries. The kernel disambiguates two identical non-USB cards by suffixing the second (`PCH` and `PCH_1`) in probe order, so for duplicate non-USB cards this form inherits the same probe-order instability the USB forms are built to avoid; a bus-address-based non-USB id is tracked as future work.

The id falls back to `hw:N,D` with `IDStable` false whenever no stable form can be built: sysfs cannot be read (a container with a partial `/sys`), or a USB card reports no serial and no derivable port, or a non-USB card has no kernel card id. A false `IDStable` is the signal not to persist it.

`Config.Device` accepts any of these, and still accepts a plain `hw:card,device` for interactive use. A stable id is resolved on every `Open`, `SupportedRates`, and `SupportedRatesVerified` call, never cached, and re-checked once more after the device is open, so a card swapped in the window between resolving and opening is caught rather than recorded. `Resolve` answers the same question without opening anything:

```go
var amb *capture.AmbiguousDeviceError
d, err := capture.Resolve(persistedID)
switch {
case errors.As(err, &amb): // *AmbiguousDeviceError
    // Two units report the same serial. Pin one with its PortID; the library
    // will not guess, because opening a coin-flip device is the failure a
    // stable id exists to prevent.
case errors.Is(err, capture.ErrDeviceGone):
    // *DeviceNotFoundError: the hardware is not attached right now.
case err == nil:
    log.Printf("%s is currently %s", d.Name, d.HWAddr)
}
```

On Linux `DeviceInfo.HWAddr` is the current-boot `hw:card,device` for display, logs, and `arecord`; it is not stable and must not be persisted. (On Windows HWAddr equals the endpoint id, which is stable; see the Windows section.)

### Upgrading from v0.5.x

`DeviceInfo.ID` changed meaning on Linux in this release. In v0.5.x it was the current-boot `hw:card,device` address; it is now the sysfs-derived stable id described above. This is a behaviour change with no signature change, so `go get -u` picks it up silently across the v0 minor. Three things to check:

- A value your application persisted under v0.5.x is a `hw:card,device` string. It still opens, because `Config.Device` accepts the current-boot form for interactive use. But it no longer equals any `Devices()[i].ID`, so code that recognises a saved device by scanning for `ID == saved` stops matching. Compare the saved value against `DeviceInfo.HWAddr` instead, or pass it to `Resolve` to find the device and read its new stable `ID` to persist going forward.
- The new `ID` for a USB card is not valid `arecord` syntax (`arecord -D usb:16d0:...` does not parse). For an external tool that wants an ALSA device string, use `DeviceInfo.HWAddr` (the `hw:card,device` form), not `ID`.
- Unkeyed `DeviceInfo{...}` composite literals no longer compile, because the struct gained fields (`HWAddr`, `IDStable`, `PortID`, `CardID`, `USB`). Switch to keyed fields (`DeviceInfo{ID: ..., Name: ...}`), which is the form that survives a struct gaining fields.

To discover which rates a device supports before opening it (e.g. to pick a capture rate, or to offer the user a menu), `SupportedRates` probes the device with the `HW_REFINE` ioctl only. It opens the device once (non-blocking) and issues one refine per candidate rate; it never runs `HW_PARAMS`, `PREPARE`, or `START`, so it does not move the device out of its current state and does not disturb a stream another process holds. A single refine reports only the continuous `[Min, Max]` window, so each standard rate inside that window is probed individually to reveal discrete gaps.

```go
rs, err := capture.SupportedRates(devs[0].ID, 1, capture.FormatS16LE)
// rs.Rates == []int{192000, 256000, 384000}       // discrete, ascending
// rs.Min, rs.Max == 192000, 384000                // raw HW_REFINE window
```

If the device is held exclusively by another process the query returns `ErrDeviceInUse`; a channel/format combination the hardware cannot do at any rate returns `*BadFormatError`; a removed device returns `ErrDeviceGone`. In each case the caller should fall back to a static rate list. `SupportedRates` is Linux-only for now and returns `ErrCapabilitiesUnsupported` on other platforms.

`cmd/gac-rec` is a small debug recorder used for hardware validation:

```
go run ./cmd/gac-rec -list
go run ./cmd/gac-rec -d 'usb:16d0:06f3:s=0384_2474750763FA81C9:if=0,0' -r 256000 -c 1 -f s16 -t 10s -o out.wav
go run ./cmd/gac-rec -d hw:1,0 -r 256000 -c 1 -f s16 -t 10s -o out.wav   # the unstable address still works
```

Validated against the `snd-aloop` loopback (the same kernel ioctl path as a physical card) at 48/192/384 kHz S16 and 48 kHz S32, FFT-verified: a 60 kHz tone at 384 kHz round-trips with all spectral energy above 24 kHz and zero xruns, the exact ultrasonic case dsnoop broke. Field validation on real arm64 hardware with an ultrasonic USB mic is tracked in the tracker.

Architectures: the Linux backend supports the little-endian LP64 arches (`amd64`, `arm64`, `riscv64`, `loong64`) and little-endian ILP32 arches (`386`, `arm`), all of which use the generic `asm-generic/ioctl.h` encoding. `amd64`, `arm64`, `386`, and `arm` are additionally hardware-validated; `riscv64` and `loong64` build on the identical, C-verified LP64 layout and ioctl numbers. The kernel's `snd_pcm_uframes_t` is a C `unsigned long`, so the ioctl struct layouts and the size-encoded ioctl numbers differ between 64- and 32-bit builds; both sets are pinned against `sound/asound.h` (the 32-bit set C-verified with `gcc -m32`) and asserted in the layout tests, with the ILP32 assertions run under `GOARCH=386`. A 32-bit binary was validated capturing from real USB hardware (a 384 kHz AudioMoth mic and a ZOOM AMS-24) through the kernel's 32-bit compat path, negotiating byte-for-byte identically to the 64-bit build. Any other `GOARCH` fails to build rather than silently emitting wrong ioctl numbers: big-endian targets (their `snd_interval` flag bit-packing is little-endian only) and the PowerPC and MIPS families including the little-endian `ppc64le`/`mips64le` (they use an architecture-specific ioctl encoding, so supporting them needs a per-arch ioctl encoder, not just this layout).

## Phase 2: Windows WASAPI

The Windows backend talks to WASAPI through hand-rolled COM over `golang.org/x/sys/windows` (no cgo, no third-party COM or audio dependency), the Windows analog of the ALSA backend. It captures in **exclusive mode only** (`AUDCLNT_SHAREMODE_EXCLUSIVE`), the WASAPI equivalent of ALSA `hw:` access: the format is negotiated directly with the endpoint. Shared mode is deliberately unsupported, because the OS mixer resamples to the engine mix rate and converts the sample format behind the caller's back, the same silent conversion the library exists to avoid. `AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM` is never used.

The public API is identical to Linux; only the device string differs. `DeviceInfo.ID` holds the opaque WASAPI endpoint-id string, which is also reported as `HWAddr` with `IDStable` true (Windows has no separate unstable address as Linux does); `Card`, `Device`, `CardID`, `PortID`, and `USB` are Linux-only and stay at their zero values. `Config.Device` takes that endpoint-id string, or `""` / `"default"` for the default capture endpoint. The requested rate, channel count, and sample format are negotiated exactly or `Open` fails with a typed error:

- `*BadRateError`: the exact rate is unsupported (carries the endpoint's supported range when it can be determined).
- `*BadFormatError`: the channel-count / sample-format combination is unsupported. Exclusive endpoints commonly accept only specific layouts (e.g. stereo S16 but not mono), and the library returns this rather than up/down-mixing or converting.
- `ErrExclusiveNotAllowed`, `ErrDeviceInUse`, `ErrDeviceGone`: exclusive access disabled for the endpoint, held by another application, or the endpoint was invalidated (unplugged) mid-capture.

```go
devs, _ := capture.Devices() // []DeviceInfo{ID: "{0.0.1.00000000}.{guid}", Name: "Microphone (...)"}

s, err := capture.Open(capture.Config{
    Device:   "default", // or an endpoint-id string from Devices()
    Rate:     48000,     // exact; no silent resampling
    Channels: 2,
    Format:   capture.FormatS16LE,
})
// ... Start / Read / Close exactly as on Linux
```

Capture is event-driven: `Read` blocks on the audio-ready event and an internal close event, so `Close` unblocks a parked `Read` from another goroutine. `AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY` and device overruns are counted via `Stream.Xruns()`. Delivered frames are kept equal to the device's own advance (via the packet `devicePosition`), so drivers that re-present buffers do not over-deliver and high-rate streams do not silently drop. The WASAPI backend is 64-bit only (`amd64`/`arm64`).

`cmd/gac-rec` is cross-platform; on Windows the device is an endpoint-id string or `"default"`:

```
go run ./cmd/gac-rec -list
go run ./cmd/gac-rec -d "default" -r 48000 -c 2 -f s16 -t 10s -o out.wav
```

Validated on real hardware, a Sound Blaster ZxR (48 kHz) and a Solid State Logic SSL 2 MkII (48 and 192 kHz): captured audio duration matches wall-clock (real-time), zero xruns, gap-free including at 192 kHz, with `*BadRateError`/`*BadFormatError` confirmed on unsupported rates and channel layouts.

## Performance vs malgo/miniaudio

This library exists for debuggability and a clean cgo-free build, not for raw speed, and a direct measurement bears that out: at typical capture rates the CPU cost is the same, and the measurable wins are memory and process footprint.

Both paths captured from the same USB interface at its native 48 kHz / 2 ch / S32LE, so neither side resampled or converted. The malgo path was configured the way BirdNET-Go drives it (miniaudio defaults, `Alsa.NoMMap=1`, device selected by id). Each figure is the mean of three 60 s steady-state windows (3 s warmup discarded), self-measured with `getrusage(RUSAGE_SELF)` (which aggregates miniaudio's own capture thread), Go `runtime.MemStats`, and `/proc/self/status`. The native binary was built `CGO_ENABLED=0`.

| Metric (48 kHz stereo S32, 60 s window) | this library (pure Go) | malgo / miniaudio (cgo) |
|---|---|---|
| CPU, % of one core | ~0.42% | ~0.40% |
| Peak resident memory (RSS) | 3.9 MB | 8.3 MB |
| Go live heap | ~340 KiB | ~330 KiB |
| Heap allocated over the window | ~6 KiB, 0 GC | ~6 KiB, 0 GC |
| OS threads | 5 | 7 |
| cgo calls / s | 0 | ~47 |
| Dropped frames / xruns | 0 | 0 |

- CPU is a wash. Both sit near 0.4% of one core, and the run-to-run spread is wider than the gap between them: steady-state capture is dominated by the same ALSA read syscall on both sides.
- Resident memory is roughly 2x lower. The Go heap is tiny and near-identical on both, so the extra ~4 MB on the malgo side is miniaudio's C runtime and buffers, off-heap and invisible to Go's GC, visible only as process RSS.
- Both are allocation-free in steady state (no GC cycles across 60 s), so neither adds GC pressure to a host application.
- The cgo backend crosses the C/Go boundary about once per callback and runs two extra OS threads. The pure-Go backend does neither and links as a static binary with no libasound and no C toolchain.

Caveats: these numbers are one machine, one device, at 48 kHz. They do not cover high sample rates (for example 256 kHz ultrasonic), where zero-copy period handling could diverge, and they measure the bare capture path (counting delivered PCM), not any downstream conversion or routing.

## Development

```
task check   # build (amd64/arm64/arm/386/riscv64/loong64, CGO off), vet, lint, gofmt, race tests + GOARCH=386 test run
```
