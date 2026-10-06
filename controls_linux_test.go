//go:build linux

package capture

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/tphakala/go-audio-capture/internal/alsa"
)

// fakeCtlElem is one element of fakeCtl's table. Unlike a real kernel the fake
// does no range or step validation on write (CONFIG_SND_CTL_INPUT_VALIDATION
// off), so a missing library-side check lets the write land and a test fail.
type fakeCtlElem struct {
	id       alsa.CtlElemID
	typ      int32
	access   uint32
	count    uint32
	min, max int64
	step     int64
	items    []string
	vals     []int64
	tlv      []uint32
}

// fakeCtl is a hardware-free ctlHandle over an element table.
type fakeCtl struct {
	elems []*fakeCtlElem
	calls []string
	// failOp returns the error for an operation name ("list", "info", "read",
	// "write", "tlv") when non-nil and it returns non-nil.
	failOp       func(op string) error
	probeFn      func() error
	disconnected bool
	closeCalls   int
	onInfo       func(f *fakeCtl, name string)
	onTLV        func(f *fakeCtl, numid uint32)
}

const (
	plainVol   = "Plain Capture Volume"
	enumName   = "Enum"
	kindValue  = "value"
	kindAccess = "access"
)

func wrapErrno(e error) error { return &wrappedErrnoError{err: e} }

func (f *fakeCtl) add(name string, iface, typ int32, count uint32) *fakeCtlElem {
	id, err := alsa.NewCtlElemID(iface, 0, 0, name, 0)
	if err != nil {
		panic(err)
	}
	for _, e := range f.elems { // same name again: next index
		if e.id.NameString() == name && e.id.Iface == iface {
			id.Index = e.id.Index + 1
		}
	}
	id.Numid = uint32(len(f.elems) + 1)
	e := &fakeCtlElem{id: id, typ: typ, access: alsa.CtlAccessRead | alsa.CtlAccessWrite, count: count, vals: make([]int64, count)}
	f.elems = append(f.elems, e)
	return e
}

func (f *fakeCtl) vol(name string, lo, hi, step int64, count uint32) *fakeCtlElem {
	e := f.add(name, int32(ControlMixer), alsa.CtlTypeInteger, count)
	e.min, e.max, e.step = lo, hi, step
	return e
}

func (f *fakeCtl) fail(op string) error {
	f.calls = append(f.calls, op)
	if f.disconnected {
		return wrapErrno(unix.ENODEV)
	}
	if f.failOp != nil {
		return f.failOp(op)
	}
	return nil
}

func (f *fakeCtl) find(id alsa.CtlElemID) *fakeCtlElem {
	for _, e := range f.elems {
		if id.Numid != 0 {
			if e.id.Numid == id.Numid {
				return e
			}
			continue
		}
		if e.id.Iface == id.Iface && e.id.Device == id.Device && e.id.Subdevice == id.Subdevice && e.id.Index == id.Index && e.id.Name == id.Name {
			return e
		}
	}
	return nil
}

func (f *fakeCtl) count(op string) int {
	n := 0
	for _, c := range f.calls {
		if c == op {
			n++
		}
	}
	return n
}

func (f *fakeCtl) List() ([]alsa.CtlElemID, error) {
	if err := f.fail("list"); err != nil {
		return nil, err
	}
	ids := make([]alsa.CtlElemID, len(f.elems))
	for i, e := range f.elems {
		ids[i] = e.id
	}
	return ids, nil
}

func (f *fakeCtl) Info(id alsa.CtlElemID) (alsa.ElemInfo, error) {
	if f.onInfo != nil {
		f.onInfo(f, id.NameString())
	}
	if err := f.fail("info"); err != nil {
		return alsa.ElemInfo{}, err
	}
	e := f.find(id)
	if e == nil {
		return alsa.ElemInfo{}, wrapErrno(unix.ENOENT)
	}
	return alsa.ElemInfo{ID: e.id, Type: e.typ, Access: e.access, Count: e.count, Min: e.min, Max: e.max, Step: e.step, Items: uint32(len(e.items))}, nil
}

func (f *fakeCtl) EnumItemName(id alsa.CtlElemID, item uint32) (string, error) {
	if err := f.fail("item"); err != nil {
		return "", err
	}
	e := f.find(id)
	if e == nil {
		return "", wrapErrno(unix.ENOENT)
	}
	return e.items[item], nil
}

func (f *fakeCtl) ReadValues(id alsa.CtlElemID, _ int32, count int) ([]int64, error) {
	if err := f.fail("read"); err != nil {
		return nil, err
	}
	e := f.find(id)
	if e == nil {
		return nil, wrapErrno(unix.ENOENT)
	}
	return slices.Clone(e.vals[:count]), nil
}

func (f *fakeCtl) WriteValues(id alsa.CtlElemID, _ int32, values []int64) error {
	if err := f.fail("write"); err != nil {
		return err
	}
	e := f.find(id)
	if e == nil {
		return wrapErrno(unix.ENOENT)
	}
	e.vals = slices.Clone(values)
	return nil
}

