//go:build linux && arm

package alsa

// snd_ctl_elem_value on ARM EABI: long long aligns to 8, so the union starts at
// 72 and the struct is 712 bytes. Derived from AAPCS alignment and from
// snd_ctl_elem_value32 on a non-x86 64-bit kernel (control_compat.c:140-151);
// checked live only through a GOARCH=arm binary on an aarch64 kernel (the
// compat path), not on an armv7 kernel. A wrong value fails every ELEM_READ and
// ELEM_WRITE with ENOTTY, it cannot corrupt data.
const (
	wantCtlValueSize     = 712
	wantCtlValueValue    = 72
	wantCtlValueReserved = 584

	wantIocCtlElemRead  = 0xc2c85512
	wantIocCtlElemWrite = 0xc2c85513
)
