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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

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
	if n := len(hook.AllEntries()); n != 0 {
		t.Errorf("five empty reports wrote %d log line(s); the first is %q",
			n, hook.LastEntry().Message)
	}
}

// The warning text is pinned because TestFailure_UnansweredRenewalsCounted greps renewalWarnMarker on the privileged
// lane (#940).
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

func TestRenewalWiring_OnlyThePersistentClientReports(t *testing.T) {
	p := &Plugin{}

	var persistent dhcp.DHCPClientOptions
	p.renewalWiring(&persistent, "net1", "ep1", false)
	if persistent.OnRenewalStats == nil {
		t.Error("the persistent client has no renewal reporter, so nothing folds its counters and " +
			"#940 is unfixed on the only path that renews")
	}

	var noPlugin dhcp.DHCPClientOptions
	var nilPlugin *Plugin
	nilPlugin.renewalWiring(&noPlugin, "net1", "ep1", false)
	if noPlugin.OnRenewalStats != nil {
		t.Error("a client built with no plugin behind it was given a reporter with nowhere to report")
	}
}

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
