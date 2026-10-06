//go:build linux

package capture

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/tphakala/go-audio-capture/internal/alsa"
)

// ctlHandle is the control-device seam Controls drives; *alsa.Ctl satisfies it
// and tests inject a hardware-free fake.
type ctlHandle interface {
	List() ([]alsa.CtlElemID, error)
	Info(id alsa.CtlElemID) (alsa.ElemInfo, error)
	EnumItemName(id alsa.CtlElemID, item uint32) (string, error)
	ReadValues(id alsa.CtlElemID, typ int32, count int) ([]int64, error)
	WriteValues(id alsa.CtlElemID, typ int32, values []int64) error
	TLV(numid uint32) ([]uint32, error)
	Probe() error
	Close() error
}

// openCtl is a package var so tests can substitute a fake control device.
var openCtl = func(card int) (ctlHandle, error) {
	c, err := alsa.OpenCtl(card)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Controls is an open handle on a card's ALSA control device
// (/dev/snd/controlC<N>): the mixer elements such as capture gain. It is
// separate from Open on purpose: opening a stream never reads or changes a
// control, and nothing here is applied behind the caller's back. Settings are
// not persisted by this library; another mixer, a sound server or
// "alsactl restore" can change them afterwards.
//
// A handle opened from a stable id is bound to the card instance it was opened
// on (the identity is verified after the open), so after an unplug it keeps
// failing with ErrDeviceGone even if another unit takes the card number. A
// handle opened from a numeric id is not verified against a unit. Methods are safe
// for concurrent use; Close may be called from another goroutine.
type Controls struct {
	h      ctlHandle
	closed atomic.Bool
}

// OpenControls opens the control device of the card d names. It resolves d the
// way OpenDevice does. A stable id is re-verified against live sysfs after the
// open, and a card that is not the unit d names gives ErrDeviceGone, so the
// handle never talks to a different unit than the one asked for. A numeric
// hw:N,D id is not verified (as in OpenDevice): it opens whatever holds that
// card number. Errors from resolving d are those of OpenDevice. A control node
// that is missing while the card is present is returned as the open error.
//
//nolint:gocritic // hugeParam: DeviceInfo is passed by value like OpenDevice.
func OpenControls(d DeviceInfo) (*Controls, error) {
	r, err := resolveDeviceInfo(&d)
	if err != nil {
		return nil, err
	}
	h, err := openCtl(r.card)
	if err != nil {
		// As in openQuery: attribute the failure to the card only once it is
		// shown to be the unit asked for.
		if verr := verifyCardIdentity(r); verr != nil {
			return nil, verr
		}
		// ENODEV is the card disconnecting. ENOENT is the card gone only when its
		// /proc/asound entry is gone too: with the card present it is a control
		// node that is not there (a container that maps only the PCM node), which
		// is the open error, not a lost device. A numeric id has no post-open
		// identity check, so this is the only sign it went away. Where
		// /proc/asound is hidden or unreadable (some containers) the card reads as
		// absent, so a missing control node there is ErrDeviceGone too.
		if alsa.IsCtlGone(err) || (errors.Is(err, unix.ENOENT) && !cardPresent(r.card)) {
			return nil, ErrDeviceGone
		}
		return nil, err
	}
	if err := verifyCardIdentity(r); err != nil {
		_ = h.Close()
		return nil, err
	}
	return &Controls{h: h}, nil
}

// Close releases the control device. It is idempotent; every other method
// returns ErrClosed afterwards.
func (c *Controls) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.h.Close()
}

const (
	opRead = iota
	opWrite
	opOther
)

