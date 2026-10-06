//go:build linux

package alsa

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Element types, interfaces and access bits from include/uapi/sound/asound.h
// (SNDRV_CTL_ELEM_TYPE_*, SNDRV_CTL_ELEM_IFACE_*, SNDRV_CTL_ELEM_ACCESS_*).
const (
	CtlTypeBoolean    = 1
	CtlTypeInteger    = 2
	CtlTypeEnumerated = 3
	CtlTypeBytes      = 4
	CtlTypeIEC958     = 5
	CtlTypeInteger64  = 6

	CtlAccessRead     = 1 << 0
	CtlAccessWrite    = 1 << 1
	CtlAccessTLVRead  = 1 << 4
	CtlAccessInactive = 1 << 8

	// CtlNameLen is the size of snd_ctl_elem_id.name. The kernel compares at most
	// this many bytes (control.c snd_ctl_find_id strncmp), so a longer name sent
	// truncated would match whatever element shares its first 44 bytes.
	CtlNameLen = 44

	// CtlMaxValues is the number of value slots in snd_ctl_elem_value.
	CtlMaxValues = 128

	// CtlMaxItems bounds the enumerated item-name fetches per element.
	CtlMaxItems = 1024

	ctlListAttempts = 4
	ctlMaxElements  = 1 << 16
	ctlTLVWords     = 1024 // 4 KiB: header plus the largest TLV item accepted
)

// CtlElemID mirrors struct snd_ctl_elem_id (64 bytes on every ABI).
type CtlElemID struct {
	Numid     uint32
	Iface     int32
	Device    uint32
	Subdevice uint32
	Name      [CtlNameLen]byte
	Index     uint32
}

// ctlElemList mirrors struct snd_ctl_elem_list. Pids is an unsafe.Pointer so
// the GC keeps the id slice alive across the ioctl, as Xferi.Buf does.
type ctlElemList struct {
	Offset, Space, Used, Count uint32
	Pids                       unsafe.Pointer
	Reserved                   [50]byte
}

// CtlElemInfo mirrors struct snd_ctl_elem_info. Value is the raw union: integer
// {min,max,step} as three C longs, enumerated {items, item, name[64]}.
type CtlElemInfo struct {
	ID       CtlElemID
	Type     int32
	Access   uint32
	Count    uint32
	Owner    int32
	Value    [128]byte
	Reserved [64]byte
}

// ctlElemValue mirrors struct snd_ctl_elem_value. The padding before the value
// union depends on the ABI (see ctlValuePad); INTEGER and BOOLEAN values are C
// longs, ENUMERATED values are unsigned ints.
type ctlElemValue struct {
	ID       CtlElemID
	Indirect uint32
	_        [ctlValuePad]byte
	Value    [ctlValueBytes]byte
	Reserved [128]byte
}

// ctlTLVHeader mirrors the fixed head of struct snd_ctl_tlv.
type ctlTLVHeader struct {
	Numid, Length uint32
}

var errCtlListUnstable = errors.New("alsa: element list kept changing")

// NewCtlElemID builds the id used to look an element up by its name tuple. The
// numid is always zero: a numeric id from an earlier card instance can name a
// different element, and the kernel prefers it over the tuple (control.c
// snd_ctl_find_id). A name over 44 bytes or containing NUL cannot belong to any
// element and is rejected rather than truncated.
func NewCtlElemID(iface int32, device, subdevice uint32, name string, index uint32) (CtlElemID, error) {
	if len(name) > CtlNameLen {
		return CtlElemID{}, fmt.Errorf("alsa: control name is %d bytes, the kernel field holds %d", len(name), CtlNameLen)
	}
	if strings.IndexByte(name, 0) >= 0 {
		return CtlElemID{}, errors.New("alsa: control name contains NUL")
	}
	id := CtlElemID{Iface: iface, Device: device, Subdevice: subdevice, Index: index}
	copy(id.Name[:], name)
	return id, nil
}

