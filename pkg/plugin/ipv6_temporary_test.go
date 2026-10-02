// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func TestDecodeOpts_IPv6TemporaryAcceptsOnlyABoolean(t *testing.T) {
	for v, want := range map[string]bool{"true": true, "1": true, "T": true, "TRUE": true, "false": false, "0": false, "F": false} {
		opts, err := decodeOpts(map[string]interface{}{"ipv6_temporary": v})
		if err != nil {
			t.Errorf("ipv6_temporary=%q was refused: %v", v, err)
			continue
		}
		if opts.IPv6Temporary != want {
			t.Errorf("ipv6_temporary=%q decoded to %v, want %v", v, opts.IPv6Temporary, want)
		}
	}
	for _, v := range []string{"yes", "on", "temporary", "tru"} {
		if _, err := decodeOpts(map[string]interface{}{"ipv6_temporary": v}); err == nil {
			t.Errorf("ipv6_temporary=%q was accepted; a value that is not a boolean must be refused at create (#927)", v)
		}
	}
	for name, in := range map[string]map[string]interface{}{
		"stored before the option existed": {"vendor_class": "acme"},
		"written with no value":            {"ipv6_temporary": ""},
	} {
		opts, err := decodeOpts(in)
		if err != nil {
			t.Fatalf("%s: decodeOpts: %v", name, err)
		}
		if opts.IPv6Temporary {
			t.Errorf("%s: IPv6Temporary = true, want false (no IA_TA asked for)", name)
		}
	}
}

// off and slaac send no Solicit, so the key could only do nothing there (#927).
func TestValidateIPv6Options_IPv6TemporaryNeedsAModeThatSendsASolicit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    DHCPNetworkOptions
		wantErr bool
	}{
		{"dhcp", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6Temporary: true}, false},
		{"auto", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto", IPv6Temporary: true}, false},
		{"the old ipv6 switch, which is dhcp", DHCPNetworkOptions{Bridge: "br0", IPv6: true, IPv6Temporary: true}, false},
		{"slaac", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac", IPv6Temporary: true}, true},
		{"off", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "off", IPv6Temporary: true}, true},
		{"no IPv6 at all", DHCPNetworkOptions{Bridge: "br0", IPv6Temporary: true}, true},
		{"slaac without the key", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac"}, false},
		{"off without the key", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "off"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIPv6Options(tc.opts, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateIPv6Options = %v, want an error: %v", err, tc.wantErr)
			}
			if err == nil {
				return
			}
			mode, _ := tc.opts.ipv6Mode()
			for _, need := range []string{"ipv6_temporary", "ipv6_mode=" + mode.String(), "issue #927"} {
				if !strings.Contains(err.Error(), need) {
					t.Errorf("the refusal %q does not say %q", err, need)
				}
			}
		})
	}
}