func (f *fakeCtl) TLV(numid uint32) ([]uint32, error) {
	if f.onTLV != nil {
		f.onTLV(f, numid)
	}
	if err := f.fail("tlv"); err != nil {
		return nil, err
	}
	for _, e := range f.elems {
		if e.id.Numid == numid {
			if e.tlv == nil {
				return nil, wrapErrno(unix.ENXIO)
			}
			return e.tlv, nil
		}
	}
	return nil, wrapErrno(unix.ENOENT)
}

func (f *fakeCtl) Probe() error {
	f.calls = append(f.calls, "probe")
	if f.disconnected {
		return wrapErrno(unix.ENODEV)
	}
	if f.probeFn != nil {
		return f.probeFn()
	}
	return nil
}

func (f *fakeCtl) Close() error { f.closeCalls++; return nil }

func withOpenCtl(t *testing.T, fn func(card int) (ctlHandle, error)) {
	t.Helper()
	prev := openCtl
	openCtl = fn
	t.Cleanup(func() { openCtl = prev })
}

func newTestControls(f *fakeCtl) *Controls { return &Controls{h: f} }

func mixerID(name string) ControlID { return ControlID{Interface: ControlMixer, Name: name} }

func TestOpenNeverOpensControls(t *testing.T) {
	useFixture(t, hostLayout())
	withOpenCtl(t, func(int) (ctlHandle, error) {
		t.Error("Open or OpenDevice opened a control device")
		return nil, errShouldNotOpen
	})
	withOpenPCM(t, func(int, int) (pcm, error) { return &fakePCM{}, nil })
	s, err := Open(Config{Device: wantSerialID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = s.Close()
	s, err = OpenDevice(mustResolve(t, wantSerialID), odCfg)
	if err != nil {
		t.Fatalf("OpenDevice: %v", err)
	}
	_ = s.Close()
}

func TestOpenControlsOpensResolvedCard(t *testing.T) {
	useFixture(t, hostLayout())
	f := &fakeCtl{}
	got := -1
	withOpenCtl(t, func(card int) (ctlHandle, error) { got = card; return f, nil })
	c, err := OpenControls(mustResolve(t, wantSerialID))
	if err != nil {
		t.Fatalf("OpenControls: %v", err)
	}
	if got != 2 {
		t.Errorf("opened card %d, want 2", got)
	}
	if err := c.Close(); err != nil || f.closeCalls != 1 {
		t.Errorf("Close = %v, handle closed %d times, want nil and 1", err, f.closeCalls)
	}
	if err := c.Close(); err != nil || f.closeCalls != 1 {
		t.Errorf("second Close = %v, handle closed %d times, want nil and 1", err, f.closeCalls)
	}
}

func TestOpenControlsVerifiesIdentity(t *testing.T) {
	useFixture(t, hostLayout())
	d := mustResolve(t, wantSerialID)
	_, swappedSys := buildFixture(t, swappedHostLayout())

	t.Run("swap during the open", func(t *testing.T) {
		f := &fakeCtl{}
		withOpenCtl(t, func(int) (ctlHandle, error) { sysRoot = swappedSys; return f, nil })
		c, err := OpenControls(d)
		if !errors.Is(err, ErrDeviceGone) || c != nil {
			t.Fatalf("OpenControls = %v, %v, want nil and ErrDeviceGone", c, err)
		}
		if f.closeCalls != 1 {
			t.Errorf("handle closed %d times, want 1", f.closeCalls)
		}
	})
}

func TestOpenControlsFailedOpenOnStrangerIsDeviceGone(t *testing.T) {
	useFixture(t, hostLayout())
	d := mustResolve(t, wantSerialID)
	_, swappedSys := buildFixture(t, swappedHostLayout())
	withOpenCtl(t, func(int) (ctlHandle, error) {
		sysRoot = swappedSys
		return nil, wrapErrno(unix.EACCES)
	})
	if _, err := OpenControls(d); !errors.Is(err, ErrDeviceGone) || errors.Is(err, unix.EACCES) {
		t.Fatalf("OpenControls err = %v, want ErrDeviceGone for a failed open on a card that is not the unit", err)
	}
}

func TestOpenControlsOpenErrors(t *testing.T) {
	useFixture(t, hostLayout())
	d := mustResolve(t, wantSerialID)
	withOpenCtl(t, func(int) (ctlHandle, error) { return nil, wrapErrno(unix.ENODEV) })
	if _, err := OpenControls(d); !errors.Is(err, ErrDeviceGone) {
		t.Errorf("ENODEV on open = %v, want ErrDeviceGone", err)
	}
	withOpenCtl(t, func(int) (ctlHandle, error) { return nil, wrapErrno(unix.EACCES) })
	if _, err := OpenControls(d); !errors.Is(err, unix.EACCES) || errors.Is(err, ErrDeviceGone) {
		t.Errorf("permission failure = %v, want the wrapped EACCES", err)
	}
	if _, err := OpenControls(DeviceInfo{}); err == nil {
		t.Error("empty DeviceInfo accepted")
	}
}

func TestControlsSetRejectsBeforeWrite(t *testing.T) {
	build := func() (*fakeCtl, *Controls) {
		f := &fakeCtl{}
		f.vol(plainVol, 0, 100, 0, 2)
		f.vol("Stepped Capture Volume", 0, 100, 5, 1)
		f.vol("Offset Capture Volume", 3, 103, 5, 1) // (v-Min)%Step==0 but v%Step!=0 for 8
		f.vol("Negative Capture Volume", -20, 0, 5, 1)
		b := f.add("Boolean Switch", int32(ControlMixer), alsa.CtlTypeBoolean, 1)
		b.max = 1
		en := f.add(enumName, int32(ControlMixer), alsa.CtlTypeEnumerated, 1)
		en.items = []string{"a", "b"}
		f.add("Big", int32(ControlMixer), alsa.CtlTypeInteger64, 1)
		ro := f.vol("Read Only Volume", 0, 10, 0, 1)
		ro.access = alsa.CtlAccessRead
		inact := f.vol("Inactive Volume", 0, 10, 0, 1)
		inact.access = alsa.CtlAccessRead | alsa.CtlAccessWrite | alsa.CtlAccessInactive
		return f, newTestControls(f)
	}
	tests := []struct {
		name   string
		elem   string
		values []int64
		want   string // kindValue or kindAccess
	}{
		{"above max", plainVol, []int64{101, 0}, kindValue},
		{"below min", plainVol, []int64{0, -1}, kindValue},
		{"too few values", plainVol, []int64{1}, kindValue},
		{"too many values", plainVol, []int64{1, 2, 3}, kindValue},
		{"step violation", "Stepped Capture Volume", []int64{7}, kindValue},
		{"relative step passes but kernel rule fails", "Offset Capture Volume", []int64{8}, kindValue},
		{"negative value under unsigned step rule", "Negative Capture Volume", []int64{-5}, kindValue},
		{"boolean 2", "Boolean Switch", []int64{2}, kindValue},
		{"enumerated past items", enumName, []int64{2}, kindValue},
		{"enumerated negative", enumName, []int64{-1}, kindValue},
		{"integer64 type", "Big", []int64{1}, kindValue},
		{"read only", "Read Only Volume", []int64{1}, kindAccess},
		{"inactive", "Inactive Volume", []int64{1}, kindAccess},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, c := build()
			err := c.Set(mixerID(tt.elem), tt.values)
			switch tt.want {
			case kindValue:
				if _, ok := errors.AsType[*ControlValueError](err); !ok {
					t.Fatalf("Set = %v, want *ControlValueError", err)
				}
			case kindAccess:
				if _, ok := errors.AsType[*ControlAccessError](err); !ok {
					t.Fatalf("Set = %v, want *ControlAccessError", err)
				}
			}
			if n := f.count("write"); n != 0 {
				t.Errorf("%d writes reached the device for a rejected value", n)
			}
		})
	}

	t.Run("valid values are written", func(t *testing.T) {
		f, c := build()
		for _, ok := range []struct {
			elem string
			v    []int64
		}{
			{plainVol, []int64{0, 100}},
			{"Stepped Capture Volume", []int64{35}},
			{"Offset Capture Volume", []int64{5}},
			{"Negative Capture Volume", []int64{0}},
			{"Boolean Switch", []int64{1}},
			{enumName, []int64{1}},
		} {
			if err := c.Set(mixerID(ok.elem), ok.v); err != nil {
				t.Errorf("Set(%s, %v) = %v", ok.elem, ok.v, err)
			}
		}
		if n := f.count("write"); n != 6 {
			t.Errorf("%d writes, want 6", n)
		}
	})
}

