// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	ipamTestPool6    = "fd00:6470:6865::/64"
	ipamTestNetwork6 = "net-ipam6-1"
)

func TestIpamCanonicalPool6(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{ipamTestPool6, ipamTestPool6},
		{"fd00:6470:6865::7/64", ipamTestPool6},
		{"2001:db8::/32", "2001:db8::/32"},
		{"fd00:6470:6865::4/126", "fd00:6470:6865::4/126"},
	} {
		got, err := ipamCanonicalPool6(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ipamCanonicalPool6(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{
		"", "::/0", "::/64", "::/120", "::ffff:192.168.0.0/112", "::ffff:0:0/96", "::ffc0:0:0/90", "ff02::/16",
		"fd00:6470:6865::/127", "fd00:6470:6865::1/128", "192.168.99.0/24", "not-a-prefix", "fe80::1%eth0/64",
	} {
		_, err := ipamCanonicalPool6(in)
		if err == nil {
			t.Errorf("ipamCanonicalPool6(%q) was accepted", in)
			continue
		}
		if !errors.Is(err, util.ErrIPAM) {
			t.Errorf("ipamCanonicalPool6(%q) = %v, which does not wrap util.ErrIPAM", in, err)
		}
		if !strings.Contains(err.Error(), "--subnet") {
			t.Errorf("ipamCanonicalPool6(%q) = %q, which does not name the typed --subnet", in, err)
		}
	}
}

func TestIpamPoolID6_RoundTripsLikeV4(t *testing.T) {
	for _, opts := range []map[string]string{nil, {"parent": "eth0"}, {"bridge": "br-lan"}} {
		typed, err := ipamPoolID6(ipamLocalAddressSpace, "fd00:6470:6865::7/64", opts)
		if err != nil {
			t.Fatalf("ipamPoolID6: %v", err)
		}
		replayed, err := ipamPoolID6(ipamLocalAddressSpace, ipamTestPool6, opts)
		if err != nil || replayed != typed {
			t.Errorf("the typed and the replayed pool derived %q and %q (%v), so a daemon restart would not find the binding", typed, replayed, err)
		}
		pool, err := ipamPoolOfID(typed)
		if err != nil || pool.String() != ipamTestPool6 {
			t.Errorf("ipamPoolOfID(%q) = %v, %v; want %s", typed, pool, err, ipamTestPool6)
		}
	}
	if _, err := ipamPoolID6(ipamLocalAddressSpace, ipamTestPool, nil); err == nil {
		t.Error("a v4 pool derived a v6 PoolID")
	}
	if _, err := ipamPoolID(ipamLocalAddressSpace, ipamTestPool6, nil); err == nil {
		t.Error("a v6 pool derived a v4 PoolID")
	}
}

func TestIpamPoolNetworkAddress_V6(t *testing.T) {
	id, err := ipamPoolID6(ipamLocalAddressSpace, ipamTestPool6, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ipamPoolNetworkAddress(id)
	if err != nil || got.Address != ipamTestPool6 {
		t.Errorf("ipamPoolNetworkAddress(/64) = %q, %v; want %s", got.Address, err, ipamTestPool6)
	}
	for _, bits := range []string{"127", "128"} {
		bad := "dhcp/" + ipamLocalAddressSpace + "/fd00:6470:6865::/" + bits
		_, err := ipamPoolNetworkAddress(bad)
		if err == nil || !strings.Contains(err.Error(), "--gateway") {
			t.Errorf("a /%s answered an engine 26 gateway request: %v", bits, err)
		}
	}
	if _, err := ipamPoolNetworkAddress("dhcp/" + ipamLocalAddressSpace + "/fd00:6470:6865::/126"); err != nil {
		t.Errorf("a /126 was refused: %v", err)
	}
	v4 := "dhcp/" + ipamLocalAddressSpace + "/192.168.99.0/"
	if _, err := ipamPoolNetworkAddress(v4 + "30"); err != nil {
		t.Errorf("a v4 /30 was refused: %v", err)
	}
	for _, bits := range []string{"31", "32"} {
		if _, err := ipamPoolNetworkAddress(v4 + bits); err == nil {
			t.Errorf("a v4 /%s answered an engine 26 gateway request", bits)
		}
	}
}

func TestRequestPool6(t *testing.T) {
	p, _ := ipamFixture(t)
	res, err := p.RequestPool(RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, Pool: "fd00:6470:6865::7/64", V6: true})
	if err != nil {
		t.Fatalf("a typed v6 pool was refused: %v", err)
	}
	want, _ := ipamPoolID6(ipamLocalAddressSpace, ipamTestPool6, nil)
	if res.PoolID != want || res.Pool != ipamTestPool6 {
		t.Errorf("answered (%q, %q), want (%q, %q)", res.PoolID, res.Pool, want, ipamTestPool6)
	}
	if n := p.ipamPools.len(); n != 1 {
		t.Errorf("%d issues banked, want 1", n)
	}
	for _, req := range []RequestPoolRequest{
		{AddressSpace: ipamLocalAddressSpace, Pool: "::/0", V6: true},
		{AddressSpace: ipamLocalAddressSpace, Pool: "fd00:6470:6865::/127", V6: true},
		{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool, V6: true},
		{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool6, SubPool: "fd00:6470:6865::/80", V6: true},
	} {
		if _, err := p.RequestPool(req); err == nil || !errors.Is(err, util.ErrIPAM) {
			t.Errorf("RequestPool(%+v) = %v, want an ErrIPAM refusal", req, err)
		}
	}
	if n := p.ipamPools.len(); n != 1 {
		t.Errorf("a refused request banked an issue: %d issues, want 1", n)
	}
}

func TestIssuedPools_TwoFamilies(t *testing.T) {
	s := newIssuedPools()
	now := time.Now()
	id4, _ := ipamPoolID(ipamLocalAddressSpace, ipamTestPool, nil)
	id6, _ := ipamPoolID6(ipamLocalAddressSpace, ipamTestPool6, nil)
	s.add(id4, ipamLocalAddressSpace, ipamTestPool, "", now)
	s.add(id6, ipamLocalAddressSpace, ipamTestPool6, "", now)
	if got, ok, _ := s.take(ipamLocalAddressSpace, ipamTestPool6, "br0", now); !ok || got != id6 {
		t.Errorf("took %q, %v for the v6 pool, want %q", got, ok, id6)
	}
	if n := s.len(); n != 1 {
		t.Errorf("%d issues left after taking the v6 one, want the v4 one only", n)
	}
	s.drop(id4)
	if n := s.len(); n != 0 {
		t.Errorf("%d issues left, the families leak into each other", n)
	}
	s.add(id6, ipamLocalAddressSpace, ipamTestPool6, "", now)
	s.drop(id6)
	if n := s.len(); n != 0 {
		t.Errorf("a released v6 pool left %d issues", n)
	}
}

func v6PoolPlugin(t *testing.T) *Plugin {
	t.Helper()
	withStateDir(t, t.TempDir())
	withFakeBridge(t, "br-ipam6")
	p := newPluginForTest()
	p.ipamPools = newIssuedPools()
	p.ipamIndex = newIPAMIndex()
	p.docker = &fakeDocker{}
	return p
}

func createV6PoolNetwork(t *testing.T, p *Plugin, generic map[string]interface{}, v4, v6 []*IPAMData) error {
	t.Helper()
	opts := map[string]interface{}{"bridge": "br-ipam6"}
	for k, v := range generic {
		opts[k] = v
	}
	return p.CreateNetwork(CreateNetworkRequest{
		NetworkID: ipamTestNetwork6,
		Options:   map[string]interface{}{util.OptionsKeyGeneric: opts},
		IPv4Data:  v4,
		IPv6Data:  v6,
	})
}

func requestBothPools(t *testing.T, p *Plugin) {
	t.Helper()
	if _, err := p.RequestPool(RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool}); err != nil {
		t.Fatalf("v4 RequestPool: %v", err)
	}
	if _, err := p.RequestPool(RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool6, V6: true}); err != nil {
		t.Fatalf("v6 RequestPool: %v", err)
	}
}

