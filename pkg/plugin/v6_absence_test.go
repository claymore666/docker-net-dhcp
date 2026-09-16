// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// TestClassifyV6Absence covers every RAObservation value -- three
// booleans, eight inhabitants -- against both causes the classifier
// distinguishes.
//
// TWO ROWS ARE WORTH WRITING DOWN.
//
// {Seen:false, Managed:true} cannot arise from the acquisition path --
// nothing sets Managed without having seen an advertisement -- but a
// classifier that tested Managed FIRST would read it as fatal, and that
// same reordering silently turns every stateless segment fatal too.
// Pinning the impossible row states which of the two fields decides.
//
// And {Seen:true, Managed:true} with dhcp.ErrNoV6Address is the row
// where the wire overrules the diagnostic. It is reachable: the library
// switches to the Information-request on an M=0 O=1 advertisement, and
// a LATER advertisement on the same link can set M -- at which point
// Router() says managed while the segment has already answered "no
// addresses here" on the wire. A classifier that read only the
// observation would call that fatal and refuse to start the container.
func TestClassifyV6Absence(t *testing.T) {
	timeout := errors.New("timed out")
	cases := []struct {
		name  string
		ra    dhcp.RAObservation
		cause error
		want  v6Verdict
	}{
		{"managed segment", dhcp.RAObservation{Seen: true, Managed: true}, timeout, v6Fatal},
		{"managed and other", dhcp.RAObservation{Seen: true, Managed: true, Other: true}, timeout, v6Fatal},
		{"stateless", dhcp.RAObservation{Seen: true, Other: true}, timeout, v6NotOffered},
		{"slaac only", dhcp.RAObservation{Seen: true}, timeout, v6NotOffered},
		{"no router advertised", dhcp.RAObservation{}, timeout, v6NoRouter},
		{"no router, other set", dhcp.RAObservation{Other: true}, timeout, v6NoRouter},
		{"managed without an advertisement", dhcp.RAObservation{Managed: true}, timeout, v6NoRouter},
		{"managed and other without an advertisement", dhcp.RAObservation{Managed: true, Other: true}, timeout, v6NoRouter},

		// The wire beats the diagnostic, in both directions.
		{"stateless reply, quiet observation", dhcp.RAObservation{}, dhcp.ErrNoV6Address, v6NotOffered},
		{"stateless reply, managed observation", dhcp.RAObservation{Seen: true, Managed: true}, dhcp.ErrNoV6Address, v6NotOffered},
		{"stateless reply, wrapped cause", dhcp.RAObservation{Seen: true, Managed: true},
			fmt.Errorf("failed to get initial IPv6 address: %w", dhcp.ErrNoV6Address), v6NotOffered},
	}

	// NON-VACUITY, keyed on the input domain rather than on a row
	// count. A table is a universal that a deleted row satisfies
	// silently -- nothing else in the package, and not
	// check-test-weakening.sh, reports a row that stopped being there.
	// Three booleans have exactly eight inhabitants, so the domain can
	// be stated rather than counted.
	covered := map[dhcp.RAObservation]bool{}
	for _, tc := range cases {
		if tc.cause == timeout {
			covered[tc.ra] = true
		}
	}
	for _, seen := range []bool{false, true} {
		for _, managed := range []bool{false, true} {
			for _, other := range []bool{false, true} {
				ra := dhcp.RAObservation{Seen: seen, Managed: managed, Other: other}
				if !covered[ra] {
					t.Fatalf("no row for %+v under an ordinary timeout. This table is the "+
						"whole statement of which field decides the verdict, and every "+
						"RAObservation value has to be in it -- a missing row is a case "+
						"nothing judges", ra)
				}
			}
		}
	}
	// And the wire-beats-the-diagnostic half needs at least one row
	// whose observation would classify DIFFERENTLY on its own; without
	// it the ErrNoV6Address arm could be deleted and every remaining
	// row would still pass.
	overruled := false
	for _, tc := range cases {
		if errors.Is(tc.cause, dhcp.ErrNoV6Address) && classifyV6Absence(tc.ra, timeout, proto.Mode6DHCP) != tc.want {
			overruled = true
		}
	}
	if !overruled {
		t.Fatal("no row exercises dhcp.ErrNoV6Address against an observation that would " +
			"classify differently on its own, so deleting the wire-beats-the-diagnostic " +
			"arm would leave this table green")
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyV6Absence(tc.ra, tc.cause, proto.Mode6DHCP); got != tc.want {
				t.Errorf("classifyV6Absence(%+v, %v, proto.Mode6DHCP) = %v, want %v", tc.ra, tc.cause, got, tc.want)
			}
		})
	}
}

