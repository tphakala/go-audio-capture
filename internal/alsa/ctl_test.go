//go:build linux

package alsa

import (
	"bytes"
	"errors"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fakeElem is one element in fakeCtlKernel's table.
type fakeElem struct {
	id     CtlElemID
	typ    int32
	access uint32
	count  uint32
	min    int64
	max    int64
	step   int64
	items  []string
	vals   []int64
	tlv    []uint32
	// userElement elements are always range and step checked on write, as
	// snd_ctl_elem_user_put does; driver elements only when the kernel has
	// CONFIG_SND_CTL_INPUT_VALIDATION (fakeCtlKernel.validate).
	userElement bool
	locked      bool // another file holds the write lock: EPERM on write
}

// fakeCtlKernel models the parts of sound/core/control.c the package depends
// on: lookup by numid first and otherwise by a 44-byte name compare, ENOENT for
// a missing element, EPERM for a missing access bit or a lock, ENXIO for an
// element without TLV, TLV copied after the header without rewriting its length,
// ENODEV for everything once disconnected, and ENOTTY for a request number it
// does not know (so a wrong struct size fails instead of working by accident).
type fakeCtlKernel struct {
	elems        []*fakeElem
	validate     bool
	disconnected bool

	calls    []uintptr
	numids   []uint32 // Numid of every ELEM_INFO, ELEM_READ and ELEM_WRITE request
	listCall int
	// onList runs at the start of every ELEM_LIST ioctl, before it is answered.
	onList func(k *fakeCtlKernel, call int, space uint32)
	// onCall, when set, runs first in every ioctl; a non-nil result is returned.
	onCall func(req uintptr) error
}

func newFakeElem(name string, typ int32, count uint32) *fakeElem {
	id, err := NewCtlElemID(2, 0, 0, name, 0)
	if err != nil {
		panic(err)
	}
	return &fakeElem{id: id, typ: typ, access: CtlAccessRead | CtlAccessWrite, count: count, vals: make([]int64, count)}
}

func (k *fakeCtlKernel) add(e *fakeElem) *fakeElem {
	e.id.Numid = uint32(len(k.elems) + 1)
	k.elems = append(k.elems, e)
	return e
}

func (k *fakeCtlKernel) remove(e *fakeElem) {
	k.elems = slices.DeleteFunc(k.elems, func(x *fakeElem) bool { return x == e })
}

func (k *fakeCtlKernel) find(id CtlElemID) *fakeElem {
	for _, e := range k.elems {
		if id.Numid != 0 {
			if e.id.Numid == id.Numid {
				return e
			}
			continue
		}
		if e.id.Iface == id.Iface && e.id.Device == id.Device && e.id.Subdevice == id.Subdevice &&
			e.id.Index == id.Index && e.id.Name == id.Name {
			return e
		}
	}
	return nil
}

func (k *fakeCtlKernel) count(req uintptr) int {
	n := 0
	for _, c := range k.calls {
		if c == req {
			n++
		}
	}
	return n
}

func (k *fakeCtlKernel) ioctl(_ int, req uintptr, arg unsafe.Pointer) error {
	k.calls = append(k.calls, req)
	if len(k.calls) > loopGuardCalls {
		return errLoopGuard
	}
	if k.onCall != nil {
		if err := k.onCall(req); err != nil {
			return err
		}
	}
	if k.disconnected {
		return unix.ENODEV
	}
	switch req {
	case iocCtlPVersion:
		*(*int32)(arg) = 0x20009
	case iocCtlElemList:
		return k.list((*ctlElemList)(arg))
	case iocCtlElemInfo:
		return k.info((*CtlElemInfo)(arg))
	case iocCtlElemRead:
		return k.read((*ctlElemValue)(arg))
	case iocCtlElemWrite:
		return k.write((*ctlElemValue)(arg))
	case iocCtlTLVRead:
		return k.readTLV((*[ctlTLVWords]uint32)(arg))
	default:
		return unix.ENOTTY
	}
	return nil
}

func (k *fakeCtlKernel) list(l *ctlElemList) error {
	k.listCall++
	if k.onList != nil {
		k.onList(k, k.listCall, l.Space)
	}
	l.Count = uint32(len(k.elems))
	l.Used = 0
	if l.Space == 0 {
		return nil
	}
	out := unsafe.Slice((*CtlElemID)(l.Pids), l.Space)
	for i := l.Offset; i < uint32(len(k.elems)) && l.Used < l.Space; i++ {
		out[l.Used] = k.elems[i].id
		l.Used++
	}
	return nil
}

func (k *fakeCtlKernel) info(in *CtlElemInfo) error {
	k.numids = append(k.numids, in.ID.Numid)
	e := k.find(in.ID)
	if e == nil {
		return unix.ENOENT
	}
	item := uint32(0)
	if e.typ == CtlTypeEnumerated {
		item = uint32(in.Value[4]) | uint32(in.Value[5])<<8
	}
	in.ID = e.id
	in.Type = e.typ
	in.Access = e.access
	in.Count = e.count
	in.Value = [128]byte{}
	switch e.typ {
	case CtlTypeInteger:
		w := int(unsafe.Sizeof(clong(0)))
		encodeClong(in.Value[0:w], e.min)
		encodeClong(in.Value[w:2*w], e.max)
		encodeClong(in.Value[2*w:3*w], e.step)
	case CtlTypeEnumerated:
		in.Value[0] = byte(len(e.items))
		in.Value[1] = byte(len(e.items) >> 8)
		if int(item) >= len(e.items) {
			return unix.EINVAL
		}
		in.Value[4] = byte(item)
		in.Value[5] = byte(item >> 8)
		copy(in.Value[8:8+64], e.items[item])
	}
	return nil
}

func (k *fakeCtlKernel) read(v *ctlElemValue) error {
	k.numids = append(k.numids, v.ID.Numid)
	e := k.find(v.ID)
	if e == nil {
		return unix.ENOENT
	}
	if e.access&CtlAccessRead == 0 {
		return unix.EPERM
	}
	w := slotSize(e.typ)
	for i, x := range e.vals {
		b := v.Value[i*w : (i+1)*w]
		if e.typ == CtlTypeEnumerated {
			b[0], b[1], b[2], b[3] = byte(x), byte(x>>8), byte(x>>16), byte(x>>24)
		} else {
			encodeClong(b, x)
		}
	}
	return nil
}

func (k *fakeCtlKernel) write(v *ctlElemValue) error {
	k.numids = append(k.numids, v.ID.Numid)
	e := k.find(v.ID)
	if e == nil {
		return unix.ENOENT
	}
	if e.access&CtlAccessWrite == 0 || e.locked {
		return unix.EPERM
	}
	w := slotSize(e.typ)
	vals := make([]int64, e.count)
	for i := range vals {
		b := v.Value[i*w : (i+1)*w]
		if e.typ == CtlTypeEnumerated {
			vals[i] = int64(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)
		} else {
			vals[i] = decodeClong(b)
		}
	}
	if k.validate || e.userElement {
		for _, x := range vals {
			if x < e.min || x > e.max {
				return unix.EINVAL
			}
			// control.c:1044-1063: the remainder of the value itself, unsigned.
			if e.step != 0 && uint64(x)%uint64(e.step) != 0 {
				return unix.EINVAL
			}
		}
	}
	e.vals = vals
	return nil
}

func (k *fakeCtlKernel) readTLV(buf *[ctlTLVWords]uint32) error {
	if buf[0] == 0 {
		return unix.EINVAL
	}
	var e *fakeElem
	for _, x := range k.elems {
		if x.id.Numid == buf[0] {
			e = x
		}
	}
	if e == nil {
		return unix.ENOENT
	}
	if e.tlv == nil {
		return unix.ENXIO
	}
	if uint64(buf[1]) < uint64(len(e.tlv))*4 {
		return unix.ENOMEM
	}
	copy(buf[2:], e.tlv) // the header length is left as the caller set it
	return nil
}

func newFakeCtl(k *fakeCtlKernel) *Ctl { return newCtl(-1, k.ioctl) }

func TestCtlListRetriesWhenCountChanges(t *testing.T) {
	k := &fakeCtlKernel{}
	k.add(newFakeElem("A", CtlTypeInteger, 1))
	k.add(newFakeElem("B", CtlTypeInteger, 1))
	grown := false
	k.onList = func(k *fakeCtlKernel, _ int, space uint32) {
		if space > 0 && !grown { // an element appears between the count and the fetch
			grown = true
			k.add(newFakeElem("C", CtlTypeInteger, 1))
		}
	}
	ids, err := newFakeCtl(k).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("List returned %d ids, want 3 after the retry", len(ids))
	}
}

