// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

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
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	report := p.renewalReporter("net1", "ep1", false)
	for i := 0; i < 5; i++ {
		report(dhcp.RenewalStats{})
	}
	if got := p.renewalsUnansweredV4.Load(); got != 0 {
		t.Errorf("five empty reports moved the counter to %d", got)
	}
	// AND NOTHING WAS LOGGED. The counter staying at zero is only half
	// of it: the fold runs every 15 seconds for the life of every
	// endpoint, so a reporter that logged an empty gain would put four
	// warnings a minute per container into the log, each one saying the
	// server did not answer a request that was never sent. An operator
	// who learns to filter this line out is an operator who will not
	// see the real one.
	if n := len(hook.AllEntries()); n != 0 {
		t.Errorf("five empty reports wrote %d log line(s); the first is %q",
			n, hook.LastEntry().Message)
	}
}

// TestRenewalReporter_LogsTheLineTheOperatorHasToFind. #940 is a
// production host on which the server stopped answering renewals for
// 7h52m with nothing in the log at any level. A counter serves the
// operator who is already looking at a dashboard; this line is what the
// operator reading logs has to be able to find, and it carries the
// endpoint, which the plugin-wide counter cannot.
//
// THE PHRASE IS PINNED HERE because something else greps for it:
// TestFailure_UnansweredRenewalsCounted matches renewalWarnMarker
// against the running plugin's log to tie a counter rise to its own
// endpoint. That test runs only on the privileged lane, so a reworded
// message would otherwise be discovered there, an hour later, as a
// missing line rather than as a rename.
func TestRenewalReporter_LogsTheLineTheOperatorHasToFind(t *testing.T) {
	p := &Plugin{}
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	p.renewalReporter("net1234567890", "ep1234567890", false)(dhcp.RenewalStats{Unanswered: 2})

	entry := hook.LastEntry()
	if entry == nil {
		t.Fatal("two unanswered renewal requests logged nothing at all")
	}
	if entry.Level != log.WarnLevel {
		t.Errorf("logged at %s; warn is the level: the container still holds a working address, "+
			"and this is recoverable by the server coming back", entry.Level)
	}
	const marker = "did not answer this endpoint's renewal request"
	if !strings.Contains(entry.Message, marker) {
		t.Errorf("logged %q, which does not carry %q -- the phrase the integration test greps "+
			"for (test/integration/renewal_unanswered_test.go, renewalWarnMarker)",
			entry.Message, marker)
	}
	if got := entry.Data["endpoint"]; got != shortID("ep1234567890") {
		t.Errorf("endpoint field is %v, want %v; the counter is plugin-wide and this field is "+
			"the only thing that says WHICH container stopped being answered",
			got, shortID("ep1234567890"))
	}
	if got := entry.Data["family"]; got != "ipv4" {
		t.Errorf("family field is %v, want ipv4", got)
	}
	if got := entry.Data["unanswered"]; got != uint64(2) {
		t.Errorf("unanswered field is %v, want 2", got)
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

// TestRenewalWiring_IsCalledOnceAndOnlyFromSetupClient is the CALL SITE,
// and it is a source-level test for the reason family_flag_wiring_test.go
// gives: there is no seam between setupClient and a raw socket in a real
// network namespace, so what is checkable here is the wiring, and the
// wiring is where this breaks.
//
// The two failures it exists for are opposite and both silent.
//
// Deleting the call leaves every behavioural test in this package green:
// the reporter is still correct, the counter is still exposed, the
// document still carries the field, and nothing on the renewing path
// ever calls any of it. #940 would be unfixed on the only path that
// renews, and the counter would read 0 forever -- which is exactly what
// it read while the production outage ran.
//
// Adding it to a roleAcquire site is the other one. Those are
// CreateEndpoint one-shots: they acquire an address and return, holding
// no lease to renew. A reporter there would be a second writer to this
// counter on a path whose retransmissions are an ACQUISITION's, and
// every rise would then have to be argued about instead of read.
func TestRenewalWiring_IsCalledOnceAndOnlyFromSetupClient(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	type site struct{ fn string }
	var sites []site
	scanned := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "renewalWiring" {
					return true
				}
				sites = append(sites, site{fn: fn.Name.Name})
				// The family must come from setupClient's own
				// parameter. A literal here would send every endpoint's
				// reports to one half of the pair, and the half that is
				// never written reads exactly like a family with no
				// outages.
				if len(call.Args) != 4 {
					t.Errorf("renewalWiring in %s takes %d argument(s); this test reads the last "+
						"one as the family and can no longer do so", fn.Name.Name, len(call.Args))
					return true
				}
				id, ok := call.Args[3].(*ast.Ident)
				if !ok || id.Name != "v6" {
					t.Errorf("renewalWiring in %s is passed %T as its family argument, not the "+
						"`v6` parameter; a constant there attributes every endpoint's unanswered "+
						"renewals to one family", fn.Name.Name, call.Args[3])
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatal("no production sources parsed; this test would pass vacuously")
	}
	if len(sites) != 1 {
		names := make([]string, 0, len(sites))
		for _, s := range sites {
			names = append(names, s.fn)
		}
		t.Fatalf("renewalWiring is called %d time(s), in %v; want exactly one, in setupClient. "+
			"None means #940 is unfixed on the renewing path and the counter reads 0 forever; "+
			"more than one means a CreateEndpoint one-shot, which holds no lease to renew, is a "+
			"second writer to a counter about renewals.", len(sites), names)
	}
	if sites[0].fn != "setupClient" {
		t.Errorf("renewalWiring is called from %s; the persistent client is built in setupClient "+
			"and it is the only client that holds a lease long enough to renew it", sites[0].fn)
	}
}