func TestControlsNameLimitsIssueNoIoctl(t *testing.T) {
	f := &fakeCtl{}
	c := newTestControls(f)
	for _, name := range []string{strings.Repeat("A", 45), "Mic\x00Volume"} {
		id := mixerID(name)
		if _, err := c.Info(id); !errors.Is(err, ErrControlNotFound) {
			t.Errorf("Info(%q) = %v, want ErrControlNotFound", name, err)
		}
		if _, err := c.Get(id); !errors.Is(err, ErrControlNotFound) {
			t.Errorf("Get(%q) = %v, want ErrControlNotFound", name, err)
		}
		if err := c.Set(id, []int64{1}); !errors.Is(err, ErrControlNotFound) {
			t.Errorf("Set(%q) = %v, want ErrControlNotFound", name, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("ioctls issued for impossible names: %v", f.calls)
	}
}

func TestControlsErrorClassification(t *testing.T) {
	newOne := func() (*fakeCtl, *Controls) {
		f := &fakeCtl{}
		f.vol(micVol, 0, 10, 0, 1)
		return f, newTestControls(f)
	}
	t.Run("ENODEV needs no probe", func(t *testing.T) {
		f, c := newOne()
		f.failOp = func(op string) error {
			if op == accessOpRead {
				return wrapErrno(unix.ENODEV)
			}
			return nil
		}
		if _, err := c.Get(mixerID(micVol)); !errors.Is(err, ErrDeviceGone) {
			t.Fatalf("Get = %v, want ErrDeviceGone", err)
		}
		if f.count("probe") != 0 {
			t.Error("probed although the errno already said the device is gone")
		}
	})
	t.Run("EIO with a dead probe is device gone", func(t *testing.T) {
		f, c := newOne()
		f.failOp = func(op string) error {
			if op == accessOpRead {
				return wrapErrno(unix.EIO)
			}
			return nil
		}
		f.probeFn = func() error { return wrapErrno(unix.ENODEV) }
		if _, err := c.Get(mixerID(micVol)); !errors.Is(err, ErrDeviceGone) {
			t.Fatalf("Get = %v, want ErrDeviceGone", err)
		}
	})
	t.Run("EIO with a live probe keeps the errno", func(t *testing.T) {
		f, c := newOne()
		f.failOp = func(op string) error {
			if op == accessOpRead {
				return wrapErrno(unix.EIO)
			}
			return nil
		}
		_, err := c.Get(mixerID(micVol))
		if !errors.Is(err, unix.EIO) || errors.Is(err, ErrDeviceGone) {
			t.Fatalf("Get = %v, want the wrapped EIO", err)
		}
	})
	t.Run("ENOENT is a missing control, not a lost device", func(t *testing.T) {
		_, c := newOne()
		err := c.Set(mixerID("Nope"), []int64{1})
		if _, ok := errors.AsType[*ControlNotFoundError](err); !ok || errors.Is(err, ErrDeviceGone) {
			t.Fatalf("Set = %v, want *ControlNotFoundError and not ErrDeviceGone", err)
		}
	})
	t.Run("EPERM on write is a held lock", func(t *testing.T) {
		f, c := newOne()
		f.failOp = func(op string) error {
			if op == accessOpWrite {
				return wrapErrno(unix.EPERM)
			}
			return nil
		}
		err := c.Set(mixerID(micVol), []int64{3})
		ae, ok := errors.AsType[*ControlAccessError](err)
		if !ok || !ae.Locked || ae.Op != "write" {
			t.Fatalf("Set = %v, want *ControlAccessError{Locked: true}", err)
		}
	})
	t.Run("EINVAL from write is a driver rejection", func(t *testing.T) {
		f, c := newOne()
		f.failOp = func(op string) error {
			if op == accessOpWrite {
				return wrapErrno(unix.EINVAL)
			}
			return nil
		}
		err := c.Set(mixerID(micVol), []int64{3})
		ve, ok := errors.AsType[*ControlValueError](err)
		if !ok || !slices.Equal(ve.Values, []int64{3}) || ve.Max != 10 {
			t.Fatalf("Set = %v, want *ControlValueError carrying the values and range", err)
		}
	})
	t.Run("EINVAL with a dead probe is device gone", func(t *testing.T) {
		f, c := newOne()
		f.failOp = func(op string) error {
			if op == accessOpWrite {
				return wrapErrno(unix.EINVAL)
			}
			return nil
		}
		f.probeFn = func() error { return wrapErrno(unix.ENODEV) }
		if err := c.Set(mixerID(micVol), []int64{3}); !errors.Is(err, ErrDeviceGone) {
			t.Fatalf("Set = %v, want ErrDeviceGone", err)
		}
	})
	t.Run("Close racing the probe wins", func(t *testing.T) {
		f, c := newOne()
		f.failOp = func(op string) error {
			if op == accessOpRead {
				return wrapErrno(unix.EIO)
			}
			return nil
		}
		f.probeFn = func() error { _ = c.Close(); return wrapErrno(unix.ENODEV) }
		if _, err := c.Get(mixerID(micVol)); !errors.Is(err, ErrClosed) {
			t.Fatalf("Get = %v, want ErrClosed", err)
		}
	})
}

func TestControlsAfterUnplug(t *testing.T) {
	f := &fakeCtl{}
	f.vol(micVol, 0, 10, 0, 1)
	c := newTestControls(f)
	if _, err := c.List(); err != nil {
		t.Fatalf("List: %v", err)
	}
	f.disconnected = true
	id := mixerID(micVol)
	check := func(want error) {
		t.Helper()
		_, e1 := c.List()
		_, e2 := c.Info(id)
		_, e3 := c.Get(id)
		e4 := c.Set(id, []int64{1})
		_, e5 := c.CaptureVolume()
		_, e6 := c.SetCaptureVolumePercent(50)
		for i, err := range []error{e1, e2, e3, e4, e5, e6} {
			if !errors.Is(err, want) {
				t.Errorf("call %d = %v, want %v", i, err, want)
			}
		}
	}
	check(ErrDeviceGone)
	if err := c.Close(); err != nil {
		t.Errorf("Close after unplug = %v, want nil", err)
	}
	check(ErrClosed)
}

func TestControlsListSkipsElementRemovedMidList(t *testing.T) {
	t.Run("between the id list and Info", func(t *testing.T) {
		f := &fakeCtl{}
		f.vol("A Volume", 0, 10, 0, 1)
		b := f.vol("B Volume", 0, 10, 0, 1)
		f.vol("C Volume", 0, 10, 0, 1)
		f.onInfo = func(f *fakeCtl, name string) {
			if name == "A Volume" {
				f.elems = slices.DeleteFunc(f.elems, func(e *fakeCtlElem) bool { return e == b })
			}
		}
		got, err := newTestControls(f).List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if names := listNames(got); !slices.Equal(names, []string{"A Volume", "C Volume"}) {
			t.Errorf("List = %v, want B omitted", names)
		}
	})
	t.Run("between Info and TLV", func(t *testing.T) {
		f := &fakeCtl{}
		a := f.vol("A Volume", 0, 10, 0, 1)
		a.access |= alsa.CtlAccessTLVRead
		a.tlv = []uint32{1, 8, 0, 50}
		f.vol("C Volume", 0, 10, 0, 1)
		f.onTLV = func(f *fakeCtl, _ uint32) {
			f.elems = slices.DeleteFunc(f.elems, func(e *fakeCtlElem) bool { return e == a })
		}
		got, err := newTestControls(f).List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if names := listNames(got); !slices.Equal(names, []string{"C Volume"}) {
			t.Errorf("List = %v, want A omitted", names)
		}
	})
}

func listNames(l []ControlInfo) []string {
	out := make([]string, 0, len(l))
	for i := range l {
		out = append(out, l[i].ID.Name)
	}
	return out
}

func TestControlsListReportsDBAndItems(t *testing.T) {
	f := &fakeCtl{}
	v := f.vol(micVol, 0, 40, 0, 1)
	v.access |= alsa.CtlAccessTLVRead
	v.tlv = []uint32{1, 8, uint32(0xfffffb50 /* -12.00 dB */), 100} // 1.00 dB per step
	chmap := f.add("Capture Channel Map", int32(ControlPCM), alsa.CtlTypeInteger, 1)
	chmap.access |= alsa.CtlAccessTLVRead
	chmap.tlv = []uint32{0, 16, 0x101, 8, 3, 4}
	en := f.add("Mode", int32(ControlMixer), alsa.CtlTypeEnumerated, 1)
	en.items = []string{"Off", "On"}
	got, err := newTestControls(f).List()
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].HasDB || got[0].MinDB != -12 || got[0].MaxDB != 28 {
		t.Errorf("volume dB = %v %v..%v, want -12..28", got[0].HasDB, got[0].MinDB, got[0].MaxDB)
	}
	if got[1].HasDB {
		t.Error("a channel map TLV was read as dB")
	}
	if !slices.Equal(got[2].Items, []string{"Off", "On"}) || got[2].Max != 1 {
		t.Errorf("enumerated items = %v max %d", got[2].Items, got[2].Max)
	}
}

func TestControlsIdentityAcrossInstances(t *testing.T) {
	f := &fakeCtl{}
	other := f.vol("Other Volume", 0, 10, 0, 1)
	other.vals[0] = 9
	mine := f.vol("Mine Volume", 0, 10, 0, 1)
	mine.vals[0] = 4
	c := newTestControls(f)
	// A ControlID stored when "Mine Volume" had numid 1, which now names Other.
	stale := ControlID{Interface: ControlMixer, Name: "Mine Volume", NumID: 1}
	got, err := c.Get(stale)
	if err != nil || !slices.Equal(got, []int64{4}) {
		t.Fatalf("Get with a stale NumID = %v, %v, want the value of the element named by the tuple (4)", got, err)
	}
	if err := c.Set(stale, []int64{6}); err != nil {
		t.Fatal(err)
	}
	if other.vals[0] != 9 || mine.vals[0] != 6 {
		t.Errorf("write went to the wrong element: other=%d mine=%d", other.vals[0], mine.vals[0])
	}
}

func TestCaptureVolumeSelection(t *testing.T) {
	t.Run("none (AudioMoth shape)", func(t *testing.T) {
		f := &fakeCtl{}
		sw := f.add("Mic Capture Switch", int32(ControlMixer), alsa.CtlTypeBoolean, 1)
		sw.max = 1
		f.add("Capture Channel Map", int32(ControlPCM), alsa.CtlTypeInteger, 1)
		_, err := newTestControls(f).CaptureVolume()
		nf, ok := errors.AsType[*ControlNotFoundError](err)
		if !ok || !nf.Pattern || !errors.Is(err, ErrControlNotFound) {
			t.Fatalf("CaptureVolume = %v, want *ControlNotFoundError{Pattern: true}", err)
		}
		if _, err := newTestControls(f).SetCaptureVolumePercent(50); !errors.Is(err, ErrControlNotFound) {
			t.Errorf("SetCaptureVolumePercent = %v, want ErrControlNotFound", err)
		}
	})
	t.Run("one", func(t *testing.T) {
		f := &fakeCtl{}
		f.vol("Mic Playback Volume", 0, 10, 0, 1)
		f.vol(micVol, 0, 10, 0, 1)
		got, err := newTestControls(f).CaptureVolume()
		if err != nil || got.ID.Name != micVol {
			t.Fatalf("CaptureVolume = %v, %v", got.ID, err)
		}
	})
	t.Run("two indices are ambiguous", func(t *testing.T) {
		f := &fakeCtl{}
		f.vol("Capture Volume", 0, 10, 0, 1)
		f.vol("Capture Volume", 0, 10, 0, 1)
		_, err := newTestControls(f).CaptureVolume()
		ae, ok := errors.AsType[*AmbiguousControlError](err)
		if !ok || len(ae.Matches) != 2 || ae.Matches[0].Index != 0 || ae.Matches[1].Index != 1 {
			t.Fatalf("CaptureVolume = %v, want *AmbiguousControlError listing both indices", err)
		}
	})
	t.Run("read-only and inactive are not candidates", func(t *testing.T) {
		f := &fakeCtl{}
		f.vol("Capture Volume", 0, 10, 0, 1).access = alsa.CtlAccessRead
		f.vol(micVol, 0, 10, 0, 1).access = alsa.CtlAccessRead | alsa.CtlAccessWrite | alsa.CtlAccessInactive
		if _, err := newTestControls(f).CaptureVolume(); !errors.Is(err, ErrControlNotFound) {
			t.Fatalf("CaptureVolume = %v, want ErrControlNotFound", err)
		}
	})
	t.Run("name must end in Capture Volume", func(t *testing.T) {
		f := &fakeCtl{}
		f.vol("Capture Volume Boost", 0, 10, 0, 1)
		f.vol("MyCapture Volume", 0, 10, 0, 1)
		if _, err := newTestControls(f).CaptureVolume(); !errors.Is(err, ErrControlNotFound) {
			t.Fatalf("CaptureVolume = %v, want ErrControlNotFound", err)
		}
	})
	t.Run("follows the element set, never cached", func(t *testing.T) {
		f := &fakeCtl{}
		f.vol(micVol, 0, 10, 0, 1)
		c := newTestControls(f)
		first, err := c.CaptureVolume()
		if err != nil {
			t.Fatal(err)
		}
		extra := f.vol("Line Capture Volume", 0, 10, 0, 1)
		if _, err := c.CaptureVolume(); !errors.Is(err, error(&AmbiguousControlError{})) && func() bool { _, ok := errors.AsType[*AmbiguousControlError](err); return !ok }() {
			t.Fatalf("with a second match CaptureVolume = %v, want *AmbiguousControlError", err)
		}
		f.elems = slices.DeleteFunc(f.elems, func(e *fakeCtlElem) bool { return e == extra })
		again, err := c.CaptureVolume()
		if err != nil || again.ID.Name != first.ID.Name {
			t.Fatalf("after the extra element is gone CaptureVolume = %v, %v, want %v", again.ID, err, first.ID)
		}
	})
}

func TestSetCaptureVolumePercent(t *testing.T) {
	tests := []struct {
		name          string
		min, max, stp int64
		count         uint32
		percent       float64
		want          int64
	}{
		{"0 gives min", 0, 127, 0, 1, 0, 0},
		{"100 gives max", 0, 127, 0, 1, 100, 127},
		{"50 on 0..127 rounds half away from zero to 64", 0, 127, 0, 1, 50, 64},
		{"step 5 at 33 gives 35", 0, 100, 5, 1, 33, 35},
		{"step 10 tie goes to the lower value", 0, 100, 10, 1, 55, 50},
		// uint64(-11) % 5 == 0 because 2^64 mod 5 == 1, so -11 is the valid value nearest -10.
		{"negative range with step 5 uses the unsigned rule", -20, 0, 5, 1, 50, -11},
		{"negative min step 1", -60, 0, 1, 1, 50, -30},
		{"all channels get the same value", 0, 100, 0, 4, 25, 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeCtl{}
			e := f.vol(micVol, tt.min, tt.max, tt.stp, tt.count)
			got, err := newTestControls(f).SetCaptureVolumePercent(tt.percent)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("returned %d, want %d", got, tt.want)
			}
			if !stepOK(got, tt.stp) {
				t.Errorf("%d breaks the kernel step rule for step %d", got, tt.stp)
			}
			for i, v := range e.vals {
				if v != got {
					t.Errorf("value %d = %d, want %d", i, v, got)
				}
			}
			if len(e.vals) != int(tt.count) {
				t.Errorf("%d values written, want %d", len(e.vals), tt.count)
			}
		})
	}
	t.Run("bad percent", func(t *testing.T) {
		f := &fakeCtl{}
		f.vol(micVol, 0, 10, 0, 1)
		c := newTestControls(f)
		for _, p := range []float64{math.NaN(), -1, 101, math.Inf(1)} {
			if _, err := c.SetCaptureVolumePercent(p); !isValueErr(err) {
				t.Errorf("percent %v = %v, want *ControlValueError", p, err)
			}
		}
		if f.count("write") != 0 || f.count("list") != 0 {
			t.Errorf("device touched for a bad percent: %v", f.calls)
		}
	})
	t.Run("no valid step value", func(t *testing.T) {
		f := &fakeCtl{}
		f.vol(micVol, 1, 4, 5, 1) // no multiple of 5 in 1..4
		if _, err := newTestControls(f).SetCaptureVolumePercent(50); !isValueErr(err) {
			t.Errorf("err = %v, want *ControlValueError", err)
		}
	})
}

