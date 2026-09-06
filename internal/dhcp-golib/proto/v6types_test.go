package proto

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

// numbered reports whether s is the default rendering of a kind with no
// String() arm: "event(11)", "action(13)". Those are the two shapes that read
// as a name in a journal and are not one.
func numbered(s string) bool {
	return strings.ContainsAny(s, "0123456789")
}

// TestAllEventKindsCoversTheConstantBlock is D-1, and it is the row that keeps
// the enum's two spellings from drifting apart.
//
// Two tests in this package derive their whole domain from AllEventKinds():
// TestStepIsTotal and TestJournalEntryEventRoundTrips. A kind added to the
// constant block and NOT to the slice does not fail either of them — it SHRINKS
// their domain, which is the failure mode a universal gate has: it is satisfied
// by emptying its own population.
//
// The cross-check is the kind ONE PAST the end of the slice. The constants are
// an iota run from zero, so if the slice is complete then the next value is
// undeclared, and an undeclared kind renders as "event(N)". Adding a constant
// to the block and giving it the String() arm that naturally comes with it
// makes that rendering a name, and this fails.
func TestAllEventKindsCoversTheConstantBlock(t *testing.T) {
	kinds := AllEventKinds()
	if len(kinds) == 0 {
		t.Fatal("AllEventKinds is empty; this test would measure nothing")
	}
	for i, k := range kinds {
		if int(k) != i {
			t.Fatalf("AllEventKinds[%d] = %s (%d); the slice must be the iota run in order, with no gap and no repeat",
				i, k, uint8(k))
		}
		if numbered(k.String()) {
			t.Errorf("event kind %d has no String() arm: %q", uint8(k), k.String())
		}
	}
	next := EventKind(len(kinds))
	if !numbered(next.String()) {
		t.Fatalf("EventKind(%d) renders as %q, so a kind was added to the constant block and not to AllEventKinds(). "+
			"TestStepIsTotal and TestJournalEntryEventRoundTrips take their whole domain from that slice, "+
			"so the new kind is not driven by anything and the hole is silent.", len(kinds), next.String())
	}
	if EvRouterAdvert >= next || EvDADResult >= next {
		t.Errorf("the v6 kinds are outside the slice: EvRouterAdvert=%d EvDADResult=%d, slice length %d",
			uint8(EvRouterAdvert), uint8(EvDADResult), len(kinds))
	}
}

// TestEveryActionKindHasAName is D-2. A new kind without a String() arm is
// invisible in production and reads as "action(13)" in the journal — the one
// place an operator looks.
//
// The domain is derived the same way: the iota run from zero up to the last
// declared kind, found by walking until the renderings stop being names. That
// walk is bounded, and the bound is checked, so the test cannot pass by finding
// nothing.
func TestEveryActionKindHasAName(t *testing.T) {
	n := 0
	for ; n < 256; n++ {
		if numbered(ActionKind(n).String()) {
			break
		}
	}
	if n == 0 {
		t.Fatal("ActionKind(0) has no name; the walk found no declared kinds and would have measured nothing")
	}
	if n == 256 {
		t.Fatal("every ActionKind value has a name; the default arm is unreachable and this walk has no end")
	}
	if int(ActRouterObserved) != n-1 {
		t.Errorf("the last named ActionKind is %d and ActRouterObserved is %d; either a kind was added after it "+
			"without a String() arm, or one was removed", n-1, uint8(ActRouterObserved))
	}
	for i := 0; i < n; i++ {
		s := ActionKind(i).String()
		if s == "" || numbered(s) {
			t.Errorf("ActionKind(%d) renders as %q", i, s)
		}
	}
	// The four this round added, by name, so a rename is a compile error here
	// rather than a silent change in the journal an operator greps.
	for _, tc := range []struct {
		k    ActionKind
		want string
	}{
		{ActSendRouterSolicit, "SendRouterSolicit"},
		{ActStartDAD, "StartDAD"},
		{ActConfigured, "Configured"},
		{ActRouterObserved, "RouterObserved"},
	} {
		if got := tc.k.String(); got != tc.want {
			t.Errorf("ActionKind %d renders as %q, want %q", uint8(tc.k), got, tc.want)
		}
	}
}

