//go:build linux || windows

// Command gac-rec is a spike/debug tool: it records a few seconds of audio from
// a capture device through go-audio-capture and writes a WAV file, printing the
// negotiated parameters and the xrun count on exit. It exists to validate
// capture on real hardware (including high-rate ultrasonic mics), not as a
// supported end-user tool. It is cross-platform: the ALSA (Linux) and WASAPI
// (Windows) backends are selected by build tag, and only the default device
// string differs per platform (see defaultDevice).
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tphakala/go-audio-capture"
)

func main() {
	device := flag.String("d", defaultDevice, "capture device id (Linux: hw:card,device; Windows: WASAPI endpoint id, or empty/\"default\")")
	rate := flag.Int("r", 48000, "sample rate in Hz")
	channels := flag.Int("c", 1, "channel count")
	format := flag.String("f", "s16", "sample format: s16, s32, or f32")
	dur := flag.Duration("t", 10*time.Second, "record duration")
	out := flag.String("o", "out.wav", "output WAV file")
	list := flag.Bool("list", false, "list capture devices and exit")
	flag.Parse()

	if *list {
		if err := listDevices(); err != nil {
			fatal(err)
		}
		return
	}
	if err := record(*device, *rate, *channels, *format, *dur, *out); err != nil {
		fatal(err)
	}
}

func listDevices() error {
	devs, err := capture.Devices()
	if err != nil {
		return err
	}
	for _, d := range devs {
		fmt.Printf("%-10s %s\n", d.ID, d.Name)
	}
	return nil
}

func record(device string, rate, channels int, format string, dur time.Duration, out string) error {
	f, err := parseFormat(format)
	if err != nil {
		return err
	}
	s, err := capture.Open(capture.Config{Device: device, Rate: rate, Channels: channels, Format: f})
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	n := s.Negotiated()
	fmt.Fprintf(os.Stderr, "negotiated: %d Hz, %d ch, %s, period %d frames, %d periods\n",
		n.Rate, n.Channels, n.Format, n.PeriodFrames, n.Periods)

	w, err := os.Create(out) //nolint:gosec // path is an operator-supplied CLI flag
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	frameBytes := n.Channels * f.BytesPerSample()
	wav, err := writeWAVHeader(w, n.Rate, n.Channels, f.BytesPerSample(), f.IsFloat())
	if err != nil {
		return err
	}

	if err := s.Start(); err != nil {
		return err
	}
	buf := make([]byte, n.PeriodFrames*frameBytes)
	start := time.Now()
	deadline := start.Add(dur)
	var dataBytes int64
	for time.Now().Before(deadline) {
		frames, rerr := s.Read(buf)
		if rerr != nil {
			return rerr
		}
		wb := frames * frameBytes
		if _, werr := w.Write(buf[:wb]); werr != nil {
			return werr
		}
		dataBytes += int64(wb)
	}
	wall := time.Since(start)
	if err := wav.patchSizes(w, dataBytes, frameBytes); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d bytes to %s, xruns: %d\n", dataBytes, out, s.Xruns())
	// Real-time capture must produce audio duration ~= wall time. A ratio far
	// from 1.0 means Stream.Read mis-accounted frames (over- or under-delivery).
	if frameBytes > 0 && wall > 0 {
		totalFrames := dataBytes / int64(frameBytes)
		audioSec := float64(totalFrames) / float64(n.Rate)
		fmt.Fprintf(os.Stderr, "timing: %d frames = %.3fs audio in %.3fs wall (ratio %.3f; 1.000 == real-time)\n",
			totalFrames, audioSec, wall.Seconds(), audioSec/wall.Seconds())
	}
	return nil
}

func parseFormat(s string) (capture.Format, error) {
	switch s {
	case "s16":
		return capture.FormatS16LE, nil
	case "s32":
		return capture.FormatS32LE, nil
	case "f32":
		return capture.FormatF32LE, nil
	default:
		return 0, fmt.Errorf("unknown format %q (want s16, s32, or f32)", s)
	}
}

// wavLayout records the header length and the byte offsets whose sizes are only
// known once recording ends, so patchSizes can fill them in. factSizeOff is 0
// when there is no fact chunk (integer PCM).
type wavLayout struct {
	headerLen   int64
	dataSizeOff int64 // offset of the data-chunk size field
	factSizeOff int64 // offset of the fact-chunk sample-count field, or 0
}

// writeWAVHeader writes the RIFF/WAVE header with placeholder sizes and reports
// where patchSizes must later fill them in. isFloat selects WAVE_FORMAT_IEEE_FLOAT
// (tag 3), which a reader needs to interpret f32 samples correctly; the WAV spec
// requires a fact chunk (sample-frame count) for every non-PCM format, so the
// float header carries one. Integer PCM (tag 1) uses the canonical 44-byte header
// with no fact chunk.
func writeWAVHeader(w *os.File, rate, channels, bytesPerSample int, isFloat bool) (wavLayout, error) {
	blockAlign := channels * bytesPerSample
	byteRate := rate * blockAlign
	formatTag := uint16(1) // WAVE_FORMAT_PCM
	if isFloat {
		formatTag = 3 // WAVE_FORMAT_IEEE_FLOAT
	}

	h := make([]byte, 0, 68)
	h = append(h, "RIFF"...)
	h = append(h, 0, 0, 0, 0) // [4:8] RIFF size, patched later
	h = append(h, "WAVE"...)
	h = append(h, "fmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16) // fmt chunk size
	h = binary.LittleEndian.AppendUint16(h, formatTag)
	h = binary.LittleEndian.AppendUint16(h, uint16(channels))
	h = binary.LittleEndian.AppendUint32(h, uint32(rate))
	h = binary.LittleEndian.AppendUint32(h, uint32(byteRate))
	h = binary.LittleEndian.AppendUint16(h, uint16(blockAlign))
	h = binary.LittleEndian.AppendUint16(h, uint16(bytesPerSample*8))

	var lay wavLayout
	if isFloat {
		h = append(h, "fact"...)
		h = binary.LittleEndian.AppendUint32(h, 4) // fact chunk size
		lay.factSizeOff = int64(len(h))
		h = append(h, 0, 0, 0, 0) // sample-frame count, patched later
	}
	h = append(h, "data"...)
	lay.dataSizeOff = int64(len(h))
	h = append(h, 0, 0, 0, 0) // data size, patched later
	lay.headerLen = int64(len(h))

	if _, err := w.Write(h); err != nil {
		return wavLayout{}, err
	}
	return lay, nil
}

// patchSizes fills in the RIFF, data, and (for float) fact sizes once dataBytes
// of samples have been written. frameBytes is the interleaved frame size, used to
// derive the fact chunk's sample-frame count.
func (lay wavLayout) patchSizes(w *os.File, dataBytes int64, frameBytes int) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(lay.headerLen-8+dataBytes)) // RIFF size
	if _, err := w.WriteAt(buf[:], 4); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(buf[:], uint32(dataBytes))
	if _, err := w.WriteAt(buf[:], lay.dataSizeOff); err != nil {
		return err
	}
	if lay.factSizeOff != 0 && frameBytes > 0 {
		binary.LittleEndian.PutUint32(buf[:], uint32(dataBytes/int64(frameBytes)))
		if _, err := w.WriteAt(buf[:], lay.factSizeOff); err != nil {
			return err
		}
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gac-rec:", err)
	os.Exit(1)
}
