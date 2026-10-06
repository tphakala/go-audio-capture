//go:build linux && 386

package alsa

// ctlValuePad is the padding between snd_ctl_elem_value's indirect word and its
// value union. On i386 long long aligns to 4, so the union starts at offset 68
// and the struct is 708 bytes. A 32-bit process on an x86_64 kernel reaches the
// same layout through control_compat.c, whose snd_ctl_elem_value32 compiles the
// s64 member out under CONFIG_X86_64 (linux v6.17 control_compat.c:140-151).
const ctlValuePad = 0
