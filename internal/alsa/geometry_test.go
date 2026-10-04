//go:build linux

package alsa

import (
	"errors"
	"math/bits"
	"math/rand/v2"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// narrowInterval intersects the incoming interval with [lo, hi], the way a
// kernel refine narrows a parameter and never widens it. An empty result sets
// the empty flag and returns EINVAL, as snd_pcm_hw_refine does.
func narrowInterval(hw *HwParams, param int, lo, hi uint32) error {
	iv := hw.interval(param)
	iv.Min = max(iv.Min, lo)
	iv.Max = min(iv.Max, hi)
	if iv.Min > iv.Max {
		iv.Flags |= intervalEmpty
		return unix.EINVAL
	}
	return nil
}

// maskSingle returns the one value a mask parameter is pinned to.
func maskSingle(hw *HwParams, param int) (uint, bool) {
	m := hw.mask(param)
	var found uint
	n := 0
	for i, w := range m.Bits {
		for ; w != 0; w &= w - 1 {
			found = uint(i*32 + bits.TrailingZeros32(w))
			n++
		}
	}
	return found, n == 1
}

func sampleBytes(format uint) uint32 {
	switch format {
	case FormatS16LE:
		return 2
	case FormatS243LE:
		return 3
	default:
		return 4
	}
}

func gcd(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// fakeStepDevice models an ALSA driver closely enough that a pinned value the
// kernel would refuse is refused here too. Refines narrow, never widen, and run
// the rules only when Rmask is set (the kernel clears Rmask after every refine
// and applies constraints only to the parameters it names). HW_PARAMS applies
// the rules unconditionally, requires rate, period size and periods to be
// pinned, and resolves the buffer as period x periods.
type fakeStepDevice struct {
	rateLo, rateHi uint32
	rates          map[uint32]bool // discrete rate set; nil means continuous
	chLo, chHi     uint32          // accepted channel counts; 0,0 means any
	formats        []uint          // accepted formats; nil means any
	periodMin      uint32
	periodMax      uint32
	periodsMin     uint32
	periodsMax     uint32
	// periodsExcludeMin makes the lowest periods value an excluded open bound
	// that the device reports with openmin set even when the caller asked for an
	// integer interval (a hostile driver, to cover the defensive +1/-1 path).
	periodsExcludeMin bool
	// periodsExcludeMax is the mirror image: the highest periods value is an
	// excluded open bound, reported with openmax set.
	periodsExcludeMax bool
	stepBytes         uint32 // period bytes must be a multiple of this; 0 means no step
	bufferMax         uint32 // buffer frames cap; 0 means none

	refineRmasks []uint32
	hwParamsN    int
	committed    HwParams
}

func (d *fakeStepDevice) frameBytes(hw *HwParams) uint32 {
	f, _ := maskSingle(hw, ParamFormat)
	ch, _ := hw.Interval(ParamChannels)
	return sampleBytes(f) * ch
}

func (d *fakeStepDevice) apply(hw *HwParams) error {
	if f, ok := maskSingle(hw, ParamFormat); ok && d.formats != nil {
		found := false
		for _, ok := range d.formats {
			found = found || ok == f
		}
		if !found {
			return unix.EINVAL
		}
	}
	if d.chHi != 0 {
		if err := narrowInterval(hw, ParamChannels, d.chLo, d.chHi); err != nil {
			return err
		}
	}
	if err := narrowInterval(hw, ParamRate, d.rateLo, d.rateHi); err != nil {
		return err
	}
	if lo, hi := hw.Interval(ParamRate); lo == hi && d.rates != nil && !d.rates[lo] {
		return unix.EINVAL
	}
	pmin, pmax, nmin, nmax := d.periodMin, d.periodMax, d.periodsMin, d.periodsMax
	if d.bufferMax != 0 {
		pmax = min(pmax, d.bufferMax/nmin)
	}
	if err := narrowInterval(hw, ParamPeriodSize, pmin, pmax); err != nil {
		return err
	}
	if d.stepBytes != 0 {
		fb := d.frameBytes(hw)
		g := d.stepBytes / gcd(d.stepBytes, fb)
		iv := hw.interval(ParamPeriodSize)
		iv.Min = (iv.Min + g - 1) / g * g
		iv.Max = iv.Max / g * g
		if iv.Min > iv.Max {
			iv.Flags |= intervalEmpty
			return unix.EINVAL
		}
	}
	if d.bufferMax != 0 {
		plo, _ := hw.Interval(ParamPeriodSize)
		nmax = min(nmax, d.bufferMax/max(plo, 1))
	}
	if err := narrowInterval(hw, ParamPeriods, nmin, nmax); err != nil {
		return err
	}
	if d.periodsExcludeMin {
		iv := hw.interval(ParamPeriods)
		if iv.Min <= d.periodsMin {
			iv.Min = d.periodsMin
			iv.Flags |= intervalOpenMin
		}
		if iv.Min == iv.Max && iv.Flags&intervalOpenMin != 0 {
			iv.Flags |= intervalEmpty
			return unix.EINVAL
		}
	}
	if d.periodsExcludeMax {
		iv := hw.interval(ParamPeriods)
		if iv.Max >= d.periodsMax {
			iv.Max = d.periodsMax
			iv.Flags |= intervalOpenMax
		}
		if iv.Min == iv.Max && iv.Flags&intervalOpenMax != 0 {
			iv.Flags |= intervalEmpty
			return unix.EINVAL
		}
	}
	return nil
}

func (d *fakeStepDevice) ioctl(_ int, req uintptr, arg unsafe.Pointer) error {
	switch req {
	case iocHwRefine:
		hw := (*HwParams)(arg)
		d.refineRmasks = append(d.refineRmasks, hw.Rmask)
		if hw.Rmask != 0 {
			if err := d.apply(hw); err != nil {
				return err
			}
		}
		hw.Rmask = 0
	case iocHwParams:
		hw := (*HwParams)(arg)
		d.hwParamsN++
		if err := d.apply(hw); err != nil {
			return err
		}
		rlo, rhi := hw.Interval(ParamRate)
		plo, phi := hw.Interval(ParamPeriodSize)
		nlo, nhi := hw.Interval(ParamPeriods)
		if rlo != rhi || plo != phi || nlo != nhi {
			return unix.EINVAL
		}
		if d.bufferMax != 0 && uint64(plo)*uint64(nlo) > uint64(d.bufferMax) {
			return unix.EINVAL
		}
		setInterval(hw, ParamBufferSize, plo*nlo, plo*nlo)
		d.committed = *hw
	}
	return nil
}

// stepDevice44100 is the issue #21 device: an HDA-like controller whose period
// bytes must be a multiple of 128, so a 882-frame S16 stereo period (3528
// bytes) is refused and 896 frames (3584 bytes) is accepted.
func stepDevice44100() *fakeStepDevice {
	return &fakeStepDevice{
		rateLo: 8000, rateHi: 192000,
		periodMin: 32, periodMax: 16384, periodsMin: 2, periodsMax: 32,
		stepBytes: 128,
	}
}

func TestNegotiatePicksNearestPeriodUnderStepConstraint(t *testing.T) {
	d := stepDevice44100()
	p := newPCM(-1, d.ioctl)
	n, err := p.Negotiate(44100, 2, FormatS16LE, DefaultPeriodFrames(44100), DefaultPeriods)
	if err != nil {
		t.Fatalf("Negotiate = %v, want success at the nearest accepted period", err)
	}
	want := Negotiated{Rate: 44100, Channels: 2, Format: FormatS16LE, PeriodFrames: 896, Periods: 4, BufferFrames: 3584}
	if n != want {
		t.Errorf("Negotiated = %+v, want %+v", n, want)
	}
	if pb := n.PeriodFrames * 4; pb%128 != 0 {
		t.Errorf("committed period = %d bytes, not a multiple of 128", pb)
	}
}

func TestVerifyRateAcceptsRateUnderStepConstraint(t *testing.T) {
	p := newPCM(-1, stepDevice44100().ioctl)
	ok, err := p.VerifyRate(2, FormatS16LE, 44100)
	if err != nil || !ok {
		t.Fatalf("VerifyRate(44100) = %v, %v; want true, nil", ok, err)
	}
}

// plainDevice has no step rule: every period size and count in range is valid.
func plainDevice() *fakeStepDevice {
	return &fakeStepDevice{
		rateLo: 8000, rateHi: 384000,
		periodMin: 16, periodMax: 65536, periodsMin: 2, periodsMax: 32,
	}
}

func TestNegotiateNearestPeriodTable(t *testing.T) {
	tests := []struct {
		name        string
		dev         func() *fakeStepDevice
		period      int
		periods     int
		wantPeriod  int
		wantPeriods int
	}{
		{"closer to lower grid value", stepDevice44100, 870, 4, 864, 4},
		{"closer to upper grid value", stepDevice44100, 890, 4, 896, 4},
		{"exact tie goes to the larger period", stepDevice44100, 880, 4, 896, 4},
		{"target below range gives the minimum", stepDevice44100, 5, 4, 32, 4},
		{"target above range gives the maximum", stepDevice44100, 1000000, 4, 16384, 4},
		{"periods forced down by a buffer cap", func() *fakeStepDevice {
			d := stepDevice44100()
			d.bufferMax = 896 * 3
			return d
		}, 880, 4, 896, 3},
		{"excluded open upper bound is never chosen", func() *fakeStepDevice {
			d := stepDevice44100()
			d.periodsExcludeMax = true
			return d
		}, 880, 100, 896, 31},
		{"excluded open lower bound is never chosen", func() *fakeStepDevice {
			d := stepDevice44100()
			d.periodsExcludeMin = true
			return d
		}, 880, 2, 896, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newPCM(-1, tt.dev().ioctl)
			n, err := p.Negotiate(44100, 2, FormatS16LE, tt.period, tt.periods)
			if err != nil {
				t.Fatalf("Negotiate = %v", err)
			}
			if n.PeriodFrames != tt.wantPeriod || n.Periods != tt.wantPeriods {
				t.Errorf("period x periods = %d x %d, want %d x %d", n.PeriodFrames, n.Periods, tt.wantPeriod, tt.wantPeriods)
			}
			if n.BufferFrames != tt.wantPeriod*tt.wantPeriods {
				t.Errorf("BufferFrames = %d, want %d", n.BufferFrames, tt.wantPeriod*tt.wantPeriods)
			}
		})
	}
}

func TestNegotiateSetsRmaskOnEveryRefine(t *testing.T) {
	d := stepDevice44100()
	p := newPCM(-1, d.ioctl)
	if _, err := p.Negotiate(44100, 2, FormatS16LE, 882, 4); err != nil {
		t.Fatalf("Negotiate = %v", err)
	}
	if len(d.refineRmasks) < 4 {
		t.Fatalf("only %d refines recorded, want the rate pin and the geometry probes too", len(d.refineRmasks))
	}
	for i, m := range d.refineRmasks {
		if m == 0 {
			t.Errorf("HW_REFINE #%d issued with Rmask 0: the kernel would skip its constraints", i+1)
		}
	}
}

func TestNegotiateGeometryUnchangedWhenTargetValid(t *testing.T) {
	// Backward compatibility: any rate and geometry the old exact pin committed
	// is committed unchanged. The plain device has no step rule, so every target
	// inside the ranges is valid.
	check := func(t *testing.T, rate, period, periods, wantPeriod, wantPeriods int) {
		t.Helper()
		p := newPCM(-1, plainDevice().ioctl)
		n, err := p.Negotiate(rate, 2, FormatS16LE, period, periods)
		if err != nil {
			t.Fatalf("Negotiate(%d, %d x %d) = %v", rate, period, periods, err)
		}
		if n.PeriodFrames != wantPeriod || n.Periods != wantPeriods {
			t.Errorf("Negotiate(%d, %d x %d) = %d x %d, want %d x %d", rate, period, periods, n.PeriodFrames, n.Periods, wantPeriod, wantPeriods)
		}
	}
	for _, rate := range []int{8000, 16000, 44100, 48000, 96000, 192000, 384000} {
		check(t, rate, DefaultPeriodFrames(rate), DefaultPeriods, DefaultPeriodFrames(rate), DefaultPeriods)
	}
	for _, c := range [][2]int{{16, 2}, {1000, 3}, {4096, 8}, {65536, 32}} {
		check(t, 48000, c[0], c[1], c[0], c[1])
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		period := 16 + rng.IntN(65536-16+1)
		periods := 2 + rng.IntN(32-2+1)
		check(t, 48000, period, periods, period, periods)
	}
	// Targets outside the ranges commit the clamped bound, as the old code did.
	check(t, 48000, 5, 1, 16, 2)
	check(t, 48000, 100000, 100, 65536, 32)
}

func TestVerifyRateMatchesNegotiate(t *testing.T) {
	// Probe equals open: for the default geometry VerifyRate is true iff
	// Negotiate commits, on devices that fail in different ways.
	gapDevice := func() ioctlFunc {
		d := plainDevice()
		d.rates = map[uint32]bool{44100: true, 48000: true, 96000: true}
		return d.ioctl
	}
	devices := map[string]func() ioctlFunc{
		"step":       func() ioctlFunc { return stepDevice44100().ioctl },
		"floor":      func() ioctlFunc { return fakeGeometryDevice(44100, 96000, 8, 64) },
		"refine lie": func() ioctlFunc { return fakeCommitDevice(8000, 384000, map[uint32]bool{48000: true, 384000: true}) },
		"gap":        gapDevice,
	}
	for name, mk := range devices {
		for _, rate := range []int{8000, 16000, 44100, 48000, 88200, 96000, 192000, 384000} {
			_, nerr := newPCM(-1, mk()).Negotiate(rate, 2, FormatS32LE, DefaultPeriodFrames(rate), DefaultPeriods)
			ok, verr := newPCM(-1, mk()).VerifyRate(2, FormatS32LE, rate)
			if verr != nil {
				t.Errorf("%s @ %d: VerifyRate error %v", name, rate, verr)
				continue
			}
			if ok != (nerr == nil) {
				t.Errorf("%s @ %d: VerifyRate = %v but Negotiate err = %v", name, rate, ok, nerr)
			}
		}
	}
}

func TestNegotiateEmptyRateIntervalIsBadFormat(t *testing.T) {
	// The first refine succeeds but empties the rate interval: an unsatisfiable
	// combination signalled without EINVAL. It is a format problem, not a rate.
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req == iocHwRefine {
			(*HwParams)(arg).Intervals[ParamRate-paramFirstInterval] = Interval{Flags: intervalEmpty}
		}
		return nil
	}
	_, err := newPCM(-1, fake).Negotiate(48000, 1, FormatS16LE, 960, 4)
	if _, ok := errors.AsType[*BadFormatError](err); !ok {
		t.Fatalf("Negotiate err = %v, want *BadFormatError", err)
	}
	if _, ok := errors.AsType[*BadRateError](err); ok {
		t.Errorf("emptied rate interval reported as *BadRateError: %v", err)
	}
}