func isValueErr(err error) bool {
	_, ok := errors.AsType[*ControlValueError](err)
	return ok
}

func TestControlsAfterCloseReturnErrClosed(t *testing.T) {
	f := &fakeCtl{}
	f.vol(micVol, 0, 10, 0, 1)
	c := newTestControls(f)
	_ = c.Close()
	n := len(f.calls)
	id := mixerID(micVol)
	_, e1 := c.List()
	_, e2 := c.Info(id)
	_, e3 := c.Get(id)
	e4 := c.Set(id, []int64{1})
	_, e5 := c.CaptureVolume()
	_, e6 := c.SetCaptureVolumePercent(10)
	for i, err := range []error{e1, e2, e3, e4, e5, e6} {
		if !errors.Is(err, ErrClosed) {
			t.Errorf("call %d = %v, want ErrClosed", i, err)
		}
	}
	if len(f.calls) != n {
		t.Errorf("ioctls after Close: %v", f.calls[n:])
	}
}

// TestControlsOutOfRangeFieldsAreNotFound pins that a ControlID field the
// kernel's 32-bit tuple cannot hold names no element, instead of wrapping
// onto one that exists.
func TestControlsOutOfRangeFieldsAreNotFound(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("an int cannot exceed 32 bits here")
	}
	f := &fakeCtl{}
	f.vol(plainVol, 0, 10, 0, 1)
	c := newTestControls(f)
	big := int64(1) << 32
	for name, id := range map[string]ControlID{
		"interface":   {Interface: ControlInterface(big + int64(ControlMixer)), Name: plainVol},
		"device":      {Interface: ControlMixer, Device: int(big), Name: plainVol},
		"subdevice":   {Interface: ControlMixer, Subdevice: int(big), Name: plainVol},
		"index":       {Interface: ControlMixer, Index: int(big), Name: plainVol},
		"neg-iface":   {Interface: -1, Name: plainVol},
		"max-int-ifc": {Interface: ControlInterface(big>>1 + 0), Name: plainVol},
	} {
		if _, err := c.Get(id); !errors.Is(err, ErrControlNotFound) {
			t.Errorf("%s: Get = %v, want ErrControlNotFound", name, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("ioctls issued for out-of-range ids: %v", f.calls)
	}
	if _, err := c.Get(mixerID(plainVol)); err != nil {
		t.Errorf("the in-range id no longer resolves: %v", err)
	}
}

// TestOpenControlsMissingNodeOnPresentCardIsNotDeviceGone pins that a control
// node that is not there (a container that maps only the PCM node) is reported
// as the open error while the card itself is still present, so the caller does
// not retire a working capture device.
func TestOpenControlsMissingNodeOnPresentCardIsNotDeviceGone(t *testing.T) {
	useFixture(t, hostLayout())
	withOpenCtl(t, func(int) (ctlHandle, error) { return nil, wrapErrno(unix.ENOENT) })
	for name, d := range map[string]DeviceInfo{
		"stable id":  mustResolve(t, wantSerialID),
		"numeric id": mustResolve(t, hwAddrCard1),
	} {
		_, err := OpenControls(d)
		if !errors.Is(err, unix.ENOENT) || errors.Is(err, ErrDeviceGone) {
			t.Errorf("%s: OpenControls = %v, want the wrapped ENOENT and not ErrDeviceGone", name, err)
		}
	}
}

// TestOpenControlsNumericIdOnAbsentCardIsDeviceGone is the counterpart: a
// numeric id has no post-open identity check, so ENOENT on a card number that
// is no longer in /proc/asound is the only sign the device went away.
func TestOpenControlsNumericIdOnAbsentCardIsDeviceGone(t *testing.T) {
	useFixture(t, hostLayout())
	withOpenCtl(t, func(int) (ctlHandle, error) { return nil, wrapErrno(unix.ENOENT) })
	d := DeviceInfo{ID: "hw:7,0", Card: 7, Device: 0}
	if _, err := OpenControls(d); !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("OpenControls = %v, want ErrDeviceGone", err)
	}
}

