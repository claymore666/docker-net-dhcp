// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const f0Bridge = "br-f0-absent"

const (
	f0Addr  = "192.168.99.10/24"
	f0Addr2 = "192.168.99.11/24"
)

func f0Parent(t *testing.T, name string) {
	t.Helper()
	link := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, Index: 7}}
	prevByName, prevList := nlLinkByName, nlAddrList
	nlLinkByName = func(n string) (netlink.Link, error) {
		if n != name {
			return nil, netlink.LinkNotFoundError{}
		}
		return link, nil
	}
	nlAddrList = func(_ netlink.Link, family int) ([]netlink.Addr, error) {
		if family != unix.AF_INET {
			return nil, nil
		}
		pa, err := netlink.ParseAddr("192.168.99.2/24")
		if err != nil {
			t.Fatalf("ParseAddr: %v", err)
		}
		return []netlink.Addr{*pa}, nil
	}
	t.Cleanup(func() { nlLinkByName, nlAddrList = prevByName, prevList })
}

func f0Fixture(t *testing.T) (*Plugin, *ipamBinding, *fakeSender, string) {
	t.Helper()
	withStateDir(t, t.TempDir())
	f0Parent(t, f0Bridge)
	sender := installSender(t, nil)

	journal := filepath.Join(t.TempDir(), recordFileName)
	p := &Plugin{
		joinHints:            make(map[string]joinHint),
		persistentDHCP:       make(map[string]*dhcpManager),
		endpointFingerprints: make(map[string]endpointFingerprint),
		ipamPools:            newIssuedPools(),
		ipamIndex:            newIPAMIndex(),
		ipamReserves:         newIPAMReserves(),
	}
	f0Reopen(t, p, journal, "proc-1")

	poolID, err := ipamPoolID(ipamLocalAddressSpace, ipamTestPool, nil)
	if err != nil {
		t.Fatalf("ipamPoolID: %v", err)
	}
	b := &ipamBinding{
		PoolID:  poolID,
		Space:   ipamLocalAddressSpace,
		Pool:    ipamTestPool,
		Gateway: "192.168.99.1",
	}
	if err := saveNetwork(ipamTestNetwork, f0Options(), b); err != nil {
		t.Fatalf("saveNetwork: %v", err)
	}
	p.ipamIndex.bind(poolID, ipamTestNetwork)
	return p, b, sender, journal
}

func f0Options() DHCPNetworkOptions {
	return DHCPNetworkOptions{Mode: ModeBridge, Bridge: f0Bridge, ReleaseLease: ReleaseOnRemove}
}

func f0Reopen(t *testing.T, p *Plugin, journal, instance string) {
	t.Helper()
	if p.records != nil {
		_ = p.records.Close()
	}
	r, err := dhcp.OpenRecords(journal, instance)
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	p.records = r
}

func f0Rec(t *testing.T, p *Plugin, id string) lease.Record {
	t.Helper()
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	rec, ok := rb.ByID(id)
	if !ok {
		t.Fatalf("record %q is gone from the store", id)
	}
	return rec
}

func f0MAC(n byte) net.HardwareAddr {
	return net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, n}
}

func f0Tombstone(t *testing.T, p *Plugin, mac net.HardwareAddr, addr string) string {
	t.Helper()
	id := p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity(mac))
	if id == "" {
		t.Fatal("recordCreated wrote nothing")
	}
	if err := p.records.Observed(id, acquired(addr, time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Bound(id); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	p.recordRetained(id, time.Now().Add(tombstoneTTL))
	return id
}

func f0Created(t *testing.T, p *Plugin, mac net.HardwareAddr, addr string) string {
	t.Helper()
	id := p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity(mac))
	if id == "" {
		t.Fatal("recordCreated wrote nothing")
	}
	if addr != "" {
		if err := p.records.Observed(id, acquired(addr, time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
	}
	return id
}

func f0Rebind(t *testing.T, p *Plugin, mac net.HardwareAddr, want string) string {
	t.Helper()
	id, addr, _, _ := p.ipamRebindCandidate(ipamTestNetwork, mac)
	if id == "" {
		t.Fatal("nothing was re-bound; the fixture has no single tombstone")
	}
	if addr != want {
		t.Fatalf("re-bound address %q, want %q", addr, want)
	}
	return id
}

func f0Reservation(p *Plugin, b *ipamBinding, mac net.HardwareAddr, recordID, addr string, err error) {
	key := ipamReserveKey(b.PoolID, mac)
	r, _ := p.ipamReserves.begin(key, time.Now())
	p.ipamReserves.finish(key, r, ipamReservation{
		addr:   netip.MustParsePrefix(addr),
		info:   dhcp.Info{IP: addr, Gateway: "192.168.99.1"},
		record: recordID,
	}, err)
}

func f0CreateRequest(mac net.HardwareAddr, addr string) CreateEndpointRequest {
	return CreateEndpointRequest{
		NetworkID:  ipamTestNetwork,
		EndpointID: "ep-f0-0000000000",
		Interface:  &EndpointInterface{MacAddress: mac.String(), Address: addr},
	}
}

func f0Docker() *fakeDocker {
	epID := f0CreateRequest(f0MAC(0x02), "").EndpointID
	return &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			ipamTestNetwork: {Containers: map[string]dNetwork.EndpointResource{
				"ctr-f0": {EndpointID: epID},
			}},
		},
		containerResult: map[string]dContainer.InspectResponse{
			"ctr-f0": {Config: &dContainer.Config{Hostname: "web"}},
		},
	}
}

