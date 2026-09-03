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
// exactly one place.
//
// WHERE THIS CAME FROM. Until the chassis swap this same structural
// property was held for the Router-Advertisement guard by
// ra_guard_wiring_test.go, which the v6 retirement deleted along with
// the field it watched. The property did not go away with it: it moved
// to `V6`, which is now the only field of dhcp.DHCPClientOptions whose
// value decides which family a client leases in, and which nothing
// asserted between that deletion and this test.
//
// WHY THIS IS A SOURCE-LEVEL TEST AND NOT A BEHAVIOURAL ONE. The value
// asserted is one field of one struct literal handed to a package that
// opens a raw socket in a real network namespace. There is no seam
// between setupClient and that socket, so a behavioural test here would
// need either a live container or a stub of the very thing it is trying
// to observe. What is checkable without either is the WIRING, and the
// wiring is where this breaks: the failure a lower-level test cannot see
// is not "it was set wrongly" but "it stopped being set from the
// parameter", which is exactly a changed field in a literal.
//
// WHAT EACH DIRECTION COSTS, in THIS build. The two arms fail
// differently, which is why both are asserted rather than just the
// presence of the field:
//
//   - Hardcoded `V6: true` at any site: pkg/dhcp refuses it with
//     ErrIPv6Unsupported at every entry point, so every container start
//     on that path fails outright. Loud, and caught by any integration
//     run.
//
//   - Hardcoded `V6: false` at the dhcp_manager.go site: setupClient is
//     the ONE call rendered for both families by the legacy dual-stack
//     path, so a constant false there silently starts a SECOND IPv4
//     client where a v6 one was asked for. The beta's IPv4-only refusal
//     — the thing that is supposed to make the missing family visible —
//     then never fires at all, and the endpoint quietly gets two v4
//     clients racing for one lease. Silent, and no integration run in
//     this lane would show it, because every one of them is IPv4.
//
// The second arm is the one this test exists for. It is also why the
// assertion is `v6` the identifier and not merely "an expression":
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
				if !ok || key.Name != "V6" {
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
		t.Fatalf("V6 is set at %d of %d DHCPClientOptions literals, want exactly 1 "+
			"(the persistent client in setupClient); the others leave it zero, which "+
			"is this build's only supported family: %+v", len(setting), len(literals), setting)
	}
	// `v6`, not `true` and not `false`: setupClient renders BOTH
	// families from this one literal. See the two arms in the comment
	// above — the false arm is silent.
	if setting[0].value != "v6" {
		t.Errorf("V6 is set to %q at %v, want the v6 parameter of setupClient; a constant "+
			"here either fails every container start (true, ErrIPv6Unsupported) or "+
			"silently starts a second IPv4 client where a v6 one was asked for (false)",
			setting[0].value, setting[0].pos)
	}
	if !strings.Contains(setting[0].pos, "dhcp_manager.go") {
		t.Errorf("V6 is set at %v, expected the persistent client in dhcp_manager.go",
			setting[0].pos)
	}
}
