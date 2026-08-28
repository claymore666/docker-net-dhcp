// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// This file deliberately carries NO `//go:build integration` tag,
// unlike every other file in this package. It reads the suite's source
// instead of running it, so it belongs in the ordinary `go test ./...`
// lane: no root, no daemon, and — the point — it runs on every engine,
// including the ones on which the test it guards can only skip.
//
// # What it guards
//
// TestInterfaceName_MultiNetworkDeterministic probes the engine for
// remote-driver DstName support and skips when it is absent. That probe
// has to happen BEFORE the ephemeral fixture is stood up, because the
// fixture asserts on teardown that its DHCP server actually granted a
// lease (#472): a fixture created ahead of a skip is torn down having
// served nobody, the guard fires, and the run reports FAIL where it
// should report SKIP — on every released engine, which is all of them
// until moby/moby#52866 ships. That regression is invisible to CI here,
// because CI's own verdict for this test is SKIP either way.
//
// The constraint used to be stated only in a comment. This is the
// executable form of it (#841).
//
// # What it can and cannot follow
//
// It is a SOURCE check. It reads positions in the file; it does not
// execute anything. Judged honestly, that means:
//
// Survives (the gate keeps working, silently and correctly):
//   - renaming the local variable the fixture is assigned to;
//   - reflowing either call across lines;
//   - aliasing or dot-changing the harness import — the constructor is
//     matched on its selector name, not on the package qualifier;
//   - changing the skip's message, or swapping Skip for Skipf/SkipNow;
//   - moving either call into a helper FUNCTION DEFINED IN THIS SAME
//     FILE and calling it by name, at any depth. The call site's
//     position stands in for the moved call, so the order is still
//     checked.
//
// Goes RED, loudly, saying it lost its subject (never quiet, never
// green):
//   - deleting or renaming TestInterfaceName_MultiNetworkDeterministic;
//   - renaming harness.NewEphemeralFixture;
//   - moving either call into another FILE, or reaching it through a
//     function value, an interface or reflection — this gate resolves
//     calls by name within one file and nothing else;
//   - both anchors landing at the same source position.
//
// A rename that makes this gate red is not a false alarm to be silenced
// with a skip: it means the gate can no longer see the thing it guards,
// and the fix is to teach it the new name in the same commit.

package integration

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

const (
	// The file and function this gate reads. Both are anchors: if
	// either stops resolving, the gate fails rather than passing
	// having inspected nothing.
	ifnameOrderFile    = "interface_name_test.go"
	ifnameOrderSubject = "TestInterfaceName_MultiNetworkDeterministic"

	// The constructor whose call must not precede the skip.
	ifnameOrderFixtureCtor = "NewEphemeralFixture"
)

// ifnameOrderIsSkip reports whether a selector call is a testing skip.
// Matched on the method name alone: the *testing.T local is not always
// called t, and a message change must not blind the gate.
func ifnameOrderIsSkip(sel string) bool {
	return sel == "Skip" || sel == "Skipf" || sel == "SkipNow"
}

// ifnameOrderScan walks a node and reports whether it contains, at any
// depth, a skip call and/or a call to the fixture constructor. It also
// collects the names of same-file functions it calls, so callers can
// propagate those answers through the file's own call graph.
func ifnameOrderScan(n ast.Node) (skips, builds bool, calls []string) {
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if ifnameOrderIsSkip(fun.Sel.Name) {
				skips = true
			}
			if fun.Sel.Name == ifnameOrderFixtureCtor {
				builds = true
			}
		case *ast.Ident:
			// Dot-imported, or a local of the same name; and every
			// bare call is a candidate same-file helper.
			if ifnameOrderIsSkip(fun.Name) {
				skips = true
			}
			if fun.Name == ifnameOrderFixtureCtor {
				builds = true
			}
			calls = append(calls, fun.Name)
		}
		return true
	})
	return skips, builds, calls
}

