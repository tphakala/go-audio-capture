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

// Devices enumerates capture-capable PCM devices from /proc/asound. A device
// appears when its card has a capture PCM node (card{C}/pcm{D}c); playback-only
// cards are skipped. The result is a point-in-time snapshot: there is no
// appear/disappear event, so a caller that hot-adds devices as hardware is
// plugged in must poll Devices (or watch /dev/snd) itself.
func Devices() ([]DeviceInfo, error) {
	return devicesFrom("/proc/asound")
}

// cardHeaderRe matches a card header line in /proc/asound/cards, capturing the
// card index and the longname after " - ", e.g.
//
//	" 1 [Device         ]: USB-Audio - C-Media USB Audio Device"
var cardHeaderRe = regexp.MustCompile(`^\s*(\d+)\s+\[[^\]]*\]:\s+\S+\s+-\s+(.+?)\s*$`)

// captureNodeRe matches a capture PCM info path, capturing card and device
// numbers, e.g. ".../card1/pcm0c/info".
var captureNodeRe = regexp.MustCompile(`card(\d+)/pcm(\d+)c/info$`)

func devicesFrom(root string) ([]DeviceInfo, error) {
	cards, err := os.ReadFile(filepath.Join(root, "cards"))
	if err != nil {
		return nil, fmt.Errorf("read %s/cards: %w", root, err)
	}
	names := parseCards(cards)

	infoPaths, err := filepath.Glob(filepath.Join(root, "card*", "pcm*c", "info"))
	if err != nil {
		return nil, fmt.Errorf("glob capture nodes: %w", err)
	}
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
		devs = append(devs, DeviceInfo{
			ID:     fmt.Sprintf("hw:%d,%d", card, device),
			Card:   card,
			Device: device,
			Name:   name,
		})
	}
	sort.Slice(devs, func(i, j int) bool {
		if devs[i].Card != devs[j].Card {
			return devs[i].Card < devs[j].Card
		}
		return devs[i].Device < devs[j].Device
	})
	return devs, nil
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
