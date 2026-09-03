//go:build linux && (386 || arm)

package alsa

// uframes mirrors the kernel's snd_pcm_uframes_t (C unsigned long) and sframes
// its signed counterpart snd_pcm_sframes_t. On the ILP32 targets C long and
// pointers are 32-bit, so both are 4 bytes. That shrinks HwParams.FifoSize, the
// SwParams threshold fields, and Xferi, which in turn changes the size-encoded
// ioctl numbers. The resulting layout (struct sizes, field offsets, and ioctl
// numbers) is C-verified against sound/asound.h in layout_ilp32_test.go.
//
// A 32-bit user binary running on a 64-bit kernel reaches the same ABI through
// the kernel's compat_ioctl path (snd_pcm_ioctl_compat), so these are the numbers
// the kernel expects from an ILP32 process on either a 32-bit or a 64-bit kernel.
//
// Defined types, not aliases: see the note in abi_lp64.go.
type uframes uint32
type sframes int32

// boundaryCap bounds the sw_params pointer-wrap boundary computed by boundary()
// in pcm.go. 1<<30 keeps both the boundary and the *2 in the doubling loop inside
// a 32-bit uframes and below LONG_MAX (2^31-1, the kernel's signed hw_ptr limit).
const boundaryCap uframes = 1 << 30
