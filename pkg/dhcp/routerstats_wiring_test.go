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

// TestOneShotAcquisition_FoldsItsOwnRouterDiscovery closes the one fold
// site no behaviour test in this package can reach.
//
// WHY IT NEEDS A WIRING TEST AT ALL. The other three fold sites are in
// translate(), which a fake runner drives, and each has a test that
// goes red when its line is deleted. acquireOnce6 is not reachable that
// way: it opens a real packet socket on a real interface in a real
// namespace (see the comment on getIP6), so nothing in this package
// executes it. MEASURED: deleting opts.routerReport(stats) from
// acquireOnce6 leaves the whole of ./pkg/... green.
//
// WHY THAT LINE MATTERS MORE THAN THE OTHER THREE, not less. A
// CreateEndpoint one-shot never reaches translate() at all. It runs for
// the whole of RFC 4861 section 6.3.7's discovery window, counts every
// solicitation it sent and every advertisement that came back, and then
// ends. No later client inherits those counters: the persistent client
// Join starts has a manager, and a set of counters, of its own. The
// line's own comment says the numbers are counted "here or nowhere",
// and until this test that claim had no observer.
//
// THE BOUND, which is the same one the acquisition-window wiring test
// states about itself. This is keyed on SPELLING. A fold reached
// through a wrapper, or through a variable holding the stats, reads as
// a violation here though it is correct; a call to something merely
// NAMED routerReport reads as correct though it is not. It is a wiring
// check beside the behaviour tests, not instead of them.
func TestOneShotAcquisition_FoldsItsOwnRouterDiscovery(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "chassis6.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing chassis6.go: %v", err)
	}

	spell := func(n ast.Node) string {
		var b strings.Builder
		if err := printer.Fprint(&b, fset, n); err != nil {
			t.Fatalf("printing %T: %v", n, err)
		}
		return b.String()
	}

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

	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "acquireOnce6" && f.Recv == nil {
			fn = f
		}
	}
	// NON-VACUITY. A renamed or moved acquireOnce6 empties every
	// assertion below, and an empty domain satisfies all of them.
	if fn == nil {
		t.Fatal("no acquireOnce6 in chassis6.go: this test's domain is empty, and an " +
			"empty domain satisfies every rule below without checking anything")
	}

	fold := calls(fn, "routerReport")
	if len(fold) != 1 {
		t.Fatalf("acquireOnce6 folds router discovery %d time(s), want exactly one. A "+
			"CreateEndpoint one-shot never reaches translate(), so its solicitations and "+
			"the advertisements they brought back are counted here or nowhere, and a host "+
			"that only ever creates endpoints then reads zero on a segment with a router "+
			"on it", len(fold))
	}
	if n := len(fold[0]); n != 1 {
		t.Fatalf("acquireOnce6 folds router discovery with %d argument(s), want 1", n)
	}

	// THE SAME SNAPSHOT THE RECORD IS WRITTEN FROM. A second
	// client.Stats() call here would be a second reading of a moving
	// counter, so the live number and the durable one would disagree
	// about the same acquisition by whatever arrived between them.
	count := calls(fn, "count")
	if len(count) != 1 || len(count[0]) != 2 {
		t.Fatalf("acquireOnce6 writes the durable record %d time(s); this test reads the "+
			"fold's argument against that call's", len(count))
	}
	if fold[0][0] != count[0][1] {
		t.Errorf("acquireOnce6 folds router discovery from %q and writes the record from "+
			"%q. Two readings of a counter that is still moving make the live number and "+
			"the durable one disagree about the same acquisition", fold[0][0], count[0][1])
	}

	// A NEW MANAGER'S SNAPSHOT STARTS AT ZERO. getIP6 runs this
	// function twice through one options value, so without this the
	// second pass's gains are subtracted away. See managerStarted.
	if got := calls(fn, "managerStarted"); len(got) != 1 {
		t.Errorf("acquireOnce6 calls managerStarted %d time(s), want exactly one: on "+
			"getIP6's errV6HintInUse retry this function runs again through the SAME "+
			"options value, and a snapshot left over from the first pass takes the whole "+
			"of the second acquisition away", len(got))
	}
}
