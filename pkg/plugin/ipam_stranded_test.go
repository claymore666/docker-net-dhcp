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

// f0Bridge is a bridge that does not exist on the host running the
// test, which is what makes createIPAMEndpoint's link build fail
// without CAP_NET_ADMIN. It is also the name f0Parent answers for on
// the release path's own seam, so the same fixture serves both.
const f0Bridge = "br-f0-absent"

const (
	f0Addr  = "192.168.99.10/24"
	f0Addr2 = "192.168.99.11/24"
)

// f0Parent is hostParent under a chosen name, so the release path can
// resolve a bridge whose real counterpart must not exist.
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

// f0Fixture is an IPAM-mode network with release_lease=on_remove, a
// reopenable journal, and the wire under a fake.
//
// release_lease=on_remove is the value every case here is measured
// under because it is the one that turns a wrong answer into a
// datagram: a record retained when it should not have been becomes a
// DHCPRELEASE for a live address about 65 seconds later, and a record
// left in place when it should have been given up never produces one
// at all. The phase alone cannot tell those apart.
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

// f0Reopen ends the process holding the journal and opens it as the
// next one, which is the only way a record's last writer can differ
// from the process reading it.
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

// f0Tombstone is one endpoint that ran and was removed: the state a
// restart re-binds from.
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

// f0Created is an endpoint whose record sits in CREATED with a real
// lease on it: what CreateEndpoint leaves behind before the bind, and
// what a running container's record looks like when recordBound never
// ran (its goroutine is one neither Join nor recovery waits for).
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

// f0Rebind is RequestAddress's half of a restart: the network's one
// tombstone taken under the container's new hardware address. The
// record is CREATED from here on, with no endpoint behind it.
func f0Rebind(t *testing.T, p *Plugin, mac net.HardwareAddr, want string) string {
	t.Helper()
	id, addr, _ := p.ipamRebindCandidate(ipamTestNetwork, mac)
	if id == "" {
		t.Fatal("nothing was re-bound; the fixture has no single tombstone")
	}
	if addr != want {
		t.Fatalf("re-bound address %q, want %q", addr, want)
	}
	return id
}

// f0Reservation puts a finished, successful reservation in the map,
// exactly as runIPAMReserve leaves one for CreateEndpoint to take.
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

// f0Docker is a daemon that answers the hostname lookup at once, so a
// CreateEndpoint drive does not spend the two-second poll budget.
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

// f0AddressIsFree answers the two refusals a stranded record produces:
// `--ip` on its address and a container pinned to the hardware address
// it was re-bound to. Both are lookups the plugin makes before any
// packet, so both are readable without a server.
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

