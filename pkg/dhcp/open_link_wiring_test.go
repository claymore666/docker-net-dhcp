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

// TestBothFamiliesOpenThroughTheIndex is the drift check the behaviour
// tests cannot make.
//
// WHY IT NEEDS A WIRING TEST. newLibClient and newLibClient6 open real
// packet sockets on a real interface in a real namespace, so nothing in
// this package executes either of them; openOnLink's own tests drive
// the retry with a fake open and say nothing about who calls it.
// MEASURED: restoring either constructor's direct call to the library
// leaves the whole of ./pkg/... green.
//
// WHAT IT ASSERTS. Each constructor reaches the library through
// openOnLink, hands it the caller's index, and makes no library call of
// its own outside the closure openOnLink drives. The second half is the
// half that matters: a constructor that keeps one direct call for its
// own namespace-free path is a family that opens by name again, on the
// route where an index was offered and ignored.
//
// THE BOUND. Keyed on SPELLING, like the other wiring checks in this
// package. A call reached through a wrapper reads as a violation though
// it is correct, and a function merely named openOnLink reads as
// correct though it is not. It stands beside the behaviour tests and
// not instead of them.
func TestBothFamiliesOpenThroughTheIndex(t *testing.T) {
	for _, cell := range []struct {
		file, fn, libCall string
	}{
		{"chassis.go", "newLibClient", "NewClient"},
		{"chassis6.go", "newLibClient6", "NewClient6"},
	} {
		t.Run(cell.fn, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, cell.file, nil, 0)
			if err != nil {
				t.Fatalf("parsing %s: %v", cell.file, err)
			}

			var decl *ast.FuncDecl
			for _, d := range file.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == cell.fn {
					decl = fn
				}
			}
			if decl == nil {
				t.Fatalf("%s no longer declares %s: this check has lost its subject", cell.file, cell.fn)
			}

			spell := func(n ast.Node) string {
				var b strings.Builder
				if err := printer.Fprint(&b, fset, n); err != nil {
					t.Fatalf("printing %T: %v", n, err)
				}
				return b.String()
			}

			var (
				litRanges [][2]token.Pos
				indexArgs []string
				direct    []string
			)
			ast.Inspect(decl, func(n ast.Node) bool {
				if lit, ok := n.(*ast.FuncLit); ok {
					litRanges = append(litRanges, [2]token.Pos{lit.Pos(), lit.End()})
				}
				return true
			})
			inClosure := func(pos token.Pos) bool {
				for _, r := range litRanges {
					if pos > r[0] && pos < r[1] {
						return true
					}
				}
				return false
			}

			ast.Inspect(decl, func(n ast.Node) bool {
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
				switch name {
				case "openOnLink":
					if len(call.Args) > 1 {
						indexArgs = append(indexArgs, spell(call.Args[1]))
					}
				case cell.libCall:
					if !inClosure(call.Pos()) {
						direct = append(direct, spell(call))
					}
				}
				return true
			})

			if len(indexArgs) == 0 {
				t.Fatalf("%s opens no client through openOnLink: the name it resolves is the one it was "+
					"handed, however long ago the engine renamed the link (#1050)", cell.fn)
			}
			for _, arg := range indexArgs {
				if arg != "opts.LinkIndex" {
					t.Errorf("%s opens on index %q, want opts.LinkIndex: an index the caller did not "+
						"give resolves some other link", cell.fn, arg)
				}
			}
			if len(direct) != 0 {
				t.Errorf("%s still calls the library directly: %v. Those opens resolve the name they "+
					"were handed and are exactly the window this closes", cell.fn, direct)
			}
		})
	}
}
