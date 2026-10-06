//go:build linux

package alsa

// TLV item types and the DB_SCALE data flags from include/uapi/sound/tlv.h. An
// item is a type word, a length word (bytes, padded to 4) and that many bytes of
// data; dB values are signed 0.01 dB.
const (
	tlvContainer    = 0
	tlvDBScale      = 1
	tlvDBRange      = 3
	tlvDBMinMax     = 4
	tlvDBMinMaxMute = 5

	tlvDBScaleMask = 0xffff  // SNDRV_CTL_TLVD_DB_SCALE_MASK: step, in 0.01 dB
	tlvDBScaleMute = 0x10000 // SNDRV_CTL_TLVD_DB_SCALE_MUTE: the lowest raw value is mute

	// tlvMaxContainerDepth is how many CONTAINER levels ParseDB descends. The
	// kernel's own chmap and dB TLVs need one.
	tlvMaxContainerDepth = 1
)

// DBSegment is one stretch of an element's raw range with a linear dB mapping:
// the dB at raw value RawMin is MinCdB and at RawMax is MaxCdB, both in 0.01 dB,
// interpolated linearly between. A DB_SCALE item is the special case whose
// MaxCdB is MinCdB plus the step times the raw span. When Mute is set the value
// RawMin is mute (no gain), whatever MinCdB says.
type DBSegment struct {
	RawMin, RawMax int64
	MinCdB, MaxCdB int64
	Mute           bool
}

// ParseDB decodes the dB information in a TLV item as returned by Ctl.TLV
// (type word, length word, data). rawMin and rawMax are the element's range,
// which DB_SCALE and DB_MINMAX items at the top level apply to; DB_RANGE entries
// carry their own raw bounds, and each entry's mapping is relative to its own
// minimum, not the element's. It handles DB_SCALE, DB_MINMAX, DB_MINMAX_MUTE and
// DB_RANGE of those, found directly or one CONTAINER level down. Any other TLV
// (DB_LINEAR, a channel map) reports false, as does anything malformed: every
// length is checked against the words that remain, so it never panics.
func ParseDB(tlv []uint32, rawMin, rawMax int64) ([]DBSegment, bool) {
	if rawMin > rawMax {
		return nil, false
	}
	segs, ok := parseDBItem(tlv, rawMin, rawMax, 0)
	if !ok || len(segs) == 0 {
		return nil, false
	}
	return segs, true
}

// tlvBody splits the item at the start of w into its type, its data words and
// the total words it occupies. ok is false when the declared length runs past w.
func tlvBody(w []uint32) (typ uint32, body []uint32, total int, ok bool) {
	if len(w) < 2 {
		return 0, nil, 0, false
	}
	words := (uint64(w[1]) + 3) / 4
	if words > uint64(len(w)-2) {
		return 0, nil, 0, false
	}
	n := int(words)
	return w[0], w[2 : 2+n], 2 + n, true
}

func parseDBItem(w []uint32, rawMin, rawMax int64, depth int) ([]DBSegment, bool) {
	typ, body, _, ok := tlvBody(w)
	if !ok {
		return nil, false
	}
	switch typ {
	case tlvDBScale:
		if len(body) < 2 {
			return nil, false
		}
		minDB := int64(int32(body[0]))
		step := int64(body[1] & tlvDBScaleMask)
		return []DBSegment{{
			RawMin: rawMin, RawMax: rawMax,
			MinCdB: minDB, MaxCdB: minDB + (rawMax-rawMin)*step,
			Mute: body[1]&tlvDBScaleMute != 0,
		}}, true
	case tlvDBMinMax, tlvDBMinMaxMute:
		if len(body) < 2 {
			return nil, false
		}
		return []DBSegment{{
			RawMin: rawMin, RawMax: rawMax,
			MinCdB: int64(int32(body[0])), MaxCdB: int64(int32(body[1])),
			Mute: typ == tlvDBMinMaxMute,
		}}, true
	case tlvDBRange:
		return parseDBRange(body)
	case tlvContainer:
		if depth >= tlvMaxContainerDepth {
			return nil, false
		}
		return parseDBContainer(body, rawMin, rawMax, depth)
	default:
		return nil, false
	}
}

// parseDBRange reads DB_RANGE data: a sequence of (rmin, rmax, item) entries
// whose items are DB_SCALE or DB_MINMAX(_MUTE), each mapped over its own bounds.
func parseDBRange(body []uint32) ([]DBSegment, bool) {
	var segs []DBSegment
	for len(body) > 0 {
		if len(body) < 2 {
			return nil, false
		}
		rmin, rmax := int64(int32(body[0])), int64(int32(body[1]))
		if rmin > rmax {
			return nil, false
		}
		_, _, total, ok := tlvBody(body[2:])
		if !ok {
			return nil, false
		}
		typ := body[2]
		if typ != tlvDBScale && typ != tlvDBMinMax && typ != tlvDBMinMaxMute {
			return nil, false
		}
		sub, ok := parseDBItem(body[2:2+total], rmin, rmax, tlvMaxContainerDepth)
		if !ok {
			return nil, false
		}
		segs = append(segs, sub...)
		body = body[2+total:]
	}
	return segs, len(segs) > 0
}

// parseDBContainer returns the dB segments of the first child that carries dB
// information. Children of other kinds (a channel map next to a volume's dB) are
// skipped; a child whose length is inconsistent with the container is an error.
func parseDBContainer(body []uint32, rawMin, rawMax int64, depth int) ([]DBSegment, bool) {
	for len(body) > 0 {
		_, _, total, ok := tlvBody(body)
		if !ok {
			return nil, false
		}
		if segs, ok := parseDBItem(body[:total], rawMin, rawMax, depth+1); ok {
			return segs, true
		}
		body = body[total:]
	}
	return nil, false
}
