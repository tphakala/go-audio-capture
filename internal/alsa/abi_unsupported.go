//go:build linux && !amd64 && !arm64 && !386 && !arm

package alsa

// This backend's struct layouts and size-encoded ioctl numbers are C-verified
// only for little-endian LP64 (amd64, arm64; see abi_lp64.go) and little-endian
// ILP32 (386, arm; see abi_ilp32.go), with the pinned sizes/offsets/ioctl numbers
// asserted in layout_lp64_test.go and layout_ilp32_test.go.
//
// Any other GOARCH is rejected at compile time rather than silently building a
// binary with the wrong ioctl numbers or, on a big-endian target, the wrong
// snd_interval flag bit-packing (see the Interval type in hwparams.go, whose flag
// layout is little-endian only). To add an architecture, verify its
// sound/asound.h layout on real hardware and extend the build tags in
// abi_lp64.go / abi_ilp32.go. See issue #12.

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