func TestNegotiateGeometryRefinePassesThroughDeviceGone(t *testing.T) {
	// Count the refines of a successful run, then fail each one after the first
	// (the rate pin and every geometry probe and pin) with each errno that must
	// pass through unclassified.
	probe := stepDevice44100()
	if _, err := newPCM(-1, probe.ioctl).Negotiate(44100, 2, FormatS16LE, 880, 4); err != nil {
		t.Fatalf("Negotiate = %v", err)
	}
	total := len(probe.refineRmasks)
	for _, errno := range []unix.Errno{unix.ENODEV, unix.ENXIO, unix.ENOENT, unix.EBADFD} {
		for k := 2; k <= total; k++ {
			d := stepDevice44100()
			refines := 0
			fake := func(fd int, req uintptr, arg unsafe.Pointer) error {
				if req == iocHwRefine {
					refines++
					if refines == k {
						return errno
					}
				}
				return d.ioctl(fd, req, arg)
			}
			_, err := newPCM(-1, fake).Negotiate(44100, 2, FormatS16LE, 880, 4)
			var bre *BadRateError
			var ge *GeometryError
			if errors.As(err, &bre) || errors.As(err, &ge) || !errors.Is(err, errno) {
				t.Errorf("%v at refine #%d: err = %v, want it to unwrap to the errno and be neither *BadRateError nor *GeometryError", errno, k, err)
			}
		}
	}
}