func TestOpenControlsRejectsBadDeviceInfoBeforeOpening(t *testing.T) {
	useFixture(t, hostLayout())
	withOpenCtl(t, func(int) (ctlHandle, error) {
		t.Error("a control device was opened for an unusable DeviceInfo")
		return nil, errShouldNotOpen
	})
	for name, tt := range map[string]struct {
		d    DeviceInfo
		want func(error) bool
	}{
		"empty": {DeviceInfo{}, func(err error) bool { _, ok := errors.AsType[*ConfigError](err); return ok }},
		"card and id disagree": {DeviceInfo{ID: hwAddrCard1, Card: 2}, func(err error) bool {
			_, ok := errors.AsType[*BadDeviceError](err)
			return ok
		}},
		"port id on a numeric id": {DeviceInfo{ID: hwAddrCard1, Card: 1, PortID: "usb:1686:067f:p=0000:00:14.0-3:if=0,0"}, func(err error) bool {
			_, ok := errors.AsType[*BadDeviceError](err)
			return ok
		}},
	} {
		if c, err := OpenControls(tt.d); c != nil || !tt.want(err) {
			t.Errorf("%s: OpenControls = %v, %v, want a typed resolve error", name, c, err)
		}
	}
}

func TestControlsGet(t *testing.T) {
	f := &fakeCtl{}
	sw := f.add("Capture Switch", int32(ControlMixer), alsa.CtlTypeBoolean, 2)
	sw.max, sw.vals = 1, []int64{1, 0}
	en := f.add(enumName, int32(ControlMixer), alsa.CtlTypeEnumerated, 1)
	en.items, en.vals = []string{"a", "b", "c"}, []int64{2}
	f.add("Big", int32(ControlMixer), alsa.CtlTypeInteger64, 1)
	wo := f.vol("Write Only Volume", 0, 10, 0, 1)
	wo.access = alsa.CtlAccessWrite
	c := newTestControls(f)

	for name, tt := range map[string]struct {
		id   string
		want []int64
	}{"boolean": {"Capture Switch", []int64{1, 0}}, "enumerated": {enumName, []int64{2}}} {
		got, err := c.Get(mixerID(tt.id))
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("%s: Get = %v, %v, want %v", name, got, err, tt.want)
		}
	}
	reads := f.count("read")
	if _, err := c.Get(mixerID("Big")); !isValueErr(err) {
		t.Errorf("Get on INTEGER64 = %v, want *ControlValueError", err)
	}
	_, err := c.Get(mixerID("Write Only Volume"))
	if ae, ok := errors.AsType[*ControlAccessError](err); !ok || ae.Op != "read" {
		t.Errorf("Get on a write-only element = %v, want *ControlAccessError for read", err)
	}
	if f.count("read") != reads {
		t.Error("a read ioctl was issued for a rejected Get")
	}
}

