//go:build linux

package capture

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	shortfallRate   = 48000
	shortfallPeriod = 960 // 20 ms at 48 kHz; the default geometry is 4 of these (80 ms)
	nsPerSec        = int64(time.Second)
)

// fakeClock replaces the monoNow seam so a test advances time by exactly the
// amount each simulated read takes, with no sleeps. Start is far from zero so a
// window that started at "time 0" by mistake would show.
type fakeClock struct{ now int64 }

func withFakeClock(t *testing.T) *fakeClock {
	t.Helper()
	c := &fakeClock{now: 1_000_000 * nsPerSec}
	prev := monoNow
	monoNow = func() int64 { return c.now }
	t.Cleanup(func() { monoNow = prev })
	return c
}

// newShortfallStream opens a 1 ch S16 stream at 48 kHz with the given geometry
// (0 for the default) on fp and returns it with a period-sized read buffer.
func newShortfallStream(t *testing.T, fp *fakePCM, periodFrames, periods int) (s *Stream, buf []byte) {
	t.Helper()
	withOpenPCM(t, func(_, _ int) (pcm, error) { return fp, nil })
	s, err := Open(Config{Device: hwAddrCard1, Rate: shortfallRate, Channels: 1, Format: FormatS16LE, PeriodFrames: periodFrames, Periods: periods})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, make([]byte, shortfallPeriod*s.frameBytes)
}

// paced returns a readFn that delivers one period per call and advances the
// clock by the time the device needs to produce it at ratio x real time, so
// ratio 1.0 is a healthy device and 0.65 the device from issue 27. Time is
// advanced in exact int64 nanoseconds from a running call count (one period is
// exactly 20 ms), so a healthy stream accumulates no rounding error over
// simulated hours.
func paced(c *fakeClock, ratioNum, ratioDen int64) func() (int, error) {
	const periodNs = int64(shortfallPeriod) * nsPerSec / shortfallRate
	var calls int64
	base := c.now
	return func() (int, error) {
		calls++
		c.now = base + calls*periodNs*ratioDen/ratioNum
		return shortfallPeriod, nil
	}
}

// readFor reads until the fake clock has advanced d, failing on any error, and
// returns the error the first failing Read gave (nil when none).
func readFor(c *fakeClock, s *Stream, buf []byte, d time.Duration) error {
	end := c.now + int64(d)
	for c.now < end {
		if _, err := s.Read(buf); err != nil {
			return err
		}
	}
	return nil
}

func TestShortfallHealthyStreamAtRealTimeNeverFlags(t *testing.T) {
	c := withFakeClock(t)
	fp := &fakePCM{}
	fp.readFn = paced(c, 1, 1)
	s, buf := newShortfallStream(t, fp, 0, 0)
	// 200 s is 100 windows of 2 s, exactly at real time.
	if err := readFor(c, s, buf, 200*time.Second); err != nil {
		t.Fatalf("Read at exactly real time: %v", err)
	}
}

func TestShortfallSlowDeviceFlagsWithTypedError(t *testing.T) {
	c := withFakeClock(t)
	fp := &fakePCM{}
	fp.readFn = paced(c, 65, 100)
	s, buf := newShortfallStream(t, fp, 0, 0)

	start := c.now
	err := readFor(c, s, buf, 60*time.Second)
	if err == nil {
		t.Fatal("a device at 0.65 x real time was never flagged")
	}
	se, ok := errors.AsType[*ShortfallError](err)
	if !ok {
		t.Fatalf("err = %v, want *ShortfallError", err)
	}
	if !errors.Is(err, ErrDeviceStalled) {
		t.Errorf("errors.Is(err, ErrDeviceStalled) = false for %v", err)
	}
	if se.Rate != shortfallRate || se.Window != 2*time.Second {
		t.Errorf("Rate, Window = %d, %v, want %d, 2s", se.Rate, se.Window, shortfallRate)
	}
	if se.Expected <= se.Delivered || se.Delivered <= 0 {
		t.Errorf("Expected %d, Delivered %d: want 0 < Delivered < Expected", se.Expected, se.Delivered)
	}
	// The first window starts after the first read and is 2 s long, so the flag
	// comes at the first evaluation after it, not later.
	if took := time.Duration(c.now - start); took > 4*time.Second {
		t.Errorf("flagged after %v of capture, want within the first window or two", took)
	}
}