func TestNegotiateRejectsSubstitutedRate(t *testing.T) {
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		switch req {
		case iocHwRefine:
			return narrowDevice(arg, 44100, 48000)
		case iocHwParams:
			setInterval((*HwParams)(arg), ParamRate, 44100, 44100)
		}
		return nil
	}
	_, err := newPCM(-1, fake).Negotiate(48000, 1, FormatS16LE, 960, 4)
	var bre *BadRateError
	if !errors.As(err, &bre) || bre.Requested != 48000 {
		t.Fatalf("Negotiate err = %v, want *BadRateError for 48000", err)
	}
}

func TestNegotiateNoAttainableGeometryIsGeometryError(t *testing.T) {
	// Neither neighbour of the target can be pinned: the device refines a wide
	// period interval but refuses every pin.
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req != iocHwRefine {
			return nil
		}
		hw := (*HwParams)(arg)
		if lo, hi := hw.Interval(ParamPeriodSize); lo == hi {
			return unix.EINVAL
		}
		return narrowDevice(arg, 8000, 192000)
	}
	_, err := newPCM(-1, fake).Negotiate(48000, 2, FormatS16LE, 960, 4)
	var ge *GeometryError
	if !errors.As(err, &ge) || !errors.Is(err, unix.EINVAL) {
		t.Fatalf("Negotiate err = %v, want *GeometryError wrapping EINVAL", err)
	}
	if ge.Rate != 48000 || ge.PeriodFrames != 960 || ge.Periods != 4 {
		t.Errorf("GeometryError = %+v, want the requested 48000 Hz, 960 x 4", ge)
	}
	if !strings.Contains(err.Error(), "HW_REFINE") {
		t.Errorf("error %q does not name the failing ioctl", err)
	}
}

