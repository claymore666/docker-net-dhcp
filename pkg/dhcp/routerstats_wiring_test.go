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

// acquireOnce6 opens a real packet socket, so no behaviour test reaches its fold, and a CreateEndpoint one-shot's
// router-discovery counters are counted there or nowhere (#814). Keyed on spelling: a wrapper reads as a violation, a
// function merely named routerReport passes.

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

	// One snapshot: a second client.Stats() call would read a moving counter twice (#814).
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

	// getIP6 runs this twice through one options value, so each new manager's snapshot must start at zero (#814).
	if got := calls(fn, "managerStarted"); len(got) != 1 {
		t.Errorf("acquireOnce6 calls managerStarted %d time(s), want exactly one: on "+
			"getIP6's errV6HintInUse retry this function runs again through the SAME "+
			"options value, and a snapshot left over from the first pass takes the whole "+
			"of the second acquisition away", len(got))
	}
}