func f0Tombstones(t *testing.T, p *Plugin, at time.Time) int {
	t.Helper()
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	return len(rb.Tombstones(ipamTestNetwork, at))
}

func f0AddressIsFree(t *testing.T, p *Plugin, addr string, mac net.HardwareAddr) (byAddr, byMAC bool) {
	t.Helper()
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	_, live := ipamLiveRecord(rb, ipamTestNetwork, netip.MustParseAddr(addr))
	_, held := ipamLiveRecordForMAC(rb, ipamTestNetwork, mac, time.Now())
	return !live, !held
}

func TestIPAMCreateEndpoint_EveryExitAfterTheTakeHandsTheAddressBack(t *testing.T) {
	first, restarted, next := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03)

	for _, tc := range []struct {
		name  string
		drive func(t *testing.T, p *Plugin, b *ipamBinding, recordID string)
	}{
		{
			name: "the link build fails",
			drive: func(t *testing.T, p *Plugin, b *ipamBinding, recordID string) {
				f0Reservation(p, b, restarted, recordID, f0Addr, nil)
				_, err := p.createIPAMEndpoint(context.Background(), f0CreateRequest(restarted, f0Addr), f0Options(), b)
				if err == nil {
					t.Fatal("CreateEndpoint succeeded; this host has the bridge the fixture needs absent")
				}
			},
		},
		{
			name: "the reservation carries an error",
			drive: func(t *testing.T, p *Plugin, b *ipamBinding, recordID string) {
				f0Reservation(p, b, restarted, recordID, f0Addr, errors.New("the exchange failed"))
				key := ipamReserveKey(b.PoolID, restarted)
				r, _ := p.ipamReserves.begin(key, time.Now())
				p.ipamReserves.finish(key, r, ipamReservation{record: recordID}, nil)
				r.err = errors.New("the exchange failed")
				if _, err := p.createIPAMEndpoint(context.Background(), f0CreateRequest(restarted, f0Addr), f0Options(), b); err == nil {
					t.Fatal("CreateEndpoint accepted a failed reservation")
				}
			},
		},
		{
			name: "Docker published a different address",
			drive: func(t *testing.T, p *Plugin, b *ipamBinding, recordID string) {
				f0Reservation(p, b, restarted, recordID, f0Addr, nil)
				if _, err := p.createIPAMEndpoint(context.Background(), f0CreateRequest(restarted, "192.168.99.44/24"), f0Options(), b); err == nil {
					t.Fatal("CreateEndpoint accepted an address the reservation does not hold")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, sender, _ := f0Fixture(t)
			p.docker = f0Docker()
			now := time.Now()
			id := f0Tombstone(t, p, first, f0Addr)
			if got := f0Rebind(t, p, restarted, "192.168.99.10"); got != id {
				t.Fatalf("the re-bind took %q, want the network's one tombstone %q", got, id)
			}

			tc.drive(t, p, b, id)

			if got := f0Rec(t, p, id).Phase; got != lease.PhaseRetained {
				t.Fatalf("after the failure the record is %v, want retained. A created record with "+
					"no endpoint on it holds its address for the life of the network: no tombstone "+
					"for the retry, and no release on any value of release_lease.", got)
			}
			if n := f0Tombstones(t, p, now); n != 1 {
				t.Errorf("tombstones = %d, want 1: the retry re-binds exactly one candidate", n)
			}
			byAddr, byMAC := f0AddressIsFree(t, p, "192.168.99.10", restarted)
			if !byAddr {
				t.Errorf("the address is still answered as held, so `docker run --ip 192.168.99.10` " +
					"is refused as belonging to another endpoint while no endpoint has it")
			}
			if !byMAC {
				t.Errorf("the hardware address is still answered as leasing, so a container pinned " +
					"with --mac-address cannot be started again at all")
			}
			againID, againAddr, _, _ := p.ipamRebindCandidate(ipamTestNetwork, next)
			if againID != id || againAddr != "192.168.99.10" {
				t.Errorf("the retry re-bound (%q, %q), want (%q, 192.168.99.10). Without the "+
					"candidate the container takes a second lease and nothing counts it.",
					againID, againAddr, id)
			}
			if n := sender.callCount(); n != 0 {
				t.Errorf("%d releases went on the wire while the address was being handed to the retry", n)
			}
		})
	}
}