// TestV6EventConstructorsCarryTheirPayload pins the shape M7b will be written
// against: which field each constructor fills, and that the event renders with
// the fact in it. A journal line that says only "RouterAdvert" tells an
// operator nothing about the advertisement.
func TestV6EventConstructorsCarryTheirPayload(t *testing.T) {
	ra := &wire.RouterAdvert{
		Managed:        true,
		Other:          true,
		RouterLifetime: 300,
		Prefixes: []wire.PrefixInfo{{
			PrefixLen: 64,
			OnLink:    true,
			Prefix:    netip.MustParseAddr("fd00:99::"),
		}},
	}
	ev := RouterAdvert(ra)
	if ev.Kind != EvRouterAdvert {
		t.Errorf("RouterAdvert() built kind %s, want %s", ev.Kind, EvRouterAdvert)
	}
	if ev.RA != ra {
		t.Errorf("RouterAdvert() did not carry the advertisement: %v", ev.RA)
	}
	if s := ev.String(); !strings.Contains(s, "fd00:99::/64") || !strings.Contains(s, "MO") {
		t.Errorf("Event.String() = %q, want the flags and the prefix", s)
	}

	addr := netip.MustParseAddr("fd00:99::183")
	for _, dup := range []bool{false, true} {
		ev := DADResult(addr, dup)
		if ev.Kind != EvDADResult {
			t.Errorf("DADResult() built kind %s, want %s", ev.Kind, EvDADResult)
		}
		if ev.DAD.Addr != addr || ev.DAD.Duplicate != dup {
			t.Errorf("DADResult(%s, %v) carried %+v", addr, dup, ev.DAD)
		}
		s := ev.String()
		if !strings.Contains(s, addr.String()) {
			t.Errorf("Event.String() = %q, missing the address", s)
		}
		if dup == strings.Contains(strings.ToLower(s), "free") {
			t.Errorf("Event.String() = %q does not distinguish a duplicate from a free address", s)
		}
	}
	// A DADOutcome renders on its own too: it is what the journal holds.
	if s := (DADOutcome{Addr: addr, Duplicate: true}).String(); !strings.Contains(s, addr.String()) {
		t.Errorf("DADOutcome.String() = %q, missing the address", s)
	}
	// The zero RA must not panic a journal line.
	if s := (Event{Kind: EvRouterAdvert}).String(); s == "" {
		t.Error("an EvRouterAdvert with no advertisement rendered as the empty string")
	}
}

// TestV6ActionPayloadsRender is the same for the four new actions. Each is
// built by hand, because no state emits one yet (D-3, open until M7b), and the
// point is that the FIELDS exist and reach the journal.
func TestV6ActionPayloadsRender(t *testing.T) {
	target := netip.MustParseAddr("fd00:99::183")
	a := Action{Kind: ActStartDAD, Target: target}
	if s := a.String(); !strings.Contains(s, target.String()) || !strings.Contains(s, "StartDAD") {
		t.Errorf("StartDAD renders as %q, want the kind and the target", s)
	}

	cfg := Config6{
		DNS:         []netip.Addr{netip.MustParseAddr("fd00:99::1")},
		Search:      []string{"fixture.invalid"},
		RefreshTime: 86400 * Second,
	}
	a = Action{Kind: ActConfigured, Config: cfg}
	s := a.String()
	for _, want := range []string{"Configured", "fd00:99::1", "fixture.invalid"} {
		if !strings.Contains(s, want) {
			t.Errorf("Configured renders as %q, missing %q", s, want)
		}
	}
	if s := cfg.String(); !strings.Contains(s, "fd00:99::1") {
		t.Errorf("Config6.String() = %q, missing the resolver", s)
	}

	for _, tc := range []struct {
		obs  RouterObservation
		want string
	}{
		{RouterObservation{}, "no router"},
		{RouterObservation{Seen: true}, "M-"},
		{RouterObservation{Seen: true, Managed: true, Other: true}, "MO"},
	} {
		a := Action{Kind: ActRouterObserved, Router: tc.obs}
		if s := a.String(); !strings.Contains(s, "RouterObserved") {
			t.Errorf("RouterObserved renders as %q", s)
		}
		if s := tc.obs.String(); s == "" {
			t.Errorf("RouterObservation%+v renders as the empty string", tc.obs)
		}
	}
	// RFC 4861 §4.2: "If neither M nor O flags are set, this indicates that no
	// information is available via DHCPv6." A router SEEN with both flags
	// clear must not read the same as no router seen at all, because the two
	// are the distinction this action exists to carry.
	none := RouterObservation{}.String()
	seenQuiet := RouterObservation{Seen: true}.String()
	if none == seenQuiet {
		t.Errorf("a router that advertised neither M nor O renders identically to no router at all (%q); "+
			"telling those apart is the whole reason ActRouterObserved exists", none)
	}

	a = Action{Kind: ActSendRouterSolicit}
	if s := a.String(); !strings.Contains(s, "SendRouterSolicit") {
		t.Errorf("SendRouterSolicit renders as %q", s)
	}
}

// TestTheNewEventKindsAreIgnoredEverywhereForNow drives D-3's boundary
// explicitly rather than leaving it to be discovered.
//
// This round declares the v6 kinds and no state acts on them. That is a
// DEFINED outcome and not an accident: every step function's default arm
// journals the event as ignored, so a v6 event arriving before M7b exists
// leaves the state alone and says so. Asserting it here is what makes M7b's
// first commit a visible change rather than a silent one.
func TestTheNewEventKindsAreIgnoredEverywhereForNow(t *testing.T) {
	ra := RouterAdvert(&wire.RouterAdvert{Managed: true})
	dad := DADResult(netip.MustParseAddr("fd00:99::183"), true)
	for _, ev := range []Event{ra, dad} {
		m := newMachine(t, acdParams(ConflictOff))
		before, _ := m.Step(0, 1, Simple(EvStart))
		after, acts := m.Step(at(1), 2, ev)
		if after != before {
			t.Errorf("%s moved the machine from %s to %s; no state has an arm for it yet (M7b)", ev.Kind, before, after)
		}
		if n := count(acts, ActJournal); n == 0 {
			t.Errorf("%s produced no journal entry; an ignored event that says nothing is indistinguishable from one that was never delivered: %v",
				ev.Kind, acts)
		}
		for _, a := range acts {
			if a.Kind != ActJournal {
				t.Errorf("%s produced %s; this round declares the kinds and acts on none of them", ev.Kind, a)
			}
		}
	}
}
