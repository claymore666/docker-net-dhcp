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

// The address family has to be asked for, and it has to be asked for in
// exactly one place -- and the same goes for the Router-Advertisement
// precondition that rides beside it.
//
// WHERE THIS CAME FROM. Until the chassis swap this same structural
// property was held for the Router-Advertisement guard by
// ra_guard_wiring_test.go, which the v6 retirement deleted along with
// the field it watched. The property did not go away with it: it moved
// to `V6`, which is the field of dhcp.DHCPClientOptions whose value
// decides which family a client leases in, and which nothing asserted
// between that deletion and this test. #911 gave `V6` a live v6 arm and
// brought the guard's field back as `HonorRouterAdverts`, so both are
// asserted here rather than in two tests reading the same literal.
//
// WHY THIS IS A SOURCE-LEVEL TEST AND NOT A BEHAVIOURAL ONE. The values
// asserted are two fields of one struct literal handed to a package that
// opens a raw socket in a real network namespace. There is no seam
// between setupClient and that socket, so a behavioural test here would
// need either a live container or a stub of the very thing it is trying
// to observe. What is checkable without either is the WIRING, and the
// wiring is where this breaks: the failure a lower-level test cannot see
// is not "it was set wrongly" but "it stopped being set from the
// parameter", which is exactly a changed field in a literal.
//
// WHAT EACH DIRECTION COSTS, in THIS build. The arms fail differently,
// which is why the identifier is asserted rather than the presence of
// the field:
//
//   - Hardcoded `V6: true`: every endpoint on the path starts a DHCPv6
//     client and no v4 one, so the container comes up with no IPv4
//     address at all. Loud, and caught by any integration run.
//
//   - Hardcoded `V6: false`: setupClient is the ONE call rendered for
//     both families by the dual-stack path, so a constant false silently
//     starts a SECOND IPv4 client where a v6 one was asked for. The
//     endpoint reports healthy, both clients race for one lease, and the
//     v6 address simply never appears -- there is no refusal left to
//     make the missing family visible now that v6 is wired. Silent, and
//     no integration run in the IPv4 lane would show it.
//
//   - `HonorRouterAdverts` hardcoded either way is loud on one family
//     and only that family: dhcp.checkRouterAdvertGuardShape refuses a
//     persistent v6 client without it and refuses every other shape with
//     it (D30 Q3), so a constant fails every start on one side of the
//     split. It is asserted here anyway, because the shape that is NOT
//     loud is the two fields drifting apart -- `V6: v6` beside
//     `HonorRouterAdverts: someOtherBool` reads correct and is refused
//     at runtime for a reason no log line connects back to this literal.
//
// The `V6: false` arm is the one this test exists for. It is also why
// the assertion is the identifier `v6` and not merely "an expression":
// `V6: someBool` where someBool is computed anywhere but the parameter
// is the same silent failure with more steps.
func TestFamilyFlag_AskedForOnceAndOnlyFromTheV6Parameter(t *testing.T) {
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

	// The fields whose value is read out of each literal, and the one
	// identifier each has to carry. Both are set from setupClient's `v6`
	// parameter: the family and its Router-Advertisement precondition
	// are one decision, and a literal that spells them differently is
	// the drift the third arm above describes.
	want := map[string]string{"V6": "v6", "HonorRouterAdverts": "v6"}

	type site struct {
		pos    string
		values map[string]string // field -> expression assigned, absent when unset
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
