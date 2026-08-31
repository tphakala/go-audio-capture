//go:build linux

package capture

import (
	"errors"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeRatePCM is a hardware-free ratePCM for exercising SupportedRates' public
// layer. It records the arguments it was called with and returns canned results.
type fakeRatePCM struct {
	rates      []int
	lo, hi     int
	ratesErr   error
	gotCh      int
	gotFormat  uint32
	gotCands   []int
	closeCalls int
	// verifiable is the set of rates VerifyRate reports as truly committable;
	// verifyErr, when set, makes every VerifyRate call fail with it.
	verifiable map[int]bool
	verifyErr  error
	gotVerify  []int
}

func (f *fakeRatePCM) SupportedRates(channels int, format uint32, candidates []int) (rates []int, lo, hi int, err error) {
	f.gotCh, f.gotFormat, f.gotCands = channels, format, candidates
	return f.rates, f.lo, f.hi, f.ratesErr
}

func (f *fakeRatePCM) VerifyRate(_ int, _ uint32, rate int) (bool, error) {
	f.gotVerify = append(f.gotVerify, rate)
	if f.verifyErr != nil {
		return false, f.verifyErr
	}
	return f.verifiable[rate], nil
}

// errShouldNotOpen lets the "unreachable after t.Fatal" seams satisfy the
// (value, error) contract without tripping the nilnil linter.
var errShouldNotOpen = errors.New("open should not be reached")

func (f *fakeRatePCM) Close() error { f.closeCalls++; return nil }

// withOpenRatePCM swaps the seam for the duration of a test.
func withOpenRatePCM(t *testing.T, fn func(card, device int) (ratePCM, error)) {
	t.Helper()
	prev := openRatePCM
	openRatePCM = fn
	t.Cleanup(func() { openRatePCM = prev })
}

func TestSupportedRatesHappyPath(t *testing.T) {
	fake := &fakeRatePCM{rates: []int{48000, 96000, 192000, 384000}, lo: 8000, hi: 384000}
	var gotCard, gotDev int
	withOpenRatePCM(t, func(card, device int) (ratePCM, error) {
		gotCard, gotDev = card, device
		return fake, nil
	})

	got, err := SupportedRates("hw:1,0", 1, FormatS32LE)
	if err != nil {
		t.Fatalf("SupportedRates: %v", err)
	}
	want := RateSupport{Rates: []int{48000, 96000, 192000, 384000}, Min: 8000, Max: 384000}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RateSupport = %+v, want %+v", got, want)
	}
	if gotCard != 1 || gotDev != 0 {
		t.Errorf("opened card=%d device=%d, want 1,0", gotCard, gotDev)
	}
	if fake.gotCh != 1 || fake.gotFormat != 10 /* alsa.FormatS32LE */ {
		t.Errorf("probe called with ch=%d format=%d, want 1, 10", fake.gotCh, fake.gotFormat)
	}
	if !reflect.DeepEqual(fake.gotCands, standardRates) {
		t.Errorf("probe candidates = %v, want standardRates", fake.gotCands)
	}
	if fake.closeCalls != 1 {
		t.Errorf("Close called %d times, want 1", fake.closeCalls)
	}
}

func TestSupportedRatesVerifiedDropsRefineLie(t *testing.T) {
	// Refine advertises 48000 and 384000; only 384000 actually commits (the
	// AudioMoth over-report). The verified probe must drop 48000.
	fake := &fakeRatePCM{
		rates:      []int{48000, 384000},
		lo:         48000,
		hi:         384000,
		verifiable: map[int]bool{384000: true},
	}
	withOpenRatePCM(t, func(int, int) (ratePCM, error) { return fake, nil })

	got, err := SupportedRatesVerified("hw:2,0", 1, FormatS32LE)
	if err != nil {
		t.Fatalf("SupportedRatesVerified: %v", err)
	}
	want := RateSupport{Rates: []int{384000}, Min: 48000, Max: 384000}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RateSupport = %+v, want %+v", got, want)
	}
	// Every advertised rate must have been HW_PARAMS-verified.
	if !reflect.DeepEqual(fake.gotVerify, []int{48000, 384000}) {
		t.Errorf("verified rates = %v, want [48000 384000]", fake.gotVerify)
	}
}

func TestSupportedRatesVerifiedEmptyAdvertisedSkipsProbe(t *testing.T) {
	// Nothing advertised: return the empty result without any HW_PARAMS probe.
	fake := &fakeRatePCM{rates: nil, lo: 500000, hi: 768000}
	withOpenRatePCM(t, func(int, int) (ratePCM, error) { return fake, nil })

	got, err := SupportedRatesVerified("hw:2,0", 1, FormatS32LE)
	if err != nil {
		t.Fatalf("SupportedRatesVerified: %v", err)
	}
	if len(got.Rates) != 0 || got.Min != 500000 || got.Max != 768000 {
		t.Errorf("RateSupport = %+v, want empty rates with window [500000, 768000]", got)
	}
	if len(fake.gotVerify) != 0 {
		t.Errorf("VerifyRate called %v, want none", fake.gotVerify)
	}
}