func TestShortfallFlagWithGoneDeviceIsErrDeviceGone(t *testing.T) {
	c := withFakeClock(t)
	fp := &fakePCM{probeFn: func() error { return unix.ENODEV }}
	fp.readFn = paced(c, 65, 100)
	s, buf := newShortfallStream(t, fp, 0, 0)
	if err := readFor(c, s, buf, 60*time.Second); !errors.Is(err, ErrDeviceGone) {
		t.Errorf("err = %v, want ErrDeviceGone when the probe shows the device gone", err)
	}
}

func TestShortfallCloseWinsOverFlag(t *testing.T) {
	c := withFakeClock(t)
	fp := &fakePCM{}
	var s *Stream
	paceFn := paced(c, 65, 100)
	fp.readFn = func() (int, error) {
		n, err := paceFn()
		// Close lands while the read that crosses the window is in flight.
		if s.winOn && c.now-s.winStart >= s.window {
			_ = s.Close()
		}
		return n, err
	}
	var buf []byte
	s, buf = newShortfallStream(t, fp, 0, 0)
	if err := readFor(c, s, buf, 60*time.Second); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed: a Close wins over a shortfall", err)
	}
}

func TestShortfallRecoveryRestartsWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"EPIPE overrun", unix.EPIPE},
		{"ESTRPIPE suspend", unix.ESTRPIPE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := withFakeClock(t)
			var recovered []error
			fp := &fakePCM{recoverFn: okRecover(&recovered)}
			slow := paced(c, 65, 100)
			phase := 0 // 0 slow, 1 gap with an error, 2 healthy
			var healthy func() (int, error)
			fp.readFn = func() (int, error) {
				switch phase {
				case 0:
					return slow()
				case 1:
					phase = 2
					// The caller was away for a second, longer than the buffer.
					c.now += nsPerSec
					healthy = paced(c, 1, 1)
					return 0, tc.err
				default:
					return healthy()
				}
			}
			s, buf := newShortfallStream(t, fp, 0, 0)
			// 1.5 s at 0.65 is inside the first 2 s window, so no flag yet.
			if err := readFor(c, s, buf, 1500*time.Millisecond); err != nil {
				t.Fatalf("slow phase flagged inside the first window: %v", err)
			}
			phase = 1
			// Healthy from here on. Without a restart the window would still hold
			// the slow 1.5 s and the 1 s gap and flag at its first evaluation.
			if err := readFor(c, s, buf, 60*time.Second); err != nil {
				t.Fatalf("Read after recovery flagged: %v", err)
			}
			if len(recovered) != 1 {
				t.Fatalf("recoveries = %d, want 1", len(recovered))
			}
		})
	}
}

func TestShortfallPauseLongerThanBufferDoesNotFlag(t *testing.T) {
	c := withFakeClock(t)
	var recovered []error
	fp := &fakePCM{recoverFn: okRecover(&recovered)}
	healthy := paced(c, 1, 1)
	pauses := 0
	next := c.now + 500*int64(time.Millisecond)
	fp.readFn = func() (int, error) {
		// Every 0.5 s the consumer stalls for 300 ms (buffer is 80 ms): the kernel
		// overruns and Read recovers; the stalled time is not a device shortfall.
		if c.now >= next && pauses < 100 {
			pauses++
			c.now += 300 * int64(time.Millisecond)
			next = c.now + 500*int64(time.Millisecond)
			healthy = paced(c, 1, 1)
			return 0, unix.EPIPE
		}
		return healthy()
	}
	s, buf := newShortfallStream(t, fp, 0, 0)
	if err := readFor(c, s, buf, 60*time.Second); err != nil {
		t.Fatalf("Read with periodic consumer stalls flagged: %v", err)
	}
	if len(recovered) == 0 {
		t.Fatal("test did not exercise a recovery")
	}
}

func TestShortfallHalfPercentSlowDeviceOverAnHourDoesNotFlag(t *testing.T) {
	c := withFakeClock(t)
	fp := &fakePCM{}
	fp.readFn = paced(c, 995, 1000)
	s, buf := newShortfallStream(t, fp, 0, 0)
	if err := readFor(c, s, buf, time.Hour); err != nil {
		t.Fatalf("a 0.5%% slow device flagged: %v", err)
	}
}