var (
	v4Ours = []*IPAMData{{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool}}
	v6Ours = []*IPAMData{{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool6,
		Gateway: "fd00:6470:6865::1/64", AuxAddresses: map[string]interface{}{"x": "fd00:6470:6865::2/64"}}}
)

func TestCreateNetwork_V6PoolBindsBothAndLeavesNoLeak(t *testing.T) {
	p := v6PoolPlugin(t)
	requestBothPools(t, p)
	if err := createV6PoolNetwork(t, p, map[string]interface{}{"ipv6": true}, v4Ours, v6Ours); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	sn, err := loadNetwork(ipamTestNetwork6)
	if err != nil || sn.Binding == nil {
		t.Fatalf("loadNetwork: %v, %+v", err, sn.Binding)
	}
	b := sn.Binding
	id6, _ := ipamPoolID6(ipamLocalAddressSpace, ipamTestPool6, nil)
	if b.PoolID6 != id6 || b.Pool6 != ipamTestPool6 || b.Gateway6 != "fd00:6470:6865::1" ||
		len(b.Aux6) != 1 || b.Aux6[0] != "fd00:6470:6865::2" {
		t.Errorf("v6 half of the binding = %+v", b)
	}
	for _, id := range []string{b.PoolID, b.PoolID6} {
		if n, ok := p.ipamIndex.network(id); !ok || n != ipamTestNetwork6 {
			t.Errorf("index maps %q to %q, %v", id, n, ok)
		}
	}
	if n := p.ipamPools.len(); n != 0 {
		t.Errorf("%d issues left after the create consumed both", n)
	}
	if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: ipamTestNetwork6}); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
	if n := p.ipamIndex.len(); n != 0 {
		t.Errorf("%d index entries left after the network was removed", n)
	}
}

