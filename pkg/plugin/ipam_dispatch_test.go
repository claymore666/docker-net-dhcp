// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

const (
	ipamTestNetwork = "net-ipam-1"
	ipamTestPool    = "192.168.99.0/24"
	ipamTestMAC     = "02:42:c0:a8:63:0a"
)

// ipamFixture is a plugin with one IPAM-mode network on disk, its pool
// bound, and a record store. No Docker client: the property asserted
// below is that these handlers never reach for one.
func ipamFixture(t *testing.T) (*Plugin, *ipamBinding) {
	t.Helper()
	withStateDir(t, t.TempDir())
	r, err := dhcp.OpenRecords(t.TempDir()+"/"+recordFileName, "test-instance")
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
	return p, b
}

// TestIpamHandlers_CallDockerZeroTimes is the property D46 was amended
// to, driven rather than argued.
//
// The daemon replays RequestPool and one RequestAddress per stored
// endpoint from inside libnetwork.New, which NewDaemon calls BEFORE the
// API listener starts. A handler that asked Docker anything there would
// block on a server that is not listening yet, and it would block on
// exactly the path a restart depends on. So the rule is not "avoid the
// API where convenient": it is that these four handlers never hold a
// Docker client at all.
//
// The observer is a plugin whose docker field is nil. A call would
// panic, which is louder than a counter and cannot be forgotten to
// assert on.
func TestIpamHandlers_CallDockerZeroTimes(t *testing.T) {
	p, b := ipamFixture(t)
	if p.docker != nil {
		t.Fatal("the fixture has a Docker client; this test proves nothing with one present")
	}
	ctx := context.Background()

	if _, err := p.RequestPool(RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool}); err != nil {
		t.Errorf("RequestPool: %v", err)
	}
	// The replay of a stored endpoint, which is the call the whole rule
	// is about. Its record is written first, as the previous process
	// would have left it.
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

// TestRequestAddress_Dispatch drives the branches that are wire-identical
// and can only be told apart by what the plugin knows.
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
		// Wire-identical to a stored endpoint's replay: an address, no
		// options. Only the binding says which it is.
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

// TestRequestPool_RefusesWhatV2_1DoesNotDo. Each refusal names what to
// do instead, because a refusal an operator cannot act on is a bug
// report.
func TestRequestPool_RefusesWhatV2_1DoesNotDo(t *testing.T) {
	p, _ := ipamFixture(t)
	cases := []struct {
		name string
		req  RequestPoolRequest
		says string
	}{
		{"an IPv6 pool", RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, V6: true}, "v2.2.0"},
		{"an --ip-range", RequestPoolRequest{AddressSpace: ipamLocalAddressSpace, Pool: ipamTestPool, SubPool: "192.168.99.128/25"}, "--ip-range"},
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
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("the refusal is %q and does not mention %q, so it tells the operator "+
					"nothing they can act on", err, c.says)
			}
		})
	}
}

// TestReleaseAddress_RetainsOnlyAReservation.
//
// A live endpoint's record is already RETAINED by DeleteEndpoint when
// this arrives, and re-retaining it would move the deadline of a
// tombstone the next container is about to claim. The case that needs
// work is the reservation nobody built an endpoint for.
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

// TestIpamBindingLost_RefusesRatherThanDegrades is defeat row A.
//
// The state file is the only place the binding lives, and Docker's own
// record does not carry it. A network served without it runs the null
// path: a second DHCP exchange, a JSON tombstone this shape does not
// use, and an address libnetwork already allocated and will refuse.
// Every one of those is silent here and arrives at the user as
// something else.
func TestIpamBindingLost_RefusesRatherThanDegrades(t *testing.T) {
	p, b := ipamFixture(t)
	path, err := stateFilePath(ipamTestNetwork)
	if err != nil {
		t.Fatalf("stateFilePath: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("corrupting the state file: %v", err)
	}
	_, err = p.RequestAddress(context.Background(), RequestAddressRequest{
		PoolID: b.PoolID, Address: "192.168.99.10",
	})
	if err == nil {
		t.Fatal("an IPAM-mode network with an unreadable binding was served anyway")
	}
	if !errors.Is(err, errIPAMBindingLost) {
		t.Errorf("error %v is not errIPAMBindingLost; the refusal has to be recognisable "+
			"to the one caller that may survive it (DeleteEndpoint)", err)
	}
}

// TestRebuildIPAMIndex_FoldsTheStateDirectory. At start-up the daemon
// replays RequestPool and RequestAddress before it serves its own API,
// so the PoolID -> network map has to come off the disk.
func TestRebuildIPAMIndex_FoldsTheStateDirectory(t *testing.T) {
	_, b := ipamFixture(t)
	// A null-mode network beside it, which must not appear in the index.
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

// TestIpamIndex_MatchesThePoolIDExactly. Two networks that share a
// subnet on two parents differ only in the suffix one of them typed, so
// a prefix match would hand the second network's requests to the first.
func TestIpamIndex_MatchesThePoolIDExactly(t *testing.T) {
	x := newIPAMIndex()
	x.bind("dhcp/dhcp-local/192.168.0.0/24", "net-a")
	if got, ok := x.network("dhcp/dhcp-local/192.168.0.0/24/parent=eth1"); ok {
		t.Errorf("the suffixed PoolID resolved to %q; the second network would take the "+
			"first one's binding", got)
	}
	if _, taken := x.boundTo("dhcp/dhcp-local/192.168.0.0/24", "net-b"); !taken {
		t.Error("a PoolID another network holds was not reported as taken, so two networks " +
			"would bind one pool")
	}
	if _, taken := x.boundTo("dhcp/dhcp-local/192.168.0.0/24", "net-a"); taken {
		t.Error("a network's own binding was reported as taken by someone else")
	}
}

// TestIpamACKInPool is D50: an ACK outside the subnet the user typed is
// refused, and a network that typed none has nothing to say about it.
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
