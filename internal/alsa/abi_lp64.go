//go:build linux && (amd64 || arm64 || riscv64 || loong64)

package alsa

// This file covers the little-endian LP64 targets that use the generic Linux
// ioctl encoding (asm-generic/ioctl.h): amd64 and arm64 (C-verified and
// hardware-validated), plus riscv64 and loong64, which share both the identical
// LP64 struct layout AND that same generic _IOC bit layout, so they build on the
// same C-verified sizes/offsets/ioctl numbers. Deliberately excluded (they fall
// to abi_unsupported.go):
//   - big-endian LP64 (s390x, ppc64, mips64): the snd_interval flag bit-packing
//     is little-endian only.
//   - PowerPC and MIPS, INCLUDING the little-endian ppc64le and mips64le: these
//     define arch-specific _IOC bitfields (SIZEBITS 13, DIRBITS 3, NONE 1,
//     WRITE 4, dir shift 29), so ioctl.go's generic encoder would emit the wrong
//     request numbers (ENOTTY at runtime) even though the struct layout matches.
//     Supporting them needs a per-arch ioctl encoder, not just this layout.
//
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
