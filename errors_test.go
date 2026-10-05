package capture

import (
	"errors"
	"testing"
	"time"
)

func TestBadFormatErrorString(t *testing.T) {
	tests := []struct {
		name string
		err  BadFormatError
		want string
	}{
		{"no range", BadFormatError{Channels: 1, Format: FormatS16LE}, "capture: format 1 ch / s16 not supported"},
		{"with range", BadFormatError{Channels: 1, Format: FormatS32LE, MinChannels: 4, MaxChannels: 4}, "capture: format 1 ch / s32 not supported (device accepts 4..4 channels in s32)"},
		{"rate and range", BadFormatError{Rate: 48000, Channels: 1, Format: FormatS16LE, MinChannels: 2, MaxChannels: 8}, "capture: format 1 ch / s16 @ 48000 Hz not supported (device accepts 2..8 channels in s16)"},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("%s: Error() = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestGeometryErrorStringAndUnwrap(t *testing.T) {
	cause := errors.New("driver said no")
	err := &GeometryError{Rate: 44100, PeriodFrames: 896, Periods: 4, Err: cause}
	want := "capture: device refused period geometry at 44100 Hz (896 frames x 4 periods): driver said no"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Error("GeometryError does not unwrap to its cause")
	}
}

func TestShortfallErrorMessageAndUnwrap(t *testing.T) {
	var err error = &ShortfallError{Rate: 48000, Window: 2 * time.Second, Expected: 96000, Delivered: 62400}
	want := "capture: device delivered 62400 of 96000 expected frames at 48000 Hz over 2s"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrDeviceStalled) {
		t.Error("errors.Is(err, ErrDeviceStalled) = false")
	}
	if errors.Is(err, ErrDeviceGone) {
		t.Error("errors.Is(err, ErrDeviceGone) = true")
	}
}
