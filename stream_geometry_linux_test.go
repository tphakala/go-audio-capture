//go:build linux

package capture

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/tphakala/go-audio-capture/internal/alsa"
)

// openGeometry opens hwAddrCard1 through the fake PCM and returns the geometry
// Negotiate received and the one Negotiated reports.
func openGeometry(t *testing.T, rate, periodFrames, periods int) (gotFrames, gotPeriods int, neg Config) {
	t.Helper()
	f := &fakePCM{}
	defer swapOpenPCM(f)()
	s, err := Open(Config{Device: hwAddrCard1, Rate: rate, Channels: 1, Format: FormatS16LE, PeriodFrames: periodFrames, Periods: periods})
	if err != nil {
		t.Fatalf("Open(%d Hz, %d x %d): %v", rate, periodFrames, periods, err)
	}
	defer func() { _ = s.Close() }()
	return f.gotPeriodFrames, f.gotPeriods, s.Negotiated()
}

func TestOpenRaisesGeometryBelowFloor(t *testing.T) {
	tests := []struct {
		name                  string
		rate, frames, periods int
		wantFrames, wantN     int
	}{
		{"issue repro 8x4", 48000, 8, 4, 48, 20},
		{"period raised then periods", 48000, 16, 16, 48, 20},
		{"default periods then buffer floor", 48000, 24, 0, 48, 20},
		{"aloop silent-loss geometry", 48000, 48, 2, 48, 20},
		{"period fine, buffer raised", 48000, 480, 1, 480, 2},
		{"44.1 kHz rounds up", 44100, 30, 4, 45, 20},
		{"384 kHz", 384000, 100, 4, 384, 20},
		{"8 kHz", 8000, 4, 2, 8, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gf, gn, neg := openGeometry(t, tt.rate, tt.frames, tt.periods)
			if gf != tt.wantFrames || gn != tt.wantN {
				t.Errorf("Negotiate got %d x %d, want %d x %d", gf, gn, tt.wantFrames, tt.wantN)
			}
			if neg.PeriodFrames != tt.wantFrames || neg.Periods != tt.wantN {
				t.Errorf("Negotiated %d x %d, want %d x %d", neg.PeriodFrames, neg.Periods, tt.wantFrames, tt.wantN)
			}
		})
	}
}

func TestOpenRejectsNegativeGeometry(t *testing.T) {
	tests := []struct {
		name      string
		cfg       Config
		wantField string
	}{
		{"negative period", Config{PeriodFrames: -1}, "periodFrames"},
		{"negative periods", Config{Periods: -1}, "periods"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withOpenPCM(t, func(_, _ int) (pcm, error) {
				t.Fatal("openPCM called for an invalid config")
				return nil, errors.New("unexpected openPCM")
			})
			cfg := tt.cfg
			cfg.Device, cfg.Rate, cfg.Channels, cfg.Format = hwAddrCard1, 48000, 1, FormatS16LE
			_, err := Open(cfg)
			ce, ok := errors.AsType[*ConfigError](err)
			if !ok || ce.Field != tt.wantField {
				t.Fatalf("Open err = %v, want *ConfigError field %q", err, tt.wantField)
			}
			_, err = OpenDevice(DeviceInfo{ID: hwAddrCard1, Card: 1, Device: 0}, cfg)
			if ce, ok := errors.AsType[*ConfigError](err); !ok || ce.Field != tt.wantField {
				t.Fatalf("OpenDevice err = %v, want *ConfigError field %q", err, tt.wantField)
			}
		})
	}
}

func TestOpenDefaultGeometryUnchangedByFloor(t *testing.T) {
	for _, rate := range []int{1, 49, 99, 100, 8000, 22050, 44100, 48000, 96000, 192000, 384000, 768000} {
		gf, gn, _ := openGeometry(t, rate, 0, 0)
		if wf := alsa.DefaultPeriodFrames(rate); gf != wf || gn != alsa.DefaultPeriods {
			t.Errorf("rate %d: Negotiate got %d x %d, want default %d x %d", rate, gf, gn, wf, alsa.DefaultPeriods)
		}
	}
}

