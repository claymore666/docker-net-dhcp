// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	ipamTestNetwork = "net-ipam-1"
	ipamTestPool    = "192.168.99.0/24"
	ipamTestMAC     = "02:42:c0:a8:63:0a"
)

func ipamFixture(t *testing.T) (*Plugin, *ipamBinding) {
	t.Helper()
	p, b, _ := ipamFixtureWithJournal(t)
	return p, b
}

func ipamFixtureWithJournal(t *testing.T) (*Plugin, *ipamBinding, string) {
	t.Helper()
	withStateDir(t, t.TempDir())
	journal := t.TempDir() + "/" + recordFileName
	r, err := dhcp.OpenRecords(journal, "test-instance")
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	p := &Plugin{
		records:              r,
		joinHints:            make(map[string]joinHint),
		persistentDHCP:       make(map[string]*dhcpManager),
		endpointFingerprints: make(map[string]endpointFingerprint),
		ipamPools:            newIssuedPools(),
		ipamIndex:            newIPAMIndex(),
		ipamReserves:         newIPAMReserves(),
	}
	poolID, err := ipamPoolID(ipamLocalAddressSpace, ipamTestPool, nil)
	if err != nil {
		t.Fatalf("ipamPoolID: %v", err)
	}
	b := &ipamBinding{
		PoolID:  poolID,
		Space:   ipamLocalAddressSpace,
		Pool:    ipamTestPool,
		Gateway: "192.168.99.1",
		Aux:     []string{"192.168.99.2"},
	}
	opts := DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-test"}
	if err := saveNetwork(ipamTestNetwork, opts, b); err != nil {
		t.Fatalf("saveNetwork: %v", err)
	}
	p.ipamIndex.bind(poolID, ipamTestNetwork)
	return p, b, journal
}

// The daemon replays RequestPool and RequestAddress inside libnetwork.New, before its API listener starts, so
// these handlers hold no Docker client; a nil docker field panics on any call (#110).
func TestIpamHandlers_CallDockerZeroTimes(t *testing.T) {
	p, b := ipamFixture(t)
	if p.docker != nil {
		t.Fatal("the fixture has a Docker client; this test proves nothing with one present")
	}
	ctx := context.Background()

	if _, err := p.RequestPool(RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool}); err != nil {
		t.Errorf("RequestPool: %v", err)
	}
	mac, _ := net.ParseMAC(ipamTestMAC)
	id := p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
	if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	res, err := p.RequestAddress(ctx, RequestAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"})
	if err != nil {
		t.Errorf("RequestAddress (replay): %v", err)
	}
	if res.Address != "192.168.99.10/24" {
		t.Errorf("replayed address = %q, want 192.168.99.10/24", res.Address)
	}
	if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
		t.Errorf("ReleaseAddress: %v", err)
	}
	p.ipamPools.drop(b.PoolID)
}

func TestRequestAddress_Dispatch(t *testing.T) {
	mac, _ := net.ParseMAC(ipamTestMAC)

	t.Run("the gateway is echoed and never leased", func(t *testing.T) {
		p, b := ipamFixture(t)
		res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID:  b.PoolID,
			Address: "192.168.99.1",
			Options: map[string]string{ipamOptRequestAddressType: ipamOptGateway},
		})
		if err != nil {
			t.Fatalf("RequestAddress: %v", err)
		}
		if res.Address != "192.168.99.1/24" {
			t.Errorf("gateway = %q, want 192.168.99.1/24 (the pool's own prefix length)", res.Address)
		}
	})

	t.Run("an aux address is echoed and never leased", func(t *testing.T) {
		p, b := ipamFixture(t)
		res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID: b.PoolID, Address: "192.168.99.2",
		})
		if err != nil {
			t.Fatalf("RequestAddress: %v", err)
		}
		if res.Address != "192.168.99.2/24" {
			t.Errorf("aux = %q, want 192.168.99.2/24", res.Address)
		}
		if n := p.ipamReplayHits.Load(); n != 0 {
			t.Errorf("an aux address counted as a replay hit (%d); the two branches are confused", n)
		}
	})

	t.Run("a stored endpoint replays from its record", func(t *testing.T) {
		p, b := ipamFixture(t)
		id := p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
		if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
		res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID: b.PoolID, Address: "192.168.99.10",
		})
		if err != nil {
			t.Fatalf("RequestAddress: %v", err)
		}
		if res.Address != "192.168.99.10/24" {
			t.Errorf("replay = %q, want 192.168.99.10/24", res.Address)
		}
		if n := p.ipamReplayHits.Load(); n != 1 {
			t.Errorf("ipam_replay_hits = %d, want 1", n)
		}
		if n := p.ipamReplayMiss.Load(); n != 0 {
			t.Errorf("ipam_replay_miss = %d, want 0 — the counter must move in one direction only", n)
		}
	})

	t.Run("a replay with no record is refused and counted", func(t *testing.T) {
		p, b := ipamFixture(t)
		_, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID: b.PoolID, Address: "192.168.99.77",
		})
		if err == nil {
			t.Fatal("an address nothing holds was confirmed; Docker would keep serving it forever")
		}
		if !errors.Is(err, util.ErrIPAM) {
			t.Errorf("error %v does not wrap util.ErrIPAM", err)
		}
		if n := p.ipamReplayMiss.Load(); n != 1 {
			t.Errorf("ipam_replay_miss = %d, want 1", n)
		}
		if n := p.ipamReplayHits.Load(); n != 0 {
			t.Errorf("ipam_replay_hits = %d, want 0", n)
		}
	})

	t.Run("a CLOSED record does not answer a replay", func(t *testing.T) {
		p, b := ipamFixture(t)
		id := p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
		if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
		if err := p.records.Closed(id); err != nil {
			t.Fatalf("Closed: %v", err)
		}
		if _, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID: b.PoolID, Address: "192.168.99.10",
		}); err == nil {
			t.Fatal("a record closed by a failed CreateEndpoint answered a replay; the phase " +
				"filter is the only thing between that and an endpoint attached to an address " +
				"nothing holds")
		}
		if n := p.ipamReplayMiss.Load(); n != 1 {
			t.Errorf("ipam_replay_miss = %d, want 1", n)
		}
	})

	t.Run("an unbound pool with no address is refused", func(t *testing.T) {
		p, _ := ipamFixture(t)
		if _, err := p.RequestAddress(context.Background(), RequestAddressRequest{PoolID: "dhcp/nobody"}); err == nil {
			t.Error("a lease was promised out of a pool no network holds")
		}
	})
}

func TestRequestPool_RefusesWhatV2_1DoesNotDo(t *testing.T) {
	p, _ := ipamFixture(t)
	cases := []struct {
		name string
		req  RequestPoolRequest
		says []string
	}{
		// The IPAM endpoint path runs no DHCPv6 exchange either, so the message names --ipam-driver null.
		{"an IPv6 pool", RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, V6: true}, []string{"#960", "--ipam-driver null", "ipv6_mode"}},
		{"an --ip-range", RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool, SubPool: "192.168.99.128/25"}, []string{"--ip-range"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.RequestPool(c.req)
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("error %v does not wrap util.ErrIPAM, so the daemon gets a 500 instead of a 400", err)
			}
			for _, want := range c.says {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is %q and does not mention %q, so it tells the operator "+
						"nothing they can act on", err, want)
				}
			}
		})
	}
}

