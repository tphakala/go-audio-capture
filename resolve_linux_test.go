//go:build linux

package capture

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestResolveDeviceStableForms(t *testing.T) {
	useFixture(t, hostLayout())

	tests := []struct {
		name, id  string
		card, dev int
	}{
		{"serial form", wantSerialID, 2, 0},
		{"port form of a serialled card", "usb:16d0:06f3:p=0000:00:14.0-2:if=0,0", 2, 0},
		{"port form", wantPortID, 1, 0},
		{"alsa-lib card form", wantLoopbackID, 0, 0},
		{"alsa-lib card form, second pcm", "hw:CARD=Loopback,DEV=1", 0, 1},
		{"alsa-lib card form, DEV omitted", "hw:CARD=Loopback", 0, 0},
		{"surrounding whitespace", "  " + wantSerialID + "  ", 2, 0},
		{"uppercase vid/pid", "usb:16D0:06F3:s=0384_2474750763FA81C9:if=0,0", 2, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := resolveDevice(tt.id)
			if err != nil {
				t.Fatalf("resolveDevice(%q): %v", tt.id, err)
			}
			if r.card != tt.card || r.device != tt.dev {
				t.Errorf("resolveDevice(%q) = card %d dev %d, want card %d dev %d", tt.id, r.card, r.device, tt.card, tt.dev)
			}
			if r.verifyID == "" {
				t.Errorf("resolveDevice(%q) verifyID is empty; a stable id must be re-checked after open", tt.id)
			}
		})
	}
}

func TestResolveDeviceNotFound(t *testing.T) {
	useFixture(t, hostLayout())

	for _, id := range []string{
		"usb:16d0:06f3:s=NOTPRESENT:if=0,0",
		"usb:dead:beef:p=0000:00:14.0-3:if=0,0", // right port, different model
		"hw:CARD=Nonexistent,DEV=0",
		"usb:16d0:06f3:s=0384_2474750763FA81C9:if=0,7", // present card, absent pcm
	} {
		_, err := resolveDevice(id)
		var nf *DeviceNotFoundError
		if !errors.As(err, &nf) {
			t.Errorf("resolveDevice(%q) err = %v, want *DeviceNotFoundError", id, err)
			continue
		}
		// Existing consumers retire a device on ErrDeviceGone; that must keep
		// working for an id that no longer matches anything.
		if !errors.Is(err, ErrDeviceGone) {
			t.Errorf("resolveDevice(%q): errors.Is(err, ErrDeviceGone) = false, want true", id)
		}
		if nf.ID != id {
			t.Errorf("DeviceNotFoundError.ID = %q, want %q", nf.ID, id)
		}
		// The rendered message must name the offending id, or a stubbed
		// Error() would go undetected. The ids here are long and distinctive,
		// so a substring match cannot collide.
		if !strings.Contains(nf.Error(), id) {
			t.Errorf("DeviceNotFoundError.Error() = %q, does not name the id %q", nf.Error(), id)
		}
	}
}

// TestResolveDeviceAmbiguous covers two units that report the same serial: the
// library must refuse rather than open a coin-flip device.
func TestResolveDeviceAmbiguous(t *testing.T) {
	twin := func(card int, devpath string) fakeCard {
		c := serialCard(card, dupSerial, devpath)
		c.CardID = "Mic" + devpath
		return c
	}
	useFixture(t, []fakeCard{twin(1, "3"), twin(2, "4")})

	id := "usb:16d0:06f3:s=DUPLICATE:if=0,0"
	_, err := resolveDevice(id)
	var amb *AmbiguousDeviceError
	if !errors.As(err, &amb) {
		t.Fatalf("resolveDevice(%q) err = %v, want *AmbiguousDeviceError", id, err)
	}
	if len(amb.Matches) != 2 {
		t.Errorf("Matches = %v, want both cards", amb.Matches)
	}
	// The message tells the user to pin one of the listed ids, so it must name the
	// two PortIDs (not the hw addresses): the twin on devpath 3 and on devpath 4.
	port3 := twinPort3ID
	port4 := twinPort4ID
	if !strings.Contains(err.Error(), port3) || !strings.Contains(err.Error(), port4) {
		t.Errorf("error should name both PortIDs %q and %q, got %q", port3, port4, err.Error())
	}

	// The escape hatch the error points at: pinning the port must resolve to
	// exactly one of them.
	for _, tc := range []struct {
		portID string
		card   int
	}{
		{twinPort3ID, 1},
		{twinPort4ID, 2},
	} {
		r, err := resolveDevice(tc.portID)
		if err != nil {
			t.Errorf("resolveDevice(%q): %v", tc.portID, err)
			continue
		}
		if r.card != tc.card {
			t.Errorf("resolveDevice(%q) = card %d, want %d", tc.portID, r.card, tc.card)
		}
	}
}

