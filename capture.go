package capture

// Format is a PCM sample format. Phase 1 supports signed 16- and 32-bit
// little-endian; wider or float formats come later.
type Format int

const (
	// FormatS16LE is signed 16-bit little-endian PCM.
	FormatS16LE Format = iota + 1
	// FormatS32LE is signed 32-bit little-endian PCM.
	FormatS32LE
)

// BytesPerSample returns the size of one sample in bytes, or 0 for an unknown
// format.
func (f Format) BytesPerSample() int {
	switch f {
	case FormatS16LE:
		return 2
	case FormatS32LE:
		return 4
	default:
		return 0
	}
}

// String returns the ALSA-style format token.
func (f Format) String() string {
	switch f {
	case FormatS16LE:
		return "s16"
	case FormatS32LE:
		return "s32"
	default:
		return "unknown"
	}
}

// DeviceInfo identifies a capture-capable PCM device. ID is the stable,
// human-usable "hw:card,device" string (never a hex-encoded token) that Config
// and the ALSA tooling accept directly.
type DeviceInfo struct {
	ID     string
	Card   int
	Device int
	Name   string
}

// Config requests a capture configuration. Rate is honored exactly or Open
// fails with *BadRateError: there is no silent resampling. PeriodFrames and
// Periods default to a 20 ms period (Rate/50) and 4 periods when left zero.
type Config struct {
	Device       string // "hw:card,device", e.g. "hw:1,0"
	Rate         int    // requested sample rate in Hz
	Channels     int    // 1 or 2
	Format       Format
	PeriodFrames int // frames per period; 0 => Rate/50 (20 ms)
	Periods      int // periods per buffer; 0 => 4
}
