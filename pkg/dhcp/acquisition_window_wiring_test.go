// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// Measured 2026-09-06: a 90s literal in place of V6AcquisitionWindow survived every other test in the package (#911).
// Bound: this is keyed on spelling, so a correct window reached through a variable reads as a violation.

func TestAcquisitionWindow_ComesFromTheDerivationAndNowhereElse(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "chassis6.go", nil, 0)
	if err != nil {
		t.Fatalf("parse chassis6.go: %v", err)
	}

	var args []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "runAcquisition6" {
			return true
		}
		if len(call.Args) == 0 {
			t.Errorf("%s: runAcquisition6 called with no arguments", fset.Position(call.Pos()))
			return true
		}
		var b strings.Builder
		if err := printer.Fprint(&b, fset, call.Args[len(call.Args)-1]); err != nil {
			t.Fatalf("printing the window argument: %v", err)
		}
		args = append(args, b.String())
		return true
	})

	if len(args) == 0 {
		t.Fatal("no call to runAcquisition6 in chassis6.go: this test's domain is empty, " +
			"and an empty domain satisfies the rule below without checking anything")
	}
	if len(args) != 1 {
		t.Errorf("runAcquisition6 is called %d times (%v); the window is one decision and "+
			"a second call site is a second answer to it", len(args), args)
	}
	for _, got := range args {
		if got != "V6AcquisitionWindow(params)" {
			t.Errorf("runAcquisition6's window argument is %q, want V6AcquisitionWindow(params); "+
				"the acquisition is running on a budget that is not the one derived from "+
				"the protocol's own schedule", got)
		}
	}
}

// getIP6 opens a packet socket on its first line, so no unit test reaches the retry wiring and it is checked by
// spelling (#911).

func TestHintlessRetry_IsWiredIntoTheOneAcquisitionCall(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "chassis6.go", nil, 0)
	if err != nil {
		t.Fatalf("parse chassis6.go: %v", err)
	}

	spell := func(n ast.Node) string {
		var b strings.Builder
		if err := printer.Fprint(&b, fset, n); err != nil {
			t.Fatalf("printing %T: %v", n, err)
		}
		return b.String()
	}

	// calls lists the calls named fn under n, with each call's arguments spelled.
	calls := func(n ast.Node, fn string) [][]string {
		var found [][]string
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			if name != fn {
				return true
			}
			var args []string
			for _, a := range call.Args {
				args = append(args, spell(a))
			}
			found = append(found, args)
			return true
		})
		return found
	}

	var getIP6Fn *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "getIP6" {
			getIP6Fn = fn
		}
	}
	if getIP6Fn == nil {
		t.Fatal("no getIP6 in chassis6.go: this test's domain is empty, and an empty " +
			"domain satisfies every rule below without checking anything")
	}

	var loops []*ast.ForStmt
	ast.Inspect(getIP6Fn, func(n ast.Node) bool {
		if f, ok := n.(*ast.ForStmt); ok {
			loops = append(loops, f)
		}
		return true
	})
	if len(loops) != 1 {
		t.Fatalf("getIP6 has %d for statements, want the one the second attempt runs in; "+
			"without it a hinted endpoint gets one attempt and the address another node "+
			"holds is the only one it ever asks for", len(loops))
	}

	acquire := calls(loops[0], "acquireOnce6")
	if len(acquire) != 1 {
		t.Fatalf("acquireOnce6 is called %d times inside getIP6's loop, want exactly one: "+
			"a second call site is a second answer to what the retry runs with", len(acquire))
	}
	if n := len(acquire[0]); n != 4 {
		t.Fatalf("acquireOnce6 is called with %d arguments, want 4", n)
	}
	if got := acquire[0][2]; got != "opts.params6" {
		t.Errorf("acquireOnce6's params argument is %q, want opts.params6: the second "+
			"pass has to run with the params retryWithoutHint6 cleared the hint out of, "+
			"and any other spelling asks the server for the declined address again", got)
	}
	if got := calls(loops[0], "retryWithoutHint6"); len(got) != 1 {
		t.Errorf("retryWithoutHint6 is called %d times in getIP6's loop, want exactly one: "+
			"it is both the decision to retry and the bound on retrying", len(got))
	}

	// Measured 2026-09-06: the mutant that widens the loop's exit survived the package without these assertions (#911).
	var (
		returns int
		conds   []string
	)
	ast.Inspect(loops[0], func(n ast.Node) bool {
		switch st := n.(type) {
		case *ast.ReturnStmt:
			returns++
		case *ast.IfStmt:
			conds = append(conds, spell(st.Cond))
		}
		return true
	})
	if returns != 1 {
		t.Errorf("getIP6's loop has %d return statements, want the one the decision "+
			"guards; a second exit is a second answer to whether the retry runs", returns)
	}
	if len(conds) != 1 || conds[0] != "!again" {
		t.Errorf("getIP6's loop is guarded by %v, want the single condition !again: the "+
			"retry is reachable only if the loop's exit is exactly what "+
			"retryWithoutHint6 answered", conds)
	}

	run := calls(file, "runAcquisition6")
	if len(run) != 1 {
		t.Fatalf("runAcquisition6 is called %d times in chassis6.go, want exactly one", len(run))
	}
	if n := len(run[0]); n < 2 {
		t.Fatalf("runAcquisition6 is called with %d arguments", n)
	}
	if got := run[0][len(run[0])-2]; got != "params.Hint" {
		t.Errorf("runAcquisition6's hint argument is %q, want params.Hint: with anything "+
			"else the conflict on an address this attempt ASKED for is indistinguishable "+
			"from one on an address the server chose", got)
	}
}
