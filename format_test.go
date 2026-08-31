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

func TestParseFormat(t *testing.T) {
	tests := []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{"s16", FormatS16LE, false},
		{"s32", FormatS32LE, false},
		{"f32", FormatF32LE, false},
		{"", 0, true},
		{"u8", 0, true},
		{"S16", 0, true}, // case-sensitive: only the lowercase token parses
	}
	for _, tt := range tests {
		got, err := ParseFormat(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseFormat(%q) err = nil, want error", tt.in)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseFormat(%q) = (%v, %v), want (%v, nil)", tt.in, got, err, tt.want)
		}
	}
}

// TestParseFormatRoundTrip pins that ParseFormat is the inverse of String for
// every real format, so the two token maps cannot silently drift apart.
func TestParseFormatRoundTrip(t *testing.T) {
	for _, f := range []Format{FormatS16LE, FormatS32LE, FormatF32LE} {
		got, err := ParseFormat(f.String())
		if err != nil || got != f {
			t.Errorf("ParseFormat(%q) = (%v, %v), want (%v, nil)", f.String(), got, err, f)
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