// DeleteEndpoint has already retained a live endpoint's record, and re-retaining would move its tombstone deadline.
func TestReleaseAddress_RetainsOnlyAReservation(t *testing.T) {
	mac, _ := net.ParseMAC(ipamTestMAC)

	t.Run("a reservation is retained", func(t *testing.T) {
		p, b := ipamFixture(t)
		id := p.recordReserved(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
		if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
		if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
			t.Fatalf("ReleaseAddress: %v", err)
		}
		if got := ipamPhaseOf(t, p, id); got != lease.PhaseRetained {
			t.Errorf("the reservation is in phase %v, want %v — nothing else would ever "+
				"reclaim it and it would answer address lookups until the network is deleted",
				got, lease.PhaseRetained)
		}
	})

	t.Run("a joined endpoint is left alone", func(t *testing.T) {
		p, b := ipamFixture(t)
		id := p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
		if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
		if err := p.records.Bound(id); err != nil {
			t.Fatalf("Bound: %v", err)
		}
		if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
			t.Fatalf("ReleaseAddress: %v", err)
		}
		if got := ipamPhaseOf(t, p, id); got != lease.PhaseJoined {
			t.Errorf("a joined endpoint's record moved to %v on a release", got)
		}
	})

	t.Run("an address nothing holds is counted", func(t *testing.T) {
		p, b := ipamFixture(t)
		if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.77"}); err != nil {
			t.Fatalf("ReleaseAddress: %v", err)
		}
		if n := p.ipamReleaseUnknown.Load(); n != 1 {
			t.Errorf("ipam_release_unknown = %d, want 1", n)
		}
	})
}

func ipamPhaseOf(t *testing.T, p *Plugin, id string) lease.Phase {
	t.Helper()
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	for _, r := range rb.Records {
		if r.ID == id {
			return r.Phase
		}
	}
	t.Fatalf("record %q is gone", id)
	return lease.PhaseUnset
}