func TestRebuildIPAMIndex_BindsTheV6PoolID(t *testing.T) {
	p := v6PoolPlugin(t)
	requestBothPools(t, p)
	if err := createV6PoolNetwork(t, p, map[string]interface{}{"ipv6": true}, v4Ours, v6Ours); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	x := newIPAMIndex()
	rebuildIPAMIndex(x)
	if n := x.len(); n != 2 {
		t.Errorf("a restart rebuilt %d index entries, want both PoolIDs", n)
	}
	id6, _ := ipamPoolID6(ipamLocalAddressSpace, ipamTestPool6, nil)
	if n, ok := x.network(id6); !ok || n != ipamTestNetwork6 {
		t.Errorf("the v6 PoolID maps to %q, %v after a restart", n, ok)
	}
}

func TestIPAMIndex_UnbindNetworkDropsBothFamilies(t *testing.T) {
	x := newIPAMIndex()
	x.bind("a", "n1")
	x.bind("b", "n1")
	x.bind("c", "n2")
	x.unbindNetwork("n1")
	if x.len() != 1 {
		t.Errorf("%d entries left, want only the other network's", x.len())
	}
}

func TestSaveNetwork_SchemaV3OnlyWithAV6Pool(t *testing.T) {
	const previousMax = 2 // what the last build before #1132 understood
	withStateDir(t, t.TempDir())
	opts := DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-test"}
	v4 := &ipamBinding{PoolID: "dhcp/dhcp-local/192.168.99.0/24", Space: ipamLocalAddressSpace, Pool: ipamTestPool}
	v6 := &ipamBinding{PoolID: v4.PoolID, Space: v4.Space, Pool: v4.Pool, PoolID6: "dhcp/dhcp-local/" + ipamTestPool6, Pool6: ipamTestPool6}
	for _, tc := range []struct {
		id   string
		b    *ipamBinding
		want int
	}{{"net-a", nil, stateSchemaVersionBase}, {"net-b", v4, stateSchemaVersionIPAM}, {"net-c", v6, stateSchemaVersion}} {
		if err := saveNetwork(tc.id, opts, tc.b); err != nil {
			t.Fatal(err)
		}
		path, _ := stateFilePath(tc.id)
		if got := schemaVersionOfFile(t, path); got != tc.want {
			t.Errorf("%s was written at schema v%d, want v%d", tc.id, got, tc.want)
		}
	}
	if stateSchemaVersion <= previousMax || stateSchemaVersionIPAM > previousMax {
		t.Errorf("a v6-pool file (v%d) must be above what the previous build reads (v%d) and a v4 one not", stateSchemaVersion, previousMax)
	}
	sn, err := loadNetwork("net-c")
	if err != nil || sn.Binding.PoolID6 != v6.PoolID6 || sn.Binding.Pool6 != ipamTestPool6 {
		t.Errorf("the v6 half did not round-trip: %v, %+v", err, sn.Binding)
	}
}

func TestCreateNetwork_V6PoolNeedsIPv6ModeDHCP(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]interface{}
		ok   bool
	}{
		{"ipv6=true", map[string]interface{}{"ipv6": true}, true},
		{"ipv6_mode=dhcp", map[string]interface{}{"ipv6_mode": "dhcp"}, true},
		{"ipv6_mode=slaac", map[string]interface{}{"ipv6_mode": "slaac"}, false},
		{"ipv6_mode=auto", map[string]interface{}{"ipv6_mode": "auto"}, false},
		{"ipv6 unset", map[string]interface{}{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := v6PoolPlugin(t)
			requestBothPools(t, p)
			err := createV6PoolNetwork(t, p, tc.opts, v4Ours, v6Ours)
			if tc.ok {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a v6 pool was accepted beside a mode that cannot serve it")
			}
			for _, want := range []string{"ipv6_mode=", "ipv6_mode=dhcp", "drop --ipv6"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not mention %q", err, want)
				}
			}
			if n := p.ipamPools.len(); n != 2 {
				t.Errorf("the refusal consumed an issue: %d left, want 2", n)
			}
		})
	}
}

func TestCreateNetwork_V6PoolNeedsAV4PoolBesideIt(t *testing.T) {
	for name, v4 := range map[string][]*IPAMData{
		"no v4 data":     nil,
		"a foreign pool": {{AddressSpace: "null", Pool: "0.0.0.0/0"}},
	} {
		t.Run(name, func(t *testing.T) {
			p := v6PoolPlugin(t)
			requestBothPools(t, p)
			err := createV6PoolNetwork(t, p, map[string]interface{}{"ipv6": true}, v4, v6Ours)
			if err == nil || !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), "needs an IPv4 pool") {
				t.Fatalf("got %v, want the refusal naming the IPv4 pool", err)
			}
		})
	}
}