// fakeStatefulRateDevice adds the stream state to a rate-only device: a
// successful HW_PARAMS moves OPEN to SETUP and records the rate, HW_FREE is
// accepted only in SETUP, and while a commit is held a refine pinning any other
// rate fails (the USB endpoint lock). A failed HW_PARAMS resets to OPEN.
type fakeStatefulRateDevice struct {
	committable map[uint32]bool
	substitute  map[uint32]uint32 // rate -> rate the commit reports instead
	hwFreeErr   error

	setup          bool
	held           uint32
	commits, frees int
	freeInOpen     int
}

func (d *fakeStatefulRateDevice) ioctl(_ int, req uintptr, arg unsafe.Pointer) error {
	hw := (*HwParams)(arg)
	switch req {
	case iocHwRefine:
		if lo, hi := hw.Interval(ParamRate); d.setup && lo == hi && lo != d.held {
			return unix.EINVAL
		}
		return narrowDevice(arg, 8000, 384000)
	case iocHwParams:
		lo, hi := hw.Interval(ParamRate)
		if lo != hi || !d.committable[lo] {
			d.setup = false
			return unix.EINVAL
		}
		d.setup, d.held = true, lo
		d.commits++
		if sub, ok := d.substitute[lo]; ok {
			setInterval(hw, ParamRate, sub, sub)
		}
	case iocHwFree:
		if !d.setup {
			d.freeInOpen++
			return unix.EBADFD
		}
		d.frees++
		if d.hwFreeErr != nil {
			return d.hwFreeErr
		}
		d.setup, d.held = false, 0
	}
	return nil
}

