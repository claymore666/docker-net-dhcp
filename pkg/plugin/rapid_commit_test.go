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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/vishvananda/netlink"
)

func TestDecodeOpts_RapidCommitAcceptsOnlyABoolean(t *testing.T) {
	for v, want := range map[string]bool{"true": true, "1": true, "T": true, "TRUE": true, "false": false, "0": false, "F": false} {
		opts, err := decodeOpts(map[string]interface{}{"rapid_commit": v})
		if err != nil {
			t.Errorf("rapid_commit=%q was refused: %v", v, err)
			continue
		}
		if opts.RapidCommit != want {
			t.Errorf("rapid_commit=%q decoded to %v, want %v", v, opts.RapidCommit, want)
		}
	}
	for _, v := range []string{"yes", "on", "rapid", "tru"} {
		if _, err := decodeOpts(map[string]interface{}{"rapid_commit": v}); err == nil {
			t.Errorf("rapid_commit=%q was accepted; a value that is not a boolean must be refused at create (#1031)", v)
		}
	}
	for name, in := range map[string]map[string]interface{}{
		"stored before the option existed": {"vendor_class": "acme"},
		"written with no value":            {"rapid_commit": ""},
	} {
		opts, err := decodeOpts(in)
		if err != nil {
			t.Fatalf("%s: decodeOpts: %v", name, err)
		}
		if opts.RapidCommit {
			t.Errorf("%s: RapidCommit = true, want false (option 80 not sent)", name)
		}
	}
}

// Each client's options are copied from the network's by hand, and a literal that forgets rapid_commit sends nothing
// with no other test failing; the IPAM DHCPv6 path is exempt, option 80 being v4 only (#1031).
func TestClientOptionLiterals_EveryV4SiteCarriesRapidCommit(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	v6Only := map[string]bool{"ipam_endpoint.go": true}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		tree, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(tree, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "DHCPClientOptions" {
				return true
			}
			keys := map[string]bool{}
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						keys[id.Name] = true
						// The value is the network's own field: a constant or a negation sends the wrong thing at every
						// site and only the lane would see it (#1031).
						if sel, isSel := kv.Value.(*ast.SelectorExpr); id.Name == "RapidCommit" && (!isSel || sel.Sel.Name != "RapidCommit") {
							t.Errorf("%s: RapidCommit is not copied from the network's RapidCommit field", fset.Position(kv.Pos()))
						}
					}
				}
			}
			if !keys["VendorClass"] {
				return true // the probe: no network options
			}
			seen[f] = true
			if v6Only[f] {
				if keys["RapidCommit"] {
					t.Errorf("%s: the DHCPv6 literal sets RapidCommit; option 80 is DHCPv4 only", fset.Position(lit.Pos()))
				}
				return true
			}
			if !keys["RapidCommit"] {
				t.Errorf("%s: DHCPClientOptions sets VendorClass but not RapidCommit, so rapid_commit is never sent from here",
					fset.Position(lit.Pos()))
			}
			return true
		})
	}
	for _, f := range []string{"parent_attached.go", "network.go", "dhcp_manager.go", "ipam_reserve.go", "ipam_endpoint.go"} {
		if !seen[f] {
			t.Errorf("no DHCPClientOptions literal with VendorClass in %s: the site moved, so this test judged nothing there", f)
		}
	}
}

func TestSetupClient_TheJoinClientCarriesTheNetworksRapidCommit(t *testing.T) {
	for _, want := range []bool{true, false} {
		m, _ := daemonFreeManager(t, &fakeDocker{})
		m.opts.RapidCommit = want
		m.ctrLink = &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: 1, Name: "lo", HardwareAddr: m.MacAddress}}
		got, opens := !want, 0
		prevNew := newDHCPClient
		newDHCPClient = func(_ string, opts *dhcp.DHCPClientOptions) (*dhcp.DHCPClient, error) {
			opens++
			got = opts.RapidCommit
			return nil, errors.New("no client in this test")
		}
		t.Cleanup(func() { newDHCPClient = prevNew })
		_, _ = m.setupClient(false)
		if opens != 1 || got != want {
			t.Errorf("%d clients built with RapidCommit %v, want one with %v: the Join client's DISCOVER carries option 80", opens, got, want)
		}
	}
}
