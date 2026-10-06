//go:build linux && arm

package alsa

// ctlValuePad is the padding between snd_ctl_elem_value's indirect word and its
// value union. ARM EABI aligns long long to 8, so the union starts at offset 72
// and the struct is 712 bytes. An arm64 kernel serving a 32-bit process uses
// control_compat.c, where snd_ctl_elem_value32 keeps its s64 member outside
// CONFIG_X86_64 and so has the same 8-aligned union (linux v6.17
// control_compat.c:140-151). Derived from source, not yet run against a kernel.
const ctlValuePad = 4