// TestIPAMCreateEndpoint_EveryExitAfterTheTakeHandsTheAddressBack is
// row 15 of the design note, driven where it actually happens.
//
// A restart re-binds the network's one tombstone before CreateEndpoint
// runs, so from the re-bind onwards the address is held by a record
// with no endpoint on it. If CreateEndpoint then fails, the reservation
// has already been taken out of the map -- the sweeper cannot see it --
// and a failed CreateEndpoint gets no DeleteEndpoint, so nothing else
// in the plugin ever reaches that record. Before this change it stayed
// CREATED for the life of the network: no tombstone for the retry to
// re-bind, `--ip` on its address refused as held by an endpoint that
// does not exist, the container's own pinned hardware address refused
// outright, and no release on the wire on any value of release_lease.
func TestIPAMCreateEndpoint_EveryExitAfterTheTakeHandsTheAddressBack(t *testing.T) {
	first, restarted, next := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03)

	for _, tc := range []struct {
		name string
		// drive runs CreateEndpoint into one exit and returns nothing;
		// the assertions below are the same for every exit.
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
				// finish deletes a failed reservation, so put it back
				// the way a caller that kept one would leave it.
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
			// The retry, inside the window: the same record, the same
			// address, which is the whole of what the window promises.
			againID, againAddr, _ := p.ipamRebindCandidate(ipamTestNetwork, next)
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

// TestIPAMCreateEndpoint_AnAddresslessRecordIsClosedNotRetained is the
// other direction, and the reason the give-up is one function.
//
// Tombstones filters on the phase and the deadline and never asks
// whether the record has an address, so a retained record holding
// nothing is a full re-bind candidate. Laid beside a real one it makes
// the pair ambiguous, and the container the real one belongs to loses
// its address to the DHCP server's choice.
func TestIPAMCreateEndpoint_AnAddresslessRecordIsClosedNotRetained(t *testing.T) {
	restarted := f0MAC(0x02)
	p, b, _, _ := f0Fixture(t)
	p.docker = f0Docker()
	now := time.Now()

	// A reserve that never got an ACK: the record exists, nothing is
	// on it. CreateEndpoint cannot normally follow one, so this drives
	// the give-up at the exit that can: the address mismatch.
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

// TestIPAMReserve_ThePreExchangeExitsGiveTheCandidateBack covers the
// two exits between the re-bind and the first packet.
//
// The re-bind is written before anything goes on the wire, because the
// exchange has to run under the identity the server already has a
// lease filed under, and that write clears the tombstone deadline. A
// reserve that then fails at its own option reads used to return with
// the record CREATED and the reservation deleted, which is the stranded
// state reached without Docker ever calling CreateEndpoint at all.
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

			// The link build needs CAP_NET_ADMIN and is the first thing
			// the reserve does, so nothing past it is reachable here
			// without the seam. Nothing is sent: both exits are before
			// the exchange.
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
			againID, againAddr, _ := p.ipamRebindCandidate(ipamTestNetwork, next)
			if againID != id || againAddr != "192.168.99.10" {
				t.Errorf("the retry re-bound (%q, %q), want (%q, 192.168.99.10)", againID, againAddr, id)
			}
			if n := sender.callCount(); n != 0 {
				t.Errorf("%d releases went on the wire for an exchange that never ran", n)
			}
		})
	}
}

// TestIPAMReserve_AnAbandonedWindowStillEndsOnTheWire is the positive
// control for every "nothing was sent" assertion above: the same
// fixture, the same failure, and nobody retrying. The address goes back
// to the server when the window closes, which is what release_lease=
// on_remove promises and what proves the sender is wired at all.
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

	// Read after the give-up, so the deadline it wrote is inside the
	// window this closes and not a moment past it.
	if n := p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle)); n != 1 {
		t.Fatalf("the sweep handed back %d addresses once the window had closed, want 1", n)
	}
	if n := sender.callCount(); n != 1 {
		t.Fatalf("%d releases on the wire, want 1: an address nobody claimed inside the window "+
			"is exactly what on_remove hands back", n)
	}
}

