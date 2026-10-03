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

// GetIP opens a real packet socket, so no behaviour test reaches its end-of-run fold. An ACK discarded for lacking
// option 90 is counted while acquiring, before any lease is bound, so the DHCPv4 one-shot is where it is first
// counted (#1119). Keyed on spelling, like the router-discovery check for acquireOnce6.

func TestOneShotAcquisition_FoldsItsOwnForcerenewCounters(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "chassis.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing chassis.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "GetIP" && f.Recv == nil {
			fn = f
		}
	}
	if fn == nil {
		t.Fatal("no GetIP in chassis.go: an empty domain satisfies every rule below without checking anything")
	}
	args := func(name string) [][]string {
		var found [][]string
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != name {
				return true
			}
			var as []string
			for _, a := range call.Args {
				var b strings.Builder
				if err := printer.Fprint(&b, fset, a); err != nil {
					t.Fatalf("printing: %v", err)
				}
				as = append(as, b.String())
			}
			found = append(found, as)
			return true
		})
		return found
	}
	fold, count := args("forcerenewReport"), args("count")
	if len(fold) != 1 || len(fold[0]) != 1 {
		t.Fatalf("GetIP folds the FORCERENEW counters %d time(s), want exactly one with one argument; a "+
			"one-shot is where a discarded ACK is first counted and it never reaches translate()", len(fold))
	}
	if len(count) != 1 || len(count[0]) != 2 || fold[0][0] != count[0][1] {
		t.Errorf("GetIP folds from %v and writes the record from %v; two readings of a moving counter make the "+
			"live number and the durable one disagree", fold, count)
	}
}