// The state file is the only home of the binding, and a parseable file without it is a null-mode file, so both
// the unparseable and the binding-less file are driven (#110).
func TestIpamBindingLost_RefusesRatherThanDegrades(t *testing.T) {
	for _, c := range []struct{ name, content string }{
		{"the file cannot be parsed", "{not json"},
		{"the file parses and carries no binding", `{"schema_version":1,"options":{"mode":"bridge","bridge":"br-test"}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, b := ipamFixture(t)
			path, err := stateFilePath(ipamTestNetwork)
			if err != nil {
				t.Fatalf("stateFilePath: %v", err)
			}
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatalf("rewriting the state file: %v", err)
			}
			_, err = p.RequestAddress(context.Background(), RequestAddressRequest{
				PoolID: b.PoolID, Address: "192.168.99.10",
			})
			if err == nil {
				t.Fatal("an IPAM-mode network whose binding could not be read was served anyway. " +
					"That path runs a second DHCP exchange, writes a tombstone this shape does " +
					"not use, and answers with an address libnetwork already allocated.")
			}
			if !errors.Is(err, errIPAMBindingLost) {
				t.Errorf("error %v is not errIPAMBindingLost; the refusal has to be recognisable "+
					"to the one caller that may survive it (DeleteEndpoint)", err)
			}
		})
	}
}

func TestIpamRefuseIPvlan(t *testing.T) {
	err := ipamRefuseIPvlan(ModeIPvlan)
	if err == nil {
		t.Fatal("ipvlan was accepted in IPAM mode. libnetwork generates a MAC per endpoint " +
			"for an IPAM driver that asks for one, and the ipvlan branch of CreateEndpoint " +
			"refuses any supplied MAC, so every container on such a network fails to start " +
			"with an error that names nothing about IPAM.")
	}
	if !errors.Is(err, util.ErrIPAM) {
		t.Errorf("the refusal %v is not a util.ErrIPAM, so it does not map to the status "+
			"code the other IPAM refusals use", err)
	}
	for _, want := range []string{"ipvlan", "--ipam-driver null", "#949"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q and does not mention %q", err, want)
		}
	}
	for _, mode := range []string{ModeBridge, ModeMacvlan, ""} {
		if err := ipamRefuseIPvlan(mode); err != nil {
			t.Errorf("mode %q was refused in IPAM mode (%v); only ipvlan is out of scope for "+
				"v2.1.0 and a refusal that reached the other modes would take the feature "+
				"away from everyone", mode, err)
		}
	}
}

func TestRebuildIPAMIndex_FoldsTheStateDirectory(t *testing.T) {
	_, b := ipamFixture(t)
	if err := saveOptions("net-null-1", DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}

	x := newIPAMIndex()
	rebuildIPAMIndex(x)
	got, ok := x.network(b.PoolID)
	if !ok || got != ipamTestNetwork {
		t.Errorf("the rebuilt index resolves %q to (%q, %v), want %q", b.PoolID, got, ok, ipamTestNetwork)
	}
	if n := x.len(); n != 1 {
		t.Errorf("the index holds %d binding(s); the null-mode network beside it has none and "+
			"must not acquire one", n)
	}
}

// Two networks sharing a subnet on two parents differ only by a typed suffix, so a prefix match would mix them.
func TestIpamIndex_MatchesThePoolIDExactly(t *testing.T) {
	x := newIPAMIndex()
	x.bind("dhcp/dhcp-local/192.168.100.0/24", "net-a")
	if got, ok := x.network("dhcp/dhcp-local/192.168.100.0/24/parent=eth1"); ok {
		t.Errorf("the suffixed PoolID resolved to %q; the second network would take the "+
			"first one's binding", got)
	}
	if _, taken := x.boundTo("dhcp/dhcp-local/192.168.100.0/24", "net-b"); !taken {
		t.Error("a PoolID another network holds was not reported as taken, so two networks " +
			"would bind one pool")
	}
	if _, taken := x.boundTo("dhcp/dhcp-local/192.168.100.0/24", "net-a"); taken {
		t.Error("a network's own binding was reported as taken by someone else")
	}
}

func TestIpamACKInPool(t *testing.T) {
	cases := []struct {
		pool, addr string
		wantErr    bool
	}{
		{ipamTestPool, "192.168.99.10", false},
		{ipamTestPool, "192.168.100.10", true},
		{ipamAnyPool, "192.168.100.10", false},
		{"", "192.168.100.10", false},
	}
	for _, c := range cases {
		err := ipamACKInPool(netip.MustParseAddr(c.addr), c.pool)
		if (err != nil) != c.wantErr {
			t.Errorf("ipamACKInPool(%v, %v) = %v, wantErr %v", c.addr, c.pool, err, c.wantErr)
		}
	}
}

func TestIpamMode_CreateEndpointIsDispatchedToTheIPAMBranch(t *testing.T) {
	p, _ := ipamFixture(t)

	_, err := p.CreateEndpoint(context.Background(), CreateEndpointRequest{
		NetworkID:  ipamTestNetwork,
		EndpointID: "ep-ipam-1",
		Interface:  &EndpointInterface{MacAddress: ipamTestMAC, Address: "192.168.99.10/24"},
	})
	if err == nil {
		t.Fatal("an endpoint was created with no reservation held for its MAC. The address " +
			"Docker published for it is then one nothing claimed at the DHCP server.")
	}
	if !strings.Contains(err.Error(), "no reservation is held") {
		t.Errorf("the refusal is %q. That is not the IPAM branch's, so CreateEndpoint took the "+
			"null path for a network whose addresses Docker allocates: it would run a second "+
			"DHCP exchange and answer with an address libnetwork did not hand out.", err)
	}
}

// In IPAM mode the re-bind candidate is the retained record; a JSON tombstone as well would offer one address twice.
func TestIpamMode_DeleteEndpointWritesNoJSONTombstone(t *testing.T) {
	p, _ := ipamFixture(t)

	const nullNetwork = "net-null-1"
	if err := saveOptions(nullNetwork, DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-test"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}

	restore := nlLinkByName
	nlLinkByName = func(string) (netlink.Link, error) { return nil, netlink.LinkNotFoundError{} }
	t.Cleanup(func() { nlLinkByName = restore })

	for _, c := range []struct {
		name      string
		network   string
		wantWrite bool
		why       string
	}{
		{
			name: "IPAM mode writes nothing", network: ipamTestNetwork, wantWrite: false,
			why: "the retained record is the only re-bind candidate in this shape; a JSON " +
				"tombstone beside it is a second claim on the same address",
		},
		{
			name: "null mode is unchanged", network: nullNetwork, wantWrite: true,
			why: "D19: the null shape is the product and its restart stability is carried by " +
				"exactly this store",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ep := "ep-" + c.network
			p.rememberEndpoint(ep, endpointFingerprint{
				MAC: ipamTestMAC, IPv4: "192.168.99.10",
			}, dhcpHostname{name: "", refused: false})

			if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
				NetworkID: c.network, EndpointID: ep,
			}); err != nil {
				t.Fatalf("DeleteEndpoint: %v", err)
			}
			mac, ipv4, _, ok := p.tombstones.consume(c.network, "some-container")
			if ok != c.wantWrite {
				t.Errorf("a JSON tombstone was consumable=%v (mac=%q ipv4=%q), want %v: %s",
					ok, mac, ipv4, c.wantWrite, c.why)
			}
		})
	}
}

// A replay of a stored endpoint carries no MAC and `--ip X` carries the new endpoint's, so a MAC marks a create.
func TestIpamReplay_AnAddressHeldByAnotherEndpointIsNotAReplay(t *testing.T) {
	holder, _ := net.ParseMAC(ipamTestMAC)
	other, _ := net.ParseMAC("02:42:c0:a8:63:0b")

	seed := func(t *testing.T) (*Plugin, *ipamBinding) {
		t.Helper()
		p, b := ipamFixture(t)
		id := p.recordCreated(ipamTestNetwork, holder, dhcp.ClientIdentity([]byte{7}))
		if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
		return p, b
	}

	t.Run("a creating endpoint may not take a running one's address", func(t *testing.T) {
		p, b := seed(t)
		_, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID:  b.PoolID,
			Address: "192.168.99.10",
			Options: map[string]string{ipamOptMacAddress: other.String()},
		})
		if err == nil {
			t.Fatal("an address a running container holds was handed to a second endpoint. " +
				"libnetwork publishes both, and the collision arrives at CreateEndpoint as " +
				"a message about a plugin restart that did not happen.")
		}
		if !errors.Is(err, util.ErrIPAM) {
			t.Errorf("error %v does not wrap util.ErrIPAM", err)
		}
		if n := p.ipamReplayHits.Load(); n != 0 {
			t.Errorf("ipam_replay_hits = %d, want 0: this was not a replay and the counter "+
				"is the denominator an operator reads ipam_replay_miss against", n)
		}
	})

	// No replay carries a MAC: moby 28.5.2 replays with ep.ipamOptions (libnetwork/endpoint.go:1330, reached from
	// controller.go:817), which MarshalJSON does not persist (endpoint.go:109-127). This shape comes from a second
	// container pinning the same `--ip` and `--mac-address` (#110).
	t.Run("a second endpoint pinning the same --ip and MAC is refused, not replayed", func(t *testing.T) {
		p, b := seed(t)
		_, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID:  b.PoolID,
			Address: "192.168.99.10",
			Options: map[string]string{ipamOptMacAddress: holder.String()},
		})
		if err == nil {
			t.Fatal("a second endpoint pinning both the address and the hardware address a " +
				"running container holds was answered. Docker publishes both endpoints on " +
				"one address and the loser fails at CreateEndpoint.")
		}
		if !errors.Is(err, util.ErrIPAM) {
			t.Errorf("error %v does not wrap util.ErrIPAM", err)
		}
		for _, want := range []string{holder.String(), "--mac-address"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal is %q; it does not contain %q", err, want)
			}
		}
		if strings.Contains(err.Error(), "restart") {
			t.Errorf("the refusal is %q. It blames a plugin restart, which is the message "+
				"this path used to reach at CreateEndpoint and the reason the refusal "+
				"moved here", err)
		}
		if n := p.ipamReplayHits.Load(); n != 0 {
			t.Errorf("ipam_replay_hits = %d, want 0: a second endpoint was counted as a "+
				"replay, and that counter is the denominator for ipam_replay_miss", n)
		}
		if n := p.ipamReserveDuplicateMAC.Load(); n != 1 {
			t.Errorf("ipam_reserve_duplicate_mac = %d, want 1: nothing counts the second "+
				"endpoint this entrance admits", n)
		}
	})

	t.Run("the replay shape, which carries no MAC, is unchanged", func(t *testing.T) {
		p, b := seed(t)
		res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID: b.PoolID, Address: "192.168.99.10",
		})
		if err != nil {
			t.Fatalf("RequestAddress: %v", err)
		}
		if res.Address != "192.168.99.10/24" {
			t.Errorf("address = %q, want 192.168.99.10/24", res.Address)
		}
		if n := p.ipamReplayHits.Load(); n != 1 {
			t.Errorf("ipam_replay_hits = %d, want 1", n)
		}
	})
}

// An unbound pool is either a create's aux address or the replay of a network rebuildIPAMIndex skipped; only the
// first may be echoed, and the start-up fold's completeness separates them (#110).
func TestIpamUnboundPool_ALostBindingIsNotConfirmed(t *testing.T) {
	const strayPool = "dhcp/dhcp-local/192.168.99.0/24"

	t.Run("a create in flight still gets its aux address back", func(t *testing.T) {
		p, _ := ipamFixture(t)
		p.ipamIndex = newIPAMIndex()
		res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID: strayPool, Address: "192.168.99.2",
		})
		if err != nil {
			t.Fatalf("RequestAddress: %v — a create carrying --aux-address is refused before "+
				"CreateNetwork can bind anything", err)
		}
		if res.Address != "192.168.99.2/32" {
			t.Errorf("aux = %q, want 192.168.99.2/32 (a host route: the any-pool carries no prefix to wear)", res.Address)
		}
	})

	t.Run("a lost binding is refused and counted", func(t *testing.T) {
		p, _ := ipamFixture(t)
		p.ipamIndex = newIPAMIndex()
		p.ipamIndex.markIncomplete()
		_, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID: strayPool, Address: "192.168.99.10",
		})
		if err == nil {
			t.Fatal("an endpoint address was confirmed by a process holding no record of it, " +
				"on a host where a state file could not be read. Docker keeps serving the " +
				"address and nothing ever says the record is gone.")
		}
		if !errors.Is(err, util.ErrIPAM) {
			t.Errorf("error %v does not wrap util.ErrIPAM", err)
		}
		if n := p.ipamReplayMiss.Load(); n != 1 {
			t.Errorf("ipam_replay_miss = %d, want 1: this is exactly what that counter "+
				"documents, an address the plugin would not confirm at a restart", n)
		}
	})

	t.Run("the gateway is answered even then", func(t *testing.T) {
		p, _ := ipamFixture(t)
		p.ipamIndex = newIPAMIndex()
		p.ipamIndex.markIncomplete()
		res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID:  strayPool,
			Address: "192.168.99.1",
			Options: map[string]string{ipamOptRequestAddressType: ipamOptGateway},
		})
		if err != nil {
			t.Fatalf("RequestAddress (gateway): %v — a gateway says so on the wire and is "+
				"never an endpoint's address", err)
		}
		if res.Address != "192.168.99.1/32" {
			t.Errorf("gateway = %q, want 192.168.99.1/32", res.Address)
		}
		if n := p.ipamReplayMiss.Load(); n != 0 {
			t.Errorf("ipam_replay_miss = %d, want 0", n)
		}
	})
}

func TestRebuildIPAMIndex_ReportsWhatItCouldNotRead(t *testing.T) {
	t.Run("a directory that reads cleanly leaves it clear", func(t *testing.T) {
		p, _ := ipamFixture(t)
		x := newIPAMIndex()
		rebuildIPAMIndex(x)
		if x.isIncomplete() {
			t.Error("the fold reported a skip on a state directory it read completely; " +
				"every create carrying --aux-address on this host is now refused")
		}
		if x.len() != 1 {
			t.Errorf("the fold bound %d pool(s), want 1", x.len())
		}
		_ = p
	})

	t.Run("the plugin's own files in the directory are not networks", func(t *testing.T) {
		p, _ := ipamFixture(t)

		// Every host that ever deleted an endpoint has tombstones.json here, and "tombstones" satisfies validNetworkID.
		if err := p.tombstones.add(ipamTestNetwork, "", ipamTestMAC, "192.168.99.10", ""); err != nil {
			t.Fatalf("laying a tombstone: %v", err)
		}
		if _, err := os.Stat(tombstoneFilePath()); err != nil {
			t.Fatalf("the tombstone store was not written: %v", err)
		}

		x := newIPAMIndex()
		rebuildIPAMIndex(x)
		if x.isIncomplete() {
			t.Error("the tombstone store was folded as a network and failed to parse as one, " +
				"so the index is incomplete on a host where nothing is wrong. Every " +
				"RequestAddress for a pool no network holds yet is then refused, and the " +
				"refusal tells the operator to repair or remove a file that is not corrupt.")
		}
		if x.len() != 1 {
			t.Errorf("the fold bound %d pool(s), want 1", x.len())
		}
	})

	t.Run("a file that will not read is reported", func(t *testing.T) {
		p, _ := ipamFixture(t)
		path, err := stateFilePath(ipamTestNetwork)
		if err != nil {
			t.Fatalf("stateFilePath: %v", err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("rewriting the state file: %v", err)
		}
		x := newIPAMIndex()
		rebuildIPAMIndex(x)
		if !x.isIncomplete() {
			t.Error("a network whose file would not read was skipped silently; its stored " +
				"endpoints then replay against an unbound pool and are echoed back")
		}
		_ = p
	})
}

// `docker plugin install <ref> --alias lan-dhcp` stores the alias, so the refusal must not key on the image name;
// the null shape must still fall through to the Docker API (#110).
func TestIpamFallback_ARemoteIPAMDriverRefusesUnderAnyName(t *testing.T) {
	for _, c := range []struct {
		name, driver string
		refuse       bool
	}{
		{"the plugin under its published reference", "ghcr.io/claymore666/docker-net-dhcp:v2.0.0", true},
		{"the same plugin installed under an alias", "lan-dhcp", true},
		{"a remote IPAM driver of some other name", "example-ipam", true},
		{"the null driver, the other supported shape", "null", false},
		{"the daemon's own IPAM", "default", false},
		{"a record naming no IPAM driver at all", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			withStateDir(t, dir)
			if err := os.WriteFile(filepath.Join(dir, "net1.json"), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			p := &Plugin{docker: &fakeDocker{inspectResult: map[string]dNetwork.Inspect{
				"net1": {
					Options: map[string]string{"mode": "bridge", "bridge": "br-test"},
					IPAM:    dNetwork.IPAM{Driver: c.driver},
				},
			}}}
			opts, err := p.netOptions(context.Background(), "net1")
			if c.refuse {
				if err == nil {
					t.Fatalf("IPAM driver %q was served from the Docker API with no binding. "+
						"That path runs a second DHCP exchange, writes a JSON tombstone this "+
						"shape does not use, and answers libnetwork with an address it has "+
						"already allocated.", c.driver)
				}
				if !errors.Is(err, errIPAMBindingLost) {
					t.Errorf("error %v is not errIPAMBindingLost", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("IPAM driver %q was refused (%v); the null shape must still fall back "+
					"to the Docker API on a load failure", c.driver, err)
			}
			if opts.Bridge != "br-test" {
				t.Errorf("fallback returned bridge %q, want br-test", opts.Bridge)
			}
		})
	}
}

// moby 28.5.2 copies an operator-set MAC into the IPAM options (libnetwork/network.go:1222, :1240), so two
// `--mac-address X` endpoints arrive as one key. Once CreateEndpoint takes the reservation only the record store
// knows. libnetwork registers its release defer after assignAddress returns (network.go:1245-1252), so a
// refused request sends no ReleaseAddress (#110).
func TestRequestAddress_TwoEndpointsCannotShareOneHardwareAddress(t *testing.T) {
	mac, _ := net.ParseMAC(ipamTestMAC)

	for _, c := range []struct {
		name        string
		seedReserve bool
		finished    bool
		phase       lease.Phase
	}{
		{"while the first exchange is still running, before its record exists", true, false, lease.PhaseUnset},
		{"after the first exchange answered and nothing claimed it", true, true, lease.PhaseReserved},
		{"after the first endpoint was created and joined", false, false, lease.PhaseJoined},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, b := ipamFixture(t)
			key := ipamReserveKey(b.PoolID, mac)

			var recordID string
			if c.phase != lease.PhaseUnset {
				recordID = p.recordReserved(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
				if err := p.records.Observed(recordID, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
					t.Fatalf("Observed: %v", err)
				}
			}
			if c.phase == lease.PhaseJoined {
				if err := p.records.Created(recordID, ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7})); err != nil {
					t.Fatalf("Created: %v", err)
				}
				if err := p.records.Bound(recordID); err != nil {
					t.Fatalf("Bound: %v", err)
				}
			}

			var first *ipamReservation
			if c.seedReserve {
				var mine bool
				first, mine = p.ipamReserves.begin(key, time.Now())
				if !mine {
					t.Fatal("the seeded reservation was not owned; the fixture is not empty")
				}
			}
			if c.finished {
				p.ipamReserves.finish(key, first, ipamReservation{
					addr:   netip.MustParsePrefix("192.168.99.10/24"),
					info:   dhcp.Info{IP: "192.168.99.10/24"},
					record: recordID,
				}, nil)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			res, err := p.RequestAddress(ctx, RequestAddressRequest{
				PoolID:  b.PoolID,
				Options: map[string]string{ipamOptMacAddress: mac.String()},
			})
			if err == nil {
				t.Fatalf("a second endpoint carrying %v was answered %q, the address the "+
					"first endpoint is holding. Docker publishes both and the loser is "+
					"refused at CreateEndpoint wearing a plugin-restart message.",
					mac, res.Address)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("the second request waited on the first instead of being refused; " +
					"a create that blocks for the whole call budget gives the operator the " +
					"daemon's timeout, not an answer")
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("error %v does not wrap util.ErrIPAM", err)
			}
			for _, want := range []string{
				mac.String(),
				"--mac-address",
				"one recently-removed endpoint",
				(tombstoneTTL + ipamSweepInterval).String(),
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is %q; it does not contain %q, which is what "+
						"tells the operator what to change and when a retry can work", err, want)
				}
			}
			if n := p.ipamReserveDuplicateMAC.Load(); n != 1 {
				t.Errorf("ipam_reserve_duplicate_mac = %d, want 1", n)
			}

			// A refusal after begin would leave a key nothing finishes or sweeps, since stale() offers only completed ones.
			want := 0
			if c.seedReserve {
				want = 1
			}
			if n := p.ipamReserves.len(); n != want {
				t.Errorf("the reserve set holds %d reservations after the refusal, want %d: "+
					"the refused request left a key behind, and a key that is never "+
					"finished is never swept either", n, want)
			}

			got, ok := p.ipamReserves.take(key)
			if c.finished && (!ok || got != first) {
				t.Error("the refused request consumed or replaced the first endpoint's " +
					"reservation; the container that won the race would be refused too")
			}
			if !c.finished && ok {
				t.Error("an unfinished or absent reservation was consumable; CreateEndpoint " +
					"would bind a link to an exchange that has not answered")
			}
			if recordID != "" {
				rb, err := p.records.Rebuilt()
				if err != nil {
					t.Fatalf("Rebuilt: %v", err)
				}
				if rec, live := rb.ByID(recordID); !live || rec.Phase != c.phase {
					t.Errorf("the first endpoint's record is %v, want %v: the refused request "+
						"moved the record the winner is holding", rec.Phase, c.phase)
				}
			}

			second := ipamSecondNetwork(t, p)
			_, err = p.RequestAddress(ctx, RequestAddressRequest{
				PoolID:  second,
				Options: map[string]string{ipamOptMacAddress: mac.String()},
			})
			if err != nil && strings.Contains(err.Error(), "already leasing an address") {
				t.Errorf("a request on a second network carrying the same MAC was refused as "+
					"a duplicate: %v. The reservation key is the (pool, MAC) pair precisely "+
					"so that one MAC on two networks is two endpoints, not one.", err)
			}
			if n := p.ipamReserveDuplicateMAC.Load(); n != 1 {
				t.Errorf("ipam_reserve_duplicate_mac = %d after a second network's request, "+
					"want 1: the pool is not part of the reservation key", n)
			}
		})
	}
}

// A restart under a pinned MAC leaves a RETAINED record with that MAC, so RETAINED stays outside ipamRecordPhases.
func TestRequestAddress_ARetainedRecordIsNotADuplicate(t *testing.T) {
	mac, _ := net.ParseMAC(ipamTestMAC)
	p, b := ipamFixture(t)

	recordID := p.recordReserved(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
	if err := p.records.Observed(recordID, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Created(recordID, ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7})); err != nil {
		t.Fatalf("Created: %v", err)
	}
	p.recordRetained(recordID, time.Now().Add(tombstoneTTL))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := p.RequestAddress(ctx, RequestAddressRequest{
		PoolID:  b.PoolID,
		Options: map[string]string{ipamOptMacAddress: mac.String()},
	})
	if err != nil && strings.Contains(err.Error(), "already leasing an address") {
		t.Errorf("a container restarting under its own pinned hardware address was refused "+
			"as a second endpoint: %v.\nThe record it collides with is its OWN tombstone, "+
			"which is the re-bind candidate that gives it its address back. Refusing here "+
			"costs every pinned-MAC container its address on every restart.", err)
	}
	if n := p.ipamReserveDuplicateMAC.Load(); n != 0 {
		t.Errorf("ipam_reserve_duplicate_mac = %d for a restart under a retained record, "+
			"want 0: RETAINED is being read as a live endpoint", n)
	}
}

// retainRecordFor lays a tombstone only when a fingerprint exists, so a record can stay JOINED for the journal's
// life. No DHCPRELEASE is sent (#962), so the lease's own expiry is the true bound on the refusal (#110).
func TestRequestAddress_AnOrphanedRecordStopsRefusingWhenItsLeaseRunsOut(t *testing.T) {
	mac, _ := net.ParseMAC(ipamTestMAC)

	// RFC 2131's infinite lease (0xffffffff) reaches lease.Lease as a zero Expire, as does a lost lease (Lease{},
	// Held=false) and a reservation that never got its ACK; only the first holds a lease.
	for _, c := range []struct {
		name       string
		shape      string
		expiresIn  time.Duration
		infinite   bool
		wantRefuse bool
	}{
		{"a lease still running refuses", "held", time.Hour, false, true},
		{"a lease that has run out does not", "held", -time.Minute, false, false},
		{"a lease with no end refuses", "held", 0, true, true},
		{"a lease lost under a live phase does not", "lost", time.Hour, false, false},
		{"a reservation that never got its ACK does not", "reserved", 0, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, b := ipamFixture(t)
			if c.shape == "reserved" {
				if id := p.recordReserved(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7})); id == "" {
					t.Fatal("recordReserved wrote no record")
				}
			} else {
				id := p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
				ev := acquired("192.168.99.10/24", c.expiresIn)
				if c.infinite {
					ev.Lease.Expire = time.Time{}
				}
				if err := p.records.Observed(id, ev, nil); err != nil {
					t.Fatalf("Observed: %v", err)
				}
				if err := p.records.Bound(id); err != nil {
					t.Fatalf("Bound: %v", err)
				}
				if c.shape == "lost" {
					if err := p.records.Observed(id, lease.Event{Kind: lease.Lost, Reason: proto.ReasonExpired}, nil); err != nil {
						t.Fatalf("Observed(lost): %v", err)
					}
				}
			}

			if c.shape != "held" {
				rb, err := p.records.Rebuilt()
				if err != nil {
					t.Fatalf("Rebuilt: %v", err)
				}
				recs := rb.ByScopeMAC(ipamTestNetwork, mac)
				if len(recs) != 1 {
					t.Fatalf("want exactly one record under the hardware address, got %d", len(recs))
				}
				if !ipamPhaseAnswers(recs[0].Phase) {
					t.Fatalf("the %s record sits in phase %v, which the phase filter already excludes: "+
						"this arm would pass without the clause it exists to drive", c.shape, recs[0].Phase)
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := p.RequestAddress(ctx, RequestAddressRequest{
				PoolID:  b.PoolID,
				Options: map[string]string{ipamOptMacAddress: mac.String()},
			})
			refused := err != nil && strings.Contains(err.Error(), "already leasing an address")
			if refused != c.wantRefuse {
				t.Errorf("refused as a duplicate = %v, want %v (err %v).\n"+
					"A record stuck in a live phase with an expired lease is an endpoint "+
					"nothing can remove any more; refusing on it locks that hardware "+
					"address out of this network for the life of the journal.",
					refused, c.wantRefuse, err)
			}
			want := int32(0)
			if c.wantRefuse {
				want = 1
			}
			if n := p.ipamReserveDuplicateMAC.Load(); n != want {
				t.Errorf("ipam_reserve_duplicate_mac = %d, want %d", n, want)
			}
		})
	}
}

// The settled half fails open, as ipamRecordFor does on the same error. The journal is replaced by a directory,
// since root ignores mode bits (#110).
func TestRequestAddress_AnUnreadableJournalDoesNotRefuse(t *testing.T) {
	mac, _ := net.ParseMAC(ipamTestMAC)
	p, b, journal := ipamFixtureWithJournal(t)

	id := p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
	if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if _, held := p.ipamEndpointHoldingMAC(ipamTestNetwork, mac); !held {
		t.Fatal("the seeded record does not answer while the journal reads; this test would " +
			"pass against a fold it never broke")
	}

	if err := os.Remove(journal); err != nil {
		t.Fatalf("remove the journal: %v", err)
	}
	if err := os.Mkdir(journal, 0o755); err != nil {
		t.Fatalf("put a directory where the journal was: %v", err)
	}
	if _, err := p.records.Rebuilt(); err == nil {
		t.Fatal("the fold still reads; nothing here drives the error branch")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := p.RequestAddress(ctx, RequestAddressRequest{
		PoolID:  b.PoolID,
		Options: map[string]string{ipamOptMacAddress: mac.String()},
	})
	if err != nil && strings.Contains(err.Error(), "already leasing an address") {
		t.Errorf("an unreadable journal was reported to the operator as a duplicate hardware "+
			"address: %v.\nEvery container start on every IPAM network on that host would "+
			"fail, with a message naming a cause that is not the one.", err)
	}
	if n := p.ipamReserveDuplicateMAC.Load(); n != 0 {
		t.Errorf("ipam_reserve_duplicate_mac = %d, want 0: a read error is being counted as "+
			"a second endpoint, and the counter is what an operator would act on", n)
	}
}

func ipamSecondNetwork(t *testing.T, p *Plugin) string {
	t.Helper()
	const id = "net-ipam-2"
	const pool = "192.168.100.0/24"
	poolID, err := ipamPoolID(ipamLocalAddressSpace, pool, nil)
	if err != nil {
		t.Fatalf("ipamPoolID: %v", err)
	}
	opts := DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-test-2"}
	if err := saveNetwork(id, opts, &ipamBinding{
		PoolID:  poolID,
		Space:   ipamLocalAddressSpace,
		Pool:    pool,
		Gateway: "192.168.100.1",
	}); err != nil {
		t.Fatalf("saveNetwork: %v", err)
	}
	p.ipamIndex.bind(poolID, id)
	return poolID
}

func createIPAMBridgeNetwork(t *testing.T, ipv6 bool, space string) error {
	t.Helper()
	return createIPAMBridgeNetworkOpts(t, map[string]interface{}{"ipv6": ipv6}, space)
}

// An `ipv6=false` beside `ipv6_mode=slaac` is refused by validateIPv6Options first, so the key is left out.
func createIPAMBridgeNetworkOpts(t *testing.T, generic map[string]interface{}, space string) error {
	t.Helper()
	const bridge = "br-ipam6"
	withFakeBridge(t, bridge)
	opts := map[string]interface{}{"bridge": bridge}
	for k, v := range generic {
		opts[k] = v
	}
	return createIPAMNetworkOptsExact(t, opts, space)
}

// `mode=ipvlan` with a bridge key is refused by validateModeOptions first, so this fixture adds no key.
func createIPAMNetworkOptsExact(t *testing.T, generic map[string]interface{}, space string) error {
	t.Helper()
	withStateDir(t, t.TempDir())

	p := newPluginForTest()
	p.ipamPools = newIssuedPools()
	p.ipamIndex = newIPAMIndex()
	p.docker = &fakeDocker{}

	if space != "null" {
		if _, err := p.RequestPool(RequestPoolRequest{AddressSpace: space, Pool: ipamTestPool}); err != nil {
			t.Fatalf("RequestPool: %v", err)
		}
	}
	data := &IPAMData{AddressSpace: space, Pool: ipamTestPool}
	if space == "null" {
		data = &IPAMData{AddressSpace: "null", Pool: "0.0.0.0/0"}
	}
	return p.CreateNetwork(CreateNetworkRequest{
		NetworkID: ipamTestNetwork,
		Options: map[string]interface{}{
			util.OptionsKeyGeneric: generic,
		},
		IPv4Data: []*IPAMData{data},
	})
}

// In IPAM mode ipam_endpoint.go runs no DHCPv6 exchange and returns no AddressIPv6, and a Join-time DUID from the
// regenerated endpoint MAC changes at every restart, so ipv6 is refused at CreateNetwork (#960).
func TestCreateNetwork_IPAMModeRefusesIPv6(t *testing.T) {
	t.Run("an IPAM network with ipv6 is refused", func(t *testing.T) {
		err := createIPAMBridgeNetwork(t, true, ipamLocalAddressSpace)
		if err == nil {
			t.Fatal("`-o ipv6=true` was accepted on a network this plugin is the IPAM driver " +
				"for. No DHCPv6 exchange runs on that path, so the operator who asked for " +
				"IPv6 in writing gets none, and the DUID the v6 manager falls back to at " +
				"Join changes at every restart because the endpoint MAC does")
		}
		if !errors.Is(err, util.ErrIPAM) {
			t.Errorf("the refusal %v is not a util.ErrIPAM, so it does not map to the status "+
				"code the other IPAM refusals use", err)
		}
		for _, want := range []string{"ipv6", "--ipam-driver null", "#960"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal is %q and does not mention %q", err, want)
			}
		}
		// `-o ipv6=true` resolves to ipv6_mode=dhcp, so the message is asserted as `ipv6_mode=<value>`.
		if want := "ipv6_mode=" + proto.Mode6DHCP.String(); !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q and does not carry %q, the mode `-o ipv6=true` "+
				"resolves to. The mode is what tells an operator which of their "+
				"options switched IPv6 on", err, want)
		}
	})

	// The modes come from dhcp.IPv6Modes and dhcp.ParseIPv6Mode, the route an operator's string takes, so a new
	// library mode reaches this loop without an edit (#817).
	t.Run("every ipv6_mode that switches IPv6 on is refused and named", func(t *testing.T) {
		spellings := dhcp.IPv6Modes()
		if len(spellings) < 2 {
			t.Fatalf("dhcp.IPv6Modes() is %v. A loop over fewer than two modes cannot tell a "+
				"message that names the resolved mode from one that names a literal",
				spellings)
		}
		drove := 0
		for _, spelling := range spellings {
			mode, set, perr := dhcp.ParseIPv6Mode(spelling)
			if perr != nil || !set {
				t.Fatalf("dhcp.ParseIPv6Mode(%q) did not accept a value dhcp.IPv6Modes() "+
					"published: mode=%v set=%v err=%v", spelling, mode, set, perr)
			}
			if mode == proto.Mode6Off {
				continue
			}
			drove++
			t.Run(spelling, func(t *testing.T) {
				err := createIPAMBridgeNetworkOpts(t,
					map[string]interface{}{"ipv6_mode": spelling}, ipamLocalAddressSpace)
				if err == nil {
					t.Fatalf("`-o ipv6_mode=%s` was accepted on a network this plugin is the "+
						"IPAM driver for. It switches IPv6 on exactly as `-o ipv6=true` "+
						"does, and the IPAM endpoint path runs no DHCPv6 exchange either "+
						"way", mode)
				}
				if !errors.Is(err, util.ErrIPAM) {
					t.Errorf("the refusal %v is not a util.ErrIPAM, so it does not map to the "+
						"status code the other IPAM refusals use", err)
				}
				if want := "ipv6_mode=" + mode.String(); !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is %q and does not carry %q. That is the whole "+
						"finding: the operator is told about an option they did not "+
						"write, and not about the one they did", err, want)
				}
				// `#960` is on three refusals and only this one is reachable from CreateNetwork, so the phrase's
				// uniqueness is measured over the package (#960).
				const want = "Two spellings reach this refusal"
				if n := errorConstructionsNaming(t, want); n != 1 {
					t.Fatalf("%q occurs in %d error construction(s) in this module, so it "+
						"does not say which guard answered. Pick a phrase that occurs "+
						"in exactly one", want, n)
				}
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is %q and does not carry %q, so it did not come "+
						"from ipamRefuseIPv6. This case is measuring a different "+
						"guard", err, want)
				}
			})
		}
		if drove == 0 {
			t.Fatal("no mode switched IPv6 on, so every assertion in this loop was skipped " +
				"and the arm passed by having an empty domain")
		}
	})

	// A version in a refusal dates it; TestRefusals_NameNoReleaseVersion is the static rule.
	t.Run("no refusal on this path dates itself to a release", func(t *testing.T) {
		type arm struct {
			where string
			want  string
			run   func(t *testing.T) error
		}
		arms := []arm{
			{
				where: "ipv6=true at CreateNetwork",
				want:  "Two spellings reach this refusal",
				run: func(t *testing.T) error {
					return createIPAMBridgeNetworkOpts(t,
						map[string]interface{}{"ipv6": true}, ipamLocalAddressSpace)
				},
			},
			{
				where: "ipv6_mode=slaac at CreateNetwork",
				want:  "Two spellings reach this refusal",
				run: func(t *testing.T) error {
					return createIPAMBridgeNetworkOpts(t,
						map[string]interface{}{"ipv6_mode": proto.Mode6SLAAC.String()},
						ipamLocalAddressSpace)
				},
			},
			{
				where: "ipvlan at CreateNetwork",
				want:  "#949",
				run: func(t *testing.T) error {
					return createIPAMNetworkOptsExact(t,
						map[string]interface{}{"mode": "ipvlan", "parent": "eth0"},
						ipamLocalAddressSpace)
				},
			},
			{
				where: "an IPv6 pool at RequestPool",
				want:  "does not allocate IPv6 pools",
				run: func(t *testing.T) error {
					p := newPluginForTest()
					_, err := p.RequestPool(RequestPoolRequest{
						AddressSpace: ipamLocalAddressSpace, V6: true})
					return err
				},
			},
		}
		for _, a := range arms {
			if n := errorConstructionsNaming(t, a.want); n != 1 {
				t.Errorf("the discriminator %q for %s occurs in %d error construction(s), "+
					"so a message carrying it does not say which guard answered",
					a.want, a.where, n)
				continue
			}
			err := a.run(t)
			if err == nil {
				t.Errorf("%s was accepted, so its refusal was never produced and cannot be "+
					"checked for a version", a.where)
				continue
			}
			msg := err.Error()
			if !strings.Contains(msg, a.want) {
				t.Errorf("%s was refused by a different guard: the message is %q and does "+
					"not carry %q. A version check over the wrong sentence reports "+
					"the right answer about nothing", a.where, msg, a.want)
				continue
			}
			if v := releaseVersionIn(msg); v != "" {
				t.Errorf("the refusal for %s names the release %s: %q. The combination is "+
					"refused in every release that carries this code, so the version "+
					"tells the operator nothing and goes stale at the next tag",
					a.where, v, msg)
			}
		}
	})

	t.Run("the message names every option that reaches it", func(t *testing.T) {
		spellings := ipv6SwitchingOptions(t)
		if len(spellings) < 2 {
			t.Fatalf("only %v can switch IPv6 on, so a message naming one option would "+
				"satisfy this arm and the count word below would carry no claim",
				spellings)
		}
		err := createIPAMBridgeNetworkOpts(t,
			map[string]interface{}{"ipv6_mode": proto.Mode6SLAAC.String()}, ipamLocalAddressSpace)
		if err == nil {
			t.Fatal("the refusal did not fire, so there is no message to read")
		}
		msg := err.Error()
		for _, spelling := range spellings {
			// `-o ipv6=`, since a bare `ipv6` is a substring of `ipv6_mode`.
			want := "-o " + spelling + "="
			if !strings.Contains(msg, want) {
				t.Errorf("`%s` can switch IPv6 on and reaches this refusal, and the "+
					"message does not name it: %q. An operator who wrote it is told "+
					"to remove an option they never typed", want, msg)
			}
		}
		t.Logf("PASS  the refusal names %v, derived by driving ipv6Mode over "+
			"%d DHCPNetworkOptions field(s)", spellings,
			reflect.TypeOf(DHCPNetworkOptions{}).NumField())
		want := englishCount(t, len(spellings)) + " spellings reach this refusal"
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not carry %q: %q. %d option(s) reach this refusal, "+
				"and a count in a shipped sentence is a claim like any other",
				want, msg, len(spellings))
		}
	})

	t.Run("an IPAM network without ipv6 is created", func(t *testing.T) {
		if err := createIPAMBridgeNetwork(t, false, ipamLocalAddressSpace); err != nil {
			t.Fatalf("an IPAM network with no ipv6 option was refused: %v. The refusal is "+
				"about one combination and must not reach the ordinary shape", err)
		}
	})

	t.Run("a null-IPAM network with ipv6 is created", func(t *testing.T) {
		if err := createIPAMBridgeNetwork(t, true, "null"); err != nil {
			t.Fatalf("`-o ipv6=true` was refused on a --ipam-driver null network: %v. That is "+
				"the shipping product since v1.x and nothing in this issue touches it", err)
		}
	})

	t.Run("an IPAM network with ipv6_mode=off is created", func(t *testing.T) {
		if err := createIPAMBridgeNetworkOpts(t,
			map[string]interface{}{"ipv6_mode": proto.Mode6Off.String()}, ipamLocalAddressSpace); err != nil {
			t.Fatalf("`-o ipv6_mode=off` was refused on an IPAM network: %v. It switches "+
				"nothing on, so it is the ordinary shape written out", err)
		}
	})

	t.Run("a null-IPAM network with ipv6_mode=slaac is created", func(t *testing.T) {
		if err := createIPAMBridgeNetworkOpts(t,
			map[string]interface{}{"ipv6_mode": proto.Mode6SLAAC.String()}, "null"); err != nil {
			t.Fatalf("`-o ipv6_mode=slaac` was refused on a --ipam-driver null network: %v. "+
				"The refusal is about the IPAM shape and must not reach the shape that "+
				"serves every mode", err)
		}
	})
}