func statefulDevice() *fakeStatefulRateDevice {
	return &fakeStatefulRateDevice{
		committable: map[uint32]bool{44100: true, 48000: true, 96000: true, 192000: true},
		substitute:  map[uint32]uint32{192000: 96000},
	}
}

func TestVerifyRateOrderIndependentOnOneFD(t *testing.T) {
	sequences := [][]int{
		{48000, 96000, 44100},
		{44100, 96000, 48000},
		{48000, 88200, 96000, 44100},
		{48000, 192000, 96000},
	}
	for _, seq := range sequences {
		shared := statefulDevice()
		p := newPCM(-1, shared.ioctl)
		for _, rate := range seq {
			got, err := p.VerifyRate(2, FormatS16LE, rate)
			if err != nil {
				t.Fatalf("%v: VerifyRate(%d) on one fd = %v", seq, rate, err)
			}
			want, err := newPCM(-1, statefulDevice().ioctl).VerifyRate(2, FormatS16LE, rate)
			if err != nil {
				t.Fatalf("%v: fresh VerifyRate(%d) = %v", seq, rate, err)
			}
			if got != want {
				t.Errorf("%v: VerifyRate(%d) = %v on a shared fd, %v on a fresh fd", seq, rate, got, want)
			}
		}
		if shared.frees != shared.commits {
			t.Errorf("%v: %d HW_FREE for %d successful commits", seq, shared.frees, shared.commits)
		}
		if shared.freeInOpen != 0 {
			t.Errorf("%v: %d HW_FREE issued from OPEN (after a failed commit)", seq, shared.freeInOpen)
		}
		if shared.setup {
			t.Errorf("%v: PCM left in SETUP, want OPEN", seq)
		}
	}
}

