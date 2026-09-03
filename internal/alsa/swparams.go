//go:build linux

package alsa

// SwParams mirrors struct snd_pcm_sw_params. Only AvailMin (wakeup threshold)
// and StartThreshold (set high so capture starts only on an explicit START)
// are configured by this backend; the rest are carried so the struct matches
// the kernel ABI. Boundary and the threshold fields are snd_pcm_uframes_t (see
// uframes: 8-byte unsigned long on LP64, 4-byte on ILP32), which changes the
// field offsets and the struct size between word sizes. Offsets are asserted per
// word size in layout_{lp64,ilp32}_test.go.
type SwParams struct {
	TstampMode       int32
	PeriodStep       uint32
	SleepMin         uint32
	AvailMin         uframes
	XferAlign        uframes
	StartThreshold   uframes
	StopThreshold    uframes
	SilenceThreshold uframes
	SilenceSize      uframes
	Boundary         uframes
	Proto            uint32
	TstampType       uint32
	Reserved         [56]uint8
}
