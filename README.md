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
| 1 | Linux | ALSA via kernel ioctl (/dev/snd), no libasound | implemented (see below) |
| 2 | Windows | WASAPI via COM/syscall | planned |
| 3 | macOS | CoreAudio/AudioToolbox via purego (still cgo-free) | planned |

Non-goals: playback, mobile platforms, full miniaudio parity, pro-audio latency.

## Phase 1: Linux ALSA

The Linux backend talks directly to the ALSA PCM character devices under `/dev/snd` via kernel ioctls, with no dependency on libasound. This is `hw:`-level access only (no `plug`, `dsnoop`, `dmix`, or `default`, which are alsa-lib userspace plugins): a deliberate choice, because dsnoop's silent resampling is the failure this library exists to avoid, and `sysdefault` already fails inside containers.

Policy: no silent resampling or format conversion. The requested rate is honored exactly or `Open` fails with `*BadRateError`, so what the hardware negotiates is exactly what `Read` delivers, and `Stream.Negotiated` reports it honestly.

```go
devs, _ := capture.Devices() // []DeviceInfo{ID: "hw:1,0", Name: ...}

s, err := capture.Open(capture.Config{
    Device:   "hw:1,0",
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

`cmd/gac-rec` is a small debug recorder used for hardware validation:

```
go run ./cmd/gac-rec -list
go run ./cmd/gac-rec -d hw:1,0 -r 256000 -c 1 -f s16 -t 10s -o out.wav
```

Validated against the `snd-aloop` loopback (the same kernel ioctl path as a physical card) at 48/192/384 kHz S16 and 48 kHz S32, FFT-verified: a 60 kHz tone at 384 kHz round-trips with all spectral energy above 24 kHz and zero xruns, the exact ultrasonic case dsnoop broke. Field validation on real arm64 hardware with an ultrasonic USB mic is tracked in the tracker.

## Development

```
task check   # build (amd64 + arm64, CGO off), vet, lint, gofmt, race tests
```

## Status

Design notes, prior-art research, and phase plans live in the issue tracker, not in the repo:

- #1 Feasibility study and prior art research
- #2 Phase 1: public API design (capture only)
- #3 Phase 1: Linux ALSA capture backend via kernel ioctl
- #4 Phase 2: Windows WASAPI capture backend
- #5 Phase 3: macOS CoreAudio capture backend via purego