func TestIPAMCreateEndpoint_AnAddresslessRecordIsClosedNotRetained(t *testing.T) {
	restarted := f0MAC(0x02)
	p, b, _, _ := f0Fixture(t)
	p.docker = f0Docker()
	now := time.Now()

	id := p.recordReserved(ipamTestNetwork, restarted, dhcp.ClientIdentity(restarted))
	f0Reservation(p, b, restarted, id, f0Addr, nil)
	if _, err := p.createIPAMEndpoint(context.Background(), f0CreateRequest(restarted, "192.168.99.44/24"), f0Options(), b); err == nil {
		t.Fatal("CreateEndpoint accepted an address the reservation does not hold")
	}
	if got := f0Rec(t, p, id).Phase; got != lease.PhaseClosed {
		t.Errorf("phase = %v, want closed. A record holding no address must not become a "+
			"tombstone: it would be a second re-bind candidate, and two candidates is the "+
			"documented ambiguity that costs a real container its address.", got)
	}
	if n := f0Tombstones(t, p, now); n != 0 {
		t.Errorf("tombstones = %d, want 0", n)
	}
}

func TestIPAMReserve_ThePreExchangeExitsGiveTheCandidateBack(t *testing.T) {
	first, restarted, next := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03)

	for _, tc := range []struct {
		name string
		opts func() DHCPNetworkOptions
	}{
		{
			name: "the server policy cannot be read",
			opts: func() DHCPNetworkOptions {
				o := f0Options()
				o.DHCPServers = "not-an-address"
				return o
			},
		},
		{
			name: "the conflict check cannot be read",
			opts: func() DHCPNetworkOptions {
				o := f0Options()
				o.ConflictCheck = "sometimes"
				return o
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, sender, _ := f0Fixture(t)
			id := f0Tombstone(t, p, first, f0Addr)

			removed := 0
			prev := ipamAddReserveLink
			ipamAddReserveLink = func(_ *Plugin, _ context.Context, _, _, _ string, _ DHCPNetworkOptions, _ net.HardwareAddr) (func(), error) {
				return func() { removed++ }, nil
			}
			t.Cleanup(func() { ipamAddReserveLink = prev })

			sn := storedNetwork{Options: tc.opts(), Binding: b}
			if _, err := p.ipamReserveAddress(context.Background(), ipamTestNetwork, sn, restarted, ""); err == nil {
				t.Fatal("the reserve accepted options it cannot read")
			}
			if removed != 1 {
				t.Errorf("the reservation link was removed %d times, want 1", removed)
			}
			if got := f0Rec(t, p, id).Phase; got != lease.PhaseRetained {
				t.Fatalf("the re-bound record is %v, want retained. The attempt never reached the "+
					"server, so it must not spend the container's restart window.", got)
			}
			againID, againAddr, _, _ := p.ipamRebindCandidate(ipamTestNetwork, next)
			if againID != id || againAddr != "192.168.99.10" {
				t.Errorf("the retry re-bound (%q, %q), want (%q, 192.168.99.10)", againID, againAddr, id)
			}
			if n := sender.callCount(); n != 0 {
				t.Errorf("%d releases went on the wire for an exchange that never ran", n)
			}
		})
	}
}

func TestIPAMReserve_AnAbandonedWindowStillEndsOnTheWire(t *testing.T) {
	first, restarted := f0MAC(0x01), f0MAC(0x02)
	p, b, sender, _ := f0Fixture(t)
	p.docker = f0Docker()
	id := f0Tombstone(t, p, first, f0Addr)
	f0Rebind(t, p, restarted, "192.168.99.10")
	f0Reservation(p, b, restarted, id, f0Addr, nil)
	if _, err := p.createIPAMEndpoint(context.Background(), f0CreateRequest(restarted, f0Addr), f0Options(), b); err == nil {
		t.Fatal("CreateEndpoint succeeded; this host has the bridge the fixture needs absent")
	}

	if n := p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle)); n != 1 {
		t.Fatalf("the sweep handed back %d addresses once the window had closed, want 1", n)
	}
	if n := sender.callCount(); n != 1 {
		t.Fatalf("%d releases on the wire, want 1: an address nobody claimed inside the window "+
			"is exactly what on_remove hands back", n)
	}
}

func TestIPAMReserve_ARefusedACKIsNotHandedToTheNextContainer(t *testing.T) {
	first, mine, next := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03)
	const refused = "10.9.9.9/24"

	for _, tc := range []struct {
		name string
		take func(t *testing.T, p *Plugin) string
	}{
		{
			name: "a fresh reservation",
			take: func(t *testing.T, p *Plugin) string {
				t.Helper()
				id := p.recordReserved(ipamTestNetwork, mine, dhcp.ClientIdentity(mine))
				if id == "" {
					t.Fatal("recordReserved wrote nothing")
				}
				return id
			},
		},
		{
			name: "a re-bound record",
			take: func(t *testing.T, p *Plugin) string {
				t.Helper()
				f0Tombstone(t, p, first, f0Addr)
				return f0Rebind(t, p, mine, "192.168.99.10")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, sender, _ := f0Fixture(t)
			now := time.Now()
			id := tc.take(t, p)

			if err := p.records.Observed(id, acquired(refused, time.Hour), nil); err != nil {
				t.Fatalf("Observed: %v", err)
			}
			if _, err := ipamAcceptedReservation(dhcp.Info{IP: refused}, id, b.Pool, ""); err == nil {
				t.Fatal("the ACK was accepted, so this test no longer drives the exit it is about")
			}

			p.ipamGiveUpAttempt(id, false, now)

			if got := f0Rec(t, p, id).Phase; got != lease.PhaseClosed {
				t.Errorf("phase = %v, want closed", got)
			}
			if n := f0Tombstones(t, p, now); n != 0 {
				t.Errorf("tombstones = %d, want 0", n)
			}
			gotID, gotAddr, gotIdentity, _ := p.ipamRebindCandidate(ipamTestNetwork, next)
			if gotID != "" {
				t.Errorf("the next container was handed record %q, carrying %q", gotID, gotAddr)
			}
			if ask, demand := ipamExchangeAddresses("", gotAddr); ask != "" || demand != "" {
				t.Errorf("the next container's exchange asks for %q and demands %q, want neither: "+
					"that address was refused once already and will be refused again", ask, demand)
			}
			fresh := dhcp.ClientIdentity(next)
			if got := ipamExchangeClientID(fresh, gotIdentity); !bytes.Equal(got, fresh) {
				t.Errorf("the next container's exchange goes out under %x, want its own identity %x", got, fresh)
			}
			if n := p.sweepDeferredReleases(now.Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
				t.Errorf("an address the plugin never accepted was released: swept=%d sent=%d", n, sender.callCount())
			}
		})
	}
}

