//go:build linux

package alsa

import (
	"math"
	"slices"
	"testing"
)

func w(v int32) uint32 { return uint32(v) }

// item builds a TLV item: type, length in bytes, data.
func item(typ uint32, data ...uint32) []uint32 {
	return append([]uint32{typ, uint32(len(data) * 4)}, data...)
}

func cat(parts ...[]uint32) []uint32 {
	var out []uint32
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestParseDB(t *testing.T) {
	scale := func(minCdB int32, step uint32) []uint32 { return item(tlvDBScale, w(minCdB), step) }
	tests := []struct {
		name   string
		tlv    []uint32
		rawMin int64
		rawMax int64
		want   []DBSegment
		wantOK bool
	}{
		{
			name: "scale without mute", tlv: scale(-1200, 50), rawMin: 0, rawMax: 100, wantOK: true,
			want: []DBSegment{{RawMin: 0, RawMax: 100, MinCdB: -1200, MaxCdB: 3800}},
		},
		{
			name: "scale with mute bit", tlv: scale(-1200, 50|tlvDBScaleMute), rawMin: 0, rawMax: 10, wantOK: true,
			want: []DBSegment{{RawMin: 0, RawMax: 10, MinCdB: -1200, MaxCdB: -700, Mute: true}},
		},
		{
			name: "negative min is signed", tlv: scale(-9999, 1), rawMin: 5, rawMax: 6, wantOK: true,
			want: []DBSegment{{RawMin: 5, RawMax: 6, MinCdB: -9999, MaxCdB: -9998}},
		},
		{
			name: "minmax", tlv: item(tlvDBMinMax, w(-3000), 600), rawMin: 0, rawMax: 40, wantOK: true,
			want: []DBSegment{{RawMin: 0, RawMax: 40, MinCdB: -3000, MaxCdB: 600}},
		},
		{
			name: "minmax with equal raw bounds", tlv: item(tlvDBMinMax, w(-300), 600), rawMin: 7, rawMax: 7, wantOK: true,
			want: []DBSegment{{RawMin: 7, RawMax: 7, MinCdB: -300, MaxCdB: 600}},
		},
		{
			name: "minmax mute", tlv: item(tlvDBMinMaxMute, w(-3000), 600), rawMin: 0, rawMax: 40, wantOK: true,
			want: []DBSegment{{RawMin: 0, RawMax: 40, MinCdB: -3000, MaxCdB: 600, Mute: true}},
		},
		{
			name: "range with one scale entry",
			tlv: item(tlvDBRange,
				cat([]uint32{0, 10}, scale(-5000, 100))...,
			),
			rawMin: 0, rawMax: 20,
			wantOK: true,
			want:   []DBSegment{{RawMin: 0, RawMax: 10, MinCdB: -5000, MaxCdB: -4000}},
		},
		{
			name: "container wrapping a scale", tlv: item(tlvContainer, scale(-600, 100)...),
			rawMin: 0, rawMax: 3, wantOK: true,
			want: []DBSegment{{RawMin: 0, RawMax: 3, MinCdB: -600, MaxCdB: -300}},
		},
		{
			name: "container with only a channel map", tlv: item(tlvContainer, item(0x101, 3, 4)...),
			rawMin: 0, rawMax: 3,
		},
		{
			name:   "container with a channel map then a scale",
			tlv:    item(tlvContainer, cat(item(0x101, 3, 4), scale(-600, 100))...),
			rawMin: 0, rawMax: 3, wantOK: true,
			want: []DBSegment{{RawMin: 0, RawMax: 3, MinCdB: -600, MaxCdB: -300}},
		},
		{
			name:   "container with a scale then a truncated child is malformed",
			tlv:    item(tlvContainer, cat(scale(-600, 100), []uint32{0x101, 400, 3})...),
			rawMin: 0, rawMax: 3,
		},
		{
			name:   "container with a scale then a second scale keeps the first",
			tlv:    item(tlvContainer, cat(scale(-600, 100), scale(-100, 10))...),
			rawMin: 0, rawMax: 3, wantOK: true,
			want: []DBSegment{{RawMin: 0, RawMax: 3, MinCdB: -600, MaxCdB: -300}},
		},
		{
			name:   "container with a scale then a malformed dB child is malformed",
			tlv:    item(tlvContainer, cat(scale(-600, 100), item(tlvDBScale, w(-100)))...),
			rawMin: 0, rawMax: 3,
		},
		{
			name:   "scale over a range too wide to be a control is refused",
			tlv:    scale(-600, 100),
			rawMin: math.MinInt64, rawMax: math.MaxInt64,
		},
		{name: "db linear is not understood", tlv: item(2, w(-600), 0), rawMin: 0, rawMax: 3},
		{
			name:   "length not a multiple of 4 rounds up",
			tlv:    []uint32{tlvDBScale, 5, w(-100), 10},
			rawMin: 0, rawMax: 2, wantOK: true,
			want: []DBSegment{{RawMin: 0, RawMax: 2, MinCdB: -100, MaxCdB: -80}},
		},
		{name: "truncated data", tlv: []uint32{tlvDBScale, 8, w(-100)}, rawMin: 0, rawMax: 2},
		{name: "length past the buffer", tlv: []uint32{tlvDBScale, 400, 0, 0}, rawMin: 0, rawMax: 2},
		{name: "length near uint32 max", tlv: []uint32{tlvDBScale, 0xffffffff, 0, 0}, rawMin: 0, rawMax: 2},
		{name: "zero length", tlv: []uint32{tlvDBScale, 0}, rawMin: 0, rawMax: 2},
		{name: "empty", tlv: nil, rawMin: 0, rawMax: 2},
		{name: "inverted raw bounds", tlv: scale(0, 1), rawMin: 5, rawMax: 2},
		{
			name:   "container nested in a container",
			tlv:    item(tlvContainer, item(tlvContainer, scale(-600, 100)...)...),
			rawMin: 0, rawMax: 3,
		},
		{
			name:   "range entry with inverted bounds",
			tlv:    item(tlvDBRange, cat([]uint32{10, 0}, scale(-5000, 100))...),
			rawMin: 0, rawMax: 20,
		},
		{
			name:   "range entry that is not a dB item",
			tlv:    item(tlvDBRange, cat([]uint32{0, 10}, item(2, 0, 0))...),
			rawMin: 0, rawMax: 20,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseDB(tt.tlv, tt.rawMin, tt.rawMax)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (segments %+v)", ok, tt.wantOK, got)
			}
			if tt.wantOK && !slices.Equal(got, tt.want) {
				t.Errorf("segments = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseDBRangeSecondEntryUsesOwnMinimum(t *testing.T) {
	tlv := item(tlvDBRange, cat(
		[]uint32{0, 10}, item(tlvDBScale, w(-5000), 100),
		[]uint32{11, 20}, item(tlvDBScale, w(-4000), 50),
	)...)
	got, ok := ParseDB(tlv, 0, 20)
	if !ok || len(got) != 2 {
		t.Fatalf("ParseDB = %+v, %v, want two segments", got, ok)
	}
	// 9 steps of 0.50 dB above -40.00 dB; relative to the element's rawMin (0)
	// the span would be 20 steps and give -3000.
	if want := (DBSegment{RawMin: 11, RawMax: 20, MinCdB: -4000, MaxCdB: -3550}); got[1] != want {
		t.Errorf("second segment = %+v, want %+v", got[1], want)
	}
}

func FuzzParseDB(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 8, 0, 0, 0, 0x50, 0xfb, 0xff, 0xff, 0x32, 0, 1, 0}, int64(0), int64(100))
	f.Add([]byte{3, 0, 0, 0, 24, 0, 0, 0, 0, 0, 0, 0, 10, 0, 0, 0, 1, 0, 0, 0, 8, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0}, int64(0), int64(10))
	f.Fuzz(func(t *testing.T, raw []byte, rawMin, rawMax int64) {
		words := make([]uint32, len(raw)/4)
		for i := range words {
			words[i] = uint32(raw[4*i]) | uint32(raw[4*i+1])<<8 | uint32(raw[4*i+2])<<16 | uint32(raw[4*i+3])<<24
		}
		segs, ok := ParseDB(words, rawMin, rawMax)
		if !ok {
			return
		}
		if len(segs) == 0 {
			t.Fatal("ok with no segments")
		}
		for _, s := range segs {
			if s.RawMin > s.RawMax {
				t.Fatalf("segment %+v has inverted raw bounds", s)
			}
		}
	})
}
