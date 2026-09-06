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

// TestAcquisitionWindow_ComesFromTheDerivationAndNowhereElse closes the
// one seam between two things that ARE tested.
//
// V6AcquisitionWindow's derivation is driven by
// TestV6SolicitWindow_CoversTheRetransmissionsItClaims, and
// runAcquisition6's use of whatever window it is given is driven by
// TestRunAcquisition6_HasItsOwnWindow. What neither can see is the
// argument that joins them: getIP6 opens a packet socket before it
// reaches the call, so no unit test can execute that line, and a
// literal there -- or lease_timeout back again -- would leave both of
// the tests above green. MEASURED 2026-09-06: the mutant that replaces
// the argument with a 90s literal survived everything else in this
// package.
//
// THE BOUND. This is keyed on the SPELLING of the call and of its
// argument. A window reached through a variable assigned from
// V6AcquisitionWindow, or through a wrapper around it, reads as a
// violation here even though it is correct; a window computed by a
// function that merely happens to be NAMED V6AcquisitionWindow reads as
// correct even though it is not. It is a wiring check and not a
// behaviour one, which is why it exists BESIDE the two behaviour tests
// rather than instead of either.
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

	// NON-VACUITY. A rename of the function, or a move of the call into
	// another file, empties the search and every assertion below then
	// holds over nothing.
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

// TestHintlessRetry_IsWiredIntoTheOneAcquisitionCall closes the seam
// the same way, for the same reason, over the second attempt.
//
// TestRetryWithoutHint6 drives the decision and its bound;
// TestRunAcquisition6_AHintedConflictEndsTheLoop drives the event that
// asks for it. Neither can see getIP6, which is where the two are
// joined and which opens a packet socket on its first line. The mutants
// that live in that gap are: run the acquisition once and return (the
// retry never happens); pass the ORIGINAL params to the second pass (it
// asks for the declined address again); and hand runAcquisition6 no
// hint (every conflict is treated as a server's own choice, and the
// loop this PR exists to break comes back).
//
// THE BOUND is the one the test above states: this is keyed on
// spelling. It says the call sites READ correctly, not that they
// BEHAVE correctly, which is why it sits beside the behaviour tests
// rather than instead of them.
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

	// calls named fn, anywhere under n, with each call's arguments spelled.
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
	// NON-VACUITY, and it is the whole of this test's domain: a renamed
	// or moved getIP6 empties every assertion below.
	if getIP6Fn == nil {
		t.Fatal("no getIP6 in chassis6.go: this test's domain is empty, and an empty " +
			"domain satisfies every rule below without checking anything")
	}

	// The retry is a loop, and the loop is the bound: see
	// retryWithoutHint6 for why it cannot run a third time.
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

	// THE LOOP'S ONLY EXIT IS THAT DECISION. MEASURED 2026-09-06: with
	// the assertions above and none of these, the mutant that widens the
	// exit condition -- so the first attempt always returns and the
	// retry is dead code -- survived the whole package, because it
	// changes neither call nor spelling of an argument.
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

	// And the hint reaches the loop that has to recognise the conflict.
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