// TestNoteV6Absence_TolerancePolarity is the one-directional half of the
// guard (#868): the two absences that are tolerated, and the one that is
// not, in one table so that a change flipping any of them cannot be read
// as a change to only its own case.
//
// The fatal row is the load-bearing one. Everything else in the fix
// makes the plugin MORE tolerant, and the whole risk of it is that
// tolerance spreads to a segment which genuinely offered DHCPv6 and then
// went silent. A `return true` in noteV6Absence's default arm makes that
// row red here and nothing else in the package.
func TestNoteV6Absence_TolerancePolarity(t *testing.T) {
	type outcome struct {
		tolerated  bool
		notOffered int32
		noRouter   int32
	}
	cases := []struct {
		name          string
		ra            dhcp.RAObservation
		wantTolerated bool
		wantNotOffer  int32
		wantNoRouter  int32
	}{
		{"stateless is tolerated", dhcp.RAObservation{Seen: true}, true, 1, 0},
		{"absent router is tolerated", dhcp.RAObservation{}, true, 0, 1},
		{"managed is fatal", dhcp.RAObservation{Seen: true, Managed: true}, false, 0, 0},
	}

	// NON-VACUITY, and it is load-bearing here rather than tidy.
	//
	// MEASURED: emptying this table -- INCLUDING the fatal row, the one
	// thing in the package that keeps a real DHCPv6 outage from being
	// waved through -- leaves the lane green and check-test-weakening.sh
	// clean. A table is a universal, and a universal over an empty set
	// is satisfied by nothing at all.
	//
	// Keyed on the three OUTCOMES rather than on a row count, because a
	// count is equally satisfied by duplicating a tolerated row over the
	// fatal one, which is the deletion that actually costs something.
	required := map[outcome]string{
		{true, 1, 0}:  "a stateless segment is TOLERATED and counted as not-offered",
		{true, 0, 1}:  "an absent router is TOLERATED and counted as no-router",
		{false, 0, 0}: "a managed segment is FATAL and moves neither counter",
	}
	for _, tc := range cases {
		delete(required, outcome{tc.wantTolerated, tc.wantNotOffer, tc.wantNoRouter})
	}
	for _, missing := range required {
		t.Fatalf("this table no longer states that %s. All three polarities belong in "+
			"ONE table so that flipping any of them cannot read as a change to only "+
			"its own case -- which is exactly what dropping the row does.", missing)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			got := p.noteV6Absence(tc.ra, "eth0", "abcdef0123456789", errors.New("timed out"), proto.Mode6DHCP)
			if got != tc.wantTolerated {
				t.Errorf("noteV6Absence(%+v, proto.Mode6DHCP) = %v, want %v", tc.ra, got, tc.wantTolerated)
			}
			if n := p.dhcpv6NotOffered.Load(); n != tc.wantNotOffer {
				t.Errorf("dhcpv6_not_offered = %d, want %d", n, tc.wantNotOffer)
			}
			if n := p.dhcpv6NoRouterAdvert.Load(); n != tc.wantNoRouter {
				t.Errorf("dhcpv6_no_router_advert = %d, want %d", n, tc.wantNoRouter)
			}
		})
	}
}

// TestNoteV6Absence_CountersAreNotOneCounter pins that the two tolerated
// cases move DIFFERENT numbers.
//
// The table above would stay green if both arms incremented the same
// counter and the assertions were written to match, so this asserts the
// property directly: after one of each, neither counter carries the
// other's event. An operator reading dhcpv6_no_router_advert is deciding
// whether to go and look for a missing router, and a merged counter
// sends them looking on every stateless network in the estate.
func TestNoteV6Absence_CountersAreNotOneCounter(t *testing.T) {
	p := &Plugin{}
	p.noteV6Absence(dhcp.RAObservation{Seen: true}, "eth0", "aaaa", nil, proto.Mode6DHCP)
	p.noteV6Absence(dhcp.RAObservation{}, "eth0", "bbbb", nil, proto.Mode6DHCP)

	if got := p.dhcpv6NotOffered.Load(); got != 1 {
		t.Errorf("dhcpv6_not_offered = %d after one stateless and one absent-router "+
			"absence, want exactly 1", got)
	}
	if got := p.dhcpv6NoRouterAdvert.Load(); got != 1 {
		t.Errorf("dhcpv6_no_router_advert = %d after one stateless and one absent-router "+
			"absence, want exactly 1", got)
	}
}

