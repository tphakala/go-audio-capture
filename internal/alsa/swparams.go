//go:build linux

package alsa

// SwParams mirrors struct snd_pcm_sw_params. Only AvailMin (wakeup threshold)
// and StartThreshold (set high so capture starts only on an explicit START)
// are configured by this backend; the rest are carried so the struct matches
// the kernel ABI. Boundary and the threshold fields are snd_pcm_uframes_t
// (8-byte unsigned long on the LP64 targets). Field offsets are asserted in
// layout_test.go.
type SwParams struct {
	TstampMode       int32
	PeriodStep       uint32
	SleepMin         uint32
	AvailMin         uint64
	XferAlign        uint64
	StartThreshold   uint64
	StopThreshold    uint64
	SilenceThreshold uint64
	SilenceSize      uint64
	Boundary         uint64
	Proto            uint32
	TstampType       uint32
	Reserved         [56]uint8
}