func TestVerifyRateSurfacesHwFreeError(t *testing.T) {
	d := statefulDevice()
	d.hwFreeErr = unix.ENODEV
	_, err := newPCM(-1, d.ioctl).VerifyRate(2, FormatS16LE, 48000)
	if !errors.Is(err, unix.ENODEV) {
		t.Fatalf("VerifyRate err = %v, want ENODEV from HW_FREE", err)
	}
}

func TestNegotiateBadFormatReportsChannelRange(t *testing.T) {
	d := plainDevice()
	d.chLo, d.chHi = 2, 2
	_, err := newPCM(-1, d.ioctl).Negotiate(48000, 1, FormatS16LE, 960, 4)
	var bfe *BadFormatError
	if !errors.As(err, &bfe) {
		t.Fatalf("Negotiate err = %v, want *BadFormatError", err)
	}
	if bfe.MinChannels != 2 || bfe.MaxChannels != 2 {
		t.Errorf("channel range = %d..%d, want 2..2", bfe.MinChannels, bfe.MaxChannels)
	}
}

func TestNegotiateBadFormatUnsupportedFormat(t *testing.T) {
	d := plainDevice()
	d.formats = []uint{FormatS32LE}
	_, err := newPCM(-1, d.ioctl).Negotiate(48000, 2, FormatS16LE, 960, 4)
	var bfe *BadFormatError
	if !errors.As(err, &bfe) {
		t.Fatalf("Negotiate err = %v, want *BadFormatError", err)
	}
	if bfe.MinChannels != 0 || bfe.MaxChannels != 0 {
		t.Errorf("channel range = %d..%d, want 0..0 for an unsupported format", bfe.MinChannels, bfe.MaxChannels)
	}
}

func TestNegotiateBadFormatProbePassesDeviceGone(t *testing.T) {
	refines := 0
	fake := func(_ int, req uintptr, _ unsafe.Pointer) error {
		if req != iocHwRefine {
			return nil
		}
		refines++
		if refines == 1 {
			return unix.EINVAL
		}
		return unix.ENODEV // the channel-range probe finds the device gone
	}
	_, err := newPCM(-1, fake).Negotiate(48000, 1, FormatS16LE, 960, 4)
	var bfe *BadFormatError
	if errors.As(err, &bfe) || !errors.Is(err, unix.ENODEV) {
		t.Fatalf("Negotiate err = %v, want ENODEV and not *BadFormatError", err)
	}
}

