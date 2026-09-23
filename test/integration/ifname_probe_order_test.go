// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: this reads the suite's source, so it runs in `go test ./...` on every engine, including those
// where the guarded test can only skip. TestInterfaceName_MultiNetworkDeterministic must skip before it builds the
// ephemeral fixture, whose teardown fails a fixture that granted no lease (#472), so a late skip reads FAIL on every
// engine before moby/moby#52866 (#841). Calls resolve by name within this file, through same-file helpers at any
// depth; a call moved to another file, a function value or a renamed anchor fails the gate instead of passing it.

package integration

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

const (
	ifnameOrderFile    = "interface_name_test.go"
	ifnameOrderSubject = "TestInterfaceName_MultiNetworkDeterministic"

	ifnameOrderFixtureCtor = "NewEphemeralFixture"
)

// ifnameOrderIsSkip reports whether a selector is a testing skip, matched on the method name alone.
func ifnameOrderIsSkip(sel string) bool {
	return sel == "Skip" || sel == "Skipf" || sel == "SkipNow"
}

// ifnameOrderScan reports whether n calls a skip or the fixture constructor at any depth, and the bare names it calls.
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
			// A dot import, a local of the same name, or a same-file helper.
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

// TestInterfaceName_ProbeSkipPrecedesTheEphemeralFixture checks that interface_name_test.go skips before it builds the ephemeral fixture (#841).
func TestInterfaceName_ProbeSkipPrecedesTheEphemeralFixture(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, ifnameOrderFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v — this gate reads that file; it cannot pass without it",
			ifnameOrderFile, err)
	}

	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
			funcs[fd.Name.Name] = fd
		}
	}

	// helperSkips[n] and helperBuilds[n]: calling n reaches a skip or a fixture construction somewhere in this file.
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

	// A helper that reaches an anchor counts at the position of the call.
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
			"On every engine without moby/moby#52866 — every line below 29.8.0 — that fixture is "+
			"created and then torn down having served no client, its lease-grant guard "+
			"(#472) fires, and the run reports FAIL where it must report SKIP. CI cannot "+
			"catch this: its verdict for this test is SKIP either way. Move the "+
			"%s call back below the skip.",
			ifnameOrderSubject, ifnameOrderFile, buildLine, ifnameOrderFile, skipLine,
			ifnameOrderFixtureCtor)
	}
}
