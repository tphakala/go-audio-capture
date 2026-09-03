//go:build linux && !amd64 && !arm64 && !386 && !arm && !riscv64 && !loong64

package alsa

// This backend's struct layouts and size-encoded ioctl numbers are defined only
// for the generic-ioctl little-endian LP64 arches (amd64, arm64, riscv64, loong64;
// see abi_lp64.go) and little-endian ILP32 arches (386, arm; see abi_ilp32.go),
// with the pinned sizes/offsets/ioctl numbers C-verified in layout_lp64_test.go
// and layout_ilp32_test.go.
//
// Any other GOARCH is rejected at compile time rather than silently building a
// binary the kernel would reject or mis-read. Two independent reasons put an arch
// here:
//   - Big-endian targets (s390x, ppc64, mips64, mips): the snd_interval flag
//     bit-packing in hwparams.go is little-endian only.
//   - PowerPC and MIPS of any width or endianness (ppc64, ppc64le, mips, mips64,
//     mips64le, mipsle): they define arch-specific _IOC bitfields (SIZEBITS 13,
//     DIRBITS 3, NONE 1, WRITE 4, dir shift 29) instead of asm-generic/ioctl.h,
//     so ioctl.go's generic encoder would emit wrong request numbers (ENOTTY at
//     runtime). ppc64le and mips64le are excluded for this reason despite being
//     little-endian LP64.
//
// To add an architecture, verify its sound/asound.h layout and ioctl encoding,
// then extend the build tags in abi_lp64.go / abi_ilp32.go (a PowerPC/MIPS port
// also needs a per-arch ioctl encoder). See issue #12.

// The word-width types are defined here too, set to the ILP32 widths, purely so
// the rest of the package still type-checks and the build fails on exactly one
// line (the undefined reference below) with a self-explanatory message, instead
// of a cryptic cascade of "undefined: uframes" across every struct.
type uframes uint32
type sframes int32

const boundaryCap uframes = 0

// The build stops here on an unverified GOARCH. The identifier is intentionally
// undefined so the compiler error itself reads as the reason.
var _ = goAudioCapture_ALSA_backend_unsupported_GOARCH_verify_struct_layout_per_arch_see_issue_12
