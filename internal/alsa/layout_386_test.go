//go:build linux && 386

package alsa

// snd_ctl_elem_value on i386: long long aligns to 4, so the union starts at 68.
// Measured with gcc -m32 against /usr/include/sound/asound.h; matches
// snd_ctl_elem_value32 on an x86_64 kernel (control_compat.c).
const (
	wantCtlValueSize     = 708
	wantCtlValueValue    = 68
	wantCtlValueReserved = 580

	wantIocCtlElemRead  = 0xc2c45512
	wantIocCtlElemWrite = 0xc2c45513
)
