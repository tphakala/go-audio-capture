# go-audio-capture

Pure Go, cgo-free audio capture for Linux, Windows, and macOS.

Capture only: device enumeration, honest format negotiation, and PCM delivery via callback. Planned replacement for the malgo/miniaudio capture path in BirdNET-Go.

## Why

miniaudio has served well, but the cgo boundary keeps costing debugging time: ALSA's dsnoop plugin silently resampling 256 kHz captures down to 48 kHz content, opaque "miniaudio: Invalid argument" failures in containers, hex-encoded device IDs, and negotiated formats only visible through a fork patch. Native Go backends make negotiation, buffering, and errors fully visible and debuggable.

## Scope

| Phase | Platform | Backend | Status |
|-------|----------|---------|--------|
| 1 | Linux | ALSA via kernel ioctl (/dev/snd), no libasound | planned, first target (~90% of users) |
| 2 | Windows | WASAPI via COM/syscall | planned |
| 3 | macOS | CoreAudio/AudioToolbox via purego (still cgo-free) | planned |

Non-goals: playback, mobile platforms, full miniaudio parity, pro-audio latency.

## Status

Planning. Design notes, prior-art research, and phase plans live in the issue tracker, not in the repo:

- #1 Feasibility study and prior art research
- #2 Phase 1: public API design (capture only)
- #3 Phase 1: Linux ALSA capture backend via kernel ioctl
- #4 Phase 2: Windows WASAPI capture backend
- #5 Phase 3: macOS CoreAudio capture backend via purego
