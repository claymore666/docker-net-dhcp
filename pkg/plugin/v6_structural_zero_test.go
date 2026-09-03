// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// endpoints.go carries a claim about what the 2.0 beta ships for the
// four DHCPv6 health counters and the `config` audit kind, verbatim:
//
//	EVERY v6 FIELD BELOW IS STRUCTURALLY ZERO IN THE 2.0 BETA
//	... A network created with ipv6=true is refused at CreateNetwork
//	(P-8, returns at M7). So no v6 client is ever constructed and
//	nothing increments any of these -- including dhcpv6_config_only,
//	dhcpv6_not_offered, dhcpv6_no_router_advert and
//	ipv6_link_enable_failures further down, and the `config` kind in
//	the audit ledger, whose only writer is the information reply.
//
// That claim is load-bearing: an operator reading a zero in
// /health has to know whether it means "nothing went wrong" or "this
// build cannot report it", and every one of these is a documented row
// of docs/reference.md. Until this file it rested on nothing but the
// comment. The two tests below are its two premises.
//
// They are premises, not the whole claim. Neither says the counters
// stay zero at RUNTIME -- that would need a v6 network, which is the
// very thing premise one says cannot exist. What they hold is: the
// refusal is real for every mode and spelling, and the population of
// writers is still exactly the one the comment enumerated. Break
// either and the comment has to be rewritten rather than quietly
// falsified.

// Premise one: the refusal exists and covers every mode.
//
// It is the only thing standing between an operator and a network on
// which the v6 paths run, and nothing asserted it. The other direction
// matters as much and is asserted as the control: the SAME options
// without ipv6 must not be refused, or the test would pass just as
// well against a validator that rejects everything.
func TestIPv6Beta_EveryModeRefusesIPv6(t *testing.T) {
	// Each row is otherwise VALID for its mode, so a refusal can only
	// come from the ipv6 arm. The control below proves that.
	for _, tc := range []struct {
		name string
		opts DHCPNetworkOptions
	}{
		{"bridge (the default mode, named)", DHCPNetworkOptions{Mode: "bridge", Bridge: "br-dhcptest"}},
		{"bridge (mode left empty, the default)", DHCPNetworkOptions{Bridge: "br-dhcptest"}},
		{"macvlan", DHCPNetworkOptions{Mode: "macvlan", Parent: "eth0"}},
		{"ipvlan", DHCPNetworkOptions{Mode: "ipvlan", Parent: "eth0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The control FIRST: without ipv6 this row must be
			// accepted. If it is not, the refusal asserted below
			// proves nothing about ipv6.
			if err := validateModeOptions(tc.opts); err != nil {
				t.Fatalf("the control (ipv6 unset) was refused with %v; this row is not a "+
					"valid network, so the ipv6 assertion below would be vacuous", err)
			}

			v6 := tc.opts
			v6.IPv6 = true
			err := validateModeOptions(v6)
			if !errors.Is(err, util.ErrIPv6Beta) {
				t.Fatalf("validateModeOptions(ipv6=true) = %v, want ErrIPv6Beta. This "+
					"refusal is the premise under the structural-zero claim in "+
					"endpoints.go: without it a v6 network can be created and the four "+
					"DHCPv6 counters and the `config` audit kind stop being "+
					"unreachable-by-construction", err)
			}
		})
	}
}

// v6WriterSite is one place that increments a structurally-zero counter
// or writes the `config` audit kind.
//
// It is a MAP KEY, so it carries only the three fields the claim is
// about. The source position deliberately is not one of them: a
// position changes whenever anything above it does, and a key that
// moves with unrelated edits would turn this test into a line-number
// check. Positions are carried alongside, for the failure message.
type v6WriterSite struct {
	file string
	fn   string
	what string
}

