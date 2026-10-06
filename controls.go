package capture

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ControlInterface is the kind of object a control element belongs to
// (SNDRV_CTL_ELEM_IFACE_*). Mixer gain elements are ControlMixer.
type ControlInterface int

// The element interfaces, valued as the kernel's SNDRV_CTL_ELEM_IFACE_*.
const (
	ControlCard ControlInterface = iota
	ControlHwdep
	ControlMixer
	ControlPCM
	ControlRawMIDI
	ControlTimer
	ControlSequencer
)

func (i ControlInterface) String() string {
	switch i {
	case ControlCard:
		return "CARD"
	case ControlHwdep:
		return "HWDEP"
	case ControlMixer:
		return "MIXER"
	case ControlPCM:
		return "PCM"
	case ControlRawMIDI:
		return "RAWMIDI"
	case ControlTimer:
		return "TIMER"
	case ControlSequencer:
		return "SEQUENCER"
	default:
		return "IFACE(" + strconv.Itoa(int(i)) + ")"
	}
}

// ControlType is the value type of a control element (SNDRV_CTL_ELEM_TYPE_*).
// Get and Set support ControlBoolean, ControlInteger and ControlEnumerated.
type ControlType int

// The element value types, valued as the kernel's SNDRV_CTL_ELEM_TYPE_*.
const (
	ControlBoolean    ControlType = 1
	ControlInteger    ControlType = 2
	ControlEnumerated ControlType = 3
	ControlBytes      ControlType = 4
	ControlIEC958     ControlType = 5
	ControlInteger64  ControlType = 6
)

func (t ControlType) String() string {
	switch t {
	case ControlBoolean:
		return "BOOLEAN"
	case ControlInteger:
		return "INTEGER"
	case ControlEnumerated:
		return "ENUMERATED"
	case ControlBytes:
		return "BYTES"
	case ControlIEC958:
		return "IEC958"
	case ControlInteger64:
		return "INTEGER64"
	default:
		return "TYPE(" + strconv.Itoa(int(t)) + ")"
	}
}

// ControlAccess is the access bit set of a control element
// (SNDRV_CTL_ELEM_ACCESS_*).
type ControlAccess uint32

// The access bits, valued as the kernel's SNDRV_CTL_ELEM_ACCESS_*.
const (
	AccessRead     ControlAccess = 1 << 0
	AccessWrite    ControlAccess = 1 << 1
	AccessVolatile ControlAccess = 1 << 2
	AccessTLVRead  ControlAccess = 1 << 4
	AccessInactive ControlAccess = 1 << 8
	AccessLock     ControlAccess = 1 << 9
	AccessOwner    ControlAccess = 1 << 10
	AccessUser     ControlAccess = 1 << 29
)

