//go:build linux && (amd64 || arm64)

package alsa

// LP64 (amd64, arm64) kernel ABI, verified 2026-08-29 by compiling an
// offsetof/sizeof probe against /usr/include/sound/asound.h (identical on both
// arches: LP64, every field fixed-width or an 8-byte unsigned long):
//
//	HWPARAMS_SIZE=608 SWPARAMS_SIZE=136 XFERI_SIZE=24
//	hw: masks@4 mres@100 intervals@260 ires@404 rmask@512 fifo@536 sync@544 reserved@560
//	sw: avail_min@16 start_threshold@32 boundary@64 proto@72 reserved@80
//	PVERSION=0x80044100 HW_REFINE=0xc2604110 HW_PARAMS=0xc2604111 SW_PARAMS=0xc0884113
//	PREPARE=0x4140 START=0x4142 DROP=0x4143 RESUME=0x4147 READI=0x80184151
const (
	wantMaskSize     = 32
	wantIntervalSize = 12

	wantHwParamsSize = 608
	wantHwMasks      = 4
	wantHwMres       = 100
	wantHwIntervals  = 260
	wantHwIres       = 404
	wantHwRmask      = 512
	wantHwFifoSize   = 536
	wantHwSync       = 544
	wantHwReserved   = 560

	wantSwParamsSize     = 136
	wantSwAvailMin       = 16
	wantSwStartThreshold = 32
	wantSwBoundary       = 64
	wantSwProto          = 72
	wantSwReserved       = 80

	wantXferiSize = 24

	wantIocPVersion = 0x80044100
	wantIocHwRefine = 0xc2604110
	wantIocHwParams = 0xc2604111
	wantIocSwParams = 0xc0884113
	wantIocPrepare  = 0x4140
	wantIocStart    = 0x4142
	wantIocDrop     = 0x4143
	wantIocResume   = 0x4147
	wantIocReadI    = 0x80184151
)