// classify maps an error from a control ioctl. Close wins; ENODEV, or any
// failure followed by a PVERSION probe that fails with ENODEV, is the device
// gone. Only with a live probe do the element errnos keep their meaning: ENOENT
// is a missing element and ENXIO a missing TLV, neither of which is a lost
// device (so alsa.IsDeviceGone is deliberately not used here).
func (c *Controls) classify(id ControlID, op int, err error) error {
	if c.closed.Load() || errors.Is(err, unix.EBADF) {
		return ErrClosed
	}
	if alsa.IsCtlGone(err) {
		return ErrDeviceGone
	}
	perr := c.h.Probe()
	if c.closed.Load() {
		return ErrClosed
	}
	if alsa.IsCtlGone(perr) {
		return ErrDeviceGone
	}
	switch {
	case errors.Is(err, unix.ENOENT):
		return &ControlNotFoundError{ID: id}
	case errors.Is(err, unix.EPERM):
		switch op {
		case opWrite:
			return &ControlAccessError{ID: id, Op: accessOpWrite, Locked: true}
		case opRead:
			return &ControlAccessError{ID: id, Op: accessOpRead}
		}
	case errors.Is(err, unix.EINVAL) && op == opWrite:
		return &ControlValueError{ID: id, Reason: "rejected by the driver"}
	}
	return err
}

// toElemID converts an id to the kernel's lookup tuple. The NumID is dropped. A
// negative or out-of-range field (the kernel's tuple fields are 32 bits), or a
// name the kernel cannot hold, names no element.
func toElemID(id ControlID) (alsa.CtlElemID, error) {
	if id.Interface < 0 || int64(id.Interface) > math.MaxInt32 || id.Device < 0 || id.Subdevice < 0 || id.Index < 0 ||
		int64(id.Device) > math.MaxUint32 || int64(id.Subdevice) > math.MaxUint32 || int64(id.Index) > math.MaxUint32 {
		return alsa.CtlElemID{}, &ControlNotFoundError{ID: id}
	}
	e, err := alsa.NewCtlElemID(int32(id.Interface), uint32(id.Device), uint32(id.Subdevice), id.Name, uint32(id.Index))
	if err != nil {
		return alsa.CtlElemID{}, &ControlNotFoundError{ID: id}
	}
	return e, nil
}

// cardPresent reports whether the card still has a /proc/asound entry.
func cardPresent(card int) bool {
	_, err := os.Stat(filepath.Join(procRoot, fmt.Sprintf("card%d", card)))
	return err == nil
}

func fromElemID(e *alsa.CtlElemID) ControlID {
	return ControlID{
		Interface: ControlInterface(e.Iface),
		Device:    int(e.Device),
		Subdevice: int(e.Subdevice),
		Name:      e.NameString(),
		Index:     int(e.Index),
		NumID:     int(e.Numid),
	}
}

func supportedType(t ControlType) bool {
	return t == ControlBoolean || t == ControlInteger || t == ControlEnumerated
}

// describe reads one element's description. With deep set it also reads the
// enumerated item names and the dB TLV.
func (c *Controls) describe(eid alsa.CtlElemID, id ControlID, deep bool) (ControlInfo, error) {
	ai, err := c.h.Info(eid)
	if err != nil {
		return ControlInfo{}, c.classify(id, opOther, err)
	}
	out := ControlInfo{
		ID:     fromElemID(&ai.ID),
		Type:   ControlType(ai.Type),
		Access: ControlAccess(ai.Access),
		Count:  int(ai.Count),
	}
	switch out.Type {
	case ControlBoolean:
		out.Max = 1
	case ControlInteger:
		out.Min, out.Max, out.Step = ai.Min, ai.Max, ai.Step
	case ControlEnumerated:
		out.Max = int64(ai.Items) - 1
		// An element with more items than the cap is listed without names, so
		// one such element cannot hide the rest of the card.
		if deep && ai.Items <= alsa.CtlMaxItems {
			out.Items = make([]string, 0, ai.Items)
			for i := range ai.Items {
				name, err := c.h.EnumItemName(eid, i)
				if err != nil {
					return ControlInfo{}, c.classify(out.ID, opOther, err)
				}
				out.Items = append(out.Items, name)
			}
		}
	default:
		// BYTES, IEC958 and INTEGER64 are listed with their type only.
	}
	if !deep {
		return out, nil
	}
	if out.Access&AccessTLVRead != 0 && out.Type == ControlInteger {
		tlv, err := c.h.TLV(ai.ID.Numid)
		switch {
		case err == nil:
			if segs, ok := alsa.ParseDB(tlv, out.Min, out.Max); ok {
				ds := make([]dbSegment, len(segs))
				for i, s := range segs {
					ds[i] = dbSegment{rawMin: s.RawMin, rawMax: s.RawMax, minCdB: s.MinCdB, maxCdB: s.MaxCdB, mute: s.Mute}
				}
				out.setDB(ds)
			}
		case errors.Is(err, unix.ENXIO), errors.Is(err, unix.ENOMEM):
			// No readable TLV, or one larger than the buffer: no dB, not an error.
		default:
			return ControlInfo{}, c.classify(out.ID, opOther, err)
		}
	}
	return out, nil
}