// TestInterfaceName_ProbeSkipPrecedesTheEphemeralFixture is the
// executable form of the ordering constraint stated in
// interface_name_test.go. See this file's header for what it follows
// and what it refuses to guess at.
func TestInterfaceName_ProbeSkipPrecedesTheEphemeralFixture(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, ifnameOrderFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v — this gate reads that file; it cannot pass without it",
			ifnameOrderFile, err)
	}

	// Every free function in the file, so a call to a same-file helper
	// can stand in for what the helper does.
	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
			funcs[fd.Name.Name] = fd
		}
	}

	// Direct answers plus the intra-file call graph, then a fixed point
	// over it: helperSkips[n] / helperBuilds[n] mean "calling n reaches
	// a skip / a fixture construction, somewhere in this file".
	helperSkips := map[string]bool{}
	helperBuilds := map[string]bool{}
	callees := map[string][]string{}
	for name, fd := range funcs {
		s, b, calls := ifnameOrderScan(fd.Body)
		helperSkips[name], helperBuilds[name], callees[name] = s, b, calls
	}
	for changed := true; changed; {
		changed = false
		for name := range funcs {
			for _, c := range callees[name] {
				if _, known := funcs[c]; !known {
					continue
				}
				if helperSkips[c] && !helperSkips[name] {
					helperSkips[name], changed = true, true
				}
				if helperBuilds[c] && !helperBuilds[name] {
					helperBuilds[name], changed = true, true
				}
			}
		}
	}

	subject := funcs[ifnameOrderSubject]
	if subject == nil {
		t.Fatalf("no function %s in %s.\n\n"+
			"This gate exists to keep the engine-capability skip ahead of the "+
			"ephemeral fixture in that test (#841, #472). It cannot check an "+
			"order in a function it cannot find, and a gate that passes when "+
			"its subject is gone is worse than no gate. If the test was "+
			"renamed, rename ifnameOrderSubject with it, in the same commit. "+
			"If it was deleted, delete this gate deliberately and say so.",
			ifnameOrderSubject, ifnameOrderFile)
	}

	// Earliest position of each anchor inside the subject. A call to a
	// same-file helper that reaches an anchor counts at the position of
	// the CALL, which is where the effect happens.
	skipPos, buildPos := token.NoPos, token.NoPos
	note := func(dst *token.Pos, p token.Pos) {
		if !dst.IsValid() || p < *dst {
			*dst = p
		}
	}
	ast.Inspect(subject.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if ifnameOrderIsSkip(fun.Sel.Name) {
				note(&skipPos, call.Pos())
			}
			if fun.Sel.Name == ifnameOrderFixtureCtor {
				note(&buildPos, call.Pos())
			}
		case *ast.Ident:
			if ifnameOrderIsSkip(fun.Name) {
				note(&skipPos, call.Pos())
			}
			if fun.Name == ifnameOrderFixtureCtor {
				note(&buildPos, call.Pos())
			}
			if _, known := funcs[fun.Name]; known {
				if helperSkips[fun.Name] {
					note(&skipPos, call.Pos())
				}
				if helperBuilds[fun.Name] {
					note(&buildPos, call.Pos())
				}
			}
		}
		return true
	})

	const lostSubject = "\n\nThis gate resolves calls by name within %s only. If the call moved " +
		"to another file, behind a function value, or behind an interface, this gate can no " +
		"longer see its subject — so it fails rather than going quiet. Either bring the call " +
		"back where it can be seen, or replace this gate with one that can follow it. Do not " +
		"delete it and leave the ordering to the comment: the comment is what failed."

	if !skipPos.IsValid() {
		t.Fatalf("%s contains no t.Skip/Skipf/SkipNow, directly or via a helper in this file.\n\n"+
			"That skip is how the test reports 'this engine cannot run me'. Without it the "+
			"test either does not gate on the engine capability at all, or gates somewhere "+
			"this gate cannot see."+lostSubject, ifnameOrderSubject, ifnameOrderFile)
	}
	if !buildPos.IsValid() {
		t.Fatalf("%s never calls %s, directly or via a helper in this file.\n\n"+
			"The second subnet is what makes this test about networks rather than about two "+
			"stable strings; the gate is anchored on that constructor by name."+lostSubject,
			ifnameOrderSubject, ifnameOrderFixtureCtor, ifnameOrderFile)
	}

	skipLine := fset.Position(skipPos).Line
	buildLine := fset.Position(buildPos).Line
	if skipPos == buildPos {
		t.Fatalf("the skip and the %s call resolve to the same position (%s:%d), so their "+
			"order is not decidable from the source."+lostSubject,
			ifnameOrderFixtureCtor, ifnameOrderFile, skipLine, ifnameOrderFile)
	}
	if skipPos > buildPos {
		t.Fatalf("%s stands up the ephemeral fixture at %s:%d, BEFORE the engine-capability "+
			"skip at %s:%d.\n\n"+
			"On every engine without moby/moby#52866 — all of them today — that fixture is "+
			"created and then torn down having served no client, its lease-grant guard "+
			"(#472) fires, and the run reports FAIL where it must report SKIP. CI cannot "+
			"catch this: its verdict for this test is SKIP either way. Move the "+
			"%s call back below the skip.",
			ifnameOrderSubject, ifnameOrderFile, buildLine, ifnameOrderFile, skipLine,
			ifnameOrderFixtureCtor)
	}
}
