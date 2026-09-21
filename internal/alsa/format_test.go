//go:build linux

package alsa

import "testing"

// TestFormatConstants pins each SNDRV_PCM_FORMAT_* id this backend requests to its
// kernel ABI value (include/uapi/sound/asound.h). These constants go straight into
// the HW_PARAMS format mask, so a wrong value silently negotiates a different wire
// format than the caller asked for, and every higher-level test drives a fake
// ioctl that echoes whatever id it is given, so nothing else in the suite would
// catch a typo here. The literals are the guard.
func TestFormatConstants(t *testing.T) {
	tests := []struct {
		name string
		got  int
		want int
	}{
		{"S16_LE", FormatS16LE, 2},
		{"S24_LE", FormatS24LE, 6},
		{"S32_LE", FormatS32LE, 10},
		{"FLOAT_LE", FormatFloatLE, 14},
		{"S24_3LE", FormatS243LE, 32},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %d, want %d (SNDRV_PCM_FORMAT_%s)", tt.name, tt.got, tt.want, tt.name)
		}
	}
}