func TestOpenGeometryAtOrAboveFloorUnchanged(t *testing.T) {
	for _, g := range [][2]int{{48, 20}, {960, 1}, {96, 10}, {960, 2}, {4096, 8}} {
		gf, gn, neg := openGeometry(t, 48000, g[0], g[1])
		if gf != g[0] || gn != g[1] || neg.PeriodFrames != g[0] || neg.Periods != g[1] {
			t.Errorf("48000 %d x %d: Negotiate %d x %d, Negotiated %d x %d, want unchanged", g[0], g[1], gf, gn, neg.PeriodFrames, neg.Periods)
		}
	}
}

func ceilDiv(a, b uint64) uint64 { return (a + b - 1) / b }

func TestGeometryFloorProperties(t *testing.T) {
	check := func(rate, pf, np int) {
		t.Helper()
		gf, gn := applyGeometryFloor(rate, pf, np)
		if gf < pf || gn < np {
			t.Fatalf("(%d,%d,%d) lowered to %d x %d", rate, pf, np, gf, gn)
		}
		if uint64(gf) < ceilDiv(uint64(rate), 1000) {
			t.Fatalf("(%d,%d,%d): period %d below 1 ms", rate, pf, np, gf)
		}
		if uint64(gn) < ceilDiv(ceilDiv(uint64(rate), 50), uint64(gf)) {
			t.Fatalf("(%d,%d,%d): %d x %d below 20 ms buffer", rate, pf, np, gf, gn)
		}
		if f2, n2 := applyGeometryFloor(rate, gf, gn); f2 != gf || n2 != gn {
			t.Fatalf("(%d,%d,%d): not idempotent, %d x %d -> %d x %d", rate, pf, np, gf, gn, f2, n2)
		}
		meets := uint64(pf) >= ceilDiv(uint64(rate), 1000) && uint64(np) >= ceilDiv(ceilDiv(uint64(rate), 50), uint64(pf))
		if meets && (gf != pf || gn != np) {
			t.Fatalf("(%d,%d,%d) already meets the floor but became %d x %d", rate, pf, np, gf, gn)
		}
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		pf := 1 + r.IntN(5000)
		np := 1 + r.IntN(64)
		switch r.IntN(4) {
		case 0:
			pf = 1 + r.IntN(math.MaxInt)
		case 1:
			np = 1 + r.IntN(math.MaxInt)
		}
		check(1+r.IntN(1_000_000), pf, np)
	}
	check(math.MaxInt32, math.MaxInt, 1)
	check(math.MaxInt32, 1, 1)
	check(1, 1, 1)
}

func TestOpenDeviceAppliesGeometryFloor(t *testing.T) {
	useFixture(t, hostLayout())
	d := mustResolve(t, wantSerialID)
	f := &fakePCM{}
	defer swapOpenPCM(f)()
	cfg := odCfg
	cfg.PeriodFrames, cfg.Periods = 8, 4
	s, err := OpenDevice(d, cfg)
	if err != nil {
		t.Fatalf("OpenDevice: %v", err)
	}
	defer func() { _ = s.Close() }()
	wantF, wantN := applyGeometryFloor(cfg.Rate, 8, 4)
	if wantF == 8 || f.gotPeriodFrames != wantF || f.gotPeriods != wantN {
		t.Errorf("Negotiate got %d x %d, want raised %d x %d", f.gotPeriodFrames, f.gotPeriods, wantF, wantN)
	}
}

func TestReopenWithNegotiatedKeepsGeometry(t *testing.T) {
	f1 := &fakePCM{}
	restore := swapOpenPCM(f1)
	s, err := Open(Config{Device: hwAddrCard1, Rate: 48000, Channels: 1, Format: FormatS16LE, PeriodFrames: 8, Periods: 4})
	restore()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	neg := s.Negotiated()
	_ = s.Close()

	f2 := &fakePCM{}
	defer swapOpenPCM(f2)()
	s2, err := Open(neg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = s2.Close()
	if f2.gotPeriodFrames != f1.gotPeriodFrames || f2.gotPeriods != f1.gotPeriods {
		t.Errorf("reopen Negotiate got %d x %d, first open got %d x %d", f2.gotPeriodFrames, f2.gotPeriods, f1.gotPeriodFrames, f1.gotPeriods)
	}
	if f2.gotPeriodFrames != 48 || f2.gotPeriods != 20 {
		t.Errorf("reopen Negotiate got %d x %d, want 48 x 20", f2.gotPeriodFrames, f2.gotPeriods)
	}
}
