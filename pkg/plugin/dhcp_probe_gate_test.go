// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

const probeGateParent = "dh-577-nosuch"

func TestRunDHCPProbe_TakesTheGateForItsParent(t *testing.T) {
	p := &Plugin{}

	holder := p.lockParent(context.Background(), probeGateParent, ModeIPvlan, "test-holder")
	defer holder.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = p.runDHCPProbe(ctx, DHCPNetworkOptions{Mode: ModeMacvlan, Parent: probeGateParent}, serverPolicy{})

	if got := p.parentLinkWaitTimeouts.Load(); got != 1 {
		t.Fatalf("parent_link_wait_timeouts = %d, want 1 — the probe did not wait on the "+
			"gate for %q. Only lockParent writes this counter, so it not moving means "+
			"the probe reached the parent without going through the gate at all.",
			got, probeGateParent)
	}
}

func TestRunDHCPProbe_ReleasesTheGateOnTheErrorPath(t *testing.T) {
	p := &Plugin{}

	if err := p.runDHCPProbe(context.Background(), DHCPNetworkOptions{Mode: ModeMacvlan, Parent: probeGateParent}, serverPolicy{}); err == nil {
		t.Fatalf("probe against %q succeeded; this test needs it to fail so it is "+
			"exercising the error path", probeGateParent)
	}

	release, ok, _ := p.parentGate.acquire(context.Background(), probeGateParent, ModeMacvlan, 0)
	defer release()
	if !ok {
		t.Fatalf("the gate for %q is still held after runDHCPProbe returned an error — "+
			"the probe leaked it, and the next operation on this parent will now wait "+
			"out the full %v before proceeding anyway", probeGateParent, parentGateBudget)
	}

	if got := p.parentLinkWaitTimeouts.Load(); got != 0 {
		t.Fatalf("parent_link_wait_timeouts = %d, want 0 — nothing contended here", got)
	}
	if got := p.parentLinkWaits.Load(); got != 0 {
		t.Fatalf("parent_link_waits = %d, want 0 — an uncontended take must stay silent "+
			"on the counters, or every endpoint creation on an idle host reports a wait", got)
	}
}

// Observing the order needs addChildLink past CAP_NET_ADMIN, so it is checked in the source (#571).
func TestRunDHCPProbe_UnlocksAfterTheProbeLinkIsRemoved(t *testing.T) {
	const file = "dhcp_probe.go"

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var fn *ast.FuncDecl
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if ok && fd.Name.Name == "runDHCPProbe" && fd.Recv != nil {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatalf("no method runDHCPProbe found in %s. If it was renamed or turned back "+
			"into a free function, this guard cannot see it and would otherwise pass "+
			"having checked nothing", file)
	}

	unlockAt, linkDelAt := -1, -1
	for i, stmt := range fn.Body.List {
		d, ok := stmt.(*ast.DeferStmt)
		if !ok {
			continue
		}
		var buf strings.Builder
		ast.Inspect(d, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				buf.WriteString(id.Name)
				buf.WriteByte(' ')
			}
			return true
		})
		body := buf.String()
		if unlockAt < 0 && strings.Contains(body, "Unlock") {
			unlockAt = i
		}
		if linkDelAt < 0 && strings.Contains(body, "LinkDel") {
			linkDelAt = i
		}
	}

	if unlockAt < 0 {
		t.Fatal("runDHCPProbe defers no Unlock. It takes the parent gate; if it no longer " +
			"releases it by defer, every early return leaks the gate for that parent.")
	}
	if linkDelAt < 0 {
		t.Fatal("runDHCPProbe defers no LinkDel. The probe link must be torn down " +
			"unconditionally, or a failed probe leaves a child on the parent forever.")
	}
	if unlockAt > linkDelAt {
		t.Fatalf("defer Unlock is registered at statement %d, after the deferred LinkDel at "+
			"%d. Defers run last-in first-out, so this releases the parent gate BEFORE "+
			"the probe link is removed — leaving a child attached to a parent that now "+
			"reads as free. Register Unlock first, immediately after lockParent.",
			unlockAt, linkDelAt)
	}
}
