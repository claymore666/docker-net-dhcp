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
		{
			callee:  "netnsPIDMismatches",
			wrapper: "openSandboxNetNS",
			why: "the counter is the refusal itself -- a PID that no longer belongs to the container is " +
				"not opened and IS counted, in one place, so a second increment site would mean a " +
				"second refusal path and operators reading netns_pid_mismatches could no longer tell " +
				"which one fired",
		},
		// The three rows above cost one line each, which was the point
		// of the table: the next instance of this shape adds a row
		// rather than another near-identical test.
		//
		// The last row names a FIELD, not a function, and that is
		// deliberate. Its increment is `...netnsPIDMismatches.Add(1)`,
		// and a row keyed on `Add` would be useless: `Add` has 56
		// production call sites in this package, one of them a
		// sync.WaitGroup. Keying on the counter asks the question that
		// actually matters -- what may touch this counter -- and it is
		// the only form in which a counter incremented INLINE (rather
		// than by a helper of its own) can be held at all.
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
				// A subject is matched as the thing being CALLED --
				// bare or qualified -- or as the RECEIVER whose method
				// is being called.
				//
				// The second form is what lets a row name a counter
				// FIELD rather than a function: `netnsPIDMismatches`
				// matches `m.plugin.netnsPIDMismatches.Add(1)`. Naming
				// the method instead would be useless, because `Add`
				// has 56 production call sites in this package and one
				// of them is a sync.WaitGroup.
				//
				// Both forms answer the same question -- what must only
				// be reached through the wrapper -- so they share a
				// column rather than needing a second one. A name that
				// happened to be both would match both, which is
				// stricter, not looser.
				//
				// The receiver form counts only MUTATING methods. A
				// counter is read wherever it is published --
				// healthSnapshot loads every one of them -- and a rule
				// that called those call sites violations would be
				// unsatisfiable, so the first person to hit it would
				// delete the row. The invariant is about who may WRITE
				// the counter.
				switch f := call.Fun.(type) {
				case *ast.Ident:
					if f.Name == callee {
						callers = append(callers, fn.Name.Name+" ("+fset.Position(call.Pos()).String()+")")
					}
				case *ast.SelectorExpr:
					if f.Sel.Name == callee || (trailingName(f.X) == callee && mutates(f.Sel.Name)) {
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

// trailingName returns the last identifier of a receiver expression --
// "netnsPIDMismatches" for m.plugin.netnsPIDMismatches, "wg" for wg --
// or "" for anything else. It is how a row names the counter a wrapper
// guards instead of the method that increments it.
func trailingName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

// mutates reports whether an atomic method WRITES its receiver. The set
// is closed on purpose: a method outside it is treated as a read, so a
// new mutator added to sync/atomic would make a row go quiet rather than
// red. That is caught by the presence half of the assertion only if the
// row's sole write used the new method -- so if this list ever needs a
// name, add it here rather than working around the row.
func mutates(method string) bool {
	switch method {
	case "Add", "Store", "Swap", "CompareAndSwap":
		return true
	}
	return false
}
