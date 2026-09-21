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

// The property no behaviour test in this package can reach: every
// lookup of a host-side veth by its generated `dh-<12 hex>` name goes
// through hostLinkByGeneratedName, and the one exception is the rename
// itself, whose lookup has to stay inside its own write section (#1051).
//
// A call site added later is the whole risk. Four sites already derive
// that name without reading one back from the kernel, the file that
// renames the link says no future call site should have to be
// remembered, and a fifth reading it raw would be invisible to every
// cell here: the window is two kernel calls wide and nothing unit-sized
// lands in it by chance.
func TestHostLinkLookupsGoThroughTheGuard(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing the package: %v", err)
	}

	fset := token.NewFileSet()
	var (
		raw          []string
		guardedRead  bool
		guardedWrite bool
		sawGuard     bool
		sawRename    bool
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
			}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 1 {
					return true
				}
				arg, ok := call.Args[0].(*ast.Ident)
				if !ok || arg.Name != "hostName" {
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

	if !sawGuard || !sawRename {
		t.Fatalf("this test lost its subject: hostLinkByGeneratedName found=%v, renameHostLink "+
			"found=%v. It asserts nothing until both are back", sawGuard, sawRename)
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

// callsOn reports whether the body calls recv.method(), which is how
// both halves of the guard are taken.
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

// isLinkByName reports whether the call is the seam or the netlink call
// it stands in for.
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
