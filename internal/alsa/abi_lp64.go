//go:build linux && (amd64 || arm64)

package alsa

// uframes mirrors the kernel's snd_pcm_uframes_t (C unsigned long) and sframes
// its signed counterpart snd_pcm_sframes_t. On the LP64 targets both are 8 bytes,
// which sets the size of HwParams.FifoSize, the SwParams threshold fields, and
// Xferi, and therefore the size-encoded ioctl numbers. The resulting layout is
// C-verified in layout_lp64_test.go.
//
// These are defined types, not aliases, so every place that stores a value into
// a uframes/sframes field needs an explicit conversion. That makes a missing
// conversion a compile error on the native LP64 build rather than a surprise that
// only surfaces when cross-compiling for ILP32.
type uframes uint64
type sframes int64

// boundaryCap bounds the sw_params pointer-wrap boundary computed by boundary()
// in pcm.go. 1<<60 stays well below LONG_MAX (2^63-1, the kernel's signed
// hw_ptr limit) with headroom for the *2 in the doubling loop.
const boundaryCap uframes = 1 << 60
