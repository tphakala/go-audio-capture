package capture

import "testing"

func TestFormatBytesPerSample(t *testing.T) {
	tests := []struct {
		f    Format
		want int
	}{
		{FormatS16LE, 2},
		{FormatS32LE, 4},
		{FormatF32LE, 4},
		{Format(0), 0},
		{Format(99), 0},
	}
	for _, tt := range tests {
		if got := tt.f.BytesPerSample(); got != tt.want {
			t.Errorf("Format(%d).BytesPerSample() = %d, want %d", int(tt.f), got, tt.want)
		}
	}
}

func TestFormatIsFloat(t *testing.T) {
	if !FormatF32LE.IsFloat() {
		t.Error("FormatF32LE.IsFloat() = false, want true")
	}
	for _, f := range []Format{FormatS16LE, FormatS32LE, Format(0), Format(99)} {
		if f.IsFloat() {
			t.Errorf("Format(%d).IsFloat() = true, want false", int(f))
		}
	}
}

func TestFormatString(t *testing.T) {
	tests := []struct {
		f    Format
		want string
	}{
		{FormatS16LE, "s16"},
		{FormatS32LE, "s32"},
		{FormatF32LE, "f32"},
		{Format(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.f.String(); got != tt.want {
			t.Errorf("Format(%d).String() = %q, want %q", int(tt.f), got, tt.want)
		}
	}
}