// Premise two: the writers are still the ones the comment enumerated.
//
// The comment's "nothing increments any of these" is a claim about a
// POPULATION, and a population claim written beside the code is an
// unrun checklist. A new writer added on a path that is not gated on
// v6 -- the plausible shape being a counter reused for something the
// v4 client can hit -- would make the comment false with no test
// failing anywhere, because in an IPv4-only lane the new writer is the
// only one that ever fires and a passing suite proves nothing about it.
//
// So: enumerate the writers from the AST and fail on any site the
// comment does not name. This does not check that each site is inside
// an `if v6` -- an AST proof of that is brittle and would break on a
// harmless refactor. It checks the weaker, sharper thing: that the SET
// has not changed. A new site fails here and forces whoever added it to
// re-argue the endpoints.go claim, which is the actual obligation.
func TestV6StructuralZero_TheWritersAreStillTheEnumeratedFour(t *testing.T) {
	// The four counter fields, by their Go identifier, and the audit
	// kind. Named here rather than derived from metrics.go: this test
	// is the checklist, and a checklist that derives its own items from
	// the thing it checks cannot notice an item going missing.
	counters := map[string]bool{
		"dhcpv6ConfigOnly":       true,
		"dhcpv6NotOffered":       true,
		"dhcpv6NoRouterAdvert":   true,
		"ipv6LinkEnableFailures": true,
	}

	// What the endpoints.go comment claims, as data. file/func/what.
	want := map[v6WriterSite]bool{
		{file: "dhcp_manager.go", fn: "handleEvent", what: "dhcpv6ConfigOnly"}:        true,
		{file: "dhcp_manager.go", fn: "handleEvent", what: `audit("config")`}:         true,
		{file: "v6_absence.go", fn: "noteV6Absence", what: "dhcpv6NotOffered"}:        true,
		{file: "v6_absence.go", fn: "noteV6Absence", what: "dhcpv6NoRouterAdvert"}:    true,
		{file: "v6_link.go", fn: "ensureIPv6Enabled", what: "ipv6LinkEnableFailures"}: true,
	}

	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()

	got := map[v6WriterSite]string{} // site -> position, for the message
	parsed := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++

		// enclosing returns the name of the FuncDecl containing pos,
		// or "" at file scope.
		enclosing := func(pos token.Pos) string {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if ok && fd.Pos() <= pos && pos < fd.End() {
					return fd.Name.Name
				}
			}
			return "<file scope>"
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			// p.<counter>.Add(...) / m.plugin.<counter>.Add(...):
			// only Add, never Load -- endpoints.go READS all four to
			// render /health and those reads are not writers.
			if sel.Sel.Name == "Add" {
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && counters[inner.Sel.Name] {
					s := v6WriterSite{file: filepath.Base(name), fn: enclosing(call.Pos()), what: inner.Sel.Name}
					got[s] = fset.Position(call.Pos()).String()
				}
				return true
			}

			// <recv>.audit("config", ...)
			if sel.Sel.Name == "audit" && len(call.Args) > 0 {
				lit, ok := call.Args[0].(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && lit.Value == `"config"` {
					s := v6WriterSite{file: filepath.Base(name), fn: enclosing(call.Pos()), what: `audit("config")`}
					got[s] = fset.Position(call.Pos()).String()
				}
			}
			return true
		})
	}
	if parsed == 0 {
		t.Fatal("parsed no files: this test's domain is empty and every assertion below is vacuous")
	}
	if len(got) == 0 {
		t.Fatal("found no writer of any structurally-zero v6 counter and no `config` audit " +
			"write in pkg/plugin. Either the identifiers were renamed out from under this " +
			"test, or the writers really are gone -- in which case the endpoints.go comment " +
			"that says M7 restores them unchanged is now wrong too")
	}

	for s, pos := range got {
		if !want[s] {
			t.Errorf("a writer the endpoints.go structural-zero claim does not name: %s "+
				"writes %s in %s (%s). That comment tells operators a zero in /health means "+
				"\"not reachable in this build\"; a writer it does not enumerate may be "+
				"reachable from the IPv4 path, where nothing in this lane would ever see it. "+
				"Re-argue the comment, then add the site here.", s.fn, s.what, s.file, pos)
		}
	}
	for s := range want {
		if _, ok := got[s]; !ok {
			t.Errorf("the endpoints.go structural-zero claim names %s writing %s in %s, and "+
				"it is gone. If it was deleted rather than moved, the comment's promise that "+
				"M7 restores these writers unchanged no longer holds.", s.fn, s.what, s.file)
		}
	}
}