func ipv6SwitchingOptions(t *testing.T) []string {
	t.Helper()
	typ := reflect.TypeOf(DHCPNetworkOptions{})
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		values := candidateOptionValues(t, field)
		for _, v := range values {
			opts := DHCPNetworkOptions{}
			reflect.ValueOf(&opts).Elem().Field(i).Set(v)
			mode, err := opts.ipv6Mode()
			if err == nil && mode != proto.Mode6Off {
				out = append(out, optionSpelling(field))
				break
			}
		}
	}
	return out
}

func optionSpelling(field reflect.StructField) string {
	if tag := field.Tag.Get("mapstructure"); tag != "" {
		return strings.Split(tag, ",")[0]
	}
	return strings.ToLower(field.Name)
}

// A field kind this does not know fails by name. For strings only the library modes and a few spellings of "on"
// are tried, so the derived option set is a lower bound (#960).
func candidateOptionValues(t *testing.T, field reflect.StructField) []reflect.Value {
	t.Helper()
	switch field.Type.Kind() {
	case reflect.Bool:
		return []reflect.Value{reflect.ValueOf(true)}
	case reflect.String:
		var out []reflect.Value
		for _, m := range dhcp.IPv6Modes() {
			out = append(out, reflect.ValueOf(m))
		}
		for _, extra := range []string{"true", "1", "on"} {
			out = append(out, reflect.ValueOf(extra))
		}
		return out
	case reflect.Int64:
		v := reflect.New(field.Type).Elem()
		v.SetInt(int64(time.Second))
		return []reflect.Value{v}
	default:
		t.Errorf("DHCPNetworkOptions.%s is a %s, which this derivation does not know how to "+
			"drive. Add it, or the question \"which options switch IPv6 on\" is answered "+
			"over the fields that happen to be driveable", field.Name, field.Type.Kind())
		return nil
	}
}