// TestIPAMReserve_ARefusedACKIsNotHandedToTheNextContainer is the exit
// where the server answered and the answer was refused.
//
// The fold files the address the moment the ACK arrives, before either
// acceptance rule has looked at it, so at this exit the record holds an
// address this plugin did not take. Laying it down as a tombstone puts
// it in front of the next container on the network, which then asks for
// it under the first container's identity and is refused in exactly the
// same way, for as long as something keeps renewing the window. On an
// on_remove network it is also a release for an address the plugin
// never accepted, about a minute later.
func TestIPAMReserve_ARefusedACKIsNotHandedToTheNextContainer(t *testing.T) {
	first, mine, next := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03)
	const refused = "10.9.9.9/24"

	for _, tc := range []struct {
		name string
		// take leaves the record in the shape the reserve reached this
		// exit with, and returns its id.
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
			// THE SAME EXIT WITH A WINDOW OWED. The re-bind took the
			// network's tombstone, and the refused ACK has overwritten
			// the address that tombstone was offering, so handing the
			// window back hands the next container an address it will
			// be refused for too.
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

			// The server answers with an address outside the pool the
			// network was created with, and the fold takes it before
			// either acceptance rule runs.
			if err := p.records.Observed(id, acquired(refused, time.Hour), nil); err != nil {
				t.Fatalf("Observed: %v", err)
			}
			if _, err := ipamAcceptedReservation(dhcp.Info{IP: refused}, id, b.Pool, ""); err == nil {
				t.Fatal("the ACK was accepted, so this test no longer drives the exit it is about")
			}

			// What the exit passes: no window, whatever was re-bound.
			p.ipamGiveUpAttempt(id, false, now)

			if got := f0Rec(t, p, id).Phase; got != lease.PhaseClosed {
				t.Errorf("phase = %v, want closed", got)
			}
			if n := f0Tombstones(t, p, now); n != 0 {
				t.Errorf("tombstones = %d, want 0", n)
			}
			gotID, gotAddr, gotIdentity := p.ipamRebindCandidate(ipamTestNetwork, next)
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

// TestIPAMReleaseAddress_GivesUpACreatedRecordWithNoEndpoint is the
// second arm.
//
// The engine rolls a failed container start back with ReleaseAddress,
// and before this change that call walked away from every phase but
// RESERVED. A re-bound record is CREATED from the moment the re-bind is
// written, so the one call that always arrives saw the one phase it did
// not act on.
func TestIPAMReleaseAddress_GivesUpACreatedRecordWithNoEndpoint(t *testing.T) {
	first, restarted, next := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03)
	p, b, sender, _ := f0Fixture(t)
	id := f0Tombstone(t, p, first, f0Addr)
	f0Rebind(t, p, restarted, "192.168.99.10")
	// CreateEndpoint took the reservation and then failed, so nothing
	// is left in the map for this handler to find.
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
	againID, againAddr, _ := p.ipamRebindCandidate(ipamTestNetwork, next)
	if againID != id || againAddr != "192.168.99.10" {
		t.Errorf("the retry re-bound (%q, %q), want (%q, 192.168.99.10)", againID, againAddr, id)
	}
	if n := sender.callCount(); n != 0 {
		t.Errorf("%d releases on the wire while the address was being handed to the retry", n)
	}
}

// TestIPAMReleaseAddress_FreesTheHardwareAddressForTheRetry is the
// other half of the same handler: the reservation has to go with the
// record.
//
// An address request whose hardware address already has a reservation
// in flight does not run its own exchange; it waits on the one that is
// there and takes its answer. That is the one-exchange rule, and it is
// right while the reservation is live. Left behind after the record it
// points at has been given up, it hands the next attempt the answer of
// an attempt that was abandoned, and the record it names no longer
// belongs to anyone.
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

// TestIPAMReleaseAddress_LeavesAnExchangeInFlightAlone is the bound on
// the arm above.
//
// A second address request under the same hardware address re-binds the
// record before it sends anything, so between that fold and the answer
// the record is created and the exchange that owns it is still running.
// A release for the old address arriving in that window must not give
// the record away under the exchange's feet: the answer would name a
// record already handed to a retry.
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

// TestIPAMReleaseAddress_ClosesARecordWhoseLeaseHasGone is the other
// direction of the same handler.
//
// The record lookup filters on the phase alone, so a record whose lease
// expired while the container was down is still the one that answers
// for the address. Handing that one back as a tombstone would offer the
// retry an address the server is free to have given to someone else,
// and would make a real candidate on the same network ambiguous.
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

// TestIPAMReleaseAddress_LeavesARunningEndpointAlone is the preservation
// control for the arm above, and the case where being wrong costs a
// running container its address.
//
// A joined record belongs to a container that is up. The release
// handler must not touch it, and on a release_lease=on_remove network
// the cost of touching it is not a phase in a file: it is a DHCPRELEASE
// for an address in use, about a minute later.
func TestIPAMReleaseAddress_LeavesARunningEndpointAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// hold puts the record in the shape a running container
		// leaves it in, and returns the phase it must still be in
		// after the handler has run.
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
			// CREATED AND UP. recordBound runs on a goroutine
			// neither Join nor recovery waits for, so a container
			// that is up can still be in the phase this change
			// taught the handler to act on. The endpoint holding
			// it is the difference, and the handler has to read
			// it: the record cannot say it.
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
			if x, _, _ := p.ipamRebindCandidate(ipamTestNetwork, other); x != "" {
				t.Errorf("a second container re-bound the running container's record %q", x)
			}
			if n := p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
				t.Errorf("a release went on the wire for a running container: swept=%d sent=%d", n, sender.callCount())
			}
		})
	}
}

