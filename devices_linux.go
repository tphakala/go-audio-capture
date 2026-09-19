//go:build linux

package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// procRoot and sysRoot are the two kernel filesystems device identity is read
// from. They are package vars, like openPCM, purely so a test can point
// enumeration and the post-open identity re-check at fixture trees built under
// t.TempDir(), including a swap driven exactly in the resolve-to-open window.
var (
	procRoot = "/proc/asound"
	sysRoot  = "/sys"
)

// Devices enumerates capture-capable PCM devices from /proc/asound. A device
// appears when its card has a capture PCM node (card{C}/pcm{D}c); playback-only
// cards are skipped. The result is a point-in-time snapshot: there is no
// appear/disappear event, so a caller that hot-adds devices as hardware is
// plugged in must poll Devices (or watch /dev/snd) itself.
func Devices() ([]DeviceInfo, error) {
	return devicesFrom(procRoot, sysRoot)
}

// cardHeaderRe matches a card header line in /proc/asound/cards, capturing the
// card index and the longname after " - ", e.g.
//
//	" 1 [Device         ]: USB-Audio - C-Media USB Audio Device"
var cardHeaderRe = regexp.MustCompile(`^\s*(\d+)\s+\[[^\]]*\]:\s+\S+\s+-\s+(.+?)\s*$`)

// captureNodeRe matches a capture PCM info path, capturing card and device
// numbers, e.g. ".../card1/pcm0c/info".
var captureNodeRe = regexp.MustCompile(`card(\d+)/pcm(\d+)c/info$`)

func devicesFrom(procRoot, sysPath string) ([]DeviceInfo, error) {
	cards, err := os.ReadFile(filepath.Join(procRoot, "cards"))
	if err != nil {
		return nil, fmt.Errorf("read %s/cards: %w", procRoot, err)
	}
	names := parseCards(cards)

	// Escape glob metacharacters in the injected root so one in it (a test tmpdir,
	// or a container mount, can contain '[' or '*') is matched literally rather
	// than parsed as glob syntax, which would silently match nothing and make
	// Devices return an empty list. Only the "card*"/"pcm*c" parts are patterns.
	infoPaths, err := filepath.Glob(filepath.Join(quoteGlobMeta(procRoot), "card*", "pcm*c", "info"))
	if err != nil {
		return nil, fmt.Errorf("glob capture nodes: %w", err)
	}
	// One sysfs read per card, not per PCM node: a card with several capture
	// PCMs has one identity, and reading it once keeps enumeration cheap.
	idents := make(map[int]cardIdent)
	devs := make([]DeviceInfo, 0, len(infoPaths))
	for _, path := range infoPaths {
		m := captureNodeRe.FindStringSubmatch(filepath.ToSlash(path))
		if m == nil {
			continue
		}
		card, _ := strconv.Atoi(m[1])
		device, _ := strconv.Atoi(m[2])
		name := names[card]
		if name == "" {
			name = fmt.Sprintf("card %d", card)
		}
		ci, ok := idents[card]
		if !ok {
			ci = readCardIdent(sysPath, card)
			idents[card] = ci
		}
		id, stable := stableID(ci, card, device)
		d := DeviceInfo{
			ID:       id,
			Card:     card,
			Device:   device,
			Name:     name,
			HWAddr:   hwAddr(card, device),
			IDStable: stable,
			PortID:   usbPortID(ci.USB, device),
			CardID:   ci.CardID,
		}
		if ci.USB != nil {
			d.USB = *ci.USB
		}
		devs = append(devs, d)
	}
	sort.Slice(devs, func(i, j int) bool {
		if devs[i].Card != devs[j].Card {
			return devs[i].Card < devs[j].Card
		}
		return devs[i].Device < devs[j].Device
	})
	return devs, nil
}

// quoteGlobMeta escapes the filepath.Match metacharacters ('*', '?', '[' and the
// escape byte '\') in a literal path segment, so an injected root that contains
// one is globbed literally rather than parsed as a pattern. path/filepath has no
// QuoteMeta of its own, and regexp.QuoteMeta escapes a different, larger set and
// is wrong for a filepath, so this is the filepath-specific analogue.
func quoteGlobMeta(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '*', '?', '[', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseCards maps each card index to its longname (the text after " - " on the
// card header line). The wrapped second line of each entry is ignored.
func parseCards(data []byte) map[int]string {
	names := make(map[int]string)
	for _, line := range strings.Split(string(data), "\n") {
		m := cardHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		names[idx] = m[2]
	}
	return names
}