func TestResolveDeviceMalformed(t *testing.T) {
	useFixture(t, hostLayout())

	for _, id := range []string{
		"usb:",
		"usb:16d0",
		"usb:16d0:06f3",
		"usb:16d0:06f3:s=SN",         // no :if=
		"usb:16d0:06f3:x=SN:if=0,0",  // unknown selector
		"usb:16d:06f3:s=SN:if=0,0",   // vid not four hex digits
		"usb:16d0:06g3:s=SN:if=0,0",  // pid not hex
		"usb:16d0:06f3:s=:if=0,0",    // empty selector value
		"usb:16d0:06f3:s=SN:if=x,0",  // interface not a number
		"usb:16d0:06f3:s=SN:if=0,-1", // negative device
		"usb:16d0:06f3:s=%ZZ:if=0,0", // bad escape
		"usb:16d0:06f3:s=SN%:if=0,0", // truncated escape
		"hw:CARD=",                   // empty card id
		"hw:CARD=Loopback,DEVICE=0",  // wrong key
		"hw:CARD=Loopback,DEV=x",     // device not a number
		"hw:NOTAKEY=Loopback",        // wrong key
	} {
		_, err := resolveDevice(id)
		var bde *BadDeviceError
		if !errors.As(err, &bde) {
			t.Errorf("resolveDevice(%q) err = %v, want *BadDeviceError", id, err)
			continue
		}
		// The rendered message must name the offending id, or a stubbed
		// Error() would go undetected. The ids here are long and distinctive,
		// so a substring match cannot collide.
		if !strings.Contains(bde.Error(), id) {
			t.Errorf("BadDeviceError.Error() = %q, does not name the id %q", bde.Error(), id)
		}
	}
}

func TestResolveExported(t *testing.T) {
	t.Run("stable serial id returns the right device", func(t *testing.T) {
		useFixture(t, hostLayout())
		d, err := Resolve(wantSerialID)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", wantSerialID, err)
		}
		if d.Card != 2 || d.Device != 0 || d.HWAddr != hwAddrCard2 || d.ID != wantSerialID {
			t.Errorf("Resolve = %+v, want the card 2 device", d)
		}
	})

	t.Run("numeric id returns the named device, not a bare success", func(t *testing.T) {
		useFixture(t, hostLayout())
		// Resolve must return the actual device the index names (card 1 is the
		// AMS-24 on port 3), so throwing the value away could hide a resolve
		// that returns the wrong device.
		d, err := Resolve(hwAddrCard1)
		if err != nil {
			t.Fatalf("Resolve(hw:1,0): %v", err)
		}
		if d.Card != 1 || d.Device != 0 || d.HWAddr != hwAddrCard1 || d.ID != wantPortID {
			t.Errorf("Resolve(hw:1,0) = %+v, want the card 1 device", d)
		}
	})

	t.Run("absent numeric id is not found and unwraps to ErrDeviceGone", func(t *testing.T) {
		useFixture(t, hostLayout())
		_, err := Resolve("hw:9,0")
		var nf *DeviceNotFoundError
		if !errors.As(err, &nf) {
			t.Errorf("Resolve(hw:9,0) err = %v, want *DeviceNotFoundError", err)
		}
		if !errors.Is(err, ErrDeviceGone) {
			t.Errorf("Resolve(hw:9,0): errors.Is(err, ErrDeviceGone) = false, want true")
		}
	})

	t.Run("malformed id is a BadDeviceError", func(t *testing.T) {
		useFixture(t, hostLayout())
		_, err := Resolve("usb:16d0:06f3:s=SN:if=x,0")
		var bde *BadDeviceError
		if !errors.As(err, &bde) {
			t.Errorf("Resolve(malformed) err = %v, want *BadDeviceError", err)
		}
	})

	t.Run("ambiguous id is an AmbiguousDeviceError", func(t *testing.T) {
		twin := func(card int, devpath string) fakeCard {
			c := serialCard(card, dupSerial, devpath)
			c.CardID = "Mic" + devpath
			return c
		}
		useFixture(t, []fakeCard{twin(1, "3"), twin(2, "4")})
		_, err := Resolve("usb:16d0:06f3:s=DUPLICATE:if=0,0")
		var amb *AmbiguousDeviceError
		if !errors.As(err, &amb) {
			t.Errorf("Resolve(ambiguous) err = %v, want *AmbiguousDeviceError", err)
		}
	})
}