func TestShortfallWindowScalesWithBuffer(t *testing.T) {
	// A 500 ms buffer (4800 x 5, window 10 s) on a device 20% short. With a fixed
	// 2 s window the buffer alone would credit 25% of the window and hide it.
	c := withFakeClock(t)
	fp := &fakePCM{}
	fp.readFn = paced(c, 80, 100)
	s, _ := newShortfallStream(t, fp, 4800, 5)
	// paced delivers shortfallPeriod frames per call, so a period-sized buffer
	// is plenty.
	buf := make([]byte, shortfallPeriod*s.frameBytes)
	err := readFor(c, s, buf, 60*time.Second)
	se, ok := errors.AsType[*ShortfallError](err)
	if !ok {
		t.Fatalf("err = %v, want *ShortfallError for a 20%% slow device behind a 500 ms buffer", err)
	}
	if se.Window != 10*time.Second {
		t.Errorf("Window = %v, want 10s (20 x the 500 ms buffer)", se.Window)
	}
}

func TestShortfallWithoutStartDoesNotFlag(t *testing.T) {
	// Read before Start, and a clock far from zero: the window starts at the first
	// successful read, not at Open, so the huge clock value cannot read as a long
	// window of nothing delivered.
	c := withFakeClock(t)
	fp := &fakePCM{}
	fp.readFn = paced(c, 1, 1)
	s, buf := newShortfallStream(t, fp, 0, 0)
	if _, err := s.Read(buf); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if err := readFor(c, s, buf, 10*time.Second); err != nil {
		t.Fatalf("Read without Start flagged: %v", err)
	}
}

func TestShortfallClockFarFromZeroKeepsInt64(t *testing.T) {
	// 768 kHz over a 2 s window: expected frames is elapsed_ns x rate, about
	// 1.5e15 before the division by 1e9, which only int64 arithmetic holds.
	c := withFakeClock(t)
	fp := &fakePCM{}
	withOpenPCM(t, func(_, _ int) (pcm, error) { return fp, nil })
	s, err := Open(Config{Device: hwAddrCard1, Rate: 768000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	period := int64(s.Negotiated().PeriodFrames)
	var frames int64
	base := c.now
	fp.readFn = func() (int, error) {
		frames += period
		c.now = base + frames*nsPerSec/768000
		return int(period), nil
	}
	buf := make([]byte, int(period)*s.frameBytes)
	if err := readFor(c, s, buf, 30*time.Second); err != nil {
		t.Fatalf("768 kHz stream at real time flagged: %v", err)
	}
}

func TestShortfallWindowRestartsAfterEachEvaluation(t *testing.T) {
	// 20 s of healthy capture (10 windows), then a device that drops to 0.65 x
	// real time. A window that kept accumulating across evaluations would dilute
	// the slow phase with the healthy 20 s and need about 8 s to flag; a window
	// that restarts flags at its first evaluation after the change.
	c := withFakeClock(t)
	fp := &fakePCM{}
	healthy := paced(c, 1, 1)
	var slow func() (int, error)
	fp.readFn = func() (int, error) {
		if slow != nil {
			return slow()
		}
		return healthy()
	}
	s, buf := newShortfallStream(t, fp, 0, 0)
	if err := readFor(c, s, buf, 20*time.Second); err != nil {
		t.Fatalf("healthy phase flagged: %v", err)
	}
	slow = paced(c, 65, 100)
	changed := c.now
	err := readFor(c, s, buf, 60*time.Second)
	if _, ok := errors.AsType[*ShortfallError](err); !ok {
		t.Fatalf("err = %v, want *ShortfallError after the device slowed down", err)
	}
	if took := time.Duration(c.now - changed); took > 5*time.Second {
		t.Errorf("flagged %v after the slowdown, want within about two windows", took)
	}
}

func TestShortfallThresholdIsTolerancePlusBuffer(t *testing.T) {
	// Default geometry: an 80 ms buffer is 4% of the 2 s window, the tolerance is
	// 10%, so a device flags only when more than about 14% short.
	for _, tc := range []struct {
		name     string
		num, den int64
		wantFlag bool
	}{
		{"7% short is inside the tolerance", 93, 100, false},
		{"12% short is inside tolerance plus buffer", 88, 100, false},
		{"16% short is outside it", 84, 100, true},
		{"20% short is outside it", 80, 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := withFakeClock(t)
			fp := &fakePCM{}
			fp.readFn = paced(c, tc.num, tc.den)
			s, buf := newShortfallStream(t, fp, 0, 0)
			err := readFor(c, s, buf, 60*time.Second)
			if _, flagged := errors.AsType[*ShortfallError](err); flagged != tc.wantFlag {
				t.Errorf("flagged = %v (err %v), want %v", flagged, err, tc.wantFlag)
			}
		})
	}
}
