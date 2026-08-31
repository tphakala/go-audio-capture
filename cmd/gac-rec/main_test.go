//go:build linux || windows

package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteWAVHeaderFormatTag pins the WAV format tag (offset 20, 2 bytes): 1 for
// integer PCM, 3 (WAVE_FORMAT_IEEE_FLOAT) for float. A reader uses this tag to
// decide whether the samples are integers or floats, so a wrong tag corrupts f32
// playback silently. It also pins the fmt-chunk size (16 for PCM, 18 for the
// non-PCM float header that carries a cbSize field) and the fact chunk.
func TestWriteWAVHeaderFormatTag(t *testing.T) {
	tests := []struct {
		name        string
		isFloat     bool
		wantTag     uint16
		wantFmtSize uint32
		wantLen     int64 // 44 for PCM; 58 for float (fmt 18 + cbSize + fact chunk)
		wantFact    bool
	}{
		{"integer PCM", false, 1, 16, 44, false},
		{"IEEE float", true, 3, 18, 58, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out.wav")
			w, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			lay, err := writeWAVHeader(w, 48000, 1, 4, tt.isFloat)
			if err != nil {
				t.Fatalf("writeWAVHeader: %v", err)
			}
			// Patch as if 10 frames (10 * 4 bytes/frame mono f32) were captured.
			const frameBytes = 4
			const dataBytes = 40
			if err := lay.patchSizes(w, dataBytes, frameBytes); err != nil {
				t.Fatalf("patchSizes: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(b)) != tt.wantLen {
				t.Fatalf("header is %d bytes, want %d", len(b), tt.wantLen)
			}
			if got := binary.LittleEndian.Uint16(b[20:22]); got != tt.wantTag {
				t.Errorf("WAV format tag = %d, want %d", got, tt.wantTag)
			}
			// fmt-chunk size at offset 16: 16 for integer PCM, 18 for a non-PCM
			// format that must declare a cbSize field.
			if got := binary.LittleEndian.Uint32(b[16:20]); got != tt.wantFmtSize {
				t.Errorf("fmt chunk size = %d, want %d", got, tt.wantFmtSize)
			}
			// RIFF size = whole file minus the 8-byte "RIFF"+size prefix.
			if got := binary.LittleEndian.Uint32(b[4:8]); int64(got) != tt.wantLen-8+dataBytes {
				t.Errorf("RIFF size = %d, want %d", got, tt.wantLen-8+dataBytes)
			}
			hasFact := bytes.Contains(b, []byte("fact"))
			if hasFact != tt.wantFact {
				t.Errorf("fact chunk present = %v, want %v", hasFact, tt.wantFact)
			}
			if tt.wantFact {
				// The fact chunk's sample-frame count sits at factSizeOff and must
				// equal dataBytes/frameBytes (10 frames), so a reader knows the length.
				if got := binary.LittleEndian.Uint32(b[lay.factSizeOff : lay.factSizeOff+4]); got != dataBytes/frameBytes {
					t.Errorf("fact sample count = %d, want %d", got, dataBytes/frameBytes)
				}
			}
			// The data-chunk size field must carry the captured byte count.
			if got := binary.LittleEndian.Uint32(b[lay.dataSizeOff : lay.dataSizeOff+4]); got != dataBytes {
				t.Errorf("data size = %d, want %d", got, dataBytes)
			}
		})
	}
}