func TestSerialEscapeRoundTrip(t *testing.T) {
	for _, serial := range []string{
		"PLAIN123",
		"with,comma",
		"with:colon",
		"with%percent",
		"with space",
		"with\ttab",
		"mixed %,: end",
		"\x00\x01binary",
		"ünicode",
		"",
	} {
		esc := escapeSerial(serial)
		if strings.ContainsAny(esc, ",: \t") {
			t.Errorf("escapeSerial(%q) = %q, still contains a delimiter", serial, esc)
		}
		got, err := unescapeIDField(esc)
		if err != nil {
			t.Errorf("unescapeIDField(%q): %v", esc, err)
			continue
		}
		if got != serial {
			t.Errorf("round trip of %q gave %q (escaped %q)", serial, got, esc)
		}
	}
}

// TestResolveSerialWithDelimiters proves an awkward serial survives the whole
// path: enumeration escapes it, and the resulting id resolves back.
func TestResolveSerialWithDelimiters(t *testing.T) {
	useFixture(t, []fakeCard{serialCard(1, "A,B:C%D E", "3")})

	devs, err := Devices()
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(devs), devs)
	}
	id := devs[0].ID
	if want := "usb:16d0:06f3:s=A%2CB%3AC%25D%20E:if=0,0"; id != want {
		t.Fatalf("ID = %q, want %q", id, want)
	}
	r, err := resolveDevice(id)
	if err != nil {
		t.Fatalf("resolveDevice(%q): %v", id, err)
	}
	if r.card != 1 || r.device != 0 {
		t.Errorf("resolved to card %d dev %d, want 1/0", r.card, r.device)
	}
}

func TestCanonicalStableIDNormalisesEscapeCase(t *testing.T) {
	lower, err := canonicalStableID("usb:16d0:06f3:s=A%2cB:if=0,0")
	if err != nil {
		t.Fatalf("canonicalStableID: %v", err)
	}
	upper, err := canonicalStableID("usb:16d0:06f3:s=A%2CB:if=0,0")
	if err != nil {
		t.Fatalf("canonicalStableID: %v", err)
	}
	if lower != upper {
		t.Errorf("escape case should not matter: %q vs %q", lower, upper)
	}
}