func englishCount(t *testing.T, n int) string {
	t.Helper()
	words := []string{"Zero", "One", "Two", "Three", "Four", "Five", "Six"}
	if n < 0 || n >= len(words) {
		t.Fatalf("%d has no word here, so the count in the shipped sentence cannot be "+
			"checked. Extend the list", n)
	}
	return words[n]
}

// Engines below 28 lack GwAllocCheck, so moby's network.go asks the IPAM driver for a gateway with no address and
// `docker network create` fails with "failed to allocate gateway ()" on a refusal. The lane runs engine 29, which
// suppresses the call. The answer is the pool's own network address, which no DHCP server hands out (#1012).
func TestRequestAddress_TheGatewayOnAnEngineThatDoesNotAskGwAllocCheck(t *testing.T) {
	cases := []struct {
		name    string
		subnet  string
		want    string
		gateway string
	}{
		{name: "a network created with --subnet", subnet: ipamTestPool, want: ipamTestPool, gateway: "192.168.99.0"},
		{name: "a network created with no --subnet", subnet: "", want: ipamAnyPool, gateway: "0.0.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			const bridge = "br-ipam-gw"
			withFakeBridge(t, bridge)

			p := newPluginForTest()
			p.ipamPools = newIssuedPools()
			p.ipamIndex = newIPAMIndex()
			p.docker = &fakeDocker{}
			r, err := dhcp.OpenRecords(t.TempDir()+"/"+recordFileName, "test-instance")
			if err != nil {
				t.Fatalf("OpenRecords: %v", err)
			}
			t.Cleanup(func() { _ = r.Close() })
			p.records = r

			pool, err := p.RequestPool(RequestPoolRequest{
				AddressSpace: ipamLocalAddressSpace,
				Pool:         tc.subnet,
			})
			if err != nil {
				t.Fatalf("RequestPool: %v", err)
			}

			res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
				PoolID:  pool.PoolID,
				Options: map[string]string{ipamOptRequestAddressType: ipamOptGateway},
			})
			if err != nil {
				t.Fatalf("the gateway request an engine below 28 sends at `docker network "+
					"create` was refused: %v. The daemon turns this into \"failed to "+
					"allocate gateway ()\" and the network is not created (#1012)", err)
			}
			if res.Address != tc.want {
				t.Fatalf("gateway = %q, want %q: the pool's own network address, wearing the "+
					"pool's prefix", res.Address, tc.want)
			}

			if err := p.CreateNetwork(CreateNetworkRequest{
				NetworkID: ipamTestNetwork,
				Options: map[string]interface{}{
					util.OptionsKeyGeneric: map[string]interface{}{"bridge": bridge},
				},
				IPv4Data: []*IPAMData{{
					AddressSpace: ipamLocalAddressSpace,
					Pool:         pool.Pool,
					Gateway:      res.Address,
				}},
			}); err != nil {
				t.Fatalf("CreateNetwork with the gateway this driver answered: %v", err)
			}

			sn, err := ipamNetwork(ipamTestNetwork)
			if err != nil {
				t.Fatalf("the network was not bound: %v", err)
			}
			if sn.Binding.Gateway != tc.gateway {
				t.Errorf("the persisted gateway is %q, want %q. It is what `docker network "+
					"inspect` reports on a network created with no --subnet, and what the "+
					"echo branch compares an address against for the life of the network",
					sn.Binding.Gateway, tc.gateway)
			}

			// `docker network rm` releases the gateway first; no lease record holds it, so nothing is handed back.
			if err := p.ReleaseAddress(ReleaseAddressRequest{
				PoolID:  pool.PoolID,
				Address: tc.gateway,
			}); err != nil {
				t.Errorf("releasing the gateway at network delete: %v", err)
			}
			if n := p.ipamReleaseUnknown.Load(); n != 1 {
				t.Errorf("ipam_release_unknown = %d after the gateway was released, want 1. "+
					"The release either never reached the handler or was read as a "+
					"reservation being handed back", n)
			}

			back, err := p.RequestAddress(context.Background(), RequestAddressRequest{
				PoolID:  pool.PoolID,
				Address: tc.gateway,
			})
			if err != nil {
				t.Fatalf("the daemon-start replay of the gateway was refused: %v", err)
			}
			if bareAddress(back.Address) != tc.gateway {
				t.Errorf("the replayed gateway came back as %q, want %q", back.Address, tc.gateway)
			}
		})
	}
}