func TestCtlListGivesUpAfterBoundedAttempts(t *testing.T) {
	k := &fakeCtlKernel{}
	k.add(newFakeElem("A", CtlTypeInteger, 1))
	k.onList = func(k *fakeCtlKernel, _ int, space uint32) {
		if space > 0 {
			k.add(newFakeElem("X", CtlTypeInteger, 1))
		}
	}
	_, err := newFakeCtl(k).List()
	if !errors.Is(err, errCtlListUnstable) {
		t.Fatalf("List error = %v, want errCtlListUnstable", err)
	}
	if got, want := k.count(iocCtlElemList), 2*ctlListAttempts; got != want {
		t.Errorf("ELEM_LIST issued %d times, want exactly %d (two per round)", got, want)
	}
}

func TestCtlListEmpty(t *testing.T) {
	ids, err := newFakeCtl(&fakeCtlKernel{}).List()
	if err != nil || len(ids) != 0 {
		t.Fatalf("List on an empty card = %v, %v", ids, err)
	}
}

func TestCtlRequestsNeverSendCallerNumid(t *testing.T) {
	k := &fakeCtlKernel{}
	k.add(newFakeElem("Other", CtlTypeInteger, 1)) // numid 1
	mine := k.add(newFakeElem("Mine", CtlTypeInteger, 1))
	mine.id.Numid = 7
	mine.max = 10
	c := newFakeCtl(k)

	// An id that carries a stale numid 1, which now belongs to "Other".
	id := mine.id
	id.Numid = 1
	info, err := c.Info(id)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.ID.NameString() != "Mine" || info.ID.Numid != 7 {
		t.Errorf("Info resolved %q numid %d, want Mine numid 7", info.ID.NameString(), info.ID.Numid)
	}
	if _, err := c.ReadValues(id, CtlTypeInteger, 1); err != nil {
		t.Fatalf("ReadValues: %v", err)
	}
	if err := c.WriteValues(id, CtlTypeInteger, []int64{3}); err != nil {
		t.Fatalf("WriteValues: %v", err)
	}
	if mine.vals[0] != 3 {
		t.Errorf("write landed on the wrong element: Mine = %d", mine.vals[0])
	}
	pick := k.add(newFakeElem("Pick", CtlTypeEnumerated, 1))
	pick.items = []string{"Off", "On"}
	pick.id.Numid = 9
	enumID := pick.id
	enumID.Numid = 1
	if name, err := c.EnumItemName(enumID, 1); err != nil || name != "On" {
		t.Errorf("EnumItemName with a stale numid = %q, %v, want On from Pick", name, err)
	}
	if len(k.numids) != 4 {
		t.Fatalf("recorded %d requests, want 4", len(k.numids))
	}
	for i, n := range k.numids {
		if n != 0 {
			t.Errorf("request %d carried numid %d, want 0", i, n)
		}
	}
}

