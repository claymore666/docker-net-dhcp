// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// TestClassifyV6Absence covers all four RAObservation values, which is
// every input the type has: two booleans, one of them (Managed) only
// meaningful when the other is set.
//
// The fourth row is the one worth writing down. {Seen:false,
// Managed:true} cannot arise from the acquisition path — nothing sets
// Managed without having seen an advertisement — but a classifier that
// tested Managed FIRST would read it as fatal, and that same reordering
// silently turns every stateless segment fatal too. Pinning the
// impossible row states which of the two fields decides.
func TestClassifyV6Absence(t *testing.T) {
	cases := []struct {
		name string
		ra   dhcp.RAObservation
		want v6Verdict
	}{
		{"managed segment", dhcp.RAObservation{Seen: true, Managed: true}, v6Fatal},
		{"stateless or slaac", dhcp.RAObservation{Seen: true}, v6NotOffered},
		{"no router advertised", dhcp.RAObservation{}, v6NoRouter},
		{"managed without an advertisement", dhcp.RAObservation{Managed: true}, v6NoRouter},
	}

	// NON-VACUITY, keyed on the input domain rather than on a row
	// count. "All four RAObservation values" is what the comment above
	// claims, and a table is a universal that a deleted row satisfies
	// silently -- nothing else in the package, and not
	// check-test-weakening.sh, reports a row that stopped being there.
	// Two booleans have exactly four inhabitants, so the domain can be
	// stated rather than counted.
	covered := map[dhcp.RAObservation]bool{}
	for _, tc := range cases {
		covered[tc.ra] = true
	}
	for _, seen := range []bool{false, true} {
		for _, managed := range []bool{false, true} {
			ra := dhcp.RAObservation{Seen: seen, Managed: managed}
			if !covered[ra] {
				t.Fatalf("no row for %+v. This table is the whole statement of which "+
					"field decides the verdict, and every RAObservation value has to "+
					"be in it -- a missing row is a case nothing judges", ra)
			}
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyV6Absence(tc.ra); got != tc.want {
				t.Errorf("classifyV6Absence(%+v) = %v, want %v", tc.ra, got, tc.want)
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
			got := p.noteV6Absence(tc.ra, "eth0", "abcdef0123456789", errors.New("timed out"))
			if got != tc.wantTolerated {
				t.Errorf("noteV6Absence(%+v) = %v, want %v", tc.ra, got, tc.wantTolerated)
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
	p.noteV6Absence(dhcp.RAObservation{Seen: true}, "eth0", "aaaa", nil)
	p.noteV6Absence(dhcp.RAObservation{}, "eth0", "bbbb", nil)

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
	p.noteV6Absence(dhcp.RAObservation{}, "eth7", "0123456789abcdef", cause)

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
	p.noteV6Absence(dhcp.RAObservation{Seen: true}, "eth7", "0123456789abcdef", cause)
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