func TestIpamPoolOfID_ReadsThePoolBackOutOfTheIdentity(t *testing.T) {
	for _, tc := range []struct {
		opts map[string]string
		pool string
		want string
	}{
		{pool: ipamTestPool, want: ipamTestPool},
		{pool: "", want: ipamAnyPool},
		{pool: "192.168.99.7/24", want: ipamTestPool},
		{opts: map[string]string{"parent": "eth0"}, pool: ipamTestPool, want: ipamTestPool},
		{opts: map[string]string{"bridge": "br-lan"}, pool: "", want: ipamAnyPool},
	} {
		id, err := ipamPoolID(ipamLocalAddressSpace, tc.pool, tc.opts)
		if err != nil {
			t.Fatalf("ipamPoolID(%q, %v): %v", tc.pool, tc.opts, err)
		}
		got, err := ipamPoolOfID(id)
		if err != nil {
			t.Fatalf("ipamPoolOfID(%q): %v", id, err)
		}
		if got.String() != tc.want {
			t.Errorf("ipamPoolOfID(%q) = %q, want %q", id, got, tc.want)
		}
	}

	for _, bad := range []string{
		"null/0.0.0.0/0",
		"dhcp/somewhere-else/192.168.99.0/24",
		"dhcp/" + ipamLocalAddressSpace + "/192.168.99.0",
		"dhcp/" + ipamLocalAddressSpace + "/not-a-prefix/24",
		"dhcp/" + ipamLocalAddressSpace + "/fd00::/64",
	} {
		if _, err := ipamPoolOfID(bad); err == nil {
			t.Errorf("ipamPoolOfID(%q) was answered; it names no pool this driver issued", bad)
		} else if !errors.Is(err, util.ErrIPAM) {
			t.Errorf("ipamPoolOfID(%q) refused with %v, which does not wrap util.ErrIPAM", bad, err)
		}
	}
}

