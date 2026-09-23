// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Each counter's wrapper must be the only caller of what it wraps (#769). The check reads names and direct binds in
// one package's AST; a struct field, closure, other package or reflection is not seen, and neither is a wrapper
// reached only from dead code. The netns rows name openSandboxNetNSLazyPID, where #417 moved the counted body.
func TestCountingWrappers_AreTheOnlyCallers(t *testing.T) {
	tests := []struct {
		callee  string
		wrapper string
		why     string
	}{
		{
			callee:  "awaitContainerNetNS",
			wrapper: "openSandboxNetNSLazyPID",
			why: "netns_pid_mismatches is counted around the open, so a second caller would obtain the " +
				"container's network namespace without counting a PID-reuse refusal -- and " +
				"docs/reference.md tells operators that counter is the only thing distinguishing that " +
				"refusal from a slow container start",
		},
		{
			callee:  "netnsPIDMismatches",
			wrapper: "openSandboxNetNSLazyPID",
			why: "the counter is the refusal itself -- a PID that no longer belongs to the container is " +
				"not opened and IS counted, in one place, so a second increment site would mean a " +
				"second refusal path and operators reading netns_pid_mismatches could no longer tell " +
				"which one fired",
		},
		{
			callee:  "dhcpServerPolicyTimeouts",
			wrapper: "countOutageTick",
			why: "dhcp_server_policy_timeouts is a strict subset of dhcp_timeouts, counted in one place so " +
				"the subset relation is a property of the code rather than of two call sites agreeing -- a " +
				"second bumper of the outer counter on a policy-restricted path would leave the subset " +
				"intact while silently under-reporting the inner one",
		},
		{
			callee:  "prepareIPv6Link",
			wrapper: "ensureIPv6Enabled",
			why: "TWO counters are taken around this one call -- ipv6_link_enable_failures and " +
				"router_advert_guard_failures -- so a second caller would clear disable_ipv6 and write " +
				"the Router Advertisement sysctls without counting either failure. The first counter is " +
				"the only thing separating \"this segment is quiet\" from \"nothing IPv6 could ever have " +
				"arrived on this link\", which otherwise present identically as DHCPv6 timeouts (#868); " +
				"the second is the only thing that reports a guard that did not take, which otherwise " +
				"presents as a container that is healthy until the advertisement it holds expires (#875)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.callee, func(t *testing.T) {
			assertSoleCaller(t, tc.callee, tc.wrapper, tc.why)
		})
	}
}

func assertSoleCaller(t *testing.T, callee, wrapper, why string) {
	t.Helper()

	fset := token.NewFileSet()

	// parser.ParseDir is deprecated as of Go 1.25 for ignoring build tags, so the files are read one by one.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no production Go files parsed; this test would otherwise pass having read nothing")
	}

	callers := callSitesOf(fset, files, callee)

	// The wrapper must be reached too (#790): reverting countOutageTick's one call site to its pre-#769 form left the
	// package green while dhcp_server_policy_timeouts could no longer move. Self-calls do not count.
	wrapperCallers := callSitesOf(fset, files, wrapper)
	var reached []string
	for _, c := range wrapperCallers {
		if !strings.HasPrefix(c, wrapper+" ") {
			reached = append(reached, c)
		}
	}
	if len(reached) == 0 {
		t.Fatalf("nothing in production calls %s, the wrapper for %s.\n"+
			"  The callee's own presence and exclusivity below can BOTH still hold here: %s is\n"+
			"  called only from %s, and %s is called from nowhere -- so the counter fires never\n"+
			"  while every test of it passes, because they all drive the wrapper.\n"+
			"  %s.\n"+
			"  The likeliest cause is a call site reverted to increment the counter directly, or\n"+
			"  inlined. Restore the call to %s; do not delete this row.",
			wrapper, callee, callee, wrapper, wrapper, why, wrapper)
	}

	if len(callers) == 0 {
		t.Fatalf("nothing in production calls %s.\n"+
			"  The likeliest cause is that %s stopped calling it, in which case the counter it wraps\n"+
			"  now fires never and every test of that counter still passes -- they all drive the\n"+
			"  wrapper.\n"+
			"  %s.\n"+
			"  If %s was merely renamed, rename it in this table too; do not delete the row, or this\n"+
			"  test guards nothing while continuing to report green.",
			callee, wrapper, why, callee)
	}
	for _, c := range callers {
		if !strings.HasPrefix(c, wrapper+" ") {
			t.Errorf("%s is called from %s.\n"+
				"  It must be called only from %s, which is where the counter is incremented.\n"+
				"  %s.\n"+
				"  Route the call through %s.",
				callee, c, wrapper, why, wrapper)
		}
	}
	if len(callers) > 1 {
		t.Errorf("%s has %d production call sites, want 1: %v", callee, len(callers), callers)
	}
}

func callSitesOf(fset *token.FileSet, files []*ast.File, name string) []string {
	callee := name
	var callers []string
	seenAt := map[string]bool{}
	note := func(fnName string, pos token.Pos) {
		at := fnName + " (" + fset.Position(pos).String() + ")"
		if seenAt[at] {
			return
		}
		seenAt[at] = true
		callers = append(callers, at)
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			where := "package scope"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				where = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch bind := n.(type) {
				case *ast.AssignStmt:
					for _, rhs := range bind.Rhs {
						if boundName(rhs) == callee {
							note(where, rhs.Pos())
						}
					}
				case *ast.ValueSpec:
					for _, v := range bind.Values {
						if boundName(v) == callee {
							note(where, v.Pos())
						}
					}
				}

				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// A row may name a counter field, matched as the receiver of a mutating method; reads stay allowed
				// (#769).
				switch f := call.Fun.(type) {
				case *ast.Ident:
					if f.Name == callee {
						note(where, call.Pos())
					}
				case *ast.SelectorExpr:
					if f.Sel.Name == callee || (trailingName(f.X) == callee && mutates(f.Sel.Name)) {
						note(where, call.Pos())
					}
				}
				return true
			})
		}
	}
	return callers
}

func trailingName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// mutates is a closed set: a new sync/atomic mutator would make a row quiet, so add it here (#769).
func mutates(method string) bool {
	switch method {
	case "Add", "Store", "Swap", "CompareAndSwap":
		return true
	}
	return false
}

func boundName(e ast.Expr) string {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}
