//go:build linux || windows

package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/tphakala/go-audio-capture"
)

// openControls resolves device and opens its control device.
func openControls(device string) (*capture.Controls, error) {
	d, err := capture.Resolve(device)
	if err != nil {
		return nil, err
	}
	return capture.OpenControls(d)
}

// rangeText describes an element's value domain in one field.
func rangeText(e *capture.ControlInfo) string {
	switch e.Type {
	case capture.ControlInteger:
		return fmt.Sprintf("%d..%d step %d", e.Min, e.Max, e.Step)
	case capture.ControlEnumerated:
		return strings.Join(e.Items, "/")
	case capture.ControlBoolean:
		return "0..1"
	case capture.ControlBytes, capture.ControlIEC958, capture.ControlInteger64:
		return "-"
	}
	return "-"
}

func dbText(e *capture.ControlInfo) string {
	if !e.HasDB {
		return "-"
	}
	return fmt.Sprintf("%.2f..%.2f dB", e.MinDB, e.MaxDB)
}

// printControls prints one tab-separated row per element: numid, id, type,
// access, count, range, current values (or the read error) and dB range.
func printControls(device string) error {
	c, err := openControls(device)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	list, err := c.List()
	if err != nil {
		return err
	}
	for i := range list {
		e := &list[i]
		vals := "-"
		if e.Access&capture.AccessRead != 0 && e.Type != capture.ControlBytes && e.Type != capture.ControlIEC958 && e.Type != capture.ControlInteger64 {
			if v, err := c.Get(e.ID); err != nil {
				vals = "error: " + err.Error()
			} else {
				vals = fmt.Sprint(v)
			}
		}
		fmt.Printf("%d\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", e.ID.NumID, e.ID, e.Type, e.Access, e.Count, rangeText(e), vals, dbText(e))
	}
	return nil
}

// parseSet splits 'NAME[#INDEX]=V[,V...]'.
func parseSet(spec string) (name string, index int, values []int64, err error) {
	name, rhs, ok := strings.Cut(spec, "=")
	if !ok || name == "" || rhs == "" {
		return "", 0, nil, fmt.Errorf("want NAME[#INDEX]=V[,V...], got %q", spec)
	}
	if n, idx, hasIdx := strings.Cut(name, "#"); hasIdx {
		index, err = strconv.Atoi(idx)
		if err != nil || index < 0 {
			return "", 0, nil, fmt.Errorf("bad index in %q", spec)
		}
		name = n
	}
	for f := range strings.SplitSeq(rhs, ",") {
		v, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
		if err != nil {
			return "", 0, nil, fmt.Errorf("bad value %q in %q", f, spec)
		}
		values = append(values, v)
	}
	return name, index, values, nil
}

// setControl finds the element by name and index on any interface (two matches
// is an error), sets it, and prints what Get reads back.
func setControl(device, spec string) error {
	name, index, values, err := parseSet(spec)
	if err != nil {
		return err
	}
	c, err := openControls(device)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	list, err := c.List()
	if err != nil {
		return err
	}
	var matches []capture.ControlID
	for i := range list {
		if list[i].ID.Name == name && list[i].ID.Index == index {
			matches = append(matches, list[i].ID)
		}
	}
	switch len(matches) {
	case 0:
		return fmt.Errorf("no control named %q with index %d", name, index)
	case 1:
	default:
		return fmt.Errorf("%q#%d matches %d controls: %v", name, index, len(matches), matches)
	}
	if err := c.Set(matches[0], values); err != nil {
		return err
	}
	got, err := c.Get(matches[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s = %v\n", matches[0], got)
	return nil
}

// setCaptureVolume sets the capture volume to percent and prints the element,
// the raw value written and its dB when known.
func setCaptureVolume(device string, percent float64) error {
	c, err := openControls(device)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	raw, err := c.SetCaptureVolumePercent(percent)
	if err != nil {
		return err
	}
	info, err := c.CaptureVolume()
	if err != nil {
		return err
	}
	fmt.Printf("%s = %d", info.ID, raw)
	if db, ok := info.ValueDB(raw); ok {
		if math.IsInf(db, -1) {
			fmt.Print(" (mute)")
		} else {
			fmt.Printf(" (%.2f dB)", db)
		}
	}
	fmt.Println()
	return nil
}