// TestIPAMReleaseAddress_OnlyTheWholeKeyBlocksTheGiveUp is the bound on
// the guard above.
//
// The guard keys on a PAIR, the endpoint's hardware address and its
// address, and each half has its own way of being the wrong answer on
// its own. Two IPAM networks on one segment are handed addresses by the
// same server, so the same address can be live on one while the record
// on the other is the one nobody holds. And one hardware address can
// hold a different address: a fixed `--mac-address` container attached
// to a second network, or the same container after a re-bind moved its
// address. Either half of the key on its own leaves stranded exactly
// the record this handler exists to hand back.
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
			if got, _, _ := p.ipamRebindCandidate(ipamTestNetwork, f0MAC(0x02)); got == "" {
				t.Error("the retry was offered nothing, so the address is stranded")
			}
			// A record that never reached the bound phase held a
			// lease nothing ever used, so the window ends by
			// expiring and not on the wire. The address comes back
			// through the tombstone above; a datagram here would be
			// a release for an address this plugin never put on an
			// interface.
			if n := p.sweepDeferredReleases(now.Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
				t.Errorf("a release went out for a lease that was never bound: swept=%d sent=%d", n, sender.callCount())
			}
		})
	}
}

// TestIPAMReleaseAddress_AFailedTeardownDoesNotBlockTheGiveUp is the
// other bound on the same guard, and the one the guard itself created.
//
// DeleteEndpoint returns before it takes the fingerprint when the
// network's options cannot be read, and the engine releases the address
// whether or not that call succeeded. A fingerprint left behind there
// would answer "an endpoint still holds this" for an endpoint the
// engine has already torn down, and the record would sit in the created
// phase until the next plugin start. The teardown gives the fingerprint
// up on that exit, so the release that follows still reaches the
// record.
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
	if got, _, _ := p.ipamRebindCandidate(ipamTestNetwork, next); got == "" {
		t.Error("the retry was offered nothing, so the address is stranded until the next plugin start")
	}
	if n := p.sweepDeferredReleases(now.Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
		t.Errorf("a release went out for a lease that was never bound: swept=%d sent=%d", n, sender.callCount())
	}
}

// TestIPAMListedMACs_OneUnreadableEntryPoisonsTheAnswer is the guard on
// the rule's input.
//
// The rule below acts on ABSENCE, so one hardware address that does not
// make it into the set makes a record look unowned, and its container
// is running. A set short by one cannot be told from a network with one
// fewer endpoint, so the only safe answer is to write nothing this
// time round.
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

// TestIPAMStrandedRecords_TheRuleKeysOnTheWriterAndTheEngineList is the
// third arm: the same stranded state reached by the plugin ending
// between the two calls, where no handler of ours ever runs again.
//
// BOTH KEYS ARE NECESSARY and the test drives all four combinations.
// The writer alone is not enough, because a running container's record
// can sit in CREATED across a restart: the phase only moves at the bind
// inside setupClient, which runs in a goroutine neither Join nor
// recovery waits for. The engine's list alone is not enough either,
// because a container starting in THIS process is not listed until its
// CreateEndpoint has returned.
func TestIPAMStrandedRecords_TheRuleKeysOnTheWriterAndTheEngineList(t *testing.T) {
	running, restarted, inflightMAC, removed := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03), f0MAC(0x04)
	p, _, sender, journal := f0Fixture(t)

	// The previous process: one running container, and one restart
	// caught between RequestAddress and CreateEndpoint.
	runningID := f0Created(t, p, running, f0Addr)
	f0Tombstone(t, p, removed, f0Addr2)
	strandedID := f0Rebind(t, p, restarted, "192.168.99.11")

	f0Reopen(t, p, journal, "proc-2")
	now := time.Now()
	// A start in flight in the new process: written by this process,
	// not yet listed by the engine.
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

	// The retry gets the stranded address back, and only that one.
	againID, againAddr, _ := p.ipamRebindCandidate(ipamTestNetwork, f0MAC(0x05))
	if againID != strandedID || againAddr != "192.168.99.11" {
		t.Errorf("the retry re-bound (%q, %q), want (%q, 192.168.99.11)", againID, againAddr, strandedID)
	}
	if n := p.sweepDeferredReleases(time.Now().Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
		t.Errorf("a release went on the wire: swept=%d sent=%d. The stranded address was claimed "+
			"back inside the window and the other two belong to containers that are up.",
			n, sender.callCount())
	}
}

