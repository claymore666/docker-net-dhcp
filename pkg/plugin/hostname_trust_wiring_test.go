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

// TestHostnameTrustIsWired is the observer for the SEGMENT between where
// a hostname's trust is decided and where it is recorded.
//
// # WHY A SOURCE GATE AND NOT A TEST
//
// #726 was that both CreateEndpoint paths held the trust bit at their
// consumeTombstone call and dropped it two hundred lines later at their
// rememberEndpoint call. The first fix made it a `hostnameTrusted bool`
// parameter. Both ends of that wire were then pinned by real tests --
// initialDHCPHostname's trust flag at the source, tombstoneStore.consume
// at the sink -- and the segment between them was observed by NOTHING:
// substituting a literal `true` at both call sites left the entire
// package green while restoring the vulnerability in full.
//
// A runtime test cannot reach that segment. It begins inside
// CreateEndpoint after netlink.LinkAdd has made a veth pair and ends
// after a DHCP acquisition, so observing it needs CAP_NET_ADMIN and a
// live DHCP server; the unit suite has neither, and an integration test
// would only cover whichever of the two paths it exercised.
//
// So the wire is made unbreakable instead of watched. The hostname and
// its trust bit are one value (dhcpHostname) that a caller cannot take
// apart, which turns the literal-`true` mutant into a COMPILE error --
// a stronger observer than any test, because it cannot be skipped and
// costs nothing to run.
//
// This gate closes the residue: a dhcpHostname literal built at the call
// site still compiles, and `dhcpHostname{}` is the original bug exactly
// (empty name, refused=false, which the tombstone store reads as
// "matches every container on this network"). Every hostname argument
// must therefore be a plain identifier that this same function got from
// the plugin's own hostname resolvers. A constructed value, a zero
// value, or an identifier from somewhere else goes red here.
func TestHostnameTrustIsWired(t *testing.T) {
	// The functions that DECIDE trust. An identifier bound from one of
	// these carries a verdict; anything else is a value someone made up.
	resolvers := map[string]bool{
		"initialDHCPHostname": true,
		"recoveredHostname":   true,
		"safeHostname":        true,
	}
	// callee -> index of the hostname argument.
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

			// Pass 1: which identifiers in this function hold a
			// resolver's verdict?
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

			// Pass 2: every sink's hostname argument must be one of them.
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

	// TWO-SIDED ON PURPOSE. Everything above is a rule about call sites
	// that exist, so it is satisfied completely by there being none —
	// which is exactly what a refactor that deleted the wire would look
	// like, and exactly the mutant this gate exists to kill.
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