// NameString returns the id's name up to the first NUL.
func (id *CtlElemID) NameString() string {
	return cString(id.Name[:])
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// ElemInfo is a decoded ELEM_INFO reply. Min, Max and Step are set for INTEGER
// elements, Items for ENUMERATED ones.
type ElemInfo struct {
	ID       CtlElemID
	Type     int32
	Access   uint32
	Count    uint32
	Min, Max int64
	Step     int64
	Items    uint32
}

// Ctl is an open control device (/dev/snd/controlC<N>). Its fd lifetime is
// guarded like PCM's: every ioctl is in flight between acquire and release, and
// Close waits for them to drain before closing the fd, so an ioctl never runs on
// a closed descriptor. None of the control ioctls park, so Close waits at most
// for one to return.
type Ctl struct {
	fd    int
	ioctl ioctlFunc

	mu       sync.Mutex // guards closed and inflight; never held across a syscall
	cond     sync.Cond
	closed   bool
	inflight int
}

// newCtl is the single construction point (production and tests), so the
// sync.Cond is wired before the *Ctl is published.
func newCtl(fd int, ioctl ioctlFunc) *Ctl {
	c := &Ctl{fd: fd, ioctl: ioctl}
	c.cond.L = &c.mu
	return c
}

// OpenCtl opens /dev/snd/controlC{card}. It tries O_RDWR first and falls back to
// O_RDONLY on a permission error; the kernel does not check the open mode on
// ELEM_WRITE, so a read-only descriptor can still write. Opening a control
// device never sleeps (unlike a busy PCM), so no O_NONBLOCK handling is needed.
func OpenCtl(card int) (*Ctl, error) {
	path := fmt.Sprintf("/dev/snd/controlC%d", card)
	fd, err := sysOpen(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil && errors.Is(err, unix.EACCES) {
		fd, err = sysOpen(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, &ioctlError{Op: "open " + path, Err: err}
	}
	return newCtl(fd, ioctl), nil
}

// IsCtlGone reports whether err (or an error it wraps) shows the card behind a
// control fd was removed: ENODEV, which every ioctl returns after
// snd_card_disconnect swaps the file operations (init.c snd_shutdown_f_ops). It
// is deliberately not IsDeviceGone: on a control fd ENOENT means "no element
// with that id" and ENXIO means "element has no readable TLV".
func IsCtlGone(err error) bool {
	return errors.Is(err, unix.ENODEV)
}

func (c *Ctl) acquire() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return -1, unix.EBADF
	}
	c.inflight++
	return c.fd, nil
}

func (c *Ctl) release() {
	c.mu.Lock()
	c.inflight--
	if c.inflight == 0 && c.closed {
		c.cond.Broadcast()
	}
	c.mu.Unlock()
}

// call runs one ioctl inside the in-flight guard and wraps a failure with the
// ioctl's name.
func (c *Ctl) call(op string, req uintptr, arg unsafe.Pointer) error {
	fd, err := c.acquire()
	if err != nil {
		return &ioctlError{Op: op, Err: err}
	}
	defer c.release()
	if err := c.ioctl(fd, req, arg); err != nil {
		return &ioctlError{Op: op, Err: err}
	}
	return nil
}

// Close marks the Ctl closed, waits for in-flight ioctls to return and closes
// the fd. It is idempotent.
func (c *Ctl) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	fd := c.fd
	for c.inflight > 0 {
		c.cond.Wait()
	}
	c.mu.Unlock()
	return unix.Close(fd)
}

// Probe issues PVERSION. On a live control fd it cannot fail (it is a bare
// put_user), and after the card disconnects it returns ENODEV, so it tells an
// unplug from an element error.
func (c *Ctl) Probe() error {
	var v int32
	return c.call("PVERSION", iocCtlPVersion, unsafe.Pointer(&v))
}

// List returns the ids of every element on the card. The kernel reports the
// count with space 0 and fills ids on a second call; the count can change
// between the two (user elements, a re-probe), so it retries from the count a
// bounded number of times and then gives up with errCtlListUnstable.
func (c *Ctl) List() ([]CtlElemID, error) {
	for range ctlListAttempts {
		var l ctlElemList
		if err := c.call("ELEM_LIST", iocCtlElemList, unsafe.Pointer(&l)); err != nil {
			return nil, err
		}
		n := l.Count
		if n == 0 {
			return nil, nil
		}
		if n > ctlMaxElements {
			return nil, fmt.Errorf("alsa: card reports %d control elements", n)
		}
		ids := make([]CtlElemID, n)
		l2 := ctlElemList{Space: n, Pids: unsafe.Pointer(&ids[0])}
		err := c.call("ELEM_LIST", iocCtlElemList, unsafe.Pointer(&l2))
		runtime.KeepAlive(ids)
		if err != nil {
			return nil, err
		}
		if l2.Count != n || l2.Used != n {
			continue
		}
		return ids, nil
	}
	return nil, errCtlListUnstable
}

// Info looks an element up by the name tuple of id (its Numid is ignored and
// sent as zero) and returns its decoded description, including the numid the
// kernel assigned.
func (c *Ctl) Info(id CtlElemID) (ElemInfo, error) {
	id.Numid = 0
	info := CtlElemInfo{ID: id}
	if err := c.call("ELEM_INFO", iocCtlElemInfo, unsafe.Pointer(&info)); err != nil {
		return ElemInfo{}, err
	}
	out := ElemInfo{ID: info.ID, Type: info.Type, Access: info.Access, Count: info.Count}
	switch info.Type {
	case CtlTypeInteger:
		w := int(unsafe.Sizeof(clong(0)))
		out.Min = decodeClong(info.Value[0:w])
		out.Max = decodeClong(info.Value[w : 2*w])
		out.Step = decodeClong(info.Value[2*w : 3*w])
	case CtlTypeEnumerated:
		out.Items = binary.NativeEndian.Uint32(info.Value[0:4])
	}
	return out, nil
}

