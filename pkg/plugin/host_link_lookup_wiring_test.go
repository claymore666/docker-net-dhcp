// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every lookup of a host veth by its generated name goes through
// hostLinkByGeneratedName, except the rename's own lookup inside its write section (#1051).
func TestHostLinkLookupsGoThroughTheGuard(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing the package: %v", err)
	}

	fset := token.NewFileSet()
	var (
		raw          []string
		guarded      int
		guardedRead  bool
		guardedWrite bool
		sawGuard     bool
		sawRename    bool
		sawSources   int
	)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			switch fn.Name.Name {
			case "hostLinkByGeneratedName":
				sawGuard = true
				guardedRead = callsOn(fn.Body, "hostLinkNaming", "RLock")
			case "renameHostLink":
				sawRename = true
				guardedWrite = callsOn(fn.Body, "hostLinkNaming", "Lock")
			case "vethPairNames", "subLinkName":
				sawSources++
			}

			tainted := generatedNames(fn.Body)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 1 {
					return true
				}
				if !isGeneratedName(call.Args[0], tainted) {
					return true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "hostLinkByGeneratedName" {
					guarded++
					return true
				}
				if !isLinkByName(call.Fun) {
					return true
				}
				if name == "host_ifname.go" && fn.Name.Name == "renameHostLink" {
					return true
				}
				raw = append(raw, name+": "+fn.Name.Name)
				return true
			})
		}
	}

	if !sawGuard || !sawRename || sawSources != 2 {
		t.Fatalf("this test lost its subject: hostLinkByGeneratedName found=%v, renameHostLink "+
			"found=%v, name sources found=%d of 2. It asserts nothing until they are back",
			sawGuard, sawRename, sawSources)
	}
	if guarded < 4 {
		t.Fatalf("this test found only %d guarded lookup(s) of a generated name and the package has "+
			"more than that, so following the name stopped working and a raw lookup would now pass "+
			"unseen. Repair the walk before trusting the result", guarded)
	}
	if !guardedRead {
		t.Errorf("hostLinkByGeneratedName does not take hostLinkNaming for reading, so every site " +
			"that calls it can be told the host has no link with this endpoint's generated name " +
			"while another endpoint's rename is between its two kernel calls (#1051)")
	}
	if !guardedWrite {
		t.Errorf("renameHostLink does not take hostLinkNaming for writing, so the window it opens is " +
			"closed to nobody and the guarded readers wait for nothing (#1051)")
	}
	if len(raw) != 0 {
		t.Errorf("these look a host-side veth up by its generated name without the guard: %v. "+
			"DeleteEndpoint reads a miss as a teardown that already happened, so a raw lookup "+
			"landing in a rename leaves the veth on the bridge for the life of the host; use "+
			"hostLinkByGeneratedName (#1051)", raw)
	}
}

func callsOn(body *ast.BlockStmt, recv, method string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == recv {
			found = true
		}
		return true
	})
	return found
}

func isLinkByName(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "nlLinkByName"
	case *ast.SelectorExpr:
		pkg, ok := f.X.(*ast.Ident)
		return ok && pkg.Name == "netlink" && f.Sel.Name == "LinkByName"
	}
	return false
}

func generatedNames(body *ast.BlockStmt) map[string]bool {
	tainted := map[string]bool{}
	for pass := 0; pass < 2; pass++ {
		ast.Inspect(body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 {
				return true
			}
			switch rhs := as.Rhs[0].(type) {
			case *ast.CallExpr:
				fn, ok := rhs.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch fn.Name {
				case "vethPairNames":
					if len(as.Lhs) == 2 {
						mark(tainted, as.Lhs[0])
					}
				case "subLinkName":
					if len(as.Lhs) == 1 {
						mark(tainted, as.Lhs[0])
					}
				}
			case *ast.Ident:
				if len(as.Lhs) == 1 && tainted[rhs.Name] {
					mark(tainted, as.Lhs[0])
				}
			}
			return true
		})
	}
	return tainted
}

func mark(tainted map[string]bool, lhs ast.Expr) {
	if ident, ok := lhs.(*ast.Ident); ok && ident.Name != "_" {
		tainted[ident.Name] = true
	}
}

func isGeneratedName(arg ast.Expr, tainted map[string]bool) bool {
	switch a := arg.(type) {
	case *ast.Ident:
		return tainted[a.Name]
	case *ast.CallExpr:
		fn, ok := a.Fun.(*ast.Ident)
		return ok && fn.Name == "subLinkName"
	}
	return false
}