func TestCtlNameLimits(t *testing.T) {
	long44 := strings.Repeat("A", CtlNameLen)
	if _, err := NewCtlElemID(2, 0, 0, long44, 0); err != nil {
		t.Errorf("44-byte name rejected: %v", err)
	}
	for name, n := range map[string]string{
		"45 bytes":     long44 + "B",
		"embedded NUL": "Mic\x00Volume",
	} {
		if _, err := NewCtlElemID(2, 0, 0, n, 0); err == nil {
			t.Errorf("%s: accepted, want an error", name)
		}
	}
}

func TestCtlInfoDecodes(t *testing.T) {
	k := &fakeCtlKernel{}
	vol := k.add(newFakeElem("Mic Capture Volume", CtlTypeInteger, 2))
	vol.min, vol.max, vol.step = -20, 100, 5
	en := k.add(newFakeElem("Mode", CtlTypeEnumerated, 1))
	en.items = []string{"Off", "On"}
	c := newFakeCtl(k)
	got, err := c.Info(vol.id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != CtlTypeInteger || got.Count != 2 || got.Min != -20 || got.Max != 100 || got.Step != 5 {
		t.Errorf("integer info = %+v", got)
	}
	got, err = c.Info(en.id)
	if err != nil || got.Items != 2 {
		t.Fatalf("enumerated info = %+v, %v", got, err)
	}
	if name, err := c.EnumItemName(en.id, 1); err != nil || name != "On" {
		t.Errorf("EnumItemName(1) = %q, %v, want On", name, err)
	}
}

func TestCtlValueEncoding(t *testing.T) {
	k := &fakeCtlKernel{}
	vol := k.add(newFakeElem("Vol", CtlTypeInteger, 3))
	vol.min, vol.max = -100, 100
	en := k.add(newFakeElem("Enum", CtlTypeEnumerated, 2))
	en.items = []string{"a", "b", "c"}
	big := k.add(newFakeElem("Many", CtlTypeInteger, CtlMaxValues))
	big.min, big.max = -1000, 1000
	c := newFakeCtl(k)

	if err := c.WriteValues(vol.id, CtlTypeInteger, []int64{-100, -1, 77}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.ReadValues(vol.id, CtlTypeInteger, 3); !slices.Equal(got, []int64{-100, -1, 77}) {
		t.Errorf("integer round trip = %v", got)
	}
	if err := c.WriteValues(en.id, CtlTypeEnumerated, []int64{2, 1}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.ReadValues(en.id, CtlTypeEnumerated, 2); !slices.Equal(got, []int64{2, 1}) {
		t.Errorf("enumerated round trip = %v", got)
	}
	all := make([]int64, CtlMaxValues)
	for i := range all {
		all[i] = int64(i) - 64
	}
	if err := c.WriteValues(big.id, CtlTypeInteger, all); err != nil {
		t.Fatalf("128 values rejected: %v", err)
	}
	if got, _ := c.ReadValues(big.id, CtlTypeInteger, CtlMaxValues); !slices.Equal(got, all) {
		t.Error("128-value round trip differs")
	}
	before := len(k.calls)
	if err := c.WriteValues(big.id, CtlTypeInteger, make([]int64, CtlMaxValues+1)); err == nil {
		t.Error("129 values accepted")
	}
	if _, err := c.ReadValues(big.id, CtlTypeInteger, CtlMaxValues+1); err == nil {
		t.Error("reading 129 values accepted")
	}
	if _, err := c.ReadValues(big.id, CtlTypeBytes, 1); err == nil {
		t.Error("BYTES read accepted")
	}
	if len(k.calls) != before {
		t.Error("an ioctl was issued for a rejected value request")
	}
}

func TestCtlTLVIgnoresHeaderLength(t *testing.T) {
	k := &fakeCtlKernel{}
	e := k.add(newFakeElem("Vol", CtlTypeInteger, 1))
	e.tlv = []uint32{tlvDBScale, 8, 0xfffffb50, 0x10032} // min -12.00 dB, step 0.50 dB, mute
	got, err := newFakeCtl(k).TLV(1)
	if err != nil {
		t.Fatalf("TLV: %v", err)
	}
	if !slices.Equal(got, e.tlv) {
		t.Errorf("TLV = %#v, want exactly the 4 item words %#v", got, e.tlv)
	}

	// An item whose length word claims more than the buffer holds is an error,
	// not an out-of-range slice.
	e.tlv = []uint32{tlvDBScale, 1 << 20, 0, 0}
	if _, err := newFakeCtl(k).TLV(1); err == nil {
		t.Error("oversized TLV length accepted")
	}
}

func TestCtlTLVErrnos(t *testing.T) {
	k := &fakeCtlKernel{}
	k.add(newFakeElem("NoTLV", CtlTypeInteger, 1))
	c := newFakeCtl(k)
	if _, err := c.TLV(1); !errors.Is(err, unix.ENXIO) {
		t.Errorf("TLV on an element without one = %v, want ENXIO", err)
	}
	if _, err := c.TLV(9); !errors.Is(err, unix.ENOENT) {
		t.Errorf("TLV on a missing numid = %v, want ENOENT", err)
	}
}

func TestIsCtlGone(t *testing.T) {
	wrapped := func(e error) error { return &ioctlError{Op: "ELEM_READ", Err: e} }
	if !IsCtlGone(wrapped(unix.ENODEV)) {
		t.Error("ENODEV not classified as gone")
	}
	for _, e := range []error{unix.ENOENT, unix.ENXIO, unix.EBADFD, unix.EIO} {
		if IsCtlGone(wrapped(e)) {
			t.Errorf("%v classified as gone on a control fd", e)
		}
	}
}

func TestCtlDisconnectedReturnsENODEV(t *testing.T) {
	k := &fakeCtlKernel{}
	e := k.add(newFakeElem("Vol", CtlTypeInteger, 1))
	k.disconnected = true
	c := newFakeCtl(k)
	checks := map[string]error{}
	_, checks["List"] = c.List()
	_, checks["Info"] = c.Info(e.id)
	_, checks["Read"] = c.ReadValues(e.id, CtlTypeInteger, 1)
	checks["Write"] = c.WriteValues(e.id, CtlTypeInteger, []int64{0})
	_, checks["TLV"] = c.TLV(1)
	checks["Probe"] = c.Probe()
	for name, err := range checks {
		if !errors.Is(err, unix.ENODEV) {
			t.Errorf("%s after disconnect = %v, want ENODEV", name, err)
		}
	}
}

func TestCtlCloseWaitsForInflight(t *testing.T) {
	fd := openDevNull(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseOnce := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseOnce)
	c := newCtl(fd, func(_ int, req uintptr, _ unsafe.Pointer) error {
		if req == iocCtlPVersion {
			close(entered)
			<-release
		}
		return nil
	})
	go func() { _ = c.Probe() }()
	<-entered
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	// Wait until Close has marked the handle closed; it must then still be
	// blocked on the in-flight call, with the fd open.
	for {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			break
		}
		runtime.Gosched()
	}
	select {
	case <-done:
		t.Fatal("Close returned while an ioctl was in flight")
	default:
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
		t.Fatalf("the fd was closed while an ioctl was in flight: %v", err)
	}
	releaseOnce()
	<-done
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Errorf("the fd is still open after Close returned: %v", err)
	}
}

func TestCtlNoIoctlAfterClose(t *testing.T) {
	fd := openDevNull(t)
	var calls atomic.Int32
	c := newCtl(fd, func(int, uintptr, unsafe.Pointer) error { calls.Add(1); return nil })
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	id := CtlElemID{}
	_, e1 := c.List()
	_, e2 := c.Info(id)
	_, e3 := c.ReadValues(id, CtlTypeInteger, 1)
	e4 := c.WriteValues(id, CtlTypeInteger, []int64{1})
	_, e5 := c.TLV(1)
	e6 := c.Probe()
	for i, err := range []error{e1, e2, e3, e4, e5, e6} {
		if !errors.Is(err, unix.EBADF) {
			t.Errorf("call %d after Close = %v, want EBADF", i, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d ioctls reached the kernel after Close", n)
	}
}

// le writes v as a little-endian integer of w bytes without the production
// encoder, so the tests below do not agree with a wrong width by construction.
func le(w int, v int64) []byte {
	b := make([]byte, w)
	for i := range b {
		b[i] = byte(uint64(v) >> (8 * i))
	}
	return b
}

// kernelLong is the width of the kernel's `long` in a control value slot: 8 on
// LP64 and 4 on the ILP32 targets, chosen by the platform word size, not by
// the helpers under test.
func kernelLong() int {
	if strconv.IntSize == 64 {
		return 8
	}
	return 4
}

func TestCtlSlotWidthsAndEncodingAreLiteral(t *testing.T) {
	w := kernelLong()
	for name, tt := range map[string]struct {
		typ  int32
		want int
	}{
		"boolean":    {CtlTypeBoolean, w},
		"integer":    {CtlTypeInteger, w},
		"enumerated": {CtlTypeEnumerated, 4},
		"integer64":  {CtlTypeInteger64, 0},
		"bytes":      {CtlTypeBytes, 0},
		"iec958":     {CtlTypeIEC958, 0},
	} {
		if got := slotSize(tt.typ); got != tt.want {
			t.Errorf("slotSize(%s) = %d, want %d", name, got, tt.want)
		}
	}
	for _, v := range []int64{0, 1, -2, 0x01020304, -0x01020304} {
		b := make([]byte, w)
		encodeClong(b, v)
		if !bytes.Equal(b, le(w, v)) {
			t.Errorf("encodeClong(%d) = % x, want % x", v, b, le(w, v))
		}
		if got := decodeClong(le(w, v)); got != v {
			t.Errorf("decodeClong(% x) = %d, want %d", le(w, v), got, v)
		}
	}
}

func TestCtlInfoDecodesLiteralBytes(t *testing.T) {
	w := kernelLong()
	c := newCtl(-1, func(_ int, req uintptr, arg unsafe.Pointer) error {
		if req != iocCtlElemInfo {
			return unix.ENOTTY
		}
		in := (*CtlElemInfo)(arg)
		in.Type = CtlTypeInteger
		in.Count = 1
		in.Value = [128]byte{}
		copy(in.Value[0:], le(w, -20))
		copy(in.Value[w:], le(w, 100))
		copy(in.Value[2*w:], le(w, 5))
		return nil
	})
	got, err := c.Info(CtlElemID{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Min != -20 || got.Max != 100 || got.Step != 5 {
		t.Errorf("Info decoded min %d max %d step %d from literal bytes, want -20 100 5", got.Min, got.Max, got.Step)
	}
}

func TestOpenCtlFallsBackToReadOnlyOnlyOnEACCES(t *testing.T) {
	const path = "/dev/snd/controlC3"
	type call struct {
		path string
		mode int
	}
	for _, tt := range []struct {
		name      string
		errs      []error // result of each open, in order
		wantCalls []int   // open modes tried, in order
		wantErr   error
	}{
		{"rdwr works", []error{nil}, []int{unix.O_RDWR | unix.O_CLOEXEC}, nil},
		{"eacces falls back", []error{unix.EACCES, nil}, []int{unix.O_RDWR | unix.O_CLOEXEC, unix.O_RDONLY | unix.O_CLOEXEC}, nil},
		{"eacces twice", []error{unix.EACCES, unix.EACCES}, []int{unix.O_RDWR | unix.O_CLOEXEC, unix.O_RDONLY | unix.O_CLOEXEC}, unix.EACCES},
		{"other error is not retried", []error{unix.ENOENT}, []int{unix.O_RDWR | unix.O_CLOEXEC}, unix.ENOENT},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls []call
			swapOpenSeams(t, func(p string, mode int, _ uint32) (int, error) {
				calls = append(calls, call{p, mode})
				err := tt.errs[len(calls)-1]
				if err != nil {
					return -1, err
				}
				return openDevNull(t), nil
			}, nil)
			c, err := OpenCtl(3)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("OpenCtl: %v", err)
				}
				_ = c.Close()
			} else {
				if !errors.Is(err, tt.wantErr) || c != nil {
					t.Fatalf("OpenCtl = %v, %v, want a nil handle and %v", c, err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), path) {
					t.Errorf("error %q does not name %s", err, path)
				}
			}
			if len(calls) != len(tt.wantCalls) {
				t.Fatalf("open called %d times, want %d: %v", len(calls), len(tt.wantCalls), calls)
			}
			for i, m := range tt.wantCalls {
				if calls[i].path != path || calls[i].mode != m {
					t.Errorf("open %d = %v, want %s mode %#x", i, calls[i], path, m)
				}
			}
		})
	}
}