// EnumItemName returns the name of item number item of an ENUMERATED element.
func (c *Ctl) EnumItemName(id CtlElemID, item uint32) (string, error) {
	id.Numid = 0
	info := CtlElemInfo{ID: id}
	binary.NativeEndian.PutUint32(info.Value[4:8], item)
	if err := c.call("ELEM_INFO", iocCtlElemInfo, unsafe.Pointer(&info)); err != nil {
		return "", err
	}
	return cString(info.Value[8 : 8+64]), nil
}

func decodeClong(b []byte) int64 {
	if len(b) == 8 {
		return int64(binary.NativeEndian.Uint64(b))
	}
	return int64(int32(binary.NativeEndian.Uint32(b)))
}

func encodeClong(b []byte, v int64) {
	if len(b) == 8 {
		binary.NativeEndian.PutUint64(b, uint64(v))
		return
	}
	binary.NativeEndian.PutUint32(b, uint32(int32(v)))
}

// slotSize is the byte width of one value slot for typ, or 0 for a type this
// package does not read or write.
func slotSize(typ int32) int {
	switch typ {
	case CtlTypeBoolean, CtlTypeInteger:
		return int(unsafe.Sizeof(clong(0)))
	case CtlTypeEnumerated:
		return 4
	default:
		return 0
	}
}

func checkValues(typ int32, count int) (int, error) {
	w := slotSize(typ)
	if w == 0 {
		return 0, fmt.Errorf("alsa: control type %d cannot be read or written", typ)
	}
	if count < 0 || count > CtlMaxValues {
		return 0, fmt.Errorf("alsa: %d values do not fit the %d slots of an element value", count, CtlMaxValues)
	}
	return w, nil
}

// ReadValues reads count values of an element of type typ (BOOLEAN, INTEGER or
// ENUMERATED). The id's Numid is ignored.
func (c *Ctl) ReadValues(id CtlElemID, typ int32, count int) ([]int64, error) {
	w, err := checkValues(typ, count)
	if err != nil {
		return nil, err
	}
	id.Numid = 0
	v := ctlElemValue{ID: id}
	if err := c.call("ELEM_READ", iocCtlElemRead, unsafe.Pointer(&v)); err != nil {
		return nil, err
	}
	out := make([]int64, count)
	for i := range out {
		b := v.Value[i*w : (i+1)*w]
		if typ == CtlTypeEnumerated {
			out[i] = int64(binary.NativeEndian.Uint32(b))
		} else {
			out[i] = decodeClong(b)
		}
	}
	return out, nil
}

// WriteValues writes values to an element of type typ. The id's Numid is
// ignored. It does no range or step validation; the public layer does.
func (c *Ctl) WriteValues(id CtlElemID, typ int32, values []int64) error {
	w, err := checkValues(typ, len(values))
	if err != nil {
		return err
	}
	id.Numid = 0
	v := ctlElemValue{ID: id}
	for i, x := range values {
		b := v.Value[i*w : (i+1)*w]
		if typ == CtlTypeEnumerated {
			binary.NativeEndian.PutUint32(b, uint32(x))
		} else {
			encodeClong(b, x)
		}
	}
	return c.call("ELEM_WRITE", iocCtlElemWrite, unsafe.Pointer(&v))
}

// TLV reads the TLV item of the element with the given numid and returns it as
// words: type, length in bytes, data. The numid must come from an ELEM_INFO on
// the same card instance (TLV_READ has no name form). The kernel copies the item
// after the header and never writes the header's length back, so the size is
// taken from the item's own length word and checked against the buffer.
func (c *Ctl) TLV(numid uint32) ([]uint32, error) {
	var buf [ctlTLVWords]uint32
	buf[0] = numid
	buf[1] = (ctlTLVWords - 2) * 4
	if err := c.call("TLV_READ", iocCtlTLVRead, unsafe.Pointer(&buf[0])); err != nil {
		return nil, err
	}
	words := (uint64(buf[3]) + 3) / 4
	if 4+words > ctlTLVWords {
		return nil, fmt.Errorf("alsa: TLV_READ: item claims %d bytes, buffer holds %d", buf[3], (ctlTLVWords-4)*4)
	}
	out := make([]uint32, 2+words)
	copy(out, buf[2:])
	return out, nil
}
