//go:build linux

package alsa

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// _IOC bit layout from the kernel's include/uapi/asm-generic/ioctl.h. The
// request number packs a direction, the payload size, a type ("magic") byte,
// and a per-command number.
const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits     // 8
	iocSizeShift = iocTypeShift + iocTypeBits // 16
	iocDirShift  = iocSizeShift + iocSizeBits // 30

	iocNone  = 0
	iocWrite = 1
	iocRead  = 2

	// iocMagic is the 'A' type byte all SNDRV_PCM_IOCTL_* commands share.
	iocMagic = 'A'

	// iocCtlMagic is the 'U' type byte all SNDRV_CTL_IOCTL_* commands share (the
	// control interface on /dev/snd/controlC*), distinct from the PCM 'A'.
	iocCtlMagic = 'U'
)

// ioc encodes an ioctl request number exactly as the kernel's _IOC macro does:
// (dir << 30) | (size << 16) | (typ << 8) | nr.
func ioc(dir, typ, nr, size uintptr) uintptr {
	return (dir << iocDirShift) | (typ << iocTypeShift) | (nr << iocNRShift) | (size << iocSizeShift)
}

// Xferi is the argument to SNDRV_PCM_IOCTL_READI_FRAMES / WRITEI_FRAMES
// (struct snd_xferi). Result is snd_pcm_sframes_t (a signed long, see sframes),
// Frames is snd_pcm_uframes_t (an unsigned long, see uframes); both track the
// word size (8 bytes on LP64, 4 on ILP32), which sets the struct size and hence
// the READI_FRAMES ioctl number. Buf is kept as unsafe.Pointer, not uintptr, so
// the GC keeps the referenced capture buffer alive for the duration of the ioctl.
type Xferi struct {
	Result sframes
	Buf    unsafe.Pointer
	Frames uframes
}

// SNDRV_PCM ioctl request numbers, computed from the actual Go struct sizes so
// they can never drift from the layouts this package defines. layout_test.go
// pins each result to the kernel's value.
var (
	iocPVersion    = ioc(iocRead, iocMagic, 0x00, unsafe.Sizeof(int32(0)))
	iocHwRefine    = ioc(iocRead|iocWrite, iocMagic, 0x10, unsafe.Sizeof(HwParams{}))
	iocHwParams    = ioc(iocRead|iocWrite, iocMagic, 0x11, unsafe.Sizeof(HwParams{}))
	iocHwFree      = ioc(iocNone, iocMagic, 0x12, 0)
	iocSwParams    = ioc(iocRead|iocWrite, iocMagic, 0x13, unsafe.Sizeof(SwParams{}))
	iocPrepare     = ioc(iocNone, iocMagic, 0x40, 0)
	iocStart       = ioc(iocNone, iocMagic, 0x42, 0)
	iocDrop        = ioc(iocNone, iocMagic, 0x43, 0)
	iocResume      = ioc(iocNone, iocMagic, 0x47, 0)
	iocReadIFrames = ioc(iocRead, iocMagic, 0x51, unsafe.Sizeof(Xferi{}))
)

// SNDRV_CTL ioctl request numbers, derived from the Go struct sizes like the PCM
// ones above and pinned in layout_test.go. ELEM_READ and ELEM_WRITE carry
// snd_ctl_elem_value, whose size differs between the 386 and arm ABIs (see
// abi_386.go and abi_arm.go). TLV_READ encodes only the 8-byte snd_ctl_tlv
// header; the TLV payload follows it in the caller's buffer.
var (
	iocCtlPVersion  = ioc(iocRead, iocCtlMagic, 0x00, unsafe.Sizeof(int32(0)))
	iocCtlElemList  = ioc(iocRead|iocWrite, iocCtlMagic, 0x10, unsafe.Sizeof(ctlElemList{}))
	iocCtlElemInfo  = ioc(iocRead|iocWrite, iocCtlMagic, 0x11, unsafe.Sizeof(CtlElemInfo{}))
	iocCtlElemRead  = ioc(iocRead|iocWrite, iocCtlMagic, 0x12, unsafe.Sizeof(ctlElemValue{}))
	iocCtlElemWrite = ioc(iocRead|iocWrite, iocCtlMagic, 0x13, unsafe.Sizeof(ctlElemValue{}))
	iocCtlTLVRead   = ioc(iocRead|iocWrite, iocCtlMagic, 0x1a, unsafe.Sizeof(ctlTLVHeader{}))
)

// ioctl issues a raw ioctl. arg points at the command's argument struct; the
// caller must keep the pointed-at value alive across the call (passing
// unsafe.Pointer through to the syscall, and using runtime.KeepAlive where the
// struct embeds further pointers, as PCM.ReadI does for Xferi.Buf).
func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}
