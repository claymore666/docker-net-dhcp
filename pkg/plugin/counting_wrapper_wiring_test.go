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
// on the only path in production that reaches it. The rows below were
// found that way -- as surviving mutants, by running them, not by
// reading the code, and so was the third.
//
// WHAT THIS TABLE DOES NOT SEE, and it must be said here rather than
// discovered later. It matches names and binds in one package's AST. It
// is a real step up from a grep -- it will not be fooled by a comment
// or a string -- but it is not alias analysis, and an author determined
// to reach a subject some other way can:
//
//   - hand the subject through a struct field, a map, a closure capture
//     or an interface, none of which is inspected;
//   - reach it from another package, which is never parsed;
//   - reach it by reflection.
//
// The direct bind forms ARE covered, because those are the ones a
// normal person reaches for when the plain call is inconvenient: taking
// its address, assigning it to a local, or binding it at package scope.
// That is the population a wiring gate is aimed at, and the limit above
// is the honest boundary rather than a to-do.
//
// CALLED IS NOT REACHED, and this is the boundary most likely to be
// over-read given how much else this file states. Every row asks
// whether anything BYPASSES the wrapper. None asks whether the wrapper
// is on a live path. So a counter can stop firing entirely with all
// rows green: leave the wrapper as the callee's only caller, but let
// the wrapper itself be reached only from code nothing calls, and the
// table sees one caller and passes while the counter fires never.
// Driven, not reasoned: removing a row's real call site and adding a
// dead production function in its place leaves the whole table green.
// A self-call is excluded, so the one-hop version of this is closed;
// a two-hop dead chain walks straight through.
//
// Closing it properly means a transitive walk from the exported surface
// -- reachability, not call counting -- which is a different gate and a
// larger one. It is deliberately not built here. What this file
// protects is the bypass, and dead code is caught by the coverage
// ratchet and by review rather than by this table.
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
		{
			callee:  "dhcpServerPolicyTimeouts",
			wrapper: "countOutageTick",
			why: "dhcp_server_policy_timeouts is a strict subset of dhcp_timeouts, counted in one place so " +
				"the subset relation is a property of the code rather than of two call sites agreeing -- a " +
				"second bumper of the outer counter on a policy-restricted path would leave the subset " +
				"intact while silently under-reporting the inner one",
		},
		{
			callee:  "enableIPv6OnContainerLink",
			wrapper: "ensureIPv6Enabled",
			why: "ipv6_link_enable_failures is counted around the enable, so a second caller would clear " +
				"disable_ipv6 without counting the failure to -- and that counter is the only thing " +
				"separating \"this segment is quiet\" from \"nothing IPv6 could ever have arrived on this " +
				"link\", which otherwise present identically as DHCPv6 timeouts (#868)",
		},
		// The four rows above cost one line each, which was the point
		// of the table: the next instance of this shape adds a row
		// rather than another near-identical test.
		//
		// Two of them name a FIELD, not a function, and that is
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

	callers := callSitesOf(fset, files, callee)

	// AND THE PRESENCE CHECK BELOW IS ON THE CALLEE, WHICH STOPS ONE
	// LEVEL SHORT (#790). "The callee is reached, and only through the
	// wrapper" is satisfied in full while the WRAPPER itself is called by
	// nothing: the callee's only call site is inside the wrapper, so it
	// is present and exclusive, and the counter is nonetheless a
	// permanent zero in /metrics.
	//
	// Measured: reverting countOutageTick's one production call site to
	// its pre-#769 form left the package green -- 385 tests, all four
	// rows passing -- while dhcp_server_policy_timeouts stopped being
	// reachable. Every test of that counter still passed, because they
	// all drive the wrapper.
	//
	// Self-calls do not count: a wrapper that only recurses is reached
	// from nothing, and would satisfy a naive presence check the same way
	// zero callers satisfies exclusivity.
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

// callSitesOf returns every production call site of `name` in the parsed
// files, as "enclosingFunc (file:line:col)".
//
// EXTRACTED SO BOTH SIDES ARE MATCHED THE SAME WAY (#790). The wrapper's
// presence is now asserted as well as the callee's, and a second, simpler
// scan for the wrapper would have been a different matcher: it would miss
// the bind-laundering forms this one was extended to catch, so the two
// halves of a two-sided assertion would disagree about what a call site
// is. One matcher, asked twice.
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
			// PACKAGE SCOPE IS A SITE TOO. Walking only FuncDecls
			// misses `var zzAwait = awaitContainerNetNS` at file
			// level, which is the laundering mutant in its shortest
			// form -- it survived the first version of the bind rule
			// for exactly this reason, and only driving it showed
			// that. The enclosing name is then the file rather than a
			// function, which is what the report should say.
			where := "package scope"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				where = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				// A BIND IS A CALL SITE, and matching only CallExpr
				// makes this a matcher of NAMES that one local walks
				// straight past:
				//
				//   c := &m.plugin.netnsPIDMismatches; c.Add(1)
				//   var zzAwait = awaitContainerNetNS; zzAwait(...)
				//
				// Both compile, both give the subject a second reachable
				// site, and both were SURVIVING mutants until this case
				// existed -- found by driving them, not by reading. It
				// is the same failure the proc-path gate had, where
				// "/proc" + "/" + strconv.Itoa(pid) walked past a regex
				// that caught fmt.Sprintf.
				//
				// Deliberately not alias analysis. Only the TOP-LEVEL
				// value of an assignment or var spec is examined, never
				// its interior, so `x := Foo{N: p.counter.Load()}` stays
				// uncounted -- reads must not become violations (see the
				// mutates() note below). A determined author can still
				// get around this; it catches the spelling a normal
				// person reaches for when the direct one is
				// inconvenient, which is the whole population a wiring
				// gate is aimed at.
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

// boundName returns the name a bound VALUE refers to -- the subject of
// `= awaitContainerNetNS`, of `= m.plugin.netnsPIDMismatches` and of
// `= &m.plugin.netnsPIDMismatches` -- or "" for anything else,
// including a call, a literal or a composite. Only these three shapes
// hand the subject itself to another identifier; everything else has
// already been reduced to a value.
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
