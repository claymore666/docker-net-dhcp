// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// A stateless segment advertises O=1 M=0 and its dnsmasq answers a Solicit with Status
// Code 2 NoAddrsAvail; that refusal agrees with the advertisement and is not a fault
// (#821, run 35141032546). The same code under M=1 stays fatal (#816).
func TestClassifyV6Absence_ARefusalThatAgreesWithTheAdvertisementIsNotAFault(t *testing.T) {
	refused := dhcp.V6Refusal("NoAddrsAvail", "no addresses available")

	cases := []struct {
		name string
		ra   dhcp.RAObservation
		want v6Verdict
		why  string
	}{
		{"stateless: O=1, M=0", dhcp.RAObservation{Seen: true, Other: true}, v6NotOffered,
			"the segment said it hands out no addresses, and then the server said the same"},
		{"neither bit set", dhcp.RAObservation{Seen: true}, v6NotOffered,
			"an advertisement promising no addresses, confirmed on the wire"},
		{"managed: M=1", dhcp.RAObservation{Seen: true, Managed: true}, v6Refused,
			"the segment promised addresses over DHCPv6 and the server had none: still a fault"},
		{"no advertisement seen at all", dhcp.RAObservation{}, v6Refused,
			"nothing on the wire says otherwise, so the refusal is the only evidence there is"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyV6Absence(tc.ra, refused, proto.Mode6DHCP); got != tc.want {
				t.Errorf("classifyV6Absence(%+v, NoAddrsAvail) = %v, want %v -- %s",
					tc.ra, got, tc.want, tc.why)
			}
		})
	}
}

func TestNoteV6Absence_AStatelessRefusalStillCreatesTheEndpoint(t *testing.T) {
	p := newTestPlugin(t)
	refused := dhcp.V6Refusal("NoAddrsAvail", "no addresses available")
	ra := dhcp.RAObservation{Seen: true, Other: true}

	if !p.noteV6Absence(ra, "eth0", "ep-stateless", refused, proto.Mode6DHCP) {
		t.Error("the endpoint was refused on a stateless segment whose advertisement " +
			"says M=0. The server answering \"no addresses available\" there is the " +
			"segment agreeing with itself, and failing the endpoint takes the " +
			"container's IPv4 down with it (#821)")
	}
	if n := p.dhcpv6NotOffered.Load(); n != 1 {
		t.Errorf("dhcpv6_not_offered = %d, want 1: this ending has to be visible on /metrics", n)
	}
	if n := p.dhcpv6Refused.Load(); n != 0 {
		t.Errorf("dhcpv6_refused = %d, want 0: it was not a refusal", n)
	}
}

func TestClassifyV6Absence_TheWireCausesOverruleTheObservation(t *testing.T) {
	refused := dhcp.V6Refusal("NoAddrsAvail", "no addresses available")
	noPrefix := fmt.Errorf("%w: 2 option(s) refused", dhcp.ErrNoSLAACPrefix)

	cases := []struct {
		name  string
		ra    dhcp.RAObservation
		cause error
		want  v6Verdict
		alone v6Verdict
	}{
		{"refused on a managed segment", dhcp.RAObservation{Seen: true, Managed: true}, refused, v6Refused, v6Fatal},
		{"refused, wrapped by the caller", dhcp.RAObservation{Seen: true, Managed: true},
			fmt.Errorf("failed to get initial IPv6 address: %w", refused), v6Refused, v6Fatal},
		{"refused, then the budget ran out", dhcp.RAObservation{Seen: true, Managed: true},
			fmt.Errorf("%w; the DHCPv6 acquisition budget then ran out: %w", refused, errors.New("context deadline exceeded")),
			v6Refused, v6Fatal},
		{"refused on a segment with no advertisement", dhcp.RAObservation{}, refused, v6Refused, v6NoRouter},
		{"no usable prefix, advertisement without the managed flag", dhcp.RAObservation{Seen: true}, noPrefix, v6SLAACNoPrefix, v6NotOffered},
		{"no usable prefix, stateless advertisement", dhcp.RAObservation{Seen: true, Other: true}, noPrefix, v6SLAACNoPrefix, v6NotOffered},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyV6Absence(tc.ra, tc.cause, proto.Mode6DHCP); got != tc.want {
				t.Errorf("classifyV6Absence(%+v, %v, proto.Mode6DHCP) = %v, want %v", tc.ra, tc.cause, got, tc.want)
			}
			if got := classifyV6Absence(tc.ra, errors.New("timed out"), proto.Mode6DHCP); got != tc.alone {
				t.Fatalf("the same observation under an ordinary timeout = %v, want %v. "+
					"This row is here to show the cause CHANGING the verdict, and if the "+
					"observation already gave the same answer it shows nothing.", got, tc.alone)
			}
		})
	}

	if got := classifyV6Absence(dhcp.RAObservation{Seen: true, Other: true}, dhcp.ErrNoV6Address, proto.Mode6DHCP); got != v6NotOffered {
		t.Errorf("a stateless segment now classifies as %v; #868's tolerance is what the "+
			"new verdicts must not eat", got)
	}
	if got := classifyV6Absence(dhcp.RAObservation{}, fmt.Errorf("%w: 3 solicitations", dhcp.ErrNoV6Router), proto.Mode6DHCP); got != v6NoRouter {
		t.Errorf("a segment with no router classifies as %v, want v6NoRouter — the "+
			"answerless endings keep the verdict the observation gives them", got)
	}
}

