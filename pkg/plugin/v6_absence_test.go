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

		{"stateless reply, quiet observation", dhcp.RAObservation{}, dhcp.ErrNoV6Address, v6NotOffered},
		{"stateless reply, managed observation", dhcp.RAObservation{Seen: true, Managed: true}, dhcp.ErrNoV6Address, v6NotOffered},
		{"stateless reply, wrapped cause", dhcp.RAObservation{Seen: true, Managed: true},
			fmt.Errorf("failed to get initial IPv6 address: %w", dhcp.ErrNoV6Address), v6NotOffered},
	}

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

// proto.Mode6SLAAC sends no Solicit whatever the M flag says, so a seen router with no address ends a forming mode
// (#818).
func TestClassifyV6Absence_TheModeDecidesWhatASeenRouterMeans(t *testing.T) {
	timeout := errors.New("timed out")
	modes := []proto.Mode6{proto.Mode6DHCP, proto.Mode6SLAAC, proto.Mode6Auto, proto.Mode6Off}

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

	if got := classifyV6Absence(dhcp.RAObservation{Seen: true},
		fmt.Errorf("wrapped: %w", dhcp.ErrNoSLAACPrefix), proto.Mode6SLAAC); got != v6SLAACNoPrefix {
		t.Errorf("a refused-prefix cause in slaac classified as %v, want v6SLAACNoPrefix", got)
	}
}

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

func TestAllV6Verdicts_IsEveryDeclaredVerdict(t *testing.T) {
	all := allV6Verdicts()
	if len(all) != int(v6VerdictCount) {
		t.Fatalf("allV6Verdicts has %d entries and %d verdicts are declared; a verdict missing "+
			"from the slice is one no table test ever judges", len(all), int(v6VerdictCount))
	}
	seen := map[v6Verdict]bool{}
	for _, v := range all {
		if v >= v6VerdictCount {
			t.Errorf("allV6Verdicts contains %d, which is past the end of the enumeration", int(v))
		}
		if seen[v] {
			t.Errorf("allV6Verdicts lists verdict %d twice, so the count above can be right "+
				"while a verdict is still missing", int(v))
		}
		seen[v] = true
	}
}

func TestV6AbsenceTolerated_OnlyTheNoRouterRowReadsTheMode(t *testing.T) {
	want := map[v6Verdict]map[proto.Mode6]bool{
		v6Fatal:          {proto.Mode6DHCP: false, proto.Mode6SLAAC: false, proto.Mode6Auto: false, proto.Mode6Off: false},
		v6NotOffered:     {proto.Mode6DHCP: true, proto.Mode6SLAAC: true, proto.Mode6Auto: true, proto.Mode6Off: true},
		v6NoRouter:       {proto.Mode6DHCP: true, proto.Mode6SLAAC: false, proto.Mode6Auto: false, proto.Mode6Off: true},
		v6Refused:        {proto.Mode6DHCP: false, proto.Mode6SLAAC: false, proto.Mode6Auto: false, proto.Mode6Off: false},
		v6SLAACNoPrefix:  {proto.Mode6DHCP: false, proto.Mode6SLAAC: false, proto.Mode6Auto: false, proto.Mode6Off: false},
		v6SLAACNoAddress: {proto.Mode6DHCP: false, proto.Mode6SLAAC: false, proto.Mode6Auto: false, proto.Mode6Off: false},
	}

	for _, v := range allV6Verdicts() {
		row, ok := want[v]
		if !ok {
			t.Fatalf("verdict %d has no row here, so whether an endpoint it ends starts or "+
				"fails is decided by the predicate's default arm and asserted nowhere", int(v))
		}
		for _, mode := range proto.AllModes6() {
			got, ok := row[mode]
			if !ok {
				t.Fatalf("verdict %d has no cell for ipv6_mode=%s", int(v), mode)
			}
			if v6AbsenceTolerated(v, mode) != got {
				t.Errorf("v6AbsenceTolerated(%d, %s) = %v, want %v",
					int(v), mode, !got, got)
			}
		}
	}
}

func TestNoteV6Absence_ASegmentWithNoRouterEndsAFormingEndpoint(t *testing.T) {
	cases := []struct {
		mode      proto.Mode6
		tolerated bool
	}{
		{proto.Mode6DHCP, true},
		{proto.Mode6Off, true},
		{proto.Mode6SLAAC, false},
		{proto.Mode6Auto, false},
	}
	for _, c := range cases {
		t.Run(c.mode.String(), func(t *testing.T) {
			p := &Plugin{}
			got := p.noteV6Absence(dhcp.RAObservation{}, "eth0", "abcdef0123456789",
				errors.New("timed out"), c.mode)
			if got != c.tolerated {
				if c.tolerated {
					t.Errorf("an ipv6_mode=%s endpoint was refused for a segment with no router "+
						"advertisement; that mode takes its address from DHCPv6 or from nothing, "+
						"and refusing the container buys nothing (#868)", c.mode)
				} else {
					t.Errorf("an ipv6_mode=%s endpoint started on a segment with no router "+
						"advertisement; RFC 4862 section 5.5.3 forms an address from the Prefix "+
						"Information option and there was no advertisement to carry one, so the "+
						"container has no IPv6 and nothing said so (#818)", c.mode)
				}
			}
			if n := p.dhcpv6NoRouterAdvert.Load(); n != 1 {
				t.Errorf("dhcpv6_no_router_advert = %d in ipv6_mode=%s, want 1: the counter's "+
					"population is every endpoint that saw no advertisement, in every mode", n, c.mode)
			}
			for _, other := range []struct {
				name string
				got  int32
			}{
				{"dhcpv6_no_server", p.dhcpv6NoServer.Load()},
				{"dhcpv6_not_offered", p.dhcpv6NotOffered.Load()},
				{"dhcpv6_slaac_no_address", p.dhcpv6SLAACNoAddress.Load()},
				{"dhcpv6_slaac_no_prefix", p.dhcpv6SLAACNoPrefix.Load()},
				{"dhcpv6_refused", p.dhcpv6Refused.Load()},
			} {
				if other.got != 0 {
					t.Errorf("%s = %d for a segment with no router advertisement", other.name, other.got)
				}
			}
		})
	}
}