func TestIPAMReleaseAddress_GivesUpACreatedRecordWithNoEndpoint(t *testing.T) {
	first, restarted, next := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03)
	p, b, sender, _ := f0Fixture(t)
	id := f0Tombstone(t, p, first, f0Addr)
	f0Rebind(t, p, restarted, "192.168.99.10")
	f0Reservation(p, b, restarted, id, f0Addr, nil)
	p.ipamReserves.take(ipamReserveKey(b.PoolID, restarted))

	if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
		t.Fatalf("ReleaseAddress: %v", err)
	}
	if got := f0Rec(t, p, id).Phase; got != lease.PhaseRetained {
		t.Fatalf("phase = %v, want retained", got)
	}
	byAddr, byMAC := f0AddressIsFree(t, p, "192.168.99.10", restarted)
	if !byAddr || !byMAC {
		t.Errorf("after the release the address is still held (by address: %v, by hardware address: %v)", !byAddr, !byMAC)
	}
	againID, againAddr, _, _ := p.ipamRebindCandidate(ipamTestNetwork, next)
	if againID != id || againAddr != "192.168.99.10" {
		t.Errorf("the retry re-bound (%q, %q), want (%q, 192.168.99.10)", againID, againAddr, id)
	}
	if n := sender.callCount(); n != 0 {
		t.Errorf("%d releases on the wire while the address was being handed to the retry", n)
	}
}

func TestIPAMReleaseAddress_FreesTheHardwareAddressForTheRetry(t *testing.T) {
	mac := f0MAC(0x01)
	p, b, _, _ := f0Fixture(t)
	id := f0Created(t, p, mac, f0Addr)
	f0Reservation(p, b, mac, id, f0Addr, nil)

	if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
		t.Fatalf("ReleaseAddress: %v", err)
	}
	if _, owns := p.ipamReserves.begin(ipamReserveKey(b.PoolID, mac), time.Now()); !owns {
		t.Errorf("the next address request under this hardware address does not own its own " +
			"exchange: it waits on the reservation of the attempt that was just given up and " +
			"takes its answer, naming a record nobody holds")
	}
}

func TestIPAMReleaseAddress_LeavesAnExchangeInFlightAlone(t *testing.T) {
	first, restarted := f0MAC(0x01), f0MAC(0x02)
	p, b, sender, _ := f0Fixture(t)
	now := time.Now()
	id := f0Tombstone(t, p, first, f0Addr)
	f0Rebind(t, p, restarted, "192.168.99.10")
	key := ipamReserveKey(b.PoolID, restarted)
	if _, owns := p.ipamReserves.begin(key, now); !owns {
		t.Fatal("the fixture did not start an exchange")
	}

	if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
		t.Fatalf("ReleaseAddress: %v", err)
	}
	if got := f0Rec(t, p, id).Phase; got != lease.PhaseCreated {
		t.Errorf("phase = %v, want created: the exchange holding this record is still running", got)
	}
	if n := f0Tombstones(t, p, now); n != 0 {
		t.Errorf("tombstones = %d, want 0", n)
	}
	if !p.ipamReserves.inFlight(key) {
		t.Error("the running exchange lost its reservation, so its answer goes nowhere")
	}
	if n := p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle)); n != 0 {
		t.Errorf("the sweep handed back %d addresses once the window had closed, for a record nobody gave up", n)
	}
	if n := sender.callCount(); n != 0 {
		t.Errorf("%d releases went on the wire under a running exchange", n)
	}
}

func TestIPAMReleaseAddress_ClosesARecordWhoseLeaseHasGone(t *testing.T) {
	mac := f0MAC(0x01)
	p, b, _, _ := f0Fixture(t)
	now := time.Now()
	id := p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity(mac))
	if err := p.records.Observed(id, acquired(f0Addr, -time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
		t.Fatalf("ReleaseAddress: %v", err)
	}
	if got := f0Rec(t, p, id).Phase; got != lease.PhaseClosed {
		t.Errorf("phase = %v, want closed", got)
	}
	if n := f0Tombstones(t, p, now); n != 0 {
		t.Errorf("tombstones = %d, want 0", n)
	}
}