func TestNoteV6Absence_EachFailureIsItsOwnRow(t *testing.T) {
	type counts struct{ refused, noServer, noPrefix int32 }
	cases := []struct {
		name  string
		ra    dhcp.RAObservation
		cause error
		want  counts
	}{
		{"a server that refused", dhcp.RAObservation{Seen: true, Managed: true},
			dhcp.V6Refusal("NoAddrsAvail", "no addresses available"), counts{refused: 1}},
		{"a server that said nothing", dhcp.RAObservation{Seen: true, Managed: true},
			errors.New("timed out"), counts{noServer: 1}},
		{"a router with no usable prefix", dhcp.RAObservation{Seen: true},
			fmt.Errorf("%w: 2 option(s) refused", dhcp.ErrNoSLAACPrefix), counts{noPrefix: 1}},
	}

	required := map[counts]string{
		{refused: 1}:  "a refusal moves dhcpv6_refused and nothing else",
		{noServer: 1}: "a silent server moves dhcpv6_no_server and nothing else",
		{noPrefix: 1}: "a router with no usable prefix moves dhcpv6_slaac_no_prefix and nothing else",
	}
	for _, tc := range cases {
		delete(required, tc.want)
	}
	for _, missing := range required {
		t.Fatalf("this table no longer states that %s, which is what #816 asked for", missing)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			if p.noteV6Absence(tc.ra, "eth0", "abcdef0123456789", tc.cause, proto.Mode6DHCP) {
				t.Error("the endpoint was tolerated. All three of these endings leave the " +
					"container with no IPv6 address on a network configured to have one, " +
					"and the caller turns a false into the error Docker shows.")
			}
			got := counts{p.dhcpv6Refused.Load(), p.dhcpv6NoServer.Load(), p.dhcpv6SLAACNoPrefix.Load()}
			if got != tc.want {
				t.Errorf("counters = %+v, want %+v", got, tc.want)
			}
			if n := p.dhcpv6NotOffered.Load(); n != 0 {
				t.Errorf("dhcpv6_not_offered = %d on a failing endpoint, want 0", n)
			}
			if n := p.dhcpv6NoRouterAdvert.Load(); n != 0 {
				t.Errorf("dhcpv6_no_router_advert = %d on a failing endpoint, want 0", n)
			}
		})
	}
}

// NoAddrsAvail is an exhausted pool, NotOnLink an address outside the served range (RFC 8415 section 21.13).
func TestNoteV6Absence_TheRefusalNamesTheStatusCode(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	p := &Plugin{}
	p.noteV6Absence(dhcp.RAObservation{Seen: true, Managed: true}, "eth7", "0123456789abcdef",
		fmt.Errorf("acquisition failed: %w", dhcp.V6Refusal("NoAddrsAvail", "no addresses available")),
		proto.Mode6DHCP)

	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("want exactly one log entry, got %d: %v", len(entries), messagesOf(entries))
	}
	e := entries[0]
	if e.Level != log.ErrorLevel {
		t.Errorf("a refusal logged at %v; the endpoint fails, so this is an error", e.Level)
	}
	if e.Data["status_code"] != "NoAddrsAvail" {
		t.Errorf("status_code field = %v, want NoAddrsAvail. The code is the only thing "+
			"that tells the operator what the server objected to. Fields: %v",
			e.Data["status_code"], e.Data)
	}
	if e.Data["iface"] != "eth7" {
		t.Errorf("iface field = %v, want eth7", e.Data["iface"])
	}
}

func TestV6FallbackReporter_CountsTheGainAndWarns(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	p := &Plugin{}
	report := p.v6FallbackReporter("0123456789abcdef")
	report(1)
	report(2)

	if got := p.dhcpv6AutoFallbacks.Load(); got != 3 {
		t.Errorf("dhcpv6_auto_fallbacks = %d after gains of 1 and 2, want 3 — the "+
			"callback is handed the GAIN, so a reporter that stored the value would "+
			"lose every fallback after the first", got)
	}
	entries := hook.AllEntries()
	if len(entries) != 2 {
		t.Fatalf("want one log line per reported fallback, got %d: %v",
			len(entries), messagesOf(entries))
	}
	for _, e := range entries {
		if e.Level != log.WarnLevel {
			t.Errorf("the fallback logged at %v, want warn: nothing is broken and the "+
				"address came from somewhere other than the network's nominal source", e.Level)
		}
		if !strings.Contains(e.Message, "ipv6_auto_strict") {
			t.Errorf("the fallback line does not name the setting that turns it off: %q", e.Message)
		}
	}

	hook.Reset()
	report(0)
	if n := len(hook.AllEntries()); n != 0 {
		t.Errorf("a zero gain logged %d line(s), want none", n)
	}
	if got := p.dhcpv6AutoFallbacks.Load(); got != 3 {
		t.Errorf("dhcpv6_auto_fallbacks = %d after a zero gain, want 3", got)
	}
}