// A hand-copied client-options literal that forgets ipv6_temporary asks for nothing and no other test fails (#927).
func TestClientOptionLiterals_EverySiteCarriesIPv6Temporary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
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
						// The value is the network's own field: a constant or a negation asks wrongly (#927).
						if sel, isSel := kv.Value.(*ast.SelectorExpr); id.Name == "IPv6Temporary" && (!isSel || sel.Sel.Name != "IPv6Temporary") {
							t.Errorf("%s: IPv6Temporary is not copied from the network's IPv6Temporary field", fset.Position(kv.Pos()))
						}
					}
				}
			}
			if !keys["VendorClass"] {
				return true // the probe: no network options
			}
			seen[f] = true
			if !keys["IPv6Temporary"] {
				t.Errorf("%s: DHCPClientOptions sets VendorClass but not IPv6Temporary, so ipv6_temporary is never asked for from here",
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

func tempInfo() dhcp.Info {
	return dhcp.Info{
		IP: "fd00:6470::1000/64",
		Addrs: []dhcp.V6Addr{
			{IP: "fd00:6470::1000/64", ValidSeconds: 3600, PreferredSeconds: 1800},
		},
		TempAddrs: []dhcp.V6Addr{
			{IP: "fd00:6470::1:0:77/64", ValidSeconds: 900, PreferredSeconds: 600},
		},
	}
}

func TestV6WantedAddrs_ListsTheTemporaryAddressAfterTheStableOne(t *testing.T) {
	info := tempInfo()
	want, err := v6WantedAddrs(mustParseAddr(t, info.IP), info)
	if err != nil {
		t.Fatalf("v6WantedAddrs: %v", err)
	}
	if len(want) != 2 {
		t.Fatalf("a lease with one stable and one temporary address produced %d addresses: %v", len(want), want)
	}
	if want[0].key != "fd00:6470::1000/64" || want[1].key != "fd00:6470::1:0:77/64" {
		t.Errorf("the install order is %s then %s, want the stable address first so a failure on the temporary one never keeps it off the link",
			want[0].key, want[1].key)
	}
	if got := want[1].addr; got.ValidLft != 900 || got.PreferedLft != 600 || got.Flags&unix.IFA_F_NODAD == 0 {
		t.Errorf("the temporary address got ValidLft=%d PreferedLft=%d flags=%#x, want its own 900/600 and NODAD", got.ValidLft, got.PreferedLft, got.Flags)
	}
	if got := want[1].addr; got.Flags&unix.IFA_F_TEMPORARY != 0 {
		t.Errorf("the temporary address carries IFA_F_TEMPORARY: the kernel would try to regenerate it from a parent that does not exist here (#927)")
	}
}

// An address the lease holds in both lists is installed once (#927).
func TestV6WantedAddrs_AnAddressInBothListsIsInstalledOnce(t *testing.T) {
	info := tempInfo()
	info.TempAddrs = []dhcp.V6Addr{{IP: "fd00:6470::1000/64", ValidSeconds: 900, PreferredSeconds: 600}}
	want, err := v6WantedAddrs(mustParseAddr(t, info.IP), info)
	if err != nil {
		t.Fatalf("v6WantedAddrs: %v", err)
	}
	if len(want) != 1 {
		t.Fatalf("got %d entries for one address held twice: %v", len(want), want)
	}
	if want[0].addr.ValidLft != 3600 {
		t.Errorf("the temporary entry overwrote the stable address's lifetimes: ValidLft=%d, want 3600", want[0].addr.ValidLft)
	}
}

func TestV6WantedAddrs_RefusesATemporaryAddressItCannotParse(t *testing.T) {
	info := tempInfo()
	info.TempAddrs = []dhcp.V6Addr{{IP: "not-an-address"}}
	if _, err := v6WantedAddrs(mustParseAddr(t, info.IP), info); err == nil || !strings.Contains(err.Error(), "not-an-address") {
		t.Errorf("the error does not name the temporary address that failed: %v", err)
	}
}

func TestV6AddrsToWithdraw_NamesATemporaryAddressTheLeaseDropped(t *testing.T) {
	m, _ := applyManager(t)
	h := &fakeV6LinkAddrs{}
	with := tempInfo()
	if err := m.applyV6Addrs(h, mustParseAddr(t, with.IP), with); err != nil {
		t.Fatalf("applying the lease with a temporary address: %v", err)
	}
	if len(h.replaced) != 2 || h.replaced[1] != "fd00:6470::1:0:77/64" {
		t.Fatalf("AddrReplace was called for %v, want the stable address and then the temporary one", h.replaced)
	}
	if len(h.deleted) != 0 {
		t.Errorf("a first bind deleted %v", h.deleted)
	}

	without := tempInfo()
	without.TempAddrs = nil
	if err := m.applyV6Addrs(h, mustParseAddr(t, without.IP), without); err != nil {
		t.Fatalf("applying the lease after the temporary address ran out: %v", err)
	}
	if len(h.deleted) != 1 || h.deleted[0] != "fd00:6470::1:0:77/64" {
		t.Fatalf("the renewal deleted %v, want exactly the temporary address the lease no longer holds: an address nothing removes "+
			"stays on the link as a source address after the server took it back", h.deleted)
	}
}

func TestApplyV6Addrs_ATemporaryAddressIsNotACountedSLAACAddress(t *testing.T) {
	m, p := applyManager(t)
	info := tempInfo()
	if err := m.applyV6Addrs(&fakeV6LinkAddrs{}, mustParseAddr(t, info.IP), info); err != nil {
		t.Fatalf("applyV6Addrs: %v", err)
	}
	if got := p.ipv6SLAACAddresses.Load(); got != 0 {
		t.Errorf("ipv6_slaac_addresses = %d for a DHCPv6 lease, want 0: a granted address is not one formed from an advertisement", got)
	}
}

func TestHealthView_ShowsTheTemporaryAddressOfTheDHCPv6Lease(t *testing.T) {
	bound := &fakeJoinClient{
		mode: proto.ConflictWait, phase: proto.ACDDefending, bound: true,
		l: lease.Lease{Addr: netip.MustParsePrefix("192.0.2.17/24")},
	}
	newManager := func() *dhcpManager {
		m := newDHCPManager(nil, JoinRequest{EndpointID: "aaaaaaaaaaaabbbbbbbbbbbb", NetworkID: "111111111111222222222222"},
			DHCPNetworkOptions{Mode: ModeMacvlan})
		m.setHealthClient(bound)
		m.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{IP: "192.0.2.17/24"}}, false)
		return m
	}
	now := time.Now()

	t.Run("the v6 lease carries one", func(t *testing.T) {
		m := newManager()
		m.noteTempV6(tempInfo().TempAddrs, now)
		if got := m.healthView().IPv6TemporaryAddress; got != "fd00:6470::1:0:77/64" {
			t.Errorf("ipv6_temporary_address = %q, want the DHCPv6 lease's temporary address in CIDR form", got)
		}
	})
	t.Run("only the v4 lease, no temporary address", func(t *testing.T) {
		m := newManager()
		if got := m.healthView().IPv6TemporaryAddress; got != "" {
			t.Errorf("ipv6_temporary_address = %q on an endpoint with no DHCPv6 temporary address, want empty", got)
		}
	})
	t.Run("a later v6 event without one clears it", func(t *testing.T) {
		m := newManager()
		m.noteTempV6(tempInfo().TempAddrs, now)
		m.noteTempV6(nil, now)
		if got := m.healthView().IPv6TemporaryAddress; got != "" {
			t.Errorf("ipv6_temporary_address = %q after the lease dropped it, want empty", got)
		}
	})
	t.Run("shown while the v4 lease is not live", func(t *testing.T) {
		m := newDHCPManager(nil, JoinRequest{EndpointID: "ccccccccccccdddddddddddd", NetworkID: "333333333333444444444444"},
			DHCPNetworkOptions{Mode: ModeMacvlan})
		m.noteTempV6(tempInfo().TempAddrs, now)
		if got := m.healthView().IPv6TemporaryAddress; got == "" {
			t.Error("a held temporary address is hidden because the v4 client is missing; the v4 lease neither holds nor withdraws it")
		}
	})
}

