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

// A literal V6: false silently starts a second IPv4 client where v6 was asked, so V6 and
// HonorRouterAdverts must both come from setupClient's v6 parameter (#911).
func TestFamilyFlag_AskedForOnceAndOnlyFromTheV6Parameter(t *testing.T) {
	// parser.ParseDir is deprecated as of Go 1.25.
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("parsed no files: the search below has an empty domain")
	}

	want := map[string]string{"V6": "v6", "HonorRouterAdverts": "v6"}

	type site struct {
		pos    string
		values map[string]string
	}
	var literals []site

	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "DHCPClientOptions" {
				return true
			}
			s := site{pos: fset.Position(lit.Pos()).String(), values: map[string]string{}}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				if _, watched := want[key.Name]; !watched {
					continue
				}
				if id, ok := kv.Value.(*ast.Ident); ok {
					s.values[key.Name] = id.Name
				} else {
					s.values[key.Name] = "<not an identifier>"
				}
			}
			literals = append(literals, s)
			return true
		})
	}

	if len(literals) == 0 {
		t.Fatal("found no dhcp.DHCPClientOptions literal in pkg/plugin: this test's " +
			"domain is empty and every assertion below is vacuous")
	}

	for field, wantValue := range want {
		var setting []site
		for _, s := range literals {
			if _, ok := s.values[field]; ok {
				setting = append(setting, s)
			}
		}
		if len(setting) != 1 {
			t.Fatalf("%s is set at %d of %d DHCPClientOptions literals, want exactly 1 "+
				"(the persistent client in setupClient); the others leave it zero, which "+
				"is the IPv4 one-shot's shape: %+v", field, len(setting), len(literals), setting)
		}
		if got := setting[0].values[field]; got != wantValue {
			t.Errorf("%s is set to %q at %v, want the %q parameter of setupClient; see the "+
				"arms above for what a constant costs on each side",
				field, got, setting[0].pos, wantValue)
		}
		if !strings.Contains(setting[0].pos, "dhcp_manager.go") {
			t.Errorf("%s is set at %v, expected the persistent client in dhcp_manager.go",
				field, setting[0].pos)
		}
	}
}
