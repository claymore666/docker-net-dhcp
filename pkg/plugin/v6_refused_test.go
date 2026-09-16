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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// The three endings #816 added to the classifier, against the
// observations that would decide differently on their own.
//
// THE POINT OF EACH ROW IS THE DISAGREEMENT. A refusal arrives on a
// segment whose advertisement says managed, which the observation alone
// reads as v6Fatal -- the right ANSWER for the endpoint and the wrong
// DIAGNOSIS for the operator, because the two faults are a server with
// an empty pool and no server at all. `slaac_no_prefix` arrives on a
// segment that advertised without the managed flag, which the
// observation alone reads as "no DHCPv6 here, carry on" -- tolerated,
// so the container would start with no IPv6 address at all on a network
// whose whole configuration is that the address comes from the router.
func TestClassifyV6Absence_TheWireCausesOverruleTheObservation(t *testing.T) {
	refused := dhcp.V6Refusal("NoAddrsAvail", "no addresses available")
	noPrefix := fmt.Errorf("%w: 2 option(s) refused", dhcp.ErrNoSLAACPrefix)

	cases := []struct {
		name  string
		ra    dhcp.RAObservation
		cause error
		want  v6Verdict
		// alone is what the observation gives with an ordinary
		// timeout, and it is asserted as well: a row whose cause
		// changes nothing proves nothing about the cause.
		alone v6Verdict
	}{
		{"refused on a managed segment", dhcp.RAObservation{Seen: true, Managed: true}, refused, v6Refused, v6Fatal},
		{"refused, wrapped by the caller", dhcp.RAObservation{Seen: true, Managed: true},
			fmt.Errorf("failed to get initial IPv6 address: %w", refused), v6Refused, v6Fatal},
		// The deadline arm of the acquisition wraps BOTH: the cause it
		// already had, and the budget that then ran out. If it
		// overwrote the first -- which it did until #816 -- the
		// refusal would be unreachable in the field while every
		// synthetic row above stayed green.
		{"refused, then the budget ran out", dhcp.RAObservation{Seen: true, Managed: true},
			fmt.Errorf("%w; the DHCPv6 acquisition budget then ran out: %w", refused, errors.New("context deadline exceeded")),
			v6Refused, v6Fatal},
		{"refused on a segment with no advertisement", dhcp.RAObservation{}, refused, v6Refused, v6NoRouter},
		{"no usable prefix, advertisement without the managed flag", dhcp.RAObservation{Seen: true}, noPrefix, v6SLAACNoPrefix, v6NotOffered},
		{"no usable prefix, stateless advertisement", dhcp.RAObservation{Seen: true, Other: true}, noPrefix, v6SLAACNoPrefix, v6NotOffered},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyV6Absence(tc.ra, tc.cause); got != tc.want {
				t.Errorf("classifyV6Absence(%+v, %v) = %v, want %v", tc.ra, tc.cause, got, tc.want)
			}
			if got := classifyV6Absence(tc.ra, errors.New("timed out")); got != tc.alone {
				t.Fatalf("the same observation under an ordinary timeout = %v, want %v. "+
					"This row is here to show the cause CHANGING the verdict, and if the "+
					"observation already gave the same answer it shows nothing.", got, tc.alone)
			}
		})
	}

	// The preservation control for the whole widening: the endings that
	// were tolerated before #816 are still tolerated. A refusal arm
	// written as "any failure with a cause is fatal" passes every row
	// above and fails here.
	if got := classifyV6Absence(dhcp.RAObservation{Seen: true, Other: true}, dhcp.ErrNoV6Address); got != v6NotOffered {
		t.Errorf("a stateless segment now classifies as %v; #868's tolerance is what the "+
			"new verdicts must not eat", got)
	}
	if got := classifyV6Absence(dhcp.RAObservation{}, fmt.Errorf("%w: 3 solicitations", dhcp.ErrNoV6Router)); got != v6NoRouter {
		t.Errorf("a segment with no router classifies as %v, want v6NoRouter — the "+
			"answerless endings keep the verdict the observation gives them", got)
	}
}

// Every DHCPv6 ending that fails an endpoint moves ITS OWN counter and
// no other, and every one of them still fails the endpoint.
//
// ONE TABLE FOR ALL THREE, on the rule TestNoteV6Absence_TolerancePolarity
// states: a change that flipped one of them would otherwise read as a
// change to only its own case. The counters are the whole of #816 --
// before it, two of these three ends moved nothing at all and the third
// did not exist -- so "each moved by one and the others by zero" is the
// property, not "some counter moved".
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

	// NON-VACUITY, keyed on the outcomes rather than on a row count:
	// duplicating one row over another satisfies a count and empties
	// the claim.
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
			if p.noteV6Absence(tc.ra, "eth0", "abcdef0123456789", tc.cause) {
				t.Error("the endpoint was tolerated. All three of these endings leave the " +
					"container with no IPv6 address on a network configured to have one, " +
					"and the caller turns a false into the error Docker shows.")
			}
			got := counts{p.dhcpv6Refused.Load(), p.dhcpv6NoServer.Load(), p.dhcpv6SLAACNoPrefix.Load()}
			if got != tc.want {
				t.Errorf("counters = %+v, want %+v", got, tc.want)
			}
			// The two tolerated counters are the preservation control:
			// a failure that also moved one of them would make a
			// dashboard read a fault as a healthy stateless segment.
			if n := p.dhcpv6NotOffered.Load(); n != 0 {
				t.Errorf("dhcpv6_not_offered = %d on a failing endpoint, want 0", n)
			}
			if n := p.dhcpv6NoRouterAdvert.Load(); n != 0 {
				t.Errorf("dhcpv6_no_router_advert = %d on a failing endpoint, want 0", n)
			}
		})
	}
}

// The refusal's log line names the code, which is the whole of what an
// operator can act on: NoAddrsAvail is an exhausted pool, NotOnLink is
// an address asked for outside the range the server serves.
func TestNoteV6Absence_TheRefusalNamesTheStatusCode(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	p := &Plugin{}
	p.noteV6Absence(dhcp.RAObservation{Seen: true, Managed: true}, "eth7", "0123456789abcdef",
		fmt.Errorf("acquisition failed: %w", dhcp.V6Refusal("NoAddrsAvail", "no addresses available")))

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

// The auto fallback counts what FORMED and says so, and the counter is
// a gain rather than a total.
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

	// A zero gain is not an event. The chassis calls the reporter with
	// whatever delta it computed, and a line per no-op would bury the
	// real ones.
	hook.Reset()
	report(0)
	if n := len(hook.AllEntries()); n != 0 {
		t.Errorf("a zero gain logged %d line(s), want none", n)
	}
	if got := p.dhcpv6AutoFallbacks.Load(); got != 3 {
		t.Errorf("dhcpv6_auto_fallbacks = %d after a zero gain, want 3", got)
	}
}
