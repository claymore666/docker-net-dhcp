// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// TestRenewalsUnanswered_TheOperatorCanREADIt drives the reporter and
// then reads what an operator reads: the FIELDS of /Plugin.Health and
// the series on /metrics.
//
// Reading the atomics back would assert that this test can add. What
// #940 is about is a number reaching the document an operator polls, so
// the assertion is on the document and on the exposition, through
// healthSnapshot and the renderer, which is the whole path between the
// counter and the person looking for the outage.
func TestRenewalsUnanswered_TheOperatorCanREADIt(t *testing.T) {
	p := &Plugin{}
	p.renewalReporter("net1", "ep1", false)(dhcp.RenewalStats{Unanswered: 2})
	p.renewalReporter("net1", "ep1", true)(dhcp.RenewalStats{Unanswered: 3})

	h := p.healthSnapshot()
	if h.RenewalsUnansweredV4 != 2 {
		t.Errorf("renewals_unanswered_v4 = %d, want 2", h.RenewalsUnansweredV4)
	}
	if h.RenewalsUnansweredV6 != 3 {
		t.Errorf("renewals_unanswered_v6 = %d, want 3", h.RenewalsUnansweredV6)
	}
	if h.RenewalsUnanswered != 5 {
		t.Errorf("renewals_unanswered = %d, want 5 — the un-suffixed field is the SUM of the two "+
			"halves and is not a counter of its own", h.RenewalsUnanswered)
	}

	var buf bytes.Buffer
	if err := writeExposition(&buf, h); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	body := buf.String()
	for _, want := range []string{
		`net_dhcp_renewals_unanswered_total{family="ipv4"} 2`,
		`net_dhcp_renewals_unanswered_total{family="ipv6"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the exposition does not carry %q. An operator alerting on a dashboard reads "+
				"this series and nothing else.", want)
		}
	}
}

// TestRenewalsUnanswered_TheFamilyIsNotCosmetic. A v6-only silence is
// invisible in the sum, which is the whole reason these counters are
// split (#212, #730), so each reporter must move its own half and only
// its own half.
func TestRenewalsUnanswered_TheFamilyIsNotCosmetic(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		family := "ipv4"
		if v6 {
			family = "ipv6"
		}
		t.Run(family, func(t *testing.T) {
			p := &Plugin{}
			p.renewalReporter("net1", "ep1", v6)(dhcp.RenewalStats{Unanswered: 1})

			wantV4, wantV6 := int32(1), int32(0)
			if v6 {
				wantV4, wantV6 = 0, 1
			}
			if got := p.renewalsUnansweredV4.Load(); got != wantV4 {
				t.Errorf("v4 half = %d, want %d", got, wantV4)
			}
			if got := p.renewalsUnansweredV6.Load(); got != wantV6 {
				t.Errorf("v6 half = %d, want %d", got, wantV6)
			}
		})
	}
}

// TestRenewalsUnanswered_MovesNoOtherCounter is the preservation
// control. This counter is NOT an early dhcp_timeouts and must not
// double-count one outage: an unanswered renewal request and an
// acquisition that ran out of retransmissions are different facts with
// different remedies, and the pair is only readable while each moves on
// its own evidence.
func TestRenewalsUnanswered_MovesNoOtherCounter(t *testing.T) {
	p := &Plugin{}
	p.renewalReporter("net1", "ep1", false)(dhcp.RenewalStats{Unanswered: 4})
	p.renewalReporter("net1", "ep1", true)(dhcp.RenewalStats{Unanswered: 4})

	h := p.healthSnapshot()
	if h.DHCPTimeouts != 0 {
		t.Errorf("dhcp_timeouts = %d after eight unanswered renewal requests; the lease has not "+
			"run out, and counting one outage in two counters makes the pair unreadable",
			h.DHCPTimeouts)
	}
	if h.LeasesRenewed != 0 || h.DHCPServerPolicyTimeouts != 0 || h.NAKsReceived != 0 {
		t.Errorf("an unanswered renewal moved leases_renewed=%d, dhcp_server_policy_timeouts=%d, "+
			"naks_received=%d; it is evidence for none of them",
			h.LeasesRenewed, h.DHCPServerPolicyTimeouts, h.NAKsReceived)
	}
	if !h.Healthy {
		t.Error("healthy went false on an unanswered renewal. The container still holds a working " +
			"address and the client is still asking; the five counters behind `healthy` are named " +
			"in the reference and this is not one of them.")
	}
}

// TestRenewalsUnanswered_AZeroGainIsNotAReport. The chassis reports a
// DELTA and a fold with nothing new to say produces zero. A counter
// that moved on such a report would climb on every tick of an idle
// client, which is the counter saying "outage" about silence it never
// observed.
func TestRenewalsUnanswered_AZeroGainIsNotAReport(t *testing.T) {
	p := &Plugin{}
	report := p.renewalReporter("net1", "ep1", false)
	for i := 0; i < 5; i++ {
		report(dhcp.RenewalStats{})
	}
	if got := p.renewalsUnansweredV4.Load(); got != 0 {
		t.Errorf("five empty reports moved the counter to %d", got)
	}
}

// TestRenewalWiring_OnlyThePersistentClientReports. The CreateEndpoint
// one-shot acquires and returns; it holds no lease to renew. Wiring it
// would add a second writer to a counter about renewals, on a path that
// has none, and every acquisition retransmission would then have to be
// argued about.
func TestRenewalWiring_OnlyThePersistentClientReports(t *testing.T) {
	p := &Plugin{}

	var persistent dhcp.DHCPClientOptions
	p.renewalWiring(&persistent, "net1", "ep1", false)
	if persistent.OnRenewalStats == nil {
		t.Error("the persistent client has no renewal reporter, so nothing folds its counters and " +
			"#940 is unfixed on the only path that renews")
	}

	// A manager with no plugin behind it is the unit-test shape, and
	// the client still has to run.
	var noPlugin dhcp.DHCPClientOptions
	var nilPlugin *Plugin
	nilPlugin.renewalWiring(&noPlugin, "net1", "ep1", false)
	if noPlugin.OnRenewalStats != nil {
		t.Error("a client built with no plugin behind it was given a reporter with nowhere to report")
	}
}