func TestSupportedRatesVerifiedSurfacesVerifyError(t *testing.T) {
	// A real error from the HW_PARAMS pass (device vanished) surfaces as a typed
	// error, not a silently truncated list.
	fake := &fakeRatePCM{rates: []int{48000}, lo: 48000, hi: 48000, verifyErr: unix.ENODEV}
	withOpenRatePCM(t, func(int, int) (ratePCM, error) { return fake, nil })

	_, err := SupportedRatesVerified("hw:0,0", 1, FormatS32LE)
	if !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("err = %v, want ErrDeviceGone", err)
	}
}

func TestSupportedRatesVerifiedPropagatesRefinePassError(t *testing.T) {
	// If the refine pass itself fails (device busy), the verified probe returns
	// that error without attempting any HW_PARAMS commit.
	fake := &fakeRatePCM{ratesErr: &BadFormatError{Channels: 1, Format: FormatS32LE}}
	withOpenRatePCM(t, func(int, int) (ratePCM, error) { return fake, nil })

	_, err := SupportedRatesVerified("hw:0,0", 1, FormatS32LE)
	var bfe *BadFormatError
	if !errors.As(err, &bfe) {
		t.Fatalf("err = %v, want *BadFormatError", err)
	}
	if len(fake.gotVerify) != 0 {
		t.Errorf("VerifyRate called %v, want none", fake.gotVerify)
	}
}

func TestSupportedRatesRejectsBadDevice(t *testing.T) {
	withOpenRatePCM(t, func(int, int) (ratePCM, error) {
		t.Fatal("open should not be reached for a bad device id")
		return nil, errShouldNotOpen
	})
	_, err := SupportedRates("not-a-device", 1, FormatS32LE)
	var bde *BadDeviceError
	if !errors.As(err, &bde) {
		t.Fatalf("err = %v, want *BadDeviceError", err)
	}
}

func TestSupportedRatesRejectsBadChannels(t *testing.T) {
	withOpenRatePCM(t, func(int, int) (ratePCM, error) {
		t.Fatal("open should not be reached for bad channels")
		return nil, errShouldNotOpen
	})
	_, err := SupportedRates("hw:0,0", 0, FormatS32LE)
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Field != "channels" {
		t.Fatalf("err = %v, want ConfigError{channels}", err)
	}
}

func TestSupportedRatesRejectsBadFormat(t *testing.T) {
	withOpenRatePCM(t, func(int, int) (ratePCM, error) {
		t.Fatal("open should not be reached for a bad format")
		return nil, errShouldNotOpen
	})
	_, err := SupportedRates("hw:0,0", 1, Format(999))
	var ce *ConfigError
	if !errors.As(err, &ce) || ce.Field != "format" {
		t.Fatalf("err = %v, want ConfigError{format}", err)
	}
}

func TestSupportedRatesMapsBusyToDeviceInUse(t *testing.T) {
	withOpenRatePCM(t, func(int, int) (ratePCM, error) {
		return nil, &wrappedErrnoError{unix.EBUSY}
	})
	_, err := SupportedRates("hw:0,0", 1, FormatS32LE)
	if !errors.Is(err, ErrDeviceInUse) {
		t.Fatalf("err = %v, want ErrDeviceInUse", err)
	}
}

func TestSupportedRatesMapsMissingToDeviceGone(t *testing.T) {
	withOpenRatePCM(t, func(int, int) (ratePCM, error) {
		return nil, &wrappedErrnoError{unix.ENOENT}
	})
	_, err := SupportedRates("hw:9,0", 1, FormatS32LE)
	if !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("err = %v, want ErrDeviceGone", err)
	}
}

func TestSupportedRatesMapsInvalidToBadFormat(t *testing.T) {
	// EINVAL from the initial refine means the channel/format combo is rejected
	// outright; it must surface as a typed *BadFormatError with Rate 0.
	fake := &fakeRatePCM{ratesErr: &wrappedErrnoError{unix.EINVAL}}
	withOpenRatePCM(t, func(int, int) (ratePCM, error) { return fake, nil })
	_, err := SupportedRates("hw:0,0", 1, FormatS16LE)
	var bfe *BadFormatError
	if !errors.As(err, &bfe) {
		t.Fatalf("err = %v, want *BadFormatError", err)
	}
	if bfe.Channels != 1 || bfe.Format != FormatS16LE || bfe.Rate != 0 {
		t.Errorf("BadFormatError = %+v, want {Rate:0 Channels:1 Format:s16}", bfe)
	}
}

func TestSupportedRatesSurfacesProbeError(t *testing.T) {
	fake := &fakeRatePCM{ratesErr: &wrappedErrnoError{unix.ENOTTY}}
	withOpenRatePCM(t, func(int, int) (ratePCM, error) { return fake, nil })
	_, err := SupportedRates("hw:0,0", 1, FormatS32LE)
	if !errors.Is(err, unix.ENOTTY) {
		t.Fatalf("err = %v, want ENOTTY", err)
	}
	if fake.closeCalls != 1 {
		t.Errorf("Close called %d times, want 1 even on probe error", fake.closeCalls)
	}
}

// wrappedErrnoError mimics the *ioctlError wrapping that internal/alsa applies, so the
// public layer's errors.Is-based mapping is tested through a real Unwrap chain
// rather than a bare errno.
type wrappedErrnoError struct{ err error }

func (e *wrappedErrnoError) Error() string { return "alsa: " + e.err.Error() }
func (e *wrappedErrnoError) Unwrap() error { return e.err }
