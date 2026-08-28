// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The three refusal counters live in pkg/dhcp and arrive in
// healthSnapshot as one multi-value assignment. Nothing checked that
// each one then reaches ITS OWN field, and this test exists because
// that gap was measured rather than suspected (#875).
//
// MEASURED with the shared mutation harness against
// `go test ./pkg/plugin/ ./test/integration/harness/`: feeding
// RouterAdvertGuardFailures a constant zero SURVIVED, and feeding it
// the mount-prep counter SURVIVED. An operator reading a counter that
// is structurally incapable of being non-zero cannot tell it apart
// from a quiet segment, which is the entire reason these counters
// exist. The gap was not new — DirectivesRefused and MountPrepFailures
// had no coverage either — so the check is written over all three
// rather than only the one this issue added.
//
// WHY STRUCTURAL. The counters are package-level atomics in pkg/dhcp
// incremented by a stderr watcher on a real dhcpcd. From pkg/plugin
// there is no seam that makes them non-zero, so a value-comparing test
// would compare 0 against 0 and pass over every mutant above. What can
// be checked is the WIRING, and the wiring is where both survivors
// lived.
//
// The check is: the identifiers destructured from dhcp.RefusalCounts()
// reach the matching fields IN ORDER, and none of them is reassigned
// in between. The second half is what kills a substituted or constant
// value; the first half is what kills a positional swap, which is the
// realistic defect now that the call returns three interchangeable
// int32s.
func TestHealth_RefusalCountersAreWiredToTheirOwnCounter(t *testing.T) {
	// Field order here is the CONTRACT, matched positionally against
	// dhcp.RefusalCounts()'s return list. pkg/dhcp's own tests pin what
	// each position counts; this pins where each position lands.
	wantFields := []string{"DirectivesRefused", "MountPrepFailures", "RouterAdvertGuardFailures"}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "endpoints.go", nil, 0)
	if err != nil {
		t.Fatalf("parse endpoints.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "healthSnapshot" && fd.Recv != nil {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("no healthSnapshot method in endpoints.go: nothing below is checked")
	}

	// 1. Find the destructuring and record the identifier per position.
	var names []string
	var destructure *ast.AssignStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "RefusalCounts" {
			return true
		}
		destructure = as
		for _, lhs := range as.Lhs {
			if id, ok := lhs.(*ast.Ident); ok {
				names = append(names, id.Name)
			} else {
				names = append(names, "<not an identifier>")
			}
		}
		return false
	})
	if destructure == nil {
		t.Fatal("healthSnapshot does not call dhcp.RefusalCounts(); the health payload " +
			"is no longer reading the counters at all")
	}
	if len(names) != len(wantFields) {
		t.Fatalf("RefusalCounts() destructured into %d name(s) %v, but %d field(s) are "+
			"wired from it: a return value was added or removed without this contract "+
			"being revisited", len(names), names, len(wantFields))
	}
	for i, n := range names {
		if n == "_" || n == "<not an identifier>" {
			t.Fatalf("position %d of RefusalCounts() is discarded as %q, so %s cannot be "+
				"reporting it", i, n, wantFields[i])
		}
	}

	// 2. No identifier may be reassigned after the destructuring. This
	//    is the half that kills a constant, or one counter substituted
	//    for another: both leave the field reading the right NAME.
	bound := map[string]bool{}
	for _, n := range names {
		bound[n] = true
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as == destructure {
			return true
		}
		for _, lhs := range as.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && bound[id.Name] {
				t.Errorf("%v is reassigned at %v after being read from RefusalCounts(); "+
					"the health field then reports whatever that assignment says, not "+
					"the counter", id.Name, fset.Position(as.Pos()))
			}
		}
		return true
	})

	// 3. Each field must be fed the identifier at its own position.
	got := map[string]string{}
	ast.Inspect(fn, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			return true
		}
		if id, ok := kv.Value.(*ast.Ident); ok {
			got[key.Name] = id.Name
		} else {
			got[key.Name] = "<not a plain identifier>"
		}
		return true
	})
	for i, field := range wantFields {
		if got[field] != names[i] {
			t.Errorf("%s is fed %q, want %q (position %d of RefusalCounts())",
				field, got[field], names[i], i)
		}
	}
}