// Only the gateway is answered on an unbound pool; a container's lease request there has nothing to lease from.
func TestRequestAddress_OnlyTheGatewayIsAnsweredOnAnUnboundPool(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]string
	}{
		{name: "no options at all"},
		{name: "an endpoint being created", opts: map[string]string{ipamOptMacAddress: ipamTestMAC}},
		{name: "a request type that is not the gateway", opts: map[string]string{
			ipamOptRequestAddressType: "com.docker.network.endpoint.something",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b := ipamFixture(t)
			p.ipamIndex = newIPAMIndex()
			_, err := p.RequestAddress(context.Background(), RequestAddressRequest{
				PoolID:  b.PoolID,
				Options: tc.opts,
			})
			if err == nil {
				t.Fatal("an address was leased from a pool no network on this host holds. " +
					"Only a gateway-type request is answered there")
			}
			if !strings.Contains(err.Error(), "nothing to lease from") {
				t.Errorf("the refusal is %q and does not say what is wrong", err)
			}
		})
	}
}

// On a bound pool CreateNetwork has run, and the gateway comes from the DHCP server at Join, so this is refused.
func TestRequestAddress_AGatewayWithNoAddressOnABoundPoolNamesTheRemedy(t *testing.T) {
	p, b := ipamFixture(t)
	_, err := p.RequestAddress(context.Background(), RequestAddressRequest{
		PoolID:  b.PoolID,
		Options: map[string]string{ipamOptRequestAddressType: ipamOptGateway},
	})
	if err == nil {
		t.Fatal("a gateway address was allocated on a network that already exists")
	}
	if !strings.Contains(err.Error(), "--gateway") {
		t.Errorf("the refusal is %q and does not name the option that gets Docker's own "+
			"network record a gateway", err)
	}
}