func TestIPAMReleaseAddress_LeavesARunningEndpointAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		hold func(t *testing.T, p *Plugin, id string, mac net.HardwareAddr) lease.Phase
	}{
		{
			name: "joined",
			hold: func(t *testing.T, p *Plugin, id string, _ net.HardwareAddr) lease.Phase {
				t.Helper()
				if err := p.records.Bound(id); err != nil {
					t.Fatalf("Bound: %v", err)
				}
				return lease.PhaseJoined
			},
		},
		{
			name: "created, endpoint held by this process",
			hold: func(t *testing.T, p *Plugin, _ string, mac net.HardwareAddr) lease.Phase {
				t.Helper()
				p.rememberEndpoint("ep-f0-running", endpointFingerprint{
					MAC:  mac.String(),
					IPv4: "192.168.99.10",
				}, dhcpHostname{})
				return lease.PhaseCreated
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			running, other := f0MAC(0x01), f0MAC(0x02)
			p, b, sender, _ := f0Fixture(t)
			now := time.Now()

			id := f0Created(t, p, running, f0Addr)
			want := tc.hold(t, p, id, running)
			if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
				t.Fatalf("ReleaseAddress: %v", err)
			}
			if got := f0Rec(t, p, id).Phase; got != want {
				t.Fatalf("phase = %v, want %v: a running container's record was given up", got, want)
			}
			if n := f0Tombstones(t, p, now); n != 0 {
				t.Errorf("tombstones = %d, want 0", n)
			}
			if x, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, other); x != "" {
				t.Errorf("a second container re-bound the running container's record %q", x)
			}
			if n := p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
				t.Errorf("a release went on the wire for a running container: swept=%d sent=%d", n, sender.callCount())
			}
		})
	}
}

func TestIPAMReleaseAddress_OnlyTheWholeKeyBlocksTheGiveUp(t *testing.T) {
	mine, elsewhere := f0MAC(0x01), f0MAC(0x09)
	for _, tc := range []struct {
		name string
		fp   endpointFingerprint
	}{
		{
			name: "another hardware address holds this address",
			fp:   endpointFingerprint{MAC: elsewhere.String(), IPv4: "192.168.99.10"},
		},
		{
			name: "this hardware address holds another address",
			fp:   endpointFingerprint{MAC: mine.String(), IPv4: "192.168.99.11"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, sender, _ := f0Fixture(t)
			now := time.Now()
			id := f0Created(t, p, mine, f0Addr)
			p.rememberEndpoint("ep-f0-elsewhere", tc.fp, dhcpHostname{})

			if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
				t.Fatalf("ReleaseAddress: %v", err)
			}
			if got := f0Rec(t, p, id).Phase; got != lease.PhaseRetained {
				t.Fatalf("phase = %v, want retained: an endpoint holding half the key kept this record "+
					"from being handed back, and nothing else reaches it", got)
			}
			if n := f0Tombstones(t, p, now); n != 1 {
				t.Errorf("tombstones = %d, want 1", n)
			}
			if got, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, f0MAC(0x02)); got == "" {
				t.Error("the retry was offered nothing, so the address is stranded")
			}
			if n := p.sweepDeferredReleases(now.Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
				t.Errorf("a release went out for a lease that was never bound: swept=%d sent=%d", n, sender.callCount())
			}
		})
	}
}

func TestIPAMReleaseAddress_AFailedTeardownDoesNotBlockTheGiveUp(t *testing.T) {
	mine, next := f0MAC(0x01), f0MAC(0x02)
	p, b, sender, _ := f0Fixture(t)
	p.docker = &fakeDocker{inspectErr: errors.New("this network's options cannot be read")}
	now := time.Now()
	id := f0Created(t, p, mine, f0Addr)
	p.rememberEndpoint("ep-f0-rollback", endpointFingerprint{
		MAC:  mine.String(),
		IPv4: "192.168.99.10",
	}, dhcpHostname{})

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: "net-f0-unreadable", EndpointID: "ep-f0-rollback",
	}); err == nil {
		t.Fatal("DeleteEndpoint returned success for a network whose options cannot be read; " +
			"this case exists for the exit that fails, and the fixture no longer reaches it")
	}
	if p.ipamEndpointHolds(mine, "192.168.99.10") {
		t.Fatal("the fingerprint outlived a teardown that returned early, so the release handler " +
			"reads an endpoint the engine has already torn down as one that is still up")
	}

	if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
		t.Fatalf("ReleaseAddress: %v", err)
	}
	if got := f0Rec(t, p, id).Phase; got != lease.PhaseRetained {
		t.Fatalf("phase = %v, want retained: the record was not handed back after a failed teardown", got)
	}
	if n := f0Tombstones(t, p, now); n != 1 {
		t.Errorf("tombstones = %d, want 1", n)
	}
	if got, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, next); got == "" {
		t.Error("the retry was offered nothing, so the address is stranded until the next plugin start")
	}
	if n := p.sweepDeferredReleases(now.Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
		t.Errorf("a release went out for a lease that was never bound: swept=%d sent=%d", n, sender.callCount())
	}
}