func TestIpamBind6(t *testing.T) {
	two := append(append([]*IPAMData{}, v6Ours...), &IPAMData{AddressSpace: ipamLocalAddressSpace, Pool: "fd00:6470:6866::/64"})
	for _, tc := range []struct {
		name string
		prep func(*Plugin)
		data []*IPAMData
		want string
	}{
		{"two v6 pools", requestBoth, two, "one IPv6 pool per network"},
		{"never issued", func(*Plugin) {}, v6Ours, "no issued IPv6 pool"},
		{"already held", func(p *Plugin) {
			requestBoth(p)
			id, _ := ipamPoolID6(ipamLocalAddressSpace, ipamTestPool6, nil)
			p.ipamIndex.bind(id, "net-other")
		}, v6Ours, "already holds IPv6 pool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := v6PoolPlugin(t)
			tc.prep(p)
			err := p.ipamBind6("net-x", &ipamBinding{}, tc.data, "br-ipam6")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

func requestBoth(p *Plugin) {
	_, _ = p.RequestPool(RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool6, V6: true})
}

func v6BoundFixture(t *testing.T) (*Plugin, *ipamBinding) {
	t.Helper()
	p, b := ipamFixture(t)
	b.PoolID6, _ = ipamPoolID6(ipamLocalAddressSpace, ipamTestPool6, nil)
	b.Pool6, b.Gateway6, b.Aux6 = ipamTestPool6, "fd00:6470:6865::1", []string{"fd00:6470:6865::2"}
	if err := saveNetwork(ipamTestNetwork, DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-test"}, b); err != nil {
		t.Fatal(err)
	}
	p.ipamIndex.bind(b.PoolID6, ipamTestNetwork)
	return p, b
}

func TestRequestAddress_V6GatewayAndAuxEchoed(t *testing.T) {
	p, b := v6BoundFixture(t)
	for _, tc := range []struct{ name, addr, typ, want string }{
		{"gateway", "fd00:6470:6865::1", ipamOptGateway, "fd00:6470:6865::1/64"},
		{"aux", "fd00:6470:6865::2", "", "fd00:6470:6865::2/64"},
		{"gateway re-requested untyped", "fd00:6470:6865::1", "", "fd00:6470:6865::1/64"},
	} {
		res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID: b.PoolID6, Address: tc.addr, Options: map[string]string{ipamOptRequestAddressType: tc.typ}})
		if err != nil || res.Address != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, res.Address, err, tc.want)
		}
	}
	// the v4 side of the same network still echoes with the v4 prefix length
	res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
		PoolID: b.PoolID, Address: "192.168.99.1", Options: map[string]string{ipamOptRequestAddressType: ipamOptGateway}})
	if err != nil || res.Address != "192.168.99.1/24" {
		t.Errorf("v4 gateway: got %q, %v", res.Address, err)
	}
}

func TestRequestAddress_V6PoolRefusesUntilTheAddressSideLands(t *testing.T) {
	p, b := v6BoundFixture(t)
	for _, addr := range []string{"", "fd00:6470:6865::20"} {
		_, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID: b.PoolID6, Address: addr,
			Options: map[string]string{ipamOptMacAddress: ipamTestMAC}})
		if err == nil || !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), "follows in the same release") {
			t.Errorf("address %q: got %v, want the not-yet-built refusal", addr, err)
		}
	}
}

func TestRequestAddress_UnboundV6GatewayIsTheNetworkAddress(t *testing.T) {
	p, _ := ipamFixture(t)
	id, _ := ipamPoolID6(ipamLocalAddressSpace, ipamTestPool6, nil)
	res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
		PoolID: id, Options: map[string]string{ipamOptRequestAddressType: ipamOptGateway}})
	if err != nil || res.Address != ipamTestPool6 {
		t.Errorf("got %q, %v; want %s", res.Address, err, ipamTestPool6)
	}
}

func TestIpamEchoAddress_DefaultPrefixIsTheFamilysHostLength(t *testing.T) {
	for _, tc := range []struct{ addr, pool, want string }{
		{"fd00:6470:6865::9", "", "fd00:6470:6865::9/128"},
		{"192.168.99.9", "", "192.168.99.9/32"},
		{"fd00:6470:6865::9", ipamTestPool6, "fd00:6470:6865::9/64"},
	} {
		res, err := ipamEchoAddress(tc.addr, tc.pool)
		if err != nil || res.Address != tc.want {
			t.Errorf("ipamEchoAddress(%q, %q) = %q, %v; want %q", tc.addr, tc.pool, res.Address, err, tc.want)
		}
	}
}