func TestTempV6Address_IsEmptyOnceItsValidLifetimeHasPassed(t *testing.T) {
	m := newDHCPManager(nil, JoinRequest{}, DHCPNetworkOptions{})
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	m.noteTempV6(tempInfo().TempAddrs, now)
	if got := m.tempV6Address(now.Add(899 * time.Second)); got == "" {
		t.Error("the temporary address is hidden 1 s before its valid lifetime ends")
	}
	if got := m.tempV6Address(now.Add(900 * time.Second)); got != "" {
		t.Errorf("the temporary address %q is still shown when its valid lifetime has ended", got)
	}
	m.noteTempV6([]dhcp.V6Addr{{IP: "fd00:6470::1:0:78/64"}}, now)
	if got := m.tempV6Address(now.Add(100 * time.Hour)); got == "" {
		t.Error("an address with no valid lifetime (infinite) was hidden")
	}
}

// A renewal feeds the health record in production; this one goes through renew() itself (#927).
func TestRenew_ARenewedV6LeaseFeedsTheTemporaryAddressRecord(t *testing.T) {
	m := &dhcpManager{plugin: &Plugin{}}

	if err := m.renew(true, tempInfo()); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if got := m.tempV6Address(time.Now()); got != "fd00:6470::1:0:77/64" {
		t.Errorf("temporary address after a renewal that carried one = %q, want fd00:6470::1:0:77/64", got)
	}

	plain := tempInfo()
	plain.TempAddrs = nil
	if err := m.renew(true, plain); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if got := m.tempV6Address(time.Now()); got != "" {
		t.Errorf("temporary address after a renewal that dropped it = %q, want empty", got)
	}
}
