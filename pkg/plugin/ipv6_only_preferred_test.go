// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// RFC 8925 section 3.2 forbids an IPv4-requiring host to ask for option 108, and every client path here is one (#1027);
// the scan is by name, so a bare 108 is for TestBuildParams_NeverAsksForIPv6OnlyPreferred and proto.New.
func TestNoClientPathSetsIPv6OnlyPreferred(t *testing.T) {
	var files []string
	for _, dir := range []string{".", "../dhcp"} {
		fs, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, fs...)
	}
	fset := token.NewFileSet()
	literals := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		tree, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(tree, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "IPv6OnlyPreferred" || x.Name == "OptIPv6OnlyPreferred" {
					t.Errorf("%s: %s is named; this plugin never asks for option 108 (RFC 8925 section 3.2, #1027)", fset.Position(x.Pos()), x.Name)
				}
			case *ast.CompositeLit:
				sel, ok := x.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "DHCPClientOptions" {
					return true
				}
				for _, e := range x.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "VendorClass" {
							literals[filepath.Base(f)]++
						}
					}
				}
			}
			return true
		})
	}
	want := map[string]int{"parent_attached.go": 1, "network.go": 1, "dhcp_manager.go": 1, "ipam_reserve.go": 1, "ipam_endpoint.go": 1}
	for f, n := range want {
		if literals[f] != n {
			t.Errorf("%s holds %d DHCPClientOptions literals with VendorClass, want %d: a site moved or a new one needs a look", f, literals[f], n)
		}
	}
	for f := range literals {
		if _, ok := want[f]; !ok {
			t.Errorf("%s holds a DHCPClientOptions literal with VendorClass that this test does not list", f)
		}
	}
}
