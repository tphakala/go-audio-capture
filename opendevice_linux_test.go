//go:build linux

package capture

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// swappedHostLayout is hostLayout with the two USB cards trading indices, so
// card 2 is the AMS-24 and card 1 is the AudioMoth.
func swappedHostLayout() []fakeCard {
	return []fakeCard{
		loopbackCard(0),
		serialCard(1, audiomothSerial, "2"),
		portCard(2, "3"),
	}
}

// TestOpenBusyOnSwappedCardIsDeviceGone pins that an open failure is attributed
// to the card only after it is shown to be the unit that was asked for. Card 2
// is busy, but it is no longer the AudioMoth that resolved, so the caller must
// be told the device is gone (resolve again), not busy (retry later), which
// would retry against an unrelated device forever.
func TestOpenBusyOnSwappedCardIsDeviceGone(t *testing.T) {
	useFixture(t, hostLayout())
	_, swappedSys := buildFixture(t, swappedHostLayout())
	withOpenPCM(t, func(_, _ int) (pcm, error) {
		sysRoot = swappedSys
		return nil, &recoverError{unix.EBUSY}
	})

	_, err := Open(Config{Device: wantSerialID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if !errors.Is(err, ErrDeviceGone) || errors.Is(err, ErrDeviceInUse) {
		t.Fatalf("Open err = %v, want ErrDeviceGone for a busy card that is not the resolved unit", err)
	}
}

// TestOpenBusyOnUnchangedCardIsDeviceInUse is the counterpart: the right card
// that is busy is still ErrDeviceInUse.
func TestOpenBusyOnUnchangedCardIsDeviceInUse(t *testing.T) {
	useFixture(t, hostLayout())
	withOpenPCM(t, func(_, _ int) (pcm, error) {
		return nil, &recoverError{unix.EBUSY}
	})

	_, err := Open(Config{Device: wantSerialID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if !errors.Is(err, ErrDeviceInUse) {
		t.Fatalf("Open err = %v, want ErrDeviceInUse", err)
	}
}

// TestSupportedRatesBusyOnSwappedCardIsDeviceGone is the query-path twin of
// TestOpenBusyOnSwappedCardIsDeviceGone.
func TestSupportedRatesBusyOnSwappedCardIsDeviceGone(t *testing.T) {
	useFixture(t, hostLayout())
	_, swappedSys := buildFixture(t, swappedHostLayout())
	withOpenRatePCM(t, func(_, _ int) (ratePCM, error) {
		sysRoot = swappedSys
		return nil, &recoverError{unix.EBUSY}
	})

	_, err := SupportedRates(wantSerialID, 1, FormatS16LE)
	if !errors.Is(err, ErrDeviceGone) || errors.Is(err, ErrDeviceInUse) {
		t.Fatalf("SupportedRates err = %v, want ErrDeviceGone for a busy card that is not the resolved unit", err)
	}
}

// TestSupportedRatesBusyOnUnchangedCardIsDeviceInUse is the counterpart.
func TestSupportedRatesBusyOnUnchangedCardIsDeviceInUse(t *testing.T) {
	useFixture(t, hostLayout())
	withOpenRatePCM(t, func(_, _ int) (ratePCM, error) {
		return nil, &recoverError{unix.EBUSY}
	})

	_, err := SupportedRates(wantSerialID, 1, FormatS16LE)
	if !errors.Is(err, ErrDeviceInUse) {
		t.Fatalf("SupportedRates err = %v, want ErrDeviceInUse", err)
	}
}

var odCfg = Config{Rate: 48000, Channels: 1, Format: FormatS16LE}

// twinLayout is two same-serial units on ports 3 and 4. swap puts the port-4
// unit on card 1 and the port-3 unit on card 2.
func twinLayout(swap bool) []fakeCard {
	twin := func(card int, devpath string) fakeCard {
		c := serialCard(card, dupSerial, devpath)
		c.CardID = "Mic" + devpath
		return c
	}
	if swap {
		return []fakeCard{twin(1, "4"), twin(2, "3")}
	}
	return []fakeCard{twin(1, "3"), twin(2, "4")}
}

// breakEnumeration points procRoot at a tree that does not exist, so Devices
// fails, while leaving sysfs readable for the post-open identity check.
func breakEnumeration(t *testing.T, sys string) {
	t.Helper()
	setRoots(t, absentSysRoot(t), sys)
}

func recordOpen(t *testing.T) (card, device *int) {
	t.Helper()
	card, device = new(int), new(int)
	*card, *device = -1, -1
	withOpenPCM(t, func(c, d int) (pcm, error) {
		*card, *device = c, d
		return &fakePCM{}, nil
	})
	return card, device
}

func mustResolve(t *testing.T, id string) DeviceInfo {
	t.Helper()
	d, err := Resolve(id)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", id, err)
	}
	return d
}

// TestOpenDeviceDoesNotEnumerate pins the point of OpenDevice: with the device
// already resolved, the open does not read /proc/asound again.
func TestOpenDeviceDoesNotEnumerate(t *testing.T) {
	_, sys := useFixture(t, hostLayout())
	d := mustResolve(t, wantSerialID)
	breakEnumeration(t, sys)
	card, device := recordOpen(t)

	// Open resolves the id by enumerating, so with enumeration broken it fails.
	// That proves the fixture really breaks enumeration, and that an OpenDevice
	// implemented as Open(Config{Device: d.ID}) could not pass below.
	cfg := odCfg
	cfg.Device = wantSerialID
	if _, err := Open(cfg); !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("Open with enumeration broken: err = %v, want ErrDeviceGone", err)
	}

	s, err := OpenDevice(d, odCfg)
	if err != nil {
		t.Fatalf("OpenDevice: %v", err)
	}
	defer func() { _ = s.Close() }()
	if *card != 2 || *device != 0 {
		t.Errorf("opened card %d device %d, want 2/0", *card, *device)
	}
}

// TestOpenDeviceAcrossHotplugTransitions walks the resolve-then-open window: the
// DeviceInfo is resolved against one layout and opened against another.
func TestOpenDeviceAcrossHotplugTransitions(t *testing.T) {
	type when int
	const (
		before when = iota // second layout applied before OpenDevice is called
		inside             // second layout applied inside the open window
	)
	tests := []struct {
		name     string
		second   []fakeCard
		when     when
		wantGone bool
		wantCard int
	}{
		{name: "unchanged", second: hostLayout(), when: before, wantCard: 2},
		{name: "swap before the call", second: swappedHostLayout(), when: before, wantGone: true},
		{name: "swap inside the open window", second: swappedHostLayout(), when: inside, wantGone: true},
		{
			name:   "card loses sysfs inside the window",
			second: []fakeCard{loopbackCard(0), portCard(1, "3")}, when: inside, wantGone: true,
		},
		{
			name:   "same unit replugged on another port",
			second: []fakeCard{loopbackCard(0), portCard(1, "3"), serialCard(2, audiomothSerial, "5")},
			when:   before, wantGone: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useFixture(t, hostLayout())
			d := mustResolve(t, wantSerialID)
			proc2, sys2 := buildFixture(t, tt.second)

			p := &fakePCM{}
			opened := -1
			withOpenPCM(t, func(c, _ int) (pcm, error) {
				opened = c
				if tt.when == inside {
					sysRoot = sys2
				}
				return p, nil
			})
			if tt.when == before {
				setRoots(t, proc2, sys2)
			}

			s, err := OpenDevice(d, odCfg)
			if tt.wantGone {
				if !errors.Is(err, ErrDeviceGone) {
					t.Fatalf("OpenDevice err = %v, want ErrDeviceGone", err)
				}
				if p.closeCalls != 1 {
					t.Errorf("PCM closed %d times, want 1", p.closeCalls)
				}
				// The reconnect loop: resolve again on the new layout and open the
				// fresh DeviceInfo.
				setRoots(t, proc2, sys2)
				sysRoot = sys2
				d2, rerr := Resolve(wantSerialID)
				if rerr != nil {
					// The unit is gone for good (its card lost its identity): the
					// reconnect loop then reads it as absent, not as something to open.
					if !errors.Is(rerr, ErrDeviceGone) {
						t.Fatalf("Resolve after the transition: %v, want ErrDeviceGone", rerr)
					}
					return
				}
				s2, err2 := OpenDevice(d2, odCfg)
				if err2 != nil {
					t.Fatalf("OpenDevice after re-Resolve: %v", err2)
				}
				if opened != d2.Card {
					t.Errorf("reopened card %d, want %d", opened, d2.Card)
				}
				_ = s2.Close()
				return
			}
			if err != nil {
				t.Fatalf("OpenDevice: %v", err)
			}
			defer func() { _ = s.Close() }()
			if opened != tt.wantCard {
				t.Errorf("opened card %d, want %d", opened, tt.wantCard)
			}
		})
	}
}

// TestOpenDeviceTwinPinnedByPort: a DeviceInfo for the port-4 twin carries the
// serial form as ID, which both twins share, so the port must be checked too.
func TestOpenDeviceTwinPinnedByPort(t *testing.T) {
	useFixture(t, twinLayout(false))
	d := mustResolve(t, twinPort4ID)
	if d.Card != 2 {
		t.Fatalf("setup: port-4 twin on card %d, want 2", d.Card)
	}

	t.Run("unchanged opens card 2", func(t *testing.T) {
		card, _ := recordOpen(t)
		s, err := OpenDevice(d, odCfg)
		if err != nil {
			t.Fatalf("OpenDevice: %v", err)
		}
		defer func() { _ = s.Close() }()
		if *card != 2 {
			t.Errorf("opened card %d, want 2", *card)
		}
	})

	t.Run("twins trade indices", func(t *testing.T) {
		_, swappedSys := buildFixture(t, twinLayout(true))
		p := &fakePCM{}
		withOpenPCM(t, func(_, _ int) (pcm, error) {
			sysRoot = swappedSys
			return p, nil
		})
		_, err := OpenDevice(d, odCfg)
		if !errors.Is(err, ErrDeviceGone) {
			t.Fatalf("OpenDevice err = %v, want ErrDeviceGone (card 2 is now the port-3 twin)", err)
		}
		if p.closeCalls != 1 {
			t.Errorf("PCM closed %d times, want 1", p.closeCalls)
		}
	})
}

// TestOpenDeviceSerialWithoutPortSearches: with a serial-form ID and no PortID
// nothing read after the open can tell same-serial units apart, so OpenDevice
// resolves the id as Open does.
func TestOpenDeviceSerialWithoutPortSearches(t *testing.T) {
	noPort := func(c fakeCard) fakeCard {
		c.USB.NoDevPath = true
		return c
	}
	t.Run("twins are ambiguous", func(t *testing.T) {
		layout := twinLayout(false)
		for i := range layout {
			layout[i] = noPort(layout[i])
		}
		useFixture(t, layout)
		devs, err := Devices()
		if err != nil {
			t.Fatalf("Devices: %v", err)
		}
		d := devs[len(devs)-1]
		if d.PortID != "" || !d.IDStable || d.USB.Serial == "" {
			t.Fatalf("setup: want a serial-form ID without PortID, got %+v", d)
		}
		withOpenPCM(t, func(_, _ int) (pcm, error) {
			t.Fatal("must not open when the serial matches two units")
			return nil, errShouldNotOpen
		})
		_, err = OpenDevice(d, odCfg)
		if _, ok := errors.AsType[*AmbiguousDeviceError](err); !ok {
			t.Fatalf("OpenDevice err = %v, want *AmbiguousDeviceError", err)
		}
	})
	t.Run("single unit opens via a search", func(t *testing.T) {
		_, sys := useFixture(t, []fakeCard{loopbackCard(0), noPort(serialCard(1, audiomothSerial, "2"))})
		devs, err := Devices()
		if err != nil {
			t.Fatalf("Devices: %v", err)
		}
		d := findDevice(t, devs, "hw:1,0")
		if d.PortID != "" || d.ID == "" || !d.IDStable {
			t.Fatalf("setup: %+v", d)
		}
		card, _ := recordOpen(t)
		s, err := OpenDevice(d, odCfg)
		if err != nil {
			t.Fatalf("OpenDevice: %v", err)
		}
		_ = s.Close()
		if *card != 1 {
			t.Errorf("opened card %d, want 1", *card)
		}
		breakEnumeration(t, sys)
		if _, err := OpenDevice(d, odCfg); !errors.Is(err, ErrDeviceGone) {
			t.Fatalf("OpenDevice with enumeration broken: err = %v, want ErrDeviceGone (this shape searches)", err)
		}
	})
}

// TestOpenDeviceBusyWrongCardIsDeviceGone: the card at the resolved index is
// busy, but it is not the unit that resolved.
func TestOpenDeviceBusyWrongCardIsDeviceGone(t *testing.T) {
	useFixture(t, hostLayout())
	d := mustResolve(t, wantSerialID)
	_, swappedSys := buildFixture(t, swappedHostLayout())

	t.Run("swapped", func(t *testing.T) {
		// The fake swaps sysRoot, which the "unchanged" case must not inherit.
		origSys := sysRoot
		t.Cleanup(func() { sysRoot = origSys })
		withOpenPCM(t, func(_, _ int) (pcm, error) {
			sysRoot = swappedSys
			return nil, &recoverError{unix.EBUSY}
		})
		_, err := OpenDevice(d, odCfg)
		if !errors.Is(err, ErrDeviceGone) || errors.Is(err, ErrDeviceInUse) {
			t.Fatalf("OpenDevice err = %v, want ErrDeviceGone", err)
		}
	})
	t.Run("unchanged", func(t *testing.T) {
		withOpenPCM(t, func(_, _ int) (pcm, error) {
			return nil, &recoverError{unix.EBUSY}
		})
		_, err := OpenDevice(d, odCfg)
		if !errors.Is(err, ErrDeviceInUse) {
			t.Fatalf("OpenDevice err = %v, want ErrDeviceInUse", err)
		}
	})
}

func TestOpenDeviceRejectsBadDeviceInfo(t *testing.T) {
	useFixture(t, hostLayout())
	withOpenPCM(t, func(_, _ int) (pcm, error) {
		t.Fatal("must not open anything for an unusable DeviceInfo")
		return nil, errShouldNotOpen
	})
	tests := []struct {
		name    string
		d       DeviceInfo
		wantCfg bool
	}{
		{name: "zero value", d: DeviceInfo{}, wantCfg: true},
		{name: "unparsable id", d: DeviceInfo{ID: "garbage"}},
		{name: "numeric id disagrees with Card", d: DeviceInfo{ID: "hw:1,0", Card: 2}},
		{name: "negative card", d: DeviceInfo{ID: "hw:0,0", Card: -1}},
		{name: "numeric id with PortID", d: DeviceInfo{ID: hwAddrCard2, Card: 2, PortID: twinPort4ID}},
		{name: "serial id disagrees with Device", d: DeviceInfo{ID: wantSerialID, Card: 2, Device: 1}},
		{name: "card-form id with PortID", d: DeviceInfo{ID: wantLoopbackID, PortID: twinPort4ID}},
		{name: "PortID in serial form", d: DeviceInfo{ID: wantSerialID, Card: 2, PortID: wantSerialID}},
		{name: "PortID with another vid:pid", d: DeviceInfo{ID: wantSerialID, Card: 2, PortID: "usb:1686:067f:p=0000:00:14.0-2:if=0,0"}},
		{name: "PortID with another interface", d: DeviceInfo{ID: wantSerialID, Card: 2, PortID: "usb:16d0:06f3:p=0000:00:14.0-2:if=1,0"}},
		{name: "PortID with another device", d: DeviceInfo{ID: wantSerialID, Card: 2, PortID: "usb:16d0:06f3:p=0000:00:14.0-2:if=0,1"}},
		{name: "port id differs from PortID", d: DeviceInfo{ID: twinPort3ID, Card: 2, PortID: twinPort4ID}},
		{name: "unparsable PortID", d: DeviceInfo{ID: wantSerialID, Card: 2, PortID: "usb:nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := OpenDevice(tt.d, odCfg)
			if tt.wantCfg {
				ce, ok := errors.AsType[*ConfigError](err)
				if !ok || ce.Field != "device" {
					t.Fatalf("err = %v, want *ConfigError for field device", err)
				}
				return
			}
			if _, ok := errors.AsType[*BadDeviceError](err); !ok {
				t.Fatalf("err = %v, want *BadDeviceError", err)
			}
		})
	}
}

func TestOpenDeviceChecksConfigBeforeDevice(t *testing.T) {
	withOpenPCM(t, func(_, _ int) (pcm, error) {
		t.Fatal("must not open")
		return nil, errShouldNotOpen
	})
	tests := []struct {
		cfg   Config
		field string
	}{
		{Config{Rate: 0, Channels: 1, Format: FormatS16LE}, "rate"},
		{Config{Rate: 48000, Channels: 0, Format: FormatS16LE}, "channels"},
		{Config{Rate: 48000, Channels: 1, Format: Format(99)}, fieldFormat},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			// The DeviceInfo is also invalid (empty), so a config-first order is
			// the only way to see the config error.
			_, err := OpenDevice(DeviceInfo{}, tt.cfg)
			ce, ok := errors.AsType[*ConfigError](err)
			if !ok || ce.Field != tt.field {
				t.Fatalf("err = %v, want *ConfigError for field %s", err, tt.field)
			}
		})
	}
}

