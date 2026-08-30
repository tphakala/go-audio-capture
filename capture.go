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

// DeviceInfo identifies a capture-capable PCM device. ID is a stable,
// platform-specific identifier that Config.Device accepts directly: on Linux the
// human-usable "hw:card,device" string (never a hex-encoded token), on Windows
// the WASAPI endpoint-id string. Card and Device are populated on Linux only.
type DeviceInfo struct {
	ID     string
	Card   int
	Device int
	Name   string
}

// RateSupport reports which sample rates a device accepts for a given channel
// count and format, as discovered by SupportedRates. Rates lists the accepted
// standard rates in ascending order. Min and Max bound the raw hardware rate
// window (from a single HW_REFINE): for a device with continuous rate support
// they describe the whole range, so a caller may pick a value not in Rates that
// still falls within [Min, Max].
type RateSupport struct {
	Rates    []int
	Min, Max int
}

// Config requests a capture configuration. Rate is honored exactly or Open
// fails with *BadRateError: there is no silent resampling. PeriodFrames and
// Periods default to a 20 ms period (Rate/50) and 4 periods when left zero on
// Linux; on Windows (WASAPI exclusive mode) the endpoint dictates the buffer
// period, so both fields are ignored and Negotiated reports the actual period.
type Config struct {
	Device       string // Linux "hw:card,device" (e.g. "hw:1,0"); Windows WASAPI endpoint id, or ""/"default"
	Rate         int    // requested sample rate in Hz
	Channels     int    // 1 or 2
	Format       Format
	PeriodFrames int // frames per period; Linux: 0 => Rate/50 (20 ms); ignored on Windows
	Periods      int // periods per buffer; Linux: 0 => 4; ignored on Windows
}
