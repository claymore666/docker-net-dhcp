// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// TestIpamReserve_ASecondRequestJoinsTheFirst is defeat row 14 and the
// reason ipamReserves exists at all.
//
// A plugin enabled with a --timeout below the reserve's budget has its
// RequestAddress re-sent with the SAME body while the first exchange is
// still running. Two exchanges for one endpoint means two DISCOVERs, two
// leases on the server and one container, and the second lease is never
// released: nothing holds it and no DHCPRELEASE goes on the wire.
//
// Driven through the real entry point, with the exchange occupied by
// hand: the netlink and DHCP half needs a parent NIC and a server, and
// the concurrency half is what this is about.
func TestIpamReserve_ASecondRequestJoinsTheFirst(t *testing.T) {
	p, b := ipamFixture(t)
	mac, _ := net.ParseMAC(ipamTestMAC)
	key := ipamReserveKey(b.PoolID, mac)

	first, mine := p.ipamReserves.begin(key, time.Now())
	if !mine {
		t.Fatal("the first caller does not own the exchange")
	}

	sn, err := ipamNetwork(ipamTestNetwork)
	if err != nil {
		t.Fatalf("ipamNetwork: %v", err)
	}

	joined := make(chan *ipamReservation, 1)
	go func() {
		res, err := p.ipamReserveAddress(context.Background(), ipamTestNetwork, sn, mac, "")
		if err != nil {
			joined <- nil
			return
		}
		joined <- res
	}()

	// Let the joiner reach the wait before the result lands, so the test
	// drives the joining path rather than the already-finished one.
	deadline := time.Now().Add(2 * time.Second)
	for p.ipamReserveJoined.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := p.ipamReserveJoined.Load(); n != 1 {
		t.Fatalf("ipam_reserve_joined = %d, want 1 — the second request started its own "+
			"exchange, which is a second lease for one container", n)
	}

	p.ipamReserves.finish(key, first, ipamReservation{
		addr:   netip.MustParsePrefix("192.168.99.10/24"),
		info:   dhcp.Info{IP: "192.168.99.10/24", Gateway: "192.168.99.1"},
		record: "rec-1",
	}, nil)

	select {
	case got := <-joined:
		if got == nil {
			t.Fatal("the joining request failed; it must receive the first one's result")
		}
		if got.addr.String() != "192.168.99.10/24" {
			t.Errorf("the joiner got %v, the exchange won 192.168.99.10/24", got.addr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the joining request never woke; a re-sent body would hang the daemon's retry")
	}
}

// TestIpamReserves_AFailedReservationIsNotRemembered. The next attempt
// has to run a fresh exchange rather than be handed this one's error
// forever.
func TestIpamReserves_AFailedReservationIsNotRemembered(t *testing.T) {
	s := newIPAMReserves()
	res, _ := s.begin("k", time.Now())
	s.finish("k", res, ipamReservation{}, context.DeadlineExceeded)
	if n := s.len(); n != 0 {
		t.Errorf("%d failed reservation(s) kept; the next attempt would be answered with a "+
			"stale error instead of running an exchange", n)
	}
}

// TestIpamReserves_TakeOnlyCompletes. CreateEndpoint must not consume a
// reservation whose exchange is still running: it would read a zero
// address and bind the endpoint to nothing.
func TestIpamReserves_TakeOnlyCompletes(t *testing.T) {
	s := newIPAMReserves()
	res, _ := s.begin("k", time.Now())
	if _, ok := s.take("k"); ok {
		t.Fatal("an in-flight reservation was taken")
	}
	s.finish("k", res, ipamReservation{addr: netip.MustParsePrefix("192.168.99.10/24"), record: "r"}, nil)
	if _, ok := s.take("k"); !ok {
		t.Error("a completed reservation was not taken")
	}
	if _, ok := s.take("k"); ok {
		t.Error("a reservation was taken twice; two endpoints would share one address")
	}
}

// TestSweepIPAMReservations_RetainsWhatDockerNeverBuilt.
//
// Only a RETAINED record carries a deadline, so a RESERVED one with a
// live lease and no endpoint -- the daemon spent every retry, or fell
// over between RequestAddress and CreateEndpoint -- would keep answering
// address lookups until the network is deleted.
func TestSweepIPAMReservations_RetainsWhatDockerNeverBuilt(t *testing.T) {
	p, b := ipamFixture(t)
	mac, _ := net.ParseMAC(ipamTestMAC)
	id := p.recordReserved(ipamTestNetwork, mac, dhcp.ClientIdentity([]byte{7}))
	if err := p.records.Observed(id, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}

	key := ipamReserveKey(b.PoolID, mac)
	started := time.Now()
	res, _ := p.ipamReserves.begin(key, started)
	p.ipamReserves.finish(key, res, ipamReservation{
		addr: netip.MustParsePrefix("192.168.99.10/24"), record: id,
	}, nil)

	// Nothing is swept while the endpoint could still arrive. The
	// control matters: a sweeper that retained immediately would take
	// the address away from the CreateEndpoint on its way in.
	if n := p.sweepIPAMReservations(started.Add(time.Second)); n != 0 {
		t.Fatalf("%d reservation(s) swept a second after they were made; CreateEndpoint had "+
			"not run yet", n)
	}
	if got := ipamPhaseOf(t, p, id); got != lease.PhaseReserved {
		t.Fatalf("the record is in phase %v before the sweep, want %v", got, lease.PhaseReserved)
	}

	if n := p.sweepIPAMReservations(started.Add(tombstoneTTL + time.Second)); n != 1 {
		t.Fatalf("%d reservation(s) swept after the tombstone TTL, want 1", n)
	}
	if got := ipamPhaseOf(t, p, id); got != lease.PhaseRetained {
		t.Errorf("the swept record is in phase %v, want %v — a reservation nothing retains "+
			"answers address lookups for the life of the network", got, lease.PhaseRetained)
	}
	if n := p.ipamReserves.len(); n != 0 {
		t.Errorf("%d reservation(s) left in memory after the sweep", n)
	}
}

// TestRetainOrphanedReservations_AtStartUpEveryReservationIsOrphaned.
//
// The age does not need measuring here, and that is what makes this arm
// different from the sweeper. A RESERVED record is one an address was
// answered for and no CreateEndpoint ever bound a link to; the process
// that could still have bound it is gone, and the fold admits no Create
// from another.
func TestRetainOrphanedReservations_AtStartUpEveryReservationIsOrphaned(t *testing.T) {
	p, _ := ipamFixture(t)
	mac, _ := net.ParseMAC(ipamTestMAC)
	ident := dhcp.ClientIdentity([]byte{7})

	reserved := p.recordReserved(ipamTestNetwork, mac, ident)
	if err := p.records.Observed(reserved, acquired("192.168.99.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	// A live endpoint beside it, which must be left alone: retaining a
	// JOINED record would have restart recovery decline to resume a
	// container that is running.
	live := p.recordCreated(ipamTestNetwork, mac, ident)
	if err := p.records.Observed(live, acquired("192.168.99.11/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Bound(live); err != nil {
		t.Fatalf("Bound: %v", err)
	}

	if n := retainOrphanedReservations(p.records, time.Now()); n != 1 {
		t.Fatalf("retained %d record(s), want 1", n)
	}
	if got := ipamPhaseOf(t, p, reserved); got != lease.PhaseRetained {
		t.Errorf("the orphaned reservation is in phase %v, want %v", got, lease.PhaseRetained)
	}
	if got := ipamPhaseOf(t, p, live); got != lease.PhaseJoined {
		t.Errorf("a live endpoint's record moved to %v at start-up", got)
	}
}

// TestIpamRebindCandidate_AmbiguityIsCountedNotGuessed.
//
// A RequestAddress carries no hostname and no endpoint id, so when
// several containers on one network restart together there is nothing to
// match a request to a previous lease on. Picking one would hand a
// container's address to a different container.
func TestIpamRebindCandidate_AmbiguityIsCountedNotGuessed(t *testing.T) {
	mac, _ := net.ParseMAC(ipamTestMAC)
	ident := dhcp.ClientIdentity([]byte{7})

	tombstone := func(t *testing.T, p *Plugin, addr string) string {
		t.Helper()
		id := p.recordCreated(ipamTestNetwork, mac, ident)
		if err := p.records.Observed(id, acquired(addr, time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
		if err := p.records.Retained(id, time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("Retained: %v", err)
		}
		return id
	}

	t.Run("exactly one candidate is re-bound", func(t *testing.T) {
		p, _ := ipamFixture(t)
		id := tombstone(t, p, "192.168.99.10/24")
		newMAC, _ := net.ParseMAC("02:42:c0:a8:63:0b")
		gotID, gotAddr := p.ipamRebindCandidate(ipamTestNetwork, newMAC)
		if gotID != id {
			t.Errorf("re-bound record %q, want %q", gotID, id)
		}
		if gotAddr != "192.168.99.10" {
			t.Errorf("asking for %q, the tombstone holds 192.168.99.10", gotAddr)
		}
		if n := p.ipamRebindAmbiguous.Load(); n != 0 {
			t.Errorf("ipam_rebind_ambiguous = %d with one candidate", n)
		}
	})

	t.Run("two candidates are counted and neither is taken", func(t *testing.T) {
		p, _ := ipamFixture(t)
		tombstone(t, p, "192.168.99.10/24")
		tombstone(t, p, "192.168.99.11/24")
		newMAC, _ := net.ParseMAC("02:42:c0:a8:63:0b")
		gotID, gotAddr := p.ipamRebindCandidate(ipamTestNetwork, newMAC)
		if gotID != "" || gotAddr != "" {
			t.Errorf("chose (%q, %q) between two candidates; there is nothing to choose on, "+
				"so one container would take another's address", gotID, gotAddr)
		}
		if n := p.ipamRebindAmbiguous.Load(); n != 1 {
			t.Errorf("ipam_rebind_ambiguous = %d, want 1 — the documented limit is only a "+
				"limit if it is visible", n)
		}
	})
}

// TestSaveNetwork_StampsTheIPAMSchemaVersion is the other half of
// TestSaveOptions_StampsSchemaVersion: a file carrying a pool binding
// says so in its version, so a 2.0 build refuses it rather than reading
// the options out of it and serving the network on the null path.
func TestSaveNetwork_StampsTheIPAMSchemaVersion(t *testing.T) {
	p, b := ipamFixture(t)
	_ = p
	sn, err := loadNetwork(ipamTestNetwork)
	if err != nil {
		t.Fatalf("loadNetwork: %v", err)
	}
	if !sn.IPAMMode() {
		t.Fatal("the network loaded without its binding")
	}
	if sn.Binding.PoolID != b.PoolID {
		t.Errorf("binding PoolID = %q, want %q", sn.Binding.PoolID, b.PoolID)
	}
	path, err := stateFilePath(ipamTestNetwork)
	if err != nil {
		t.Fatalf("stateFilePath: %v", err)
	}
	v := schemaVersionOfFile(t, path)
	if v != stateSchemaVersion {
		t.Errorf(`"v" = %d, want %d — a 2.0 build must refuse this file rather than read `+
			`the options out of it and serve the network as null-IPAM`, v, stateSchemaVersion)
	}
	if stateSchemaVersion == stateSchemaVersionBase {
		t.Error("the IPAM schema version equals the base one, so nothing distinguishes a " +
			"file an older build may read from one it must refuse")
	}
}

func schemaVersionOfFile(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var vo versionedOptions
	if err := json.Unmarshal(raw, &vo); err != nil {
		t.Fatalf("%s is not a versioned options file: %v", path, err)
	}
	return vo.V
}