// On a /31 both addresses are host addresses (RFC 3021 Section 2.1) and a /32 is one, so no gateway is invented
// there; a typed --gateway passes through on any prefix, and engines from 28 never ask (#1012).
func TestRequestAddress_TheGatewayAnswerHasTwoPrefixLengthsItRefuses(t *testing.T) {
	gwOpts := map[string]string{ipamOptRequestAddressType: ipamOptGateway}

	t.Run("a pool with no address that is not a host address is refused", func(t *testing.T) {
		for _, pool := range []string{"192.168.99.4/31", "192.168.99.7/32"} {
			p, _ := ipamFixture(t)
			p.ipamIndex = newIPAMIndex()
			id, err := ipamPoolID(ipamLocalAddressSpace, pool, nil)
			if err != nil {
				t.Fatalf("ipamPoolID(%q): %v", pool, err)
			}
			_, err = p.RequestAddress(context.Background(), RequestAddressRequest{
				PoolID: id, Options: gwOpts,
			})
			if err == nil {
				t.Fatalf("pool %s: an address was invented for the gateway. Every address in "+
					"this pool can be leased to a container, and the one taken here would "+
					"be answered from the binding for the life of the network", pool)
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("pool %s: the refusal %v does not wrap util.ErrIPAM", pool, err)
			}
			if !strings.Contains(err.Error(), "--gateway") {
				t.Errorf("pool %s: the refusal is %q and does not name the option that "+
					"supplies a gateway on such a network", pool, err)
			}
		}
	})

	t.Run("one bit shorter is answered", func(t *testing.T) {
		p, _ := ipamFixture(t)
		p.ipamIndex = newIPAMIndex()
		id, err := ipamPoolID(ipamLocalAddressSpace, "192.168.99.4/30", nil)
		if err != nil {
			t.Fatalf("ipamPoolID: %v", err)
		}
		res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
			PoolID: id, Options: gwOpts,
		})
		if err != nil {
			t.Fatalf("a /30 pool was refused: %v", err)
		}
		if res.Address != "192.168.99.4/30" {
			t.Errorf("gateway = %q, want 192.168.99.4/30", res.Address)
		}
	})

	t.Run("a typed --gateway is answered on the refused prefixes", func(t *testing.T) {
		for _, tc := range []struct{ pool, gw, want string }{
			{pool: "192.168.99.4/31", gw: "192.168.99.5", want: "192.168.99.5/32"},
			{pool: "192.168.99.7/32", gw: "192.168.99.7", want: "192.168.99.7/32"},
		} {
			p, _ := ipamFixture(t)
			p.ipamIndex = newIPAMIndex()
			id, err := ipamPoolID(ipamLocalAddressSpace, tc.pool, nil)
			if err != nil {
				t.Fatalf("ipamPoolID(%q): %v", tc.pool, err)
			}
			res, err := p.RequestAddress(context.Background(), RequestAddressRequest{
				PoolID: id, Address: tc.gw, Options: gwOpts,
			})
			if err != nil {
				t.Fatalf("pool %s with --gateway %s: %v", tc.pool, tc.gw, err)
			}
			if res.Address != tc.want {
				t.Errorf("pool %s: gateway = %q, want %q", tc.pool, res.Address, tc.want)
			}
		}
	})
}
