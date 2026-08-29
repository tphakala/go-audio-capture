//go:build linux

package alsa

// Mask mirrors struct snd_mask: a 256-bit set (SNDRV_MASK_MAX = 256) used for
// the enumerated ACCESS, FORMAT, and SUBFORMAT parameters.
type Mask struct {
	Bits [8]uint32 // (256 + 31) / 32
}

// Interval mirrors struct snd_interval: a [Min, Max] range for the numeric
// parameters (rate, channels, period size, ...). Flags carries the kernel's
// openmin/openmax/integer/empty bitfield, packed low bit first on
// little-endian.
type Interval struct {
	Min   uint32
	Max   uint32
	Flags uint32
}

// Interval flag bits, in the kernel's declaration order (openmin:1, openmax:1,
// integer:1, empty:1).
const (
	intervalOpenMin = 1 << 0
	intervalOpenMax = 1 << 1
	intervalInteger = 1 << 2
	intervalEmpty   = 1 << 3
)

// HwParams mirrors struct snd_pcm_hw_params. Sync (16 bytes) and Reserved
// (48 bytes) together cover the kernel's sync[16] and reserved[48] tail; this
// backend reads neither. Field sizes and offsets are asserted in
// layout_test.go against the kernel ABI.
type HwParams struct {
	Flags     uint32
	Masks     [3]Mask      // ACCESS, FORMAT, SUBFORMAT
	Mres      [5]Mask      // reserved masks
	Intervals [12]Interval // SAMPLE_BITS .. TICK_TIME
	Ires      [9]Interval  // reserved intervals
	Rmask     uint32       // W: which params to refine
	Cmask     uint32       // R: which params changed
	Info      uint32       // R: info flags
	Msbits    uint32       // R: significant bits per sample
	RateNum   uint32       // R: rate numerator
	RateDen   uint32       // R: rate denominator
	FifoSize  uint64       // R: chip FIFO size in frames (snd_pcm_uframes_t)
	Sync      [16]uint8    // R: synchronization ID
	Reserved  [48]uint8
}

// Parameter indices. The mask parameters occupy Masks by (param - ACCESS); the
// interval parameters occupy Intervals by (param - SAMPLE_BITS).
const (
	ParamAccess    = 0
	ParamFormat    = 1
	ParamSubformat = 2

	ParamSampleBits  = 8
	ParamFrameBits   = 9
	ParamChannels    = 10
	ParamRate        = 11
	ParamPeriodTime  = 12
	ParamPeriodSize  = 13
	ParamPeriodBytes = 14
	ParamPeriods     = 15
	ParamBufferTime  = 16
	ParamBufferSize  = 17
	ParamBufferBytes = 18
	ParamTickTime    = 19

	paramFirstMask     = ParamAccess
	paramFirstInterval = ParamSampleBits
)

// Access, format, and subformat values (subset this backend uses). Values are
// the SNDRV_PCM_ACCESS_* / FORMAT_* / SUBFORMAT_* enum members.
const (
	AccessRWInterleaved = 3

	FormatS16LE = 2
	FormatS32LE = 10

	SubformatSTD = 0
)

// mask returns the Mask for a mask-type parameter (ACCESS/FORMAT/SUBFORMAT).
func (p *HwParams) mask(param int) *Mask {
	return &p.Masks[param-paramFirstMask]
}

// interval returns the Interval for an interval-type parameter.
func (p *HwParams) interval(param int) *Interval {
	return &p.Intervals[param-paramFirstInterval]
}

// FillAny opens every parameter to its full range, the equivalent of alsa-lib's
// snd_pcm_hw_params_any: all mask bits set, all intervals [0, MaxUint32], and
// Rmask requesting a refine of everything. HW_REFINE then narrows each range to
// what the hardware actually supports.
func (p *HwParams) FillAny() {
	for i := range p.Masks {
		for j := range p.Masks[i].Bits {
			p.Masks[i].Bits[j] = ^uint32(0)
		}
	}
	for i := range p.Intervals {
		p.Intervals[i].Min = 0
		p.Intervals[i].Max = ^uint32(0)
		p.Intervals[i].Flags = 0
	}
	p.Rmask = ^uint32(0)
	p.Info = ^uint32(0)
}

// SetMask restricts a mask parameter to the single bit value, e.g.
// SetMask(ParamAccess, AccessRWInterleaved).
func (p *HwParams) SetMask(param int, value uint) {
	m := p.mask(param)
	*m = Mask{}
	m.Bits[value>>5] = 1 << (value & 31)
}

// SetIntervalExact pins an interval parameter to a single value and marks it
// integer, so HW_PARAMS accepts nothing but that exact value (used to hold the
// requested sample rate: no silent substitution).
func (p *HwParams) SetIntervalExact(param int, value uint32) {
	iv := p.interval(param)
	iv.Min = value
	iv.Max = value
	iv.Flags = intervalInteger
}

// Interval returns the current [min, max] of an interval parameter, e.g. after
// HW_REFINE to read the supported rate range. The results are named lo/hi to
// avoid shadowing the predeclared min/max builtins.
func (p *HwParams) Interval(param int) (lo, hi uint32) {
	iv := p.interval(param)
	return iv.Min, iv.Max
}