// TestOpenDeviceUnstableInfoOpensAddress: an IDStable-false DeviceInfo names a
// card index, so it opens that index without consulting sysfs, as Open does for
// "hw:N,D".
func TestOpenDeviceUnstableInfoOpensAddress(t *testing.T) {
	proc, _ := buildFixture(t, hostLayout())
	setRoots(t, proc, absentSysRoot(t))
	d := findDevice(t, mustDevices(t), "hw:2,0")
	if d.IDStable || d.ID != "hw:2,0" {
		t.Fatalf("setup: %+v", d)
	}
	// If OpenDevice consulted sysfs after the open, this layout would not match.
	_, otherSys := buildFixture(t, []fakeCard{loopbackCard(0)})
	var card, device int
	withOpenPCM(t, func(c, dv int) (pcm, error) {
		card, device = c, dv
		sysRoot = otherSys
		return &fakePCM{}, nil
	})
	s, err := OpenDevice(d, odCfg)
	if err != nil {
		t.Fatalf("OpenDevice: %v", err)
	}
	_ = s.Close()
	if card != 2 || device != 0 {
		t.Errorf("opened %d/%d, want 2/0", card, device)
	}
}

func mustDevices(t *testing.T) []DeviceInfo {
	t.Helper()
	devs, err := Devices()
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	return devs
}