func (c *Controls) list(deep bool) ([]ControlInfo, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	ids, err := c.h.List()
	if err != nil {
		return nil, c.classify(ControlID{}, opOther, err)
	}
	out := make([]ControlInfo, 0, len(ids))
	for i := range ids {
		info, err := c.describe(ids[i], fromElemID(&ids[i]), deep)
		if err != nil {
			if errors.Is(err, ErrControlNotFound) {
				continue // removed between the id list and the lookup
			}
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// List returns every control element of the card, in the kernel's order, with
// its type, access, range, enumerated item names and dB information. An element
// removed while listing is left out.
func (c *Controls) List() ([]ControlInfo, error) {
	return c.list(true)
}

// Info describes one element. A missing element is *ControlNotFoundError.
func (c *Controls) Info(id ControlID) (ControlInfo, error) {
	if c.closed.Load() {
		return ControlInfo{}, ErrClosed
	}
	eid, err := toElemID(id)
	if err != nil {
		return ControlInfo{}, err
	}
	return c.describe(eid, id, true)
}

// lookup validates id and reads the element's description without the item
// names and TLV that only Info reports.
func (c *Controls) lookup(id ControlID) (alsa.CtlElemID, ControlInfo, error) {
	if c.closed.Load() {
		return alsa.CtlElemID{}, ControlInfo{}, ErrClosed
	}
	eid, err := toElemID(id)
	if err != nil {
		return alsa.CtlElemID{}, ControlInfo{}, err
	}
	info, err := c.describe(eid, id, false)
	return eid, info, err
}

// Get reads the current values of a BOOLEAN, INTEGER or ENUMERATED element: 0 or
// 1, the raw integer, or the item index, one per Count.
func (c *Controls) Get(id ControlID) ([]int64, error) {
	eid, info, err := c.lookup(id)
	if err != nil {
		return nil, err
	}
	if !supportedType(info.Type) {
		return nil, &ControlValueError{ID: info.ID, Reason: "reading " + info.Type.String() + " elements is not supported"}
	}
	if info.Access&AccessRead == 0 {
		return nil, &ControlAccessError{ID: info.ID, Op: accessOpRead}
	}
	vals, err := c.h.ReadValues(eid, int32(info.Type), info.Count)
	if err != nil {
		return nil, c.classify(info.ID, opRead, err)
	}
	return vals, nil
}

// Set writes values (exactly Count of them) to a BOOLEAN, INTEGER or ENUMERATED
// element. The library checks type, access, count, range and the kernel's step
// rule (the value itself modulo Step, unsigned) against the element's current
// description first, because the kernel validates driver elements only when
// built with CONFIG_SND_CTL_INPUT_VALIDATION; a refused value is a
// *ControlValueError and nothing is written. There is no read-back: a driver
// that quantizes the value on write leaves a different value without an error,
// so call Get to see it.
func (c *Controls) Set(id ControlID, values []int64) error {
	eid, info, err := c.lookup(id)
	if err != nil {
		return err
	}
	return c.set(eid, &info, values)
}

// set validates values against an already-read description and writes them.
func (c *Controls) set(eid alsa.CtlElemID, info *ControlInfo, values []int64) error {
	bad := func(reason string) error {
		return &ControlValueError{ID: info.ID, Values: slices.Clone(values), Min: info.Min, Max: info.Max, Step: info.Step, Reason: reason}
	}
	if !supportedType(info.Type) {
		return bad("writing " + info.Type.String() + " elements is not supported")
	}
	if info.Access&AccessWrite == 0 || info.Access&AccessInactive != 0 {
		return &ControlAccessError{ID: info.ID, Op: accessOpWrite}
	}
	if len(values) != info.Count {
		return bad(fmt.Sprintf("element holds %d values, got %d", info.Count, len(values)))
	}
	for _, v := range values {
		if v < info.Min || v > info.Max {
			return bad(fmt.Sprintf("%d is outside the range", v))
		}
		if info.Type == ControlInteger && !stepOK(v, info.Step) {
			return bad(fmt.Sprintf("%d does not satisfy the step rule (value modulo step must be 0)", v))
		}
	}
	if err := c.h.WriteValues(eid, int32(info.Type), values); err != nil {
		cerr := c.classify(info.ID, opWrite, err)
		if cv, ok := errors.AsType[*ControlValueError](cerr); ok {
			cv.Values, cv.Min, cv.Max, cv.Step = slices.Clone(values), info.Min, info.Max, info.Step
		}
		return cerr
	}
	return nil
}

// isCaptureVolume is the capture volume rule: an active, readable and writable
// INTEGER mixer element named "Capture Volume" or ending in " Capture Volume".
func isCaptureVolume(i *ControlInfo) bool {
	return i.ID.Interface == ControlMixer && i.Type == ControlInteger &&
		i.Access&AccessRead != 0 && i.Access&AccessWrite != 0 && i.Access&AccessInactive == 0 &&
		(i.ID.Name == "Capture Volume" || strings.HasSuffix(i.ID.Name, " Capture Volume"))
}

// captureMatch scans the card for the capture volume element without reading
// item names or TLVs, and returns its lookup id and shallow description.
func (c *Controls) captureMatch() (alsa.CtlElemID, ControlInfo, error) {
	all, err := c.list(false)
	if err != nil {
		return alsa.CtlElemID{}, ControlInfo{}, err
	}
	var matches []ControlInfo
	for i := range all {
		if isCaptureVolume(&all[i]) {
			matches = append(matches, all[i])
		}
	}
	switch len(matches) {
	case 0:
		return alsa.CtlElemID{}, ControlInfo{}, &ControlNotFoundError{Pattern: true}
	case 1:
		eid, err := toElemID(matches[0].ID)
		return eid, matches[0], err
	default:
		ids := make([]ControlID, len(matches))
		for i := range matches {
			ids[i] = matches[i].ID
		}
		return alsa.CtlElemID{}, ControlInfo{}, &AmbiguousControlError{Matches: ids}
	}
}

// CaptureVolume returns the card's capture volume element: the one active,
// readable and writable INTEGER mixer element named "Capture Volume" or ending
// in " Capture Volume". Zero matches is *ControlNotFoundError (with Pattern set), several is
// *AmbiguousControlError. It never prefers one candidate over another; pass the
// ControlID you want to Set instead. It scans the card on every call.
func (c *Controls) CaptureVolume() (ControlInfo, error) {
	eid, info, err := c.captureMatch()
	if err != nil {
		return ControlInfo{}, err
	}
	return c.describe(eid, info.ID, true)
}

// SetCaptureVolumePercent sets the capture volume element to percent (0 to 100)
// of its raw range, mapped linearly in raw steps: Min plus the rounded (half away
// from zero) share of Max-Min, moved to the nearest value that satisfies the
// element's step rule (the lower one on a tie). It writes that raw value to
// every one of the element's values and returns it. The mapping is linear in raw
// steps, not in dB; use CaptureVolume and ValueDB to see the resulting gain.
func (c *Controls) SetCaptureVolumePercent(percent float64) (raw int64, err error) {
	if c.closed.Load() {
		return 0, ErrClosed
	}
	if math.IsNaN(percent) || percent < 0 || percent > 100 {
		return 0, &ControlValueError{Reason: fmt.Sprintf("percent %v is outside 0..100", percent)}
	}
	eid, info, err := c.captureMatch()
	if err != nil {
		return 0, err
	}
	target := info.Min + int64(math.Round(percent/100*float64(info.Max-info.Min)))
	v, ok := nearestValid(target, info.Min, info.Max, info.Step)
	if !ok {
		return 0, &ControlValueError{ID: info.ID, Min: info.Min, Max: info.Max, Step: info.Step, Reason: "no value in the range satisfies the step rule"}
	}
	vals := make([]int64, info.Count)
	for i := range vals {
		vals[i] = v
	}
	if err := c.set(eid, &info, vals); err != nil {
		return 0, err
	}
	return v, nil
}
