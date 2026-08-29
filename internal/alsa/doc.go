// Package alsa is the Linux ALSA PCM backend for go-audio-capture. It mirrors
// the kernel's sound/asound.h ABI (hw_params/sw_params structs and the
// SNDRV_PCM_IOCTL_* request numbers) in pure Go and drives capture through
// ioctls on the /dev/snd PCM character devices, with no dependency on
// libasound.
//
// All ABI-bearing code lives in the linux-tagged files; this file carries no
// build constraint so the package remains non-empty (and `go build ./...`
// stays happy) on non-Linux platforms, where the backend is simply absent.
package alsa
