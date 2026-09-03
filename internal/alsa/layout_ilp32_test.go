//go:build linux && (386 || arm)

package alsa

// ILP32 (386, arm) kernel ABI, verified 2026-09-03 by compiling the same
// offsetof/sizeof probe against /usr/include/sound/asound.h with `gcc -m32`
// (sizeof(long)=4, sizeof(void*)=4). With no 8-byte-aligned field remaining, the
// maximum alignment is 4 and the layout is identical across every little-endian
// ILP32 ABI, so the i386 probe is a valid oracle for arm too (ARM's 8-byte
// alignment of long long/double never comes into play). A 32-bit process reaches
// these same numbers on a 64-bit kernel through the compat_ioctl path
// (snd_pcm_ioctl_compat), so the GOARCH=386-on-x86_64 test run exercises the real
// kernel ABI, not an emulation of it.
//
//	HWPARAMS_SIZE=604 SWPARAMS_SIZE=104 XFERI_SIZE=12
//	hw: masks@4 mres@100 intervals@260 ires@404 rmask@512 fifo@536 sync@540 reserved@556
//	sw: avail_min@12 start_threshold@20 boundary@36 proto@40 reserved@48
//	PVERSION=0x80044100 HW_REFINE=0xc25c4110 HW_PARAMS=0xc25c4111 SW_PARAMS=0xc0684113
//	PREPARE=0x4140 START=0x4142 DROP=0x4143 RESUME=0x4147 READI=0x800c4151
const (
	wantMaskSize     = 32
	wantIntervalSize = 12

	wantHwParamsSize = 604
	wantHwMasks      = 4
	wantHwMres       = 100
	wantHwIntervals  = 260
	wantHwIres       = 404
	wantHwRmask      = 512
	wantHwFifoSize   = 536
	wantHwSync       = 540
	wantHwReserved   = 556

	wantSwParamsSize     = 104
	wantSwAvailMin       = 12
	wantSwStartThreshold = 20
	wantSwBoundary       = 36
	wantSwProto          = 40
	wantSwReserved       = 48

	wantXferiSize = 12

	wantIocPVersion = 0x80044100
	wantIocHwRefine = 0xc25c4110
	wantIocHwParams = 0xc25c4111
	wantIocSwParams = 0xc0684113
	wantIocPrepare  = 0x4140
	wantIocStart    = 0x4142
	wantIocDrop     = 0x4143
	wantIocResume   = 0x4147
	wantIocReadI    = 0x800c4151
)