func TestControlsListToleratesMissingOrOversizedTLV(t *testing.T) {
	f := &fakeCtl{}
	none := f.vol("No Capture Volume", 0, 10, 0, 1) // claims a TLV, has none: ENXIO
	none.access |= alsa.CtlAccessTLVRead
	big := f.vol("Big Capture Volume", 0, 10, 0, 1)
	big.access |= alsa.CtlAccessTLVRead
	big.tlv = []uint32{1, 8, 0, 50}
	f.failOp = func(op string) error {
		if op == "tlv" && f.count("tlv")%2 == 0 { // second TLV read: ENOMEM
			return wrapErrno(unix.ENOMEM)
		}
		return nil
	}
	got, err := newTestControls(f).List()
	if err != nil || len(got) != 2 {
		t.Fatalf("List = %v, %v, want both elements", got, err)
	}
	if got[0].HasDB || got[1].HasDB {
		t.Error("an element without a readable TLV reports dB")
	}
}

func TestControlsListErrorsOnBrokenTLVOrItems(t *testing.T) {
	t.Run("TLV read fails with EIO", func(t *testing.T) {
		f := &fakeCtl{}
		v := f.vol("Mic Capture Volume", 0, 10, 0, 1)
		v.access |= alsa.CtlAccessTLVRead
		v.tlv = []uint32{1, 8, 0, 50}
		f.failOp = func(op string) error {
			if op == "tlv" {
				return wrapErrno(unix.EIO)
			}
			return nil
		}
		if _, err := newTestControls(f).List(); !errors.Is(err, unix.EIO) {
			t.Errorf("List = %v, want the wrapped EIO", err)
		}
	})
	t.Run("too many enumerated items", func(t *testing.T) {
		f := &fakeCtl{}
		en := f.add(enumName, int32(ControlMixer), alsa.CtlTypeEnumerated, 1)
		en.items = make([]string, alsa.CtlMaxItems+1)
		if _, err := newTestControls(f).List(); err == nil {
			t.Error("List accepted an element with more items than the cap")
		}
	})
	t.Run("item name read fails", func(t *testing.T) {
		f := &fakeCtl{}
		en := f.add(enumName, int32(ControlMixer), alsa.CtlTypeEnumerated, 1)
		en.items = []string{"a"}
		f.failOp = func(op string) error {
			if op == "item" {
				return wrapErrno(unix.EIO)
			}
			return nil
		}
		if _, err := newTestControls(f).List(); !errors.Is(err, unix.EIO) {
			t.Errorf("List = %v, want the wrapped EIO", err)
		}
	})
}
