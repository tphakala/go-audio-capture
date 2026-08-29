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
	format := flag.String("f", "s16", "sample format: s16 or s32")
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
	if err := writeWAVHeader(w, n.Rate, n.Channels, f.BytesPerSample()); err != nil {
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
	if err := patchWAVSizes(w, dataBytes); err != nil {
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
	default:
		return 0, fmt.Errorf("unknown format %q (want s16 or s32)", s)
	}
}

// writeWAVHeader writes a 44-byte canonical PCM WAV header with placeholder
// sizes; patchWAVSizes fills the RIFF and data lengths in once recording ends.
func writeWAVHeader(w *os.File, rate, channels, bytesPerSample int) error {
	blockAlign := channels * bytesPerSample
	byteRate := rate * blockAlign
	h := make([]byte, 44)
	copy(h[0:4], "RIFF")
	// h[4:8] RIFF size, patched later
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(h[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(h[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(h[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(h[34:36], uint16(bytesPerSample*8))
	copy(h[36:40], "data")
	// h[40:44] data size, patched later
	_, err := w.Write(h)
	return err
}

func patchWAVSizes(w *os.File, dataBytes int64) error {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], uint32(36+dataBytes))
	if _, err := w.WriteAt(buf[:], 4); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(buf[:], uint32(dataBytes))
	if _, err := w.WriteAt(buf[:], 40); err != nil {
		return err
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "gac-rec:", err)
	os.Exit(1)
}