// TestNoteV6Absence_TheAbsentRouterCaseCarriesTheCause pins the log
// side, because the two tolerated cases are deliberately not equally
// loud and the difference is the operator's only prompt to act.
//
// A stateless segment is working as configured, so it logs at Info and
// carries no error. An absent router is not a configuration anyone
// chose, so it logs at Warn and carries the acquisition failure that
// produced it — without the cause the warning says something is missing
// but not what was tried.
func TestNoteV6Absence_TheAbsentRouterCaseCarriesTheCause(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	cause := errors.New("no DHCPv6 lease within the budget")
	p := &Plugin{}
	p.noteV6Absence(dhcp.RAObservation{}, "eth7", "0123456789abcdef", cause, proto.Mode6DHCP)

	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("want exactly one log entry for one absence, got %d: %v",
			len(entries), messagesOf(entries))
	}
	e := entries[0]
	if e.Level != log.WarnLevel {
		t.Errorf("absent-router absence logged at %v, want warn — it is the case an "+
			"operator may need to act on", e.Level)
	}
	if e.Data[log.ErrorKey] == nil {
		t.Errorf("the absent-router warning carries no error field; the operator sees "+
			"that IPv6 is missing but not what the plugin tried. Fields: %v", e.Data)
	}
	if e.Data["iface"] != "eth7" {
		t.Errorf("iface field = %v, want eth7 — the warning must name the link it is "+
			"about, since a host can have several segments in different modes", e.Data["iface"])
	}

	hook.Reset()
	p.noteV6Absence(dhcp.RAObservation{Seen: true}, "eth7", "0123456789abcdef", cause, proto.Mode6DHCP)
	entries = hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("want exactly one log entry, got %d: %v", len(entries), messagesOf(entries))
	}
	if lvl := entries[0].Level; lvl != log.InfoLevel {
		t.Errorf("stateless absence logged at %v, want info — a segment that advertises "+
			"no DHCPv6 is working as configured, and warning on it teaches operators to "+
			"ignore this counter's warnings", lvl)
	}
}

// WHAT A SEEN ROUTER AND NO ADDRESS MEANS DEPENDS ON THE MODE (#818).
//
// The table above is the mode the plugin had before `ipv6_mode`:
// addresses come from a server, so an advertisement without the managed
// flag means there are none here and the endpoint starts without one.
// In a mode that forms its own address from the advertisement, that
// same observation is the opposite statement -- the advertisement IS
// the address source, it arrived, and nothing was formed from it -- and
// the endpoint must not start, because its only mechanism produced
// nothing.
//
// THE M=1 ROW IS THE ONE THAT WAS ACTIVELY WRONG. proto.Mode6SLAAC
// sends no Solicit whatever the M flag says, so the answer a `slaac`
// endpoint got on a managed segment was "no DHCPv6 server answered
// within N s" -- about an exchange that never happened, pointing an
// operator at a server this network does not use.
//
// `auto` keeps v6Fatal there and that is not an inconsistency: auto on
// an M=1 advertisement DID solicit, and the fallback that follows a
// silent server either forms an address (in which case this function is
// not reached) or ends the acquisition with the library's own reason.
//
// Both halves of the (observation x mode) domain are enumerated, so a
// verdict that stopped reading the mode fails on the forming rows and a
// verdict that read ONLY the mode fails on the `dhcp` rows.
func TestClassifyV6Absence_TheModeDecidesWhatASeenRouterMeans(t *testing.T) {
	timeout := errors.New("timed out")
	modes := []proto.Mode6{proto.Mode6DHCP, proto.Mode6SLAAC, proto.Mode6Auto, proto.Mode6Off}

	// Written out rather than derived from the function: seen and
	// managed decide, `other` never does, and every mode is here so a
	// new one cannot arrive without a row.
	want := map[proto.Mode6]map[[2]bool]v6Verdict{
		proto.Mode6DHCP: {
			{false, false}: v6NoRouter, {false, true}: v6NoRouter,
			{true, false}: v6NotOffered, {true, true}: v6Fatal,
		},
		proto.Mode6SLAAC: {
			{false, false}: v6NoRouter, {false, true}: v6NoRouter,
			{true, false}: v6SLAACNoAddress, {true, true}: v6SLAACNoAddress,
		},
		proto.Mode6Auto: {
			{false, false}: v6NoRouter, {false, true}: v6NoRouter,
			{true, false}: v6SLAACNoAddress, {true, true}: v6Fatal,
		},
		proto.Mode6Off: {
			{false, false}: v6NoRouter, {false, true}: v6NoRouter,
			{true, false}: v6NotOffered, {true, true}: v6Fatal,
		},
	}
	for _, m := range proto.AllModes6() {
		if _, ok := want[m]; !ok {
			t.Fatalf("the library declares ipv6_mode=%s and this table has no row for it, "+
				"so the endings that mode produces are judged by whatever the switch's "+
				"default arm happens to be", m)
		}
	}

	for _, mode := range modes {
		for _, seen := range []bool{false, true} {
			for _, managed := range []bool{false, true} {
				for _, other := range []bool{false, true} {
					ra := dhcp.RAObservation{Seen: seen, Managed: managed, Other: other}
					got := classifyV6Absence(ra, timeout, mode)
					if got != want[mode][[2]bool{seen, managed}] {
						t.Errorf("classifyV6Absence(%+v, timeout, %s) = %v, want %v",
							ra, mode, got, want[mode][[2]bool{seen, managed}])
					}
				}
			}
		}
	}

	// The wire still beats the mode, the way it beats the observation:
	// a router that advertised prefixes this client refused names the
	// thing to fix, and a forming mode must not overwrite it with the
	// vaguer ending.
	if got := classifyV6Absence(dhcp.RAObservation{Seen: true},
		fmt.Errorf("wrapped: %w", dhcp.ErrNoSLAACPrefix), proto.Mode6SLAAC); got != v6SLAACNoPrefix {
		t.Errorf("a refused-prefix cause in slaac classified as %v, want v6SLAACNoPrefix", got)
	}
}