// TestIPAMStrandedRecords_TheRunningEndpointStillHeals is the rest of
// the preservation control: recovery's own bind, which runs beside the
// rule, still takes the record the rule left alone.
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

// TestIPAMStrandedRecords_ARecordHoldingNothingIsClosed is the same
// direction as the CreateEndpoint case: a record with no address must
// not become a re-bind candidate.
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

// TestIPAMStrandedRecords_RunTwiceWritesOnce pins that the rule is not
// a clock: the second pass of a plugin that recovers twice (#383 runs
// recovery again once the socket is listening) must find nothing left
// to do, or every pass would reset a window a container is counting on.
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

// TestIPAMStrandedRecords_AnotherNetworkIsNotTouched pins the scope: the
// rule reads one network's inspect answer, so it may only write to that
// network's records. A second IPAM network on the same host has its own
// endpoints and its own list.
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

// TestRecoverEndpoints_RunsTheStrandedRuleOnIPAMNetworksOnly is the
// call site: the rule reads the answer recovery already has, so a
// network whose inspect failed is skipped before it and a network this
// plugin does not allocate for never reaches it.
func TestRecoverEndpoints_RunsTheStrandedRuleOnIPAMNetworksOnly(t *testing.T) {
	restarted, removed := f0MAC(0x02), f0MAC(0x04)

	for _, tc := range []struct {
		name string
		// arrange returns the docker fake and whether the network is
		// IPAM-mode on disk.
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

// TestIPAMStranded_DocumentedLimits pins the two shapes the rule cannot
// see, so that a change which silently alters either of them is a
// failing test and not a discovery in production.
func TestIPAMStranded_DocumentedLimits(t *testing.T) {
	// LIMIT 1: an engine that answers 200 with a SHORT list. A
	// network's endpoint enumeration logs a store read error and
	// returns what it has, and an endpoint whose own read fails is
	// dropped from the answer; both look exactly like a network with
	// fewer endpoints. There is no second source to check against: an
	// IPAM handler may not ask Docker anything, and recovery has the
	// one answer. A running container's record is given up.
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

	// LIMIT 2: a listed placeholder that is rolled back afterwards.
	// CreateEndpoint succeeded in the previous process, so the engine
	// has stored the endpoint and lists it as ep-<id> while it retries
	// Join. The rule leaves it, which is the safe direction. The
	// engine then rolls the endpoint back, and what closes the record
	// is the release handler, not the rule.
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
		// The rollback: DeleteEndpoint finds no fingerprint in this
		// process, and ReleaseAddress is what is left.
		if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
			t.Fatalf("ReleaseAddress: %v", err)
		}
		if got := f0Rec(t, p, id).Phase; got != lease.PhaseRetained {
			t.Errorf("phase = %v, want retained: the rule ran while the endpoint was listed, so "+
				"the rollback's release is the only call left that can hand the address back", got)
		}
	})
}

// TestIPAMReserve_EveryExitAfterTheReBindHandsTheWindowBack reads the
// reserve's source, because the exits below it need a DHCP server.
//
// Two properties, and each of them has been wrong in this function.
// The first is that no error leaves without giving the record up: an
// exit added without that line takes the network's one tombstone and
// keeps it, and the container that was restarting gets a fresh address
// with nothing counting it. The second is WHICH give-up: the one for a
// reservation the plugin accepted retains a record that still holds a
// lease, and at these exits the lease is either absent or one the
// acceptance rules refused, so using it here lays a tombstone carrying
// an address the next container will be refused for as well.
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

	// The refused ACK is the exit that must NOT keep the window: the
	// fold has already put the refused address on the record.
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
