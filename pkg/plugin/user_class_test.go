// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
	"github.com/vishvananda/netlink"
)

func TestDecodeOpts_UserClassKey(t *testing.T) {
	opts, err := decodeOpts(map[string]interface{}{"user_class": "web, tier 1"})
	if err != nil {
		t.Fatalf("decodeOpts: %v", err)
	}
	if opts.UserClass != "web, tier 1" {
		t.Errorf("user_class decoded to %q, want the value untouched: nothing between the operator and option 77 may split it", opts.UserClass)
	}

	for name, in := range map[string]map[string]interface{}{
		"stored before the option existed": {"vendor_class": "acme"},
		"written with no value":            {"user_class": ""},
	} {
		opts, err := decodeOpts(in)
		if err != nil {
			t.Fatalf("%s: decodeOpts: %v", name, err)
		}
		if opts.UserClass != "" {
			t.Errorf("%s: UserClass = %q, want empty (option 77 not sent)", name, opts.UserClass)
		}
	}
}

func TestValidateModeOptions_UserClassBound(t *testing.T) {
	bridge := func(v string) DHCPNetworkOptions {
		return DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0", UserClass: v}
	}
	// Literal edges, not the constant: a constant moved to 255 must turn this red (#1120).
	for _, n := range []int{0, 1, 254} {
		if err := validateModeOptions(bridge(strings.Repeat("a", n))); err != nil {
			t.Errorf("a %d-octet user_class: err = %v, want nil (0 is unset, 254 is the most one instance carries)", n, err)
		}
	}
	for _, n := range []int{255, 1000} {
		err := validateModeOptions(bridge(strings.Repeat("a", n)))
		if !errors.Is(err, util.ErrIPAM) {
			t.Fatalf("a %d-octet user_class: err = %v, want ErrIPAM at network creation, not a failure at docker run", n, err)
		}
		for _, want := range []string{"user_class", strconv.Itoa(n), strconv.Itoa(maxUserClassOctets)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name %q", err, want)
			}
		}
	}
}

// Each client's options are copied from the network's by hand, and a literal that forgets user_class sends nothing
// with no other test failing; the IPAM DHCPv6 path is exempt, option 77 being v4 only (#1120).
func TestClientOptionLiterals_EveryV4SiteCarriesUserClass(t *testing.T) {
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
					}
				}
			}
			if !keys["VendorClass"] {
				return true // the probe: no network options
			}
			seen[f] = true
			if v6Only[f] {
				if keys["UserClass"] {
					t.Errorf("%s: the DHCPv6 literal sets UserClass; option 77 is DHCPv4 only", fset.Position(lit.Pos()))
				}
				return true
			}
			if !keys["UserClass"] {
				t.Errorf("%s: DHCPClientOptions sets VendorClass but not UserClass, so user_class is never sent from here",
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

func TestSetupClient_TheJoinClientCarriesTheNetworksUserClass(t *testing.T) {
	for _, want := range []string{"web, tier 1", ""} {
		m, _ := daemonFreeManager(t, &fakeDocker{})
		m.opts.UserClass = want
		m.ctrLink = &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: 1, Name: "lo", HardwareAddr: m.MacAddress}}
		got, opens := "unset", 0
		prevNew := newDHCPClient
		newDHCPClient = func(_ string, opts *dhcp.DHCPClientOptions) (*dhcp.DHCPClient, error) {
			opens++
			got = opts.UserClass
			return nil, errors.New("no client in this test")
		}
		t.Cleanup(func() { newDHCPClient = prevNew })
		_, _ = m.setupClient(false)
		if opens != 1 || got != want {
			t.Errorf("%d clients built with user class %q, want one with %q: the Join client's requests and renewals carry option 77", opens, got, want)
		}
	}
}