// The new ending has its own counter and its own tolerance, and both
// directions are asserted in one place.
//
// A verdict that was counted on an existing counter would be invisible
// on /metrics -- an operator would read dhcpv6_no_server and go looking
// for a DHCPv6 server on a network that never speaks to one. A verdict
// that TOLERATED the endpoint would be worse: `ipv6_mode=slaac` says
// the advertisement is where this network's addresses come from, so an
// endpoint with none has nothing left, and starting it hides that in a
// container that simply has no IPv6.
func TestNoteV6Absence_AFormingModeWithNoAddressIsItsOwnEnding(t *testing.T) {
	p := &Plugin{}
	tolerated := p.noteV6Absence(dhcp.RAObservation{Seen: true, Managed: true},
		"eth0", "abcdef0123456789", errors.New("timed out"), proto.Mode6SLAAC)

	if tolerated {
		t.Error("an ipv6_mode=slaac endpoint with a router heard and no address was started " +
			"anyway; the one mechanism this network is configured for produced nothing")
	}
	if got := p.dhcpv6SLAACNoAddress.Load(); got != 1 {
		t.Errorf("dhcpv6_slaac_no_address = %d, want 1", got)
	}
	for _, other := range []struct {
		name string
		got  int32
	}{
		{"dhcpv6_no_server", p.dhcpv6NoServer.Load()},
		{"dhcpv6_not_offered", p.dhcpv6NotOffered.Load()},
		{"dhcpv6_no_router_advert", p.dhcpv6NoRouterAdvert.Load()},
		{"dhcpv6_slaac_no_prefix", p.dhcpv6SLAACNoPrefix.Load()},
		{"dhcpv6_refused", p.dhcpv6Refused.Load()},
	} {
		if other.got != 0 {
			t.Errorf("%s = %d for an ending that is none of them", other.name, other.got)
		}
	}

	// The preservation control: the same observation on a `dhcp`
	// network is still the pre-#818 answer, counter included.
	q := &Plugin{}
	if q.noteV6Absence(dhcp.RAObservation{Seen: true, Managed: true}, "eth0", "abcdef0123456789",
		errors.New("timed out"), proto.Mode6DHCP) {
		t.Error("a managed segment that went silent became tolerated on a dhcp network")
	}
	if got := q.dhcpv6NoServer.Load(); got != 1 {
		t.Errorf("dhcpv6_no_server = %d on a dhcp network, want 1", got)
	}
	if got := q.dhcpv6SLAACNoAddress.Load(); got != 0 {
		t.Errorf("dhcpv6_slaac_no_address = %d on a dhcp network, want 0", got)
	}
}