// TestOpenRejectsCardSwappedDuringOpen drives the resolve-to-open race: the
// card is swapped for a different one in the very window between resolving the
// id and the PCM open returning. Open must notice and refuse.
func TestOpenRejectsCardSwappedDuringOpen(t *testing.T) {
	proc, sys := buildFixture(t, hostLayout())
	setRoots(t, proc, sys)

	// Build the swapped sysfs tree up front: the two USB cards trade indices, so
	// card 2 is now the AMS-24, not the AudioMoth that resolved. Building it
	// before the fake is installed keeps the fake body to a single sysRoot
	// assignment and its t.Fatalf paths out of the open seam.
	_, swappedSys := buildFixture(t, []fakeCard{
		loopbackCard(0),
		serialCard(1, audiomothSerial, "2"),
		portCard(2, "3"),
	})
	wrongCard := &fakePCM{}
	withOpenPCM(t, func(_, _ int) (pcm, error) {
		// The swap lands in the window between resolving the id and the PCM open
		// returning: card 2 is now a different unit.
		sysRoot = swappedSys
		return wrongCard, nil
	})

	_, err := Open(Config{Device: wantSerialID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("Open err = %v, want ErrDeviceGone after a swap in the open window", err)
	}
	if wrongCard.closeCalls != 1 {
		t.Errorf("the PCM opened on the wrong card was closed %d times, want 1", wrongCard.closeCalls)
	}
}

// TestOpenRejectsCardThatLosesSysfsDuringOpen drives the other arm of the
// post-open identity check: after the PCM opens, the card can no longer be
// identified at all (its sysfs node is gone). It cannot be shown to be the one
// asked for, so Open must treat that as ErrDeviceGone and close the PCM, not
// assume the best.
func TestOpenRejectsCardThatLosesSysfsDuringOpen(t *testing.T) {
	proc, sys := buildFixture(t, hostLayout())
	setRoots(t, proc, sys)

	// A sysfs tree that lists cards 0 and 1 only, so card 2 (the AudioMoth that
	// resolved) has no sysfs node and reads as unidentifiable after the open.
	_, blindSys := buildFixture(t, []fakeCard{loopbackCard(0), portCard(1, "3")})
	pcmClosed := &fakePCM{}
	withOpenPCM(t, func(_, _ int) (pcm, error) {
		sysRoot = blindSys
		return pcmClosed, nil
	})

	_, err := Open(Config{Device: wantSerialID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("Open err = %v, want ErrDeviceGone when the card can no longer be identified", err)
	}
	if pcmClosed.closeCalls != 1 {
		t.Errorf("the unidentifiable PCM was closed %d times, want 1", pcmClosed.closeCalls)
	}
}

// TestOpenAcceptsUnchangedCard is the counterpart: with nothing swapped, the
// identity check must not reject a perfectly good open.
func TestOpenAcceptsUnchangedCard(t *testing.T) {
	useFixture(t, hostLayout())

	var gotCard, gotDevice int
	withOpenPCM(t, func(card, device int) (pcm, error) {
		gotCard, gotDevice = card, device
		return &fakePCM{}, nil
	})

	s, err := Open(Config{Device: wantSerialID, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	if gotCard != 2 || gotDevice != 0 {
		t.Errorf("opened card %d device %d, want 2/0", gotCard, gotDevice)
	}
	if got := s.Negotiated().Device; got != wantSerialID {
		t.Errorf("Negotiated().Device = %q, want the id as passed", got)
	}
}

// TestOpenPortIDPinsOneOfTwoTwins is the happy path behind AmbiguousDeviceError:
// a port-form id opens exactly one of two same-serial units.
func TestOpenPortIDPinsOneOfTwoTwins(t *testing.T) {
	twin := func(card int, devpath string) fakeCard {
		c := serialCard(card, dupSerial, devpath)
		c.CardID = "Mic" + devpath
		return c
	}
	useFixture(t, []fakeCard{twin(1, "3"), twin(2, "4")})

	var gotCard int
	withOpenPCM(t, func(card, _ int) (pcm, error) {
		gotCard = card
		return &fakePCM{}, nil
	})

	id := twinPort4ID
	s, err := Open(Config{Device: id, Rate: 48000, Channels: 1, Format: FormatS16LE})
	if err != nil {
		t.Fatalf("Open(%q): %v", id, err)
	}
	defer func() { _ = s.Close() }()
	if gotCard != 2 {
		t.Errorf("opened card %d, want 2 (the unit on port 4)", gotCard)
	}
}

func TestOpenNotFoundIsDeviceGone(t *testing.T) {
	useFixture(t, hostLayout())

	withOpenPCM(t, func(_, _ int) (pcm, error) {
		t.Fatal("must not open anything when the id matches nothing")
		return nil, errShouldNotOpen
	})

	_, err := Open(Config{Device: "usb:16d0:06f3:s=GONE:if=0,0", Rate: 48000, Channels: 1, Format: FormatS16LE})
	if !errors.Is(err, ErrDeviceGone) {
		t.Errorf("Open err = %v, want ErrDeviceGone", err)
	}
}

// TestSupportedRatesResolvesStableID checks the capability query takes the same
// ids as Open, since a caller holds one id for both.
func TestSupportedRatesResolvesStableID(t *testing.T) {
	useFixture(t, hostLayout())

	var gotCard, gotDevice int
	withOpenRatePCM(t, func(card, device int) (ratePCM, error) {
		gotCard, gotDevice = card, device
		return &fakeRatePCM{rates: []int{48000}, lo: 48000, hi: 48000}, nil
	})

	rs, err := SupportedRates(wantSerialID, 1, FormatS16LE)
	if err != nil {
		t.Fatalf("SupportedRates: %v", err)
	}
	if gotCard != 2 || gotDevice != 0 {
		t.Errorf("queried card %d device %d, want 2/0", gotCard, gotDevice)
	}
	if len(rs.Rates) != 1 || rs.Rates[0] != 48000 {
		t.Errorf("Rates = %v, want [48000]", rs.Rates)
	}
}

// TestSupportedRatesVerifiedResolvesOnce pins the fix for a second race: the
// refine pass and the verify pass must run against ONE resolution, or a device
// swapped between them would be refined as one unit and verified as another.
//
// A static fixture cannot catch a per-pass resolver, since re-enumerating yields
// the same index. So after the first open, the fixture changes so the target
// serial ALSO appears at a second index: any resolution done after that point
// finds two matches and fails as ambiguous, while the single shared resolution
// (taken before any open) never re-enumerates and is unaffected. Every open
// therefore still targets the originally resolved card 2, and the query still
// succeeds.
func TestSupportedRatesVerifiedResolvesOnce(t *testing.T) {
	proc, sys := buildFixture(t, hostLayout())
	setRoots(t, proc, sys)

	// The AudioMoth's serial appears at index 2 (as resolved) and again at index
	// 5, so a re-enumeration would be ambiguous.
	dupProc, dupSys := buildFixture(t, []fakeCard{
		loopbackCard(0),
		portCard(1, "3"),
		serialCard(2, audiomothSerial, "2"),
		serialCard(5, audiomothSerial, "9"),
	})

	opens := 0
	cards := map[int]int{}
	withOpenRatePCM(t, func(card, _ int) (ratePCM, error) {
		opens++
		cards[card]++
		if opens == 1 {
			// The single resolution has already happened and the refine pass has
			// opened against it; introduce the ambiguity only now.
			procRoot, sysRoot = dupProc, dupSys
		}
		return &fakeRatePCM{
			rates: []int{48000, 96000}, lo: 48000, hi: 96000,
			verifiable: map[int]bool{48000: true, 96000: true},
		}, nil
	})

	rs, err := SupportedRatesVerified(wantSerialID, 1, FormatS16LE)
	if err != nil {
		t.Fatalf("SupportedRatesVerified: %v (a per-pass resolver would re-enumerate and hit the ambiguity)", err)
	}
	if len(rs.Rates) != 2 {
		t.Errorf("Rates = %v, want both verified", rs.Rates)
	}
	// One refine open plus one per candidate rate, every one against the
	// originally resolved card 2.
	if opens != 3 {
		t.Errorf("opened %d times, want 3", opens)
	}
	if cards[2] != 3 {
		t.Errorf("opens per card = %v, want all three on card 2", cards)
	}
}

// TestResolveEnumerationFailureIsDeviceGone covers the error class that only
// the stable id forms can hit. Resolving one requires enumerating, so an
// unreadable /proc/asound fails the call; if that error escaped untyped, a
// caller following the documented contract (retire on ErrDeviceGone) would
// never fire its retire branch for the very id form the README tells it to
// persist.
func TestResolveEnumerationFailureIsDeviceGone(t *testing.T) {
	setRoots(t, absentSysRoot(t), absentSysRoot(t))

	_, err := Resolve(wantSerialID)
	if err == nil {
		t.Fatal("Resolve with an unreadable /proc/asound returned nil error")
	}
	if !errors.Is(err, ErrDeviceGone) {
		t.Errorf("errors.Is(err, ErrDeviceGone) = false, want true; err = %v", err)
	}
	// The cause must survive, so an operator can tell a missing device from a
	// missing procfs.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the underlying cause was lost; err = %v", err)
	}

	// Open takes the same path and must classify it the same way.
	if _, oerr := Open(Config{Device: wantSerialID, Rate: 48000, Channels: 1, Format: FormatS16LE}); !errors.Is(oerr, ErrDeviceGone) {
		t.Errorf("Open err = %v, want ErrDeviceGone", oerr)
	}
}

// TestResolveAmbiguousFallsBackToHWAddr covers the other half of the ambiguity
// message. Matches normally carries PortIDs, which pin the physical port and
// survive a reboot, but a twin whose port cannot be derived has no PortID and
// must still be named: an empty entry would leave the user with a list they
// cannot act on.
func TestResolveAmbiguousFallsBackToHWAddr(t *testing.T) {
	withPort := serialCard(1, "DUPLICATE", "3")
	withPort.CardID = "MicA"
	noPort := serialCard(2, "DUPLICATE", "4")
	noPort.CardID = "MicB"
	noPort.USB.NoDevPath = true // port not derivable, so PortID is empty
	useFixture(t, []fakeCard{withPort, noPort})

	id := "usb:16d0:06f3:s=DUPLICATE:if=0,0"
	_, err := resolveDevice(id)
	var amb *AmbiguousDeviceError
	if !errors.As(err, &amb) {
		t.Fatalf("resolveDevice(%q) err = %v, want *AmbiguousDeviceError", id, err)
	}
	if len(amb.Matches) != 2 {
		t.Fatalf("Matches = %v, want both cards", amb.Matches)
	}
	for _, m := range amb.Matches {
		if m == "" {
			t.Fatalf("Matches = %v: an empty entry names nothing the caller can pin", amb.Matches)
		}
	}
	// The one with a port is named by it; the one without falls back to its
	// current-boot address rather than going blank.
	if !slices.Contains(amb.Matches, twinPort3ID) {
		t.Errorf("Matches = %v, want the derivable port named by its PortID", amb.Matches)
	}
	if !slices.Contains(amb.Matches, "hw:2,0") {
		t.Errorf("Matches = %v, want the port-less twin named by its HWAddr", amb.Matches)
	}
}

// TestResolveNumericEnumerationFailureIsDeviceGone is the numeric counterpart
// of TestResolveEnumerationFailureIsDeviceGone. Both branches of Resolve reach
// Devices(), so both must classify its failure the same way; otherwise a caller
// retiring on ErrDeviceGone behaves differently depending on which id form it
// happens to hold.
func TestResolveNumericEnumerationFailureIsDeviceGone(t *testing.T) {
	setRoots(t, absentSysRoot(t), absentSysRoot(t))

	_, err := Resolve(hwAddrCard1)
	if err == nil {
		t.Fatal("Resolve with an unreadable /proc/asound returned nil error")
	}
	if !errors.Is(err, ErrDeviceGone) {
		t.Errorf("errors.Is(err, ErrDeviceGone) = false, want true; err = %v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the underlying cause was lost; err = %v", err)
	}
}

// TestAmbiguousDeviceErrorQuotesMatches pins the rendering, because every id
// form ends in ",<dev>": an unquoted comma-joined list cannot be split back
// into its entries.
func TestAmbiguousDeviceErrorQuotesMatches(t *testing.T) {
	e := &AmbiguousDeviceError{
		ID:      "usb:16d0:06f3:s=DUP:if=0,0",
		Matches: []string{twinPort3ID, "hw:2,0"},
	}
	// Assert the whole rendering, not just the quoting: the instruction has to
	// stay honest about what the listed entries are, since a fallback entry is
	// an hw address and cannot be pinned "by its PortID".
	want := fmt.Sprintf("capture: device id %q matches 2 devices (%s, %s); pin one by a listed id",
		e.ID, strconv.Quote(twinPort3ID), strconv.Quote("hw:2,0"))
	if got := e.Error(); got != want {
		t.Errorf("Error() =\n  %q\nwant\n  %q", got, want)
	}

	// The no-matches branch is unreachable from matchStableID but the type is
	// exported, so a caller can construct one; it must not render "0 devices ()".
	empty := &AmbiguousDeviceError{ID: "usb:16d0:06f3:s=DUP:if=0,0"}
	wantEmpty := fmt.Sprintf("capture: device id %q matches multiple devices; pin one of them", empty.ID)
	if got := empty.Error(); got != wantEmpty {
		t.Errorf("Error() with no matches = %q, want %q", got, wantEmpty)
	}
}
