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

// Counters whose increment lives inside a wrapper are only protected
// while that wrapper is the ONLY thing that calls the function it
// wraps. This table is that rule, one row per counter.
//
// Nothing else can enforce it. Every test of such a counter drives the
// wrapper, so a caller that reverts to the wrapped function directly
// leaves the whole suite green while the counter silently stops firing
// on the only path in production that reaches it. Both rows below were
// found that way -- as surviving mutants, by running them, not by
// reading the code.
//
// The property is about Go source, so this is a Go test using go/ast
// rather than a shell gate: no lane entry, no OUT_OF_LANE declaration
// and no meta-test of its own, because it IS its own meta-test. Add a
// second call site to any row and it goes red, which is the entire
// property. There is no hand-kept list to drift.
func TestCountingWrappers_AreTheOnlyCallers(t *testing.T) {
	tests := []struct {
		callee  string
		wrapper string
		why     string
	}{
		{
			callee:  "awaitContainerNetNS",
			wrapper: "openSandboxNetNS",
			why: "netns_pid_mismatches is counted around the open, so a second caller would obtain the " +
				"container's network namespace without counting a PID-reuse refusal -- and " +
				"docs/reference.md tells operators that counter is the only thing distinguishing that " +
				"refusal from a slow container start",
		},
		{
			callee:  "observe",
			wrapper: "observeLease",
			why: "lease_time_clamped is counted around the fold, so a second caller would take a clamped " +
				"lease time into the tracker without counting the clamp -- and the clamp is the only " +
				"signal that a server's lease time was overridden, which /metrics publishes and " +
				"nothing else records",
		},
		// The row above cost one line, which was the point of the
		// table: the next instance of this shape adds a row rather
		// than another near-identical test.
	}

	for _, tc := range tests {
		t.Run(tc.callee, func(t *testing.T) {
			assertSoleCaller(t, tc.callee, tc.wrapper, tc.why)
		})
	}
}

// assertSoleCaller parses this package's production sources and fails
// unless every call to callee sits inside wrapper.
func assertSoleCaller(t *testing.T, callee, wrapper, why string) {
	t.Helper()

	fset := token.NewFileSet()

	// The files are read and parsed here rather than through
	// parser.ParseDir, which is deprecated as of Go 1.25 for not
	// honouring build tags. This package has none, and a plain
	// directory read keeps the test free of a tooling dependency.
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

	var callers []string
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// Both the bare call and a qualified/method form.
				switch f := call.Fun.(type) {
				case *ast.Ident:
					if f.Name == callee {
						callers = append(callers, fn.Name.Name+" ("+fset.Position(call.Pos()).String()+")")
					}
				case *ast.SelectorExpr:
					if f.Sel.Name == callee {
						callers = append(callers, fn.Name.Name+" ("+fset.Position(call.Pos()).String()+")")
					}
				}
				return true
			})
		}
	}

	// TWO-SIDED ON PURPOSE, and this is the half that is easy to lose.
	// "No caller outside the wrapper" is satisfied by ZERO callers --
	// which is the very mutant this test exists to kill, the call being
	// deleted. Phrased that way it would go green over the defect it
	// was written for, for every row at once, silently and forever. So
	// presence is asserted before exclusivity.
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
