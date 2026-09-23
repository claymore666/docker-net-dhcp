// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockParent_TheCallSitesNameTheKindTheyAreAttaching(t *testing.T) {
	t.Run("the preflight probe", func(t *testing.T) {
		p := &Plugin{}
		holder := p.lockParent(context.Background(), probeGateParent, ModeMacvlan, "test-holder")
		defer holder.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_ = p.runDHCPProbe(ctx, probeGateParent, ModeMacvlan, serverPolicy{})
		assertSameKindGiveUp(t, p, "dhcp_probe.go")
	})

	t.Run("the IPAM reservation link", func(t *testing.T) {
		p := &Plugin{}
		holder := p.lockParent(context.Background(), probeGateParent, ModeMacvlan, "test-holder")
		defer holder.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := p.addIPAMReserveLink(ctx, "dh-kind-a", "dh-kind-b", ModeMacvlan,
			DHCPNetworkOptions{Parent: probeGateParent}, nil); err == nil {
			t.Fatal("the reservation link was built against a parent that does not exist")
		}
		assertSameKindGiveUp(t, p, "ipam_reserve.go")
	})

	t.Run("every site names a mode variable", func(t *testing.T) {
		sites := lockParentCallSites(t)
		if len(sites) < 4 {
			t.Fatalf("found %d lockParent call site(s) in the plugin, want at least the 4 "+
				"known ones. Fewer means this check has stopped matching and is passing "+
				"over the sites it exists to read.", len(sites))
		}
		for _, s := range sites {
			if s.arg == "" {
				t.Errorf("%s passes a literal as lockParent's kind, not a mode variable. The "+
					"kind must be the mode of the child this site is about to attach; a "+
					"constant there makes every give-up on this path report the same way "+
					"whatever the network is.", s.where)
				continue
			}
			if s.arg != "mode" && s.arg != "kind" {
				t.Errorf("%s passes %q as lockParent's kind. The sites pass the mode they are "+
					"attaching, named mode or kind; anything else is either the wrong "+
					"variable or a rename this check has to be told about.", s.where, s.arg)
			}
		}
		t.Logf("read %d lockParent call site(s)", len(sites))
	})
}

func assertSameKindGiveUp(t *testing.T, p *Plugin, site string) {
	t.Helper()
	if got := p.parentLinkWaitTimeouts.Load(); got != 0 {
		t.Errorf("parent_link_wait_timeouts = %d after a give-up against a holder of the "+
			"caller's OWN kind, want 0. %s is telling the gate the wrong kind -- or none -- "+
			"so a pair the kernel permits is reported as one it may have refused.", got, site)
	}
	if got := p.parentLinkWaits.Load(); got != 1 {
		t.Errorf("parent_link_waits = %d, want 1. The caller waited and gave up, which is "+
			"counted as a wait when the holder was attaching the same kind; %s did not "+
			"reach the gate at all if this is zero.", got, site)
	}
}

type lockParentSite struct {
	where string
	arg   string
}

func lockParentCallSites(t *testing.T) []lockParentSite {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	var sites []lockParentSite
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "lockParent" || len(call.Args) < 3 {
				return true
			}
			site := lockParentSite{where: fset.Position(call.Pos()).String()}
			if id, ok := call.Args[2].(*ast.Ident); ok {
				site.arg = id.Name
			}
			sites = append(sites, site)
			return true
		})
	}
	return sites
}
