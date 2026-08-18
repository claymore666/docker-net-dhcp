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

// The parent name these tests probe against. Deliberately not a name
// any host has: every case here ends in "parent not found", because the
// gate is taken before the parent is looked up and that is the whole
// property under test. A real NIC name would make the outcome depend on
// the machine.
const probeGateParent = "dh-577-nosuch"

// TestRunDHCPProbe_TakesTheGateForItsParent is the runtime half of #577.
//
// Before #577 the gate was taken by CreateNetwork and handed in as a
// *parentGuard. Deleting the lock from the caller would then have left
// runDHCPProbe compiling perfectly and running ungated, because a guard
// is only a parameter. Now the probe takes it itself, and the counters
// are what shows it: parent_link_wait_timeouts is written by lockParent
// and by nothing else, so it moving is proof the probe went through the
// gate for the parent it was asked about.
//
// Constructed so the wait cannot succeed — the gate is already held and
// the context is already cancelled — because that is the branch that
// leaves a mark. An uncontended take is silent by design
// (parentGateContendedFloor), which is exactly right for production and
// useless as evidence.
func TestRunDHCPProbe_TakesTheGateForItsParent(t *testing.T) {
	p := &Plugin{}

	holder := p.lockParent(context.Background(), probeGateParent, "test-holder")
	defer holder.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Errors: the parent does not exist. What matters is the counter.
	_ = p.runDHCPProbe(ctx, probeGateParent, ModeMacvlan)

	if got := p.parentLinkWaitTimeouts.Load(); got != 1 {
		t.Fatalf("parent_link_wait_timeouts = %d, want 1 — the probe did not wait on the "+
			"gate for %q. Only lockParent writes this counter, so it not moving means "+
			"the probe reached the parent without going through the gate at all.",
			got, probeGateParent)
	}
}

// TestRunDHCPProbe_ReleasesTheGateOnTheErrorPath is the other half: a
// gate that is taken and never given back is worse than one never taken,
// because the next operation on that parent then eats the full
// parentGateBudget and proceeds anyway.
//
// The error path is the one worth pinning. `defer guard.Unlock()` covers
// every return, and the probe has eight of them; a future edit that
// takes the gate somewhere less structural (inside the success branch,
// say) would still pass a happy-path test.
func TestRunDHCPProbe_ReleasesTheGateOnTheErrorPath(t *testing.T) {
	p := &Plugin{}

	if err := p.runDHCPProbe(context.Background(), probeGateParent, ModeMacvlan); err == nil {
		t.Fatalf("probe against %q succeeded; this test needs it to fail so it is "+
			"exercising the error path", probeGateParent)
	}

	// Budget 0 exercises acquire's non-blocking fast path: this either
	// takes the gate immediately or reports it still held.
	release, ok := p.parentGate.acquire(context.Background(), probeGateParent, 0)
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

// TestRunDHCPProbe_UnlocksAfterTheProbeLinkIsRemoved pins the one thing
// in runDHCPProbe that is easy to get wrong and impossible to see in a
// diff: the order the two defers are REGISTERED in.
//
// Deferred calls run last-in first-out. `defer guard.Unlock()` is
// registered first, so it runs last — after the deferred LinkDel. That
// is what makes the parent stay occupied until the probe's child link is
// actually gone rather than until the lease arrives. Swap the two
// registrations and the gate opens with the child still attached, which
// is precisely the EBUSY the gate was added for (#571, #549) and which
// nothing else here would catch: the code still compiles, both defers
// still run, every other test still passes, and the failure only appears
// as an unrelated container's `docker run` being refused on a busy host.
//
// # Why this is a source check and not a runtime one
//
// Observing the real order would mean getting past addChildLink, which
// calls netlink.LinkAdd and needs CAP_NET_ADMIN. That call deliberately
// has no test seam — it is the funnel every parent-attached link goes
// through, and scripts/check-parent-gate-accounting.sh counts the
// literal netlink.LinkAdd sites, so introducing one would be a change to
// that gate rather than to this function. So this asserts the structure
// instead, and says so rather than implying more than it checks.
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
