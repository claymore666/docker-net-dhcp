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

// The path from a hostname's trust verdict to its record needs CAP_NET_ADMIN and a DHCP
// server, so this checks by source that every hostname argument came from a resolver (#726).
func TestHostnameTrustIsWired(t *testing.T) {
	resolvers := map[string]bool{
		"initialDHCPHostname": true,
		"recoveredHostname":   true,
		"safeHostname":        true,
	}
	sinks := map[string]int{
		"rememberEndpoint": 2,
		"consumeTombstone": 1,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	fset := token.NewFileSet()
	seen := map[string]int{}
	scanned := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			trusted := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Rhs) != 1 {
					return true
				}
				call, ok := as.Rhs[0].(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !resolvers[sel.Sel.Name] {
					return true
				}
				for _, lhs := range as.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name != "_" {
						trusted[id.Name] = true
					}
				}
				return true
			})

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				idx, ok := sinks[sel.Sel.Name]
				if !ok || idx >= len(call.Args) {
					return true
				}
				seen[sel.Sel.Name]++

				pos := fset.Position(call.Args[idx].Pos())
				id, ok := call.Args[idx].(*ast.Ident)
				if !ok {
					t.Errorf("%s:%d: %s passes a CONSTRUCTED hostname to %s. It must pass the value it got "+
						"from initialDHCPHostname / recoveredHostname / safeHostname, whole and unaltered. "+
						"A dhcpHostname built here can claim a name is trusted when nothing decided that, and "+
						"dhcpHostname{} is #726 itself: an empty name with refused=false, which the tombstone "+
						"store reads as a match against every container on the network",
						pos.Filename, pos.Line, fn.Name.Name, sel.Sel.Name)
					return true
				}
				if !trusted[id.Name] {
					t.Errorf("%s:%d: %s passes %q to %s, and %q was not bound from a hostname resolver in this "+
						"function. The trust bit has to arrive from whoever DECIDED it — carrying a hostname "+
						"across a function on a local nobody set from a resolver is how #726 happened",
						pos.Filename, pos.Line, fn.Name.Name, id.Name, sel.Sel.Name, id.Name)
				}
				return true
			})
		}
	}

	if scanned == 0 {
		t.Fatal("scanned no non-test .go files; every check above would have passed vacuously")
	}
	for callee, want := range map[string]int{"rememberEndpoint": 3, "consumeTombstone": 2} {
		if seen[callee] < want {
			t.Errorf("found %d call sites of %s, want at least %d. Either a path that records or consumes an "+
				"identity no longer goes through it — in which case that path is unguarded — or this gate has "+
				"stopped finding the code it claims to check",
				seen[callee], callee, want)
		}
	}
}
