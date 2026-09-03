// Package alsa is the Linux ALSA PCM backend for go-audio-capture. It mirrors
// the kernel's sound/asound.h ABI (hw_params/sw_params structs and the
// SNDRV_PCM_IOCTL_* request numbers) in pure Go and drives capture through
// ioctls on the /dev/snd PCM character devices, with no dependency on
// libasound.
//
// All ABI-bearing code lives in the linux-tagged files; this file carries no
// build constraint so the package remains non-empty (and `go build ./...`
// stays happy) on non-Linux platforms, where the backend is simply absent.
//
// The kernel's snd_pcm_uframes_t / snd_pcm_sframes_t are C unsigned long / long,
// so the struct layouts and the size-encoded ioctl numbers depend on the word
// size. abi_lp64.go and abi_ilp32.go select the word-width types per GOARCH for
// little-endian LP64 (amd64, arm64) and ILP32 (386, arm); abi_unsupported.go
// fails the build on any other architecture rather than emitting wrong ioctls.
// The pinned sizes/offsets/ioctl numbers are C-verified in
// layout_lp64_test.go and layout_ilp32_test.go.
package alsa
