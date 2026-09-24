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

// Keyed on spelling (#1050): a call through a wrapper reads as a violation, and a function merely named openOnLink
// passes.

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