func TestSupportedRatesBadFormatReportsChannelRange(t *testing.T) {
	d := plainDevice()
	d.chLo, d.chHi = 4, 4
	_, _, _, err := newPCM(-1, d.ioctl).SupportedRates(1, FormatS32LE, []int{48000})
	var bfe *BadFormatError
	if !errors.As(err, &bfe) {
		t.Fatalf("SupportedRates err = %v, want *BadFormatError", err)
	}
	if bfe.MinChannels != 4 || bfe.MaxChannels != 4 {
		t.Errorf("channel range = %d..%d, want 4..4", bfe.MinChannels, bfe.MaxChannels)
	}
}

// narrowDevice narrows rate to [rateLo, rateHi] and gives period size and period
// count a plain range, the way a kernel refine narrows an incoming HwParams.
func narrowDevice(arg unsafe.Pointer, rateLo, rateHi uint32) error {
	hw := (*HwParams)(arg)
	if err := narrowInterval(hw, ParamRate, rateLo, rateHi); err != nil {
		return err
	}
	if err := narrowInterval(hw, ParamPeriodSize, 16, 65536); err != nil {
		return err
	}
	return narrowInterval(hw, ParamPeriods, 2, 32)
}

func TestNegotiateNoCandidateGeometryErrorNamesIoctl(t *testing.T) {
	// The target is above the period range and the down probe is refused, so no
	// candidate is ever pinned: the error still names the failing ioctl instead of
	// a bare "invalid argument".
	refines := 0
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req != iocHwRefine {
			return nil
		}
		refines++
		if refines > 2 {
			return unix.EINVAL
		}
		return narrowDevice(arg, 8000, 192000)
	}
	_, err := newPCM(-1, fake).Negotiate(48000, 2, FormatS16LE, 100000, 4)
	if _, ok := errors.AsType[*GeometryError](err); !ok {
		t.Fatalf("Negotiate err = %v, want *GeometryError", err)
	}
	if !strings.Contains(err.Error(), "HW_REFINE") {
		t.Errorf("error %q does not name the failing ioctl", err)
	}
}

func TestVerifyRateRefusedGeometryIsNotSupported(t *testing.T) {
	// No value near the period target can be pinned: VerifyRate reports the rate
	// as unsupported (false, nil) rather than returning the geometry error, so one
	// unattainable candidate does not abort SupportedRatesVerified.
	fake := func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req != iocHwRefine {
			return nil
		}
		hw := (*HwParams)(arg)
		if lo, hi := hw.Interval(ParamPeriodSize); lo == hi {
			return unix.EINVAL
		}
		return narrowDevice(arg, 8000, 192000)
	}
	ok, err := newPCM(-1, fake).VerifyRate(2, FormatS16LE, 48000)
	if err != nil || ok {
		t.Fatalf("VerifyRate = %v, %v; want false, nil", ok, err)
	}
}

func TestNegotiateCommitRefusalReportsChosenGeometry(t *testing.T) {
	// HW_PARAMS refuses after the step rule moved the 882-frame target to 896:
	// GeometryError carries the geometry the commit was attempted with, not the
	// requested one.
	d := stepDevice44100()
	fake := func(fd int, req uintptr, arg unsafe.Pointer) error {
		if req == iocHwParams {
			return unix.EINVAL
		}
		return d.ioctl(fd, req, arg)
	}
	_, err := newPCM(-1, fake).Negotiate(44100, 2, FormatS16LE, 882, 4)
	ge, ok := errors.AsType[*GeometryError](err)
	if !ok {
		t.Fatalf("Negotiate err = %v, want *GeometryError", err)
	}
	if ge.PeriodFrames != 896 || ge.Periods != 4 {
		t.Errorf("GeometryError = %+v, want the attempted 896 x 4", ge)
	}
}