func TestIPAMListedMACs_OneUnreadableEntryPoisonsTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		containers map[string]dNetwork.EndpointResource
		want       int
		ok         bool
	}{
		{"an empty network", map[string]dNetwork.EndpointResource{}, 0, true},
		{
			name:       "two endpoints",
			containers: map[string]dNetwork.EndpointResource{"a": {MacAddress: "02:00:00:00:00:01"}, "b": {MacAddress: "02:00:00:00:00:02"}},
			want:       2, ok: true,
		},
		{
			name: "a placeholder with no sandbox yet is an endpoint like any other",
			containers: map[string]dNetwork.EndpointResource{
				"ep-0123456789ab": {MacAddress: "02:00:00:00:00:01", EndpointID: "0123456789ab"},
			},
			want: 1, ok: true,
		},
		{
			name:       "one entry carries no hardware address",
			containers: map[string]dNetwork.EndpointResource{"a": {MacAddress: "02:00:00:00:00:01"}, "b": {}},
			ok:         false,
		},
		{
			name:       "one entry carries a hardware address that cannot be read",
			containers: map[string]dNetwork.EndpointResource{"a": {MacAddress: "02:00:00:00:00:01"}, "b": {MacAddress: "zz"}},
			ok:         false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ipamListedMACs(tc.containers)
			if ok != tc.ok {
				t.Fatalf("usable = %v, want %v", ok, tc.ok)
			}
			if ok && len(got) != tc.want {
				t.Errorf("listed %d hardware addresses, want %d", len(got), tc.want)
			}
			if !ok && got != nil {
				t.Errorf("an unusable answer returned %d entries; every caller must write nothing", len(got))
			}
		})
	}
}

func TestIPAMStrandedRecords_TheRuleKeysOnTheWriterAndTheEngineList(t *testing.T) {
	running, restarted, inflightMAC, removed := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03), f0MAC(0x04)
	p, _, sender, journal := f0Fixture(t)

	runningID := f0Created(t, p, running, f0Addr)
	f0Tombstone(t, p, removed, f0Addr2)
	strandedID := f0Rebind(t, p, restarted, "192.168.99.11")

	f0Reopen(t, p, journal, "proc-2")
	now := time.Now()
	inflightID := f0Created(t, p, inflightMAC, "192.168.99.12/24")

	if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, []net.HardwareAddr{running}, now); n != 1 {
		t.Fatalf("the rule gave up %d records, want 1", n)
	}
	if got := f0Rec(t, p, runningID).Phase; got != lease.PhaseCreated {
		t.Errorf("the running container's record is %v, want created: the engine lists its "+
			"hardware address, so it is owned whether or not its bind has landed", got)
	}
	if got := f0Rec(t, p, inflightID).Phase; got != lease.PhaseCreated {
		t.Errorf("a start in flight in this process is %v, want created", got)
	}
	if got := f0Rec(t, p, strandedID).Phase; got != lease.PhaseRetained {
		t.Fatalf("the stranded record is %v, want retained", got)
	}
	if n := p.ipamStrandedRecords.Load(); n != 1 {
		t.Errorf("ipam_stranded_records = %d, want 1", n)
	}

	againID, againAddr, _, _ := p.ipamRebindCandidate(ipamTestNetwork, f0MAC(0x05))
	if againID != strandedID || againAddr != "192.168.99.11" {
		t.Errorf("the retry re-bound (%q, %q), want (%q, 192.168.99.11)", againID, againAddr, strandedID)
	}
	if n := p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
		t.Errorf("a release went on the wire: swept=%d sent=%d. The stranded address was claimed "+
			"back inside the window and the other two belong to containers that are up.",
			n, sender.callCount())
	}
}

func TestIPAMStrandedRecords_TheRunningEndpointStillHeals(t *testing.T) {
	running := f0MAC(0x01)
	p, _, sender, journal := f0Fixture(t)
	id := f0Created(t, p, running, f0Addr)
	f0Reopen(t, p, journal, "proc-2")
	now := time.Now()

	if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, []net.HardwareAddr{running}, now); n != 0 {
		t.Fatalf("the rule gave up %d records, want 0", n)
	}
	rid, res := p.recordResume(ipamTestNetwork, running)
	if rid != id || res.Lease == nil {
		t.Fatalf("recovery resumed (%q, lease=%v), want (%q, a lease)", rid, res.Lease != nil, id)
	}
	p.recordBound(rid, res.Phase)
	if got := f0Rec(t, p, id); got.Phase != lease.PhaseJoined || got.Instance != "proc-2" {
		t.Errorf("after recovery's bind: phase=%v writer=%q, want joined and this process", got.Phase, got.Instance)
	}
	if n := p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
		t.Errorf("a release went on the wire for a running container: swept=%d sent=%d", n, sender.callCount())
	}
}