// String lists the set bits, for example "read|write|tlv".
func (a ControlAccess) String() string {
	names := []struct {
		bit  ControlAccess
		name string
	}{
		{AccessRead, accessOpRead}, {AccessWrite, accessOpWrite}, {AccessVolatile, "volatile"},
		{AccessTLVRead, "tlv"}, {AccessInactive, "inactive"}, {AccessLock, "lock"},
		{AccessOwner, "owner"}, {AccessUser, "user"},
	}
	var parts []string
	for _, n := range names {
		if a&n.bit != 0 {
			parts = append(parts, n.name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}

// ControlID names a control element by the tuple the kernel keeps unique per
// card: interface, device, subdevice, name and index. Persist this form, not
// NumID. NumID is reported by List and Info for display and is ignored on input,
// because numeric ids are assigned in creation order and a stored one can name a
// different element after a driver reload or a replug.
type ControlID struct {
	Interface ControlInterface
	Device    int
	Subdevice int
	Name      string // at most 44 bytes, no NUL (the kernel's field size)
	Index     int
	NumID     int
}

// String formats the id as 'Name',index=N,iface=IFACE, with device and subdevice
// added when they are not zero.
func (id ControlID) String() string {
	s := fmt.Sprintf("'%s',index=%d,iface=%s", id.Name, id.Index, id.Interface)
	if id.Device != 0 {
		s += ",device=" + strconv.Itoa(id.Device)
	}
	if id.Subdevice != 0 {
		s += ",subdevice=" + strconv.Itoa(id.Subdevice)
	}
	return s
}

// dbSegment is one stretch of an element's raw range with a linear dB mapping,
// in 0.01 dB: MinCdB at raw RawMin and MaxCdB at raw RawMax. When Mute is set,
// the value RawMin is mute.
type dbSegment struct {
	rawMin, rawMax int64
	minCdB, maxCdB int64
	mute           bool
}

// ControlInfo describes one control element.
type ControlInfo struct {
	ID     ControlID
	Type   ControlType
	Access ControlAccess
	// Count is the number of values the element holds, often one per channel.
	Count int
	// Min, Max and Step are the raw range of an INTEGER element; a BOOLEAN
	// reports 0, 1 and 0. Step 0 means no step rule.
	Min, Max, Step int64
	// Items lists the names of an ENUMERATED element, in index order. It is nil
	// when the element has more items than the library fetches names for (1024);
	// Max is still its last index.
	Items []string
	// HasDB reports that the element carries a dB TLV the library understood.
	// MinDB and MaxDB are then the dB at the first and last end of the TLV's
	// ranges; a step the driver marks as mute still has a finite MinDB, and
	// ValueDB reports it as negative infinity.
	HasDB        bool
	MinDB, MaxDB float64

	db []dbSegment
}

// ValueDB converts a raw value of the element to dB using its TLV. It reports
// false when the element has no usable dB information or raw lies outside every
// segment. A step the driver marks as mute returns negative infinity.
//
//nolint:gocritic // hugeParam: a value receiver keeps ValueDB callable on a non-addressable ControlInfo.
func (i ControlInfo) ValueDB(raw int64) (db float64, ok bool) {
	for _, s := range i.db {
		if raw < s.rawMin || raw > s.rawMax {
			continue
		}
		if s.mute && raw == s.rawMin {
			return math.Inf(-1), true
		}
		cdb := float64(s.minCdB)
		if s.rawMax > s.rawMin {
			cdb += float64(raw-s.rawMin) * float64(s.maxCdB-s.minCdB) / float64(s.rawMax-s.rawMin)
		}
		return cdb / 100, true
	}
	return 0, false
}

// setDB records the parsed dB segments and the range they give.
func (i *ControlInfo) setDB(segs []dbSegment) {
	if len(segs) == 0 {
		return
	}
	i.db = segs
	i.HasDB = true
	i.MinDB = float64(segs[0].minCdB) / 100
	i.MaxDB = float64(segs[len(segs)-1].maxCdB) / 100
}

// stepOK is the kernel's step rule (sound/core/control.c, validate_integer):
// the remainder of the value itself, computed unsigned, must be zero. It is the
// remainder of v, not of v-Min, so a negative value is tested as uint64(v).
func stepOK(v, step int64) bool {
	return step == 0 || uint64(v)%uint64(step) == 0
}

// nearestValid returns the value closest to target inside [lo, hi] that satisfies
// the step rule, preferring the lower one on a tie. The search is bounded to
// 2^20 values either side of target; ok is false when none was found within it,
// which is not proof that none exists for a very wide range with a very large
// step.
func nearestValid(target, lo, hi, step int64) (v int64, ok bool) {
	const maxProbe = 1 << 20
	if lo > hi || target < lo || target > hi {
		return 0, false
	}
	for d := range int64(maxProbe) {
		// With lo <= target <= hi the distances are exact in uint64, so the
		// checks cannot wrap near the ends of the int64 range.
		below := uint64(target)-uint64(lo) >= uint64(d)
		above := uint64(hi)-uint64(target) >= uint64(d)
		if !below && !above {
			return 0, false
		}
		if below && stepOK(target-d, step) {
			return target - d, true
		}
		if above && stepOK(target+d, step) {
			return target + d, true
		}
	}
	return 0, false
}