// TestOpenDeviceReportsDeviceInfoID: cfg.Device is ignored; the DeviceInfo
// decides what opens and what Negotiated reports.
func TestOpenDeviceReportsDeviceInfoID(t *testing.T) {
	useFixture(t, hostLayout())
	d := mustResolve(t, wantSerialID)
	card, _ := recordOpen(t)
	cfg := odCfg
	cfg.Device = "hw:9,9"
	s, err := OpenDevice(d, cfg)
	if err != nil {
		t.Fatalf("OpenDevice: %v", err)
	}
	defer func() { _ = s.Close() }()
	if *card != d.Card {
		t.Errorf("opened card %d, want %d", *card, d.Card)
	}
	if got := s.Negotiated().Device; got != d.ID {
		t.Errorf("Negotiated().Device = %q, want %q", got, d.ID)
	}
}

// TestVerifyCardIdentityPort: the port check applies even when the serial
// matches, which is the case for same-serial twins.
func TestVerifyCardIdentityPort(t *testing.T) {
	useFixture(t, twinLayout(false)) // card 1 on port 3, card 2 on port 4
	serialID := mustResolve(t, twinPort4ID).ID
	tests := []struct {
		name string
		r    resolved
		want bool // want nil
	}{
		{"serial and matching port", resolved{card: 2, verifyID: serialID, verifyPort: twinPort4ID}, true},
		{"serial matches but port is the other twin", resolved{card: 2, verifyID: serialID, verifyPort: twinPort3ID}, false},
		{"no port requirement (Open path)", resolved{card: 2, verifyID: serialID}, true},
		{"no identity requirement", resolved{card: 2}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyCardIdentity(tt.r)
			if tt.want && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if !tt.want && !errors.Is(err, ErrDeviceGone) {
				t.Fatalf("err = %v, want ErrDeviceGone", err)
			}
		})
	}
}

func TestCanonicalStableIDParsedFields(t *testing.T) {
	tests := []struct {
		in   string
		want parsedID
	}{
		{wantSerialID, parsedID{canon: wantSerialID, usb: true, vidpid: "16d0:06f3", iface: 0}},
		{"usb:16D0:06F3:p=0000:00:14.0-3:if=2,3", parsedID{
			canon: "usb:16d0:06f3:p=0000:00:14.0-3:if=2,3", device: 3, usb: true, port: true, vidpid: "16d0:06f3", iface: 2,
		}},
		{"hw:CARD=Loopback", parsedID{canon: wantLoopbackID}},
	}
	for _, tt := range tests {
		got, err := canonicalStableID(tt.in)
		if err != nil {
			t.Fatalf("canonicalStableID(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("canonicalStableID(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}