func TestIPAMStrandedRecords_ARecordHoldingNothingIsClosed(t *testing.T) {
	stale := f0MAC(0x01)
	p, _, _, journal := f0Fixture(t)
	id := f0Created(t, p, stale, "")
	f0Reopen(t, p, journal, "proc-2")
	now := time.Now()

	if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, nil, now); n != 1 {
		t.Fatalf("the rule gave up %d records, want 1", n)
	}
	if got := f0Rec(t, p, id).Phase; got != lease.PhaseClosed {
		t.Errorf("phase = %v, want closed", got)
	}
	if n := f0Tombstones(t, p, now); n != 0 {
		t.Errorf("tombstones = %d, want 0: an address-less tombstone is a full re-bind candidate "+
			"and makes a real one ambiguous", n)
	}
}

func TestIPAMStrandedRecords_RunTwiceWritesOnce(t *testing.T) {
	restarted, removed := f0MAC(0x02), f0MAC(0x04)
	p, _, _, journal := f0Fixture(t)
	f0Tombstone(t, p, removed, f0Addr)
	id := f0Rebind(t, p, restarted, "192.168.99.10")
	f0Reopen(t, p, journal, "proc-2")
	now := time.Now()

	if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, nil, now); n != 1 {
		t.Fatalf("first pass gave up %d records, want 1", n)
	}
	deadline := f0Rec(t, p, id).Deadline
	if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, nil, now.Add(30*time.Second)); n != 0 {
		t.Fatalf("second pass gave up %d records, want 0", n)
	}
	if got := f0Rec(t, p, id).Deadline; !got.Equal(deadline) {
		t.Errorf("the deadline moved from %v to %v on a second pass", deadline, got)
	}
	if n := p.ipamStrandedRecords.Load(); n != 1 {
		t.Errorf("ipam_stranded_records = %d, want 1", n)
	}
}

