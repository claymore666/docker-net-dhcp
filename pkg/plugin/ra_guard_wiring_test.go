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

// The Router-Advertisement guard has to be asked for, and it has to be
// asked for in exactly one place (#875).
//
// WHY THIS IS A SOURCE-LEVEL TEST AND NOT A BEHAVIOURAL ONE. The value
// this asserts is one field of one struct literal handed to a package
// that spawns a real dhcpcd inside a real network namespace. There is
// no seam between setupClient and that spawn, so a behavioural test
// here would either need a live container or would have to stub the
// thing it is trying to observe. What can be checked without either is
// the WIRING, and the wiring is where this would break: pkg/dhcp
// refuses the guard on the wrong client shape, so the failure this
// cannot see from below is not "it was set wrongly" but "it stopped
// being set at all", which is exactly a missing field in a literal.
//
// The other direction matters as much. The guard writes a container's
// IPv6 host configuration; on the CreateEndpoint one-shot that link is
// still in the HOST network namespace, so setting it there would change
// the host's own router-discovery behaviour. pkg/dhcp refuses that
// combination, but a refusal at CreateEndpoint is a failed container
// start, which is not a way to find out.
func TestRAGuard_AskedForOnceAndOnlyByThePersistentV6Client(t *testing.T) {
	// Every non-test .go file in the package, not a named one: the
	// claim is that NO OTHER construction site sets the flag, and a
	// claim about "no other" cannot be checked against one file.
	// parser.ParseFile in a loop rather than parser.ParseDir, which is
	// deprecated as of Go 1.25.
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

	type site struct {
		pos   string
		value string // the expression assigned, or "" when the field is absent
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
			s := site{pos: fset.Position(lit.Pos()).String()}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "HonorRouterAdverts" {
					continue
				}
				if id, ok := kv.Value.(*ast.Ident); ok {
					s.value = id.Name
				} else {
					s.value = "<not an identifier>"
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

	var setting []site
	for _, s := range literals {
		if s.value != "" {
			setting = append(setting, s)
		}
	}
	if len(setting) != 1 {
		t.Fatalf("HonorRouterAdverts is set at %d of %d DHCPClientOptions literals, want "+
			"exactly 1 (the persistent client in setupClient): %+v", len(setting), len(literals), setting)
	}
	// `v6`, not `true`: setupClient builds BOTH families from one
	// literal, so a hardcoded true would turn the guard on for the
	// DHCPv4 client as well -- and pkg/dhcp refuses that combination,
	// which would break every IPv4 lease rather than degrade one.
	if setting[0].value != "v6" {
		t.Errorf("HonorRouterAdverts is set to %q at %v, want the v6 parameter; "+
			"setupClient renders both families from this literal",
			setting[0].value, setting[0].pos)
	}
	if !strings.Contains(setting[0].pos, "dhcp_manager.go") {
		t.Errorf("HonorRouterAdverts is set at %v, expected the persistent client in "+
			"dhcp_manager.go", setting[0].pos)
	}
}
