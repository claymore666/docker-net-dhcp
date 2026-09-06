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