func TestIPAMStrandedRecords_AnotherNetworkIsNotTouched(t *testing.T) {
	mac := f0MAC(0x01)
	p, _, _, journal := f0Fixture(t)
	other := p.recordCreated("net-ipam-2", mac, dhcp.ClientIdentity(mac))
	if err := p.records.Observed(other, acquired(f0Addr, time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	f0Reopen(t, p, journal, "proc-2")

	if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, nil, time.Now()); n != 0 {
		t.Fatalf("the rule gave up %d records on a network it was not asked about", n)
	}
	if got := f0Rec(t, p, other).Phase; got != lease.PhaseCreated {
		t.Errorf("the other network's record is %v, want created", got)
	}
}

func TestRecoverEndpoints_RunsTheStrandedRuleOnIPAMNetworksOnly(t *testing.T) {
	restarted, removed := f0MAC(0x02), f0MAC(0x04)

	for _, tc := range []struct {
		name   string
		docker func() *fakeDocker
		ipam   bool
		want   lease.Phase
	}{
		{
			name: "an IPAM network the engine answers for",
			docker: func() *fakeDocker {
				return &fakeDocker{
					listResult: []dNetwork.Summary{{ID: ipamTestNetwork, Driver: testDHCPDriver}},
					inspectResult: map[string]dNetwork.Inspect{
						ipamTestNetwork: {ID: ipamTestNetwork, Driver: testDHCPDriver},
					},
				}
			},
			ipam: true,
			want: lease.PhaseRetained,
		},
		{
			name: "an endpoint whose hardware address cannot be read",
			docker: func() *fakeDocker {
				return &fakeDocker{
					listResult: []dNetwork.Summary{{ID: ipamTestNetwork, Driver: testDHCPDriver}},
					inspectResult: map[string]dNetwork.Inspect{
						ipamTestNetwork: {ID: ipamTestNetwork, Driver: testDHCPDriver, Containers: map[string]dNetwork.EndpointResource{
							"ctr": {MacAddress: "zz"},
						}},
					},
				}
			},
			ipam: true,
			want: lease.PhaseCreated,
		},
		{
			name: "an inspect that failed",
			docker: func() *fakeDocker {
				return &fakeDocker{
					listResult: []dNetwork.Summary{{ID: ipamTestNetwork, Driver: testDHCPDriver}},
					inspectErr: errors.New("inspect boom"),
				}
			},
			ipam: true,
			want: lease.PhaseCreated,
		},
		{
			name: "a network whose addresses this plugin does not allocate",
			docker: func() *fakeDocker {
				return &fakeDocker{
					listResult: []dNetwork.Summary{{ID: ipamTestNetwork, Driver: testDHCPDriver}},
					inspectResult: map[string]dNetwork.Inspect{
						ipamTestNetwork: {ID: ipamTestNetwork, Driver: testDHCPDriver},
					},
				}
			},
			ipam: false,
			want: lease.PhaseCreated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, _, journal := f0Fixture(t)
			if !tc.ipam {
				if err := saveOptions(ipamTestNetwork, f0Options()); err != nil {
					t.Fatalf("saveOptions: %v", err)
				}
			}
			_ = b
			f0Tombstone(t, p, removed, f0Addr)
			id := f0Rebind(t, p, restarted, "192.168.99.10")
			f0Reopen(t, p, journal, "proc-2")
			p.docker = tc.docker()

			p.recoverEndpoints(context.Background(), testDaemonWait)

			if got := f0Rec(t, p, id).Phase; got != tc.want {
				t.Errorf("phase = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIPAMStranded_DocumentedLimits(t *testing.T) {
	// Limit 1: an engine's endpoint enumeration drops entries it cannot read and still answers 200, so a running
	// container's record is given up (#1047).
	t.Run("a short engine list gives up a running endpoint's record", func(t *testing.T) {
		running := f0MAC(0x01)
		p, _, _, journal := f0Fixture(t)
		id := f0Created(t, p, running, f0Addr)
		f0Reopen(t, p, journal, "proc-2")

		if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, []net.HardwareAddr{}, time.Now()); n != 1 {
			t.Fatalf("gave up %d records, want 1", n)
		}
		if got := f0Rec(t, p, id).Phase; got != lease.PhaseRetained {
			t.Fatalf("phase = %v, want retained", got)
		}
		t.Log("documented limit: an engine that answers with a short list is indistinguishable " +
			"from a network with fewer endpoints, and the record of a running container in it " +
			"is handed back")
	})

	// Limit 2: an endpoint stored by a previous process is listed as ep-<id> while the engine retries Join; its
	// rollback reaches ReleaseAddress, not this rule (#1047).
	t.Run("a placeholder is left alone and the rollback gives it up", func(t *testing.T) {
		restarted, removed := f0MAC(0x02), f0MAC(0x04)
		p, b, _, journal := f0Fixture(t)
		f0Tombstone(t, p, removed, f0Addr)
		id := f0Rebind(t, p, restarted, "192.168.99.10")
		f0Reopen(t, p, journal, "proc-2")

		if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, []net.HardwareAddr{restarted}, time.Now()); n != 0 {
			t.Fatalf("the rule gave up %d listed records, want 0", n)
		}
		if got := f0Rec(t, p, id).Phase; got != lease.PhaseCreated {
			t.Fatalf("phase = %v, want created while the engine still lists it", got)
		}
		if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
			t.Fatalf("ReleaseAddress: %v", err)
		}
		if got := f0Rec(t, p, id).Phase; got != lease.PhaseRetained {
			t.Errorf("phase = %v, want retained: the rule ran while the endpoint was listed, so "+
				"the rollback's release is the only call left that can hand the address back", got)
		}
	})
}

func TestIPAMReserve_EveryExitAfterTheReBindHandsTheWindowBack(t *testing.T) {
	const (
		decl  = "giveUp := func(keepTheWindow bool) { p.ipamGiveUpAttempt(recordID, keepTheWindow, time.Now()) }"
		owned = "ipamGiveUpRecord"
	)
	body, err := os.ReadFile(filepath.Join(".", "ipam_reserve.go"))
	if err != nil {
		t.Fatalf("read ipam_reserve.go: %v", err)
	}
	src := string(body)
	from := strings.Index(src, "func (p *Plugin) runIPAMReserve(")
	if from < 0 {
		t.Fatal("runIPAMReserve is gone; this test no longer reads the function it names")
	}
	fn := src[from:]
	if end := strings.Index(fn, "\nfunc "); end > 0 {
		fn = fn[:end]
	}
	if !strings.Contains(fn, decl) {
		t.Fatalf("the reserve's give-up is not %q. The give-up for an accepted reservation retains "+
			"whatever the record holds, and what these exits hold is an address the server never "+
			"gave or one this plugin refused.", decl)
	}
	if strings.Contains(fn, owned) {
		t.Errorf("the reserve calls %s. That one is for a reservation an endpoint was going to "+
			"own; a failed exchange has none.", owned)
	}

	lines := strings.Split(fn, "\n")
	start := 0
	for i, l := range lines {
		if strings.Contains(l, decl) {
			start = i + 1
			break
		}
	}
	exits := 0
	for i := start; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(l, "return ") {
			continue
		}
		if !strings.Contains(l, "err") && !strings.Contains(l, "Errorf") {
			continue
		}
		exits++
		if prev := strings.TrimSpace(lines[i-1]); !strings.HasPrefix(prev, "giveUp(") {
			t.Errorf("the error exit %q is preceded by %q, want a give-up. The re-bind has already "+
				"taken this network's one tombstone, so an exit that keeps the record spends the "+
				"container's restart window on an attempt that failed.", l, prev)
		}
	}
	if exits < 4 {
		t.Errorf("found %d error exits after the re-bind, want at least the 4 this change covers: "+
			"the server policy, the conflict wiring, the exchange and the refused ACK", exits)
	}

	ack := -1
	for i := start; i < len(lines); i++ {
		if strings.Contains(lines[i], "ipamAcceptedReservation(") {
			ack = i
			break
		}
	}
	if ack < 0 {
		t.Fatal("the reserve no longer calls ipamAcceptedReservation; this test reads a function it no longer describes")
	}
	refused := false
	for i := ack; i < len(lines) && i < ack+20; i++ {
		if strings.TrimSpace(lines[i]) == "giveUp(false)" {
			refused = true
			break
		}
	}
	if !refused {
		t.Error("the refused-ACK exit keeps the window. The record holds the address the rules just " +
			"refused, so the tombstone it lays is offered to the next container on the network, " +
			"which asks for it under this container's identity and is refused the same way.")
	}
}
