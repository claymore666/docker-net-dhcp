// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// #800. A departing endpoint's address can be reserved for the restart
// that is coming, or handed back to the DHCP server. It must not be
// both, and before this it was: DeleteEndpoint laid a tombstone saying
// "the next CreateEndpoint re-requests exactly this MAC and these
// addresses" while a Join-abort path spawned a reclaim handing the same
// lease back. In the CI failure that caught it, the tombstone gave
// fd00:6470:6863::3d to the restarting child while the reclaim's
// release link solicited fd00:6470:6863::3d to give it away.
//
// EVERY TEST HERE DRIVES THE PRODUCT'S OWN CALL SITES — p.DeleteEndpoint
// and p.spawnOrphanRelease — and never the arbitration function on its
// own. A withdrawn earlier reproducer for this issue transcribed the
// ordering into the test body and so passed against a tree where the
// defect was fixed; the rule that fell out of it is that changing the
// product has to move these tests.
//
// The two observables are both the product's:
//
//   - a tombstone was written: consumeTombstone finds it.
//   - the reclaim RAN: orphaned_lease_release_failures moved. These
//     managers have neither a parent NIC nor a bridge, so the reclaim
//     reaches "no attachment path" and counts one failure. It is the
//     same observable TestReleaseOrphanedLease_NoAttachmentPathCountsFailure
//     uses, and it distinguishes "stood down" (0) from "ran" (1)
//     without a DHCP server or root.

// dispositionManager is orphanManager with a caller-chosen endpoint ID,
// so a reclaim and a DeleteEndpoint can be aimed at the same endpoint.
//
// No attachment path on purpose: the reclaim must be able to run to a
// countable conclusion in a unit test.
func dispositionManager(t *testing.T, endpointID string) *dhcpManager {
	t.Helper()
	m := orphanManager(t, DHCPNetworkOptions{}, "192.168.99.95/24")
	m.joinReq.EndpointID = endpointID
	return m
}

// tombstoneFor reports whether DeleteEndpoint left a tombstone for this
// container. Consuming is the only read the store offers, and these
// tests ask once.
func tombstoneFor(p *Plugin, networkID, hostname string) bool {
	_, _, _, ok := p.consumeTombstone(networkID, dhcpHostname{name: hostname})
	return ok
}

// Reserved first: the reclaim stands down.
//
// This is the ordering the CI failure was in. DeleteEndpoint gets there
// first, writes the tombstone, and the reclaim spawned by the abort
// path must then leave the lease alone — it is the address the
// restarting container is about to ask for.
func TestOrphanRelease_StandsDownWhenATombstoneReservesTheLease(t *testing.T) {
	const netID, epID, hostname = "net-800-reserved", "aaaa000000000000", "app-800"

	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{Bridge: "br-test"})
	p.rememberEndpoint(epID, endpointFingerprint{
		MAC:  "02:42:ac:11:00:01",
		IPv4: "192.168.99.95",
		IPv6: "fd00:6470:6863::3d",
	}, dhcpHostname{name: hostname})

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: netID, EndpointID: epID,
	}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}

	p.spawnOrphanRelease(dispositionManager(t, epID))
	p.orphanReleases.Wait()

	if got := p.orphanedLeaseReleaseFailures.Load(); got != 0 {
		t.Errorf("the reclaim ran (orphaned_lease_release_failures = %d, want 0): "+
			"it handed back an address the tombstone had just promised to the restart", got)
	}
	if got := p.orphanReleasesSuppressed.Load(); got != 1 {
		t.Errorf("orphan_releases_suppressed = %d, want 1", got)
	}
	if !tombstoneFor(p, netID, hostname) {
		t.Error("the tombstone must survive: reserving the lease is the outcome that keeps the address")
	}
}

// The control for the test above, varying EXACTLY ONE thing: no
// DeleteEndpoint, so nothing reserved the lease.
//
// Without it, the assertion "the reclaim did not run" is also satisfied
// by a reclaim that cannot run at all in this fixture — a check with one
// possible verdict. This says the fixture reaches the wire-attempt and
// counts it whenever nothing stands in the way.
func TestOrphanRelease_RunsWhenNothingReservesTheLease(t *testing.T) {
	const netID, epID = "net-800-unreserved", "aaaa000000000000"

	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{Bridge: "br-test"})

	p.spawnOrphanRelease(dispositionManager(t, epID))
	p.orphanReleases.Wait()

	if got := p.orphanedLeaseReleaseFailures.Load(); got != 1 {
		t.Errorf("orphaned_lease_release_failures = %d, want 1: the control must reach "+
			"the reclaim, or the reserved case proves nothing", got)
	}
	if got := p.orphanReleasesSuppressed.Load(); got != 0 {
		t.Errorf("orphan_releases_suppressed = %d, want 0", got)
	}
}

// Released first: no tombstone.
//
// The reverse ordering, and the half a wider lock or a longer budget
// could never have reached. The reclaim has handed the lease back to
// the server, which is free to give it to somebody else; a tombstone
// written afterwards is a promise about an address that is no longer
// ours. The container comes back on a new MAC and address instead, and
// the counter says so.
func TestDeleteEndpoint_WritesNoTombstoneOnceTheLeaseWasHandedBack(t *testing.T) {
	const netID, epID, hostname = "net-800-released", "bbbb000000000000", "app-801"

	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{Bridge: "br-test"})
	p.rememberEndpoint(epID, endpointFingerprint{
		MAC:  "02:42:ac:11:00:02",
		IPv4: "192.168.99.95",
	}, dhcpHostname{name: hostname})

	p.spawnOrphanRelease(dispositionManager(t, epID))
	p.orphanReleases.Wait()
	if got := p.orphanedLeaseReleaseFailures.Load(); got != 1 {
		t.Fatalf("precondition: the reclaim must have run, got %d attempts", got)
	}

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: netID, EndpointID: epID,
	}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}

	if tombstoneFor(p, netID, hostname) {
		t.Error("a tombstone was written for a lease the reclaim had already handed back: " +
			"the next CreateEndpoint would re-request an address the server has freed")
	}
	if got := p.tombstonesSuppressed.Load(); got != 1 {
		t.Errorf("tombstones_suppressed = %d, want 1", got)
	}
}

// The control for the test above, varying EXACTLY ONE thing: no
// reclaim, so nothing handed the lease back.
//
// TestDeleteEndpoint_WritesTombstoneForBridgeMode already asserts this
// shape, and it is repeated here deliberately: the pair has to sit
// beside each other for "the reclaim is what removed the tombstone" to
// be readable as a difference rather than inferred from another file.
func TestDeleteEndpoint_WritesTheTombstoneWhenNoLeaseWasHandedBack(t *testing.T) {
	const netID, epID, hostname = "net-800-kept", "bbbb000000000000", "app-801"

	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{Bridge: "br-test"})
	p.rememberEndpoint(epID, endpointFingerprint{
		MAC:  "02:42:ac:11:00:02",
		IPv4: "192.168.99.95",
	}, dhcpHostname{name: hostname})

	if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
		NetworkID: netID, EndpointID: epID,
	}); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}

	if !tombstoneFor(p, netID, hostname) {
		t.Error("no tombstone, and nothing handed the lease back: the control must reach " +
			"the write, or the released case proves nothing")
	}
	if got := p.tombstonesSuppressed.Load(); got != 0 {
		t.Errorf("tombstones_suppressed = %d, want 0", got)
	}
}

// The invariant itself, driven concurrently: for one endpoint, exactly
// one of the two outcomes happens. Never both, never neither.
//
// The two ordered tests above each pin one interleaving. This one does
// not care which side wins — it asserts what #800 actually violated, and
// it is what a check-then-act rewrite of the arbitration would fail
// while both ordered tests still passed.
//
// Run it under -race: the two paths touch p.mu, the tombstone file and
// the counters from different goroutines.
//
// A fresh endpoint ID and hostname per iteration, because the tombstone
// matcher requires EXACTLY ONE match on (network, hostname) and reusing
// either would make later iterations fail for a reason that has nothing
// to do with this.
func TestEndpointDisposition_ExactlyOneOutcomePerEndpoint(t *testing.T) {
	const netID = "net-800-race"
	const rounds = 32

	p := deleteEndpointPlugin(t, netID, DHCPNetworkOptions{Bridge: "br-test"})

	type outcome struct {
		reserved bool
		released bool
	}
	outcomes := make([]outcome, rounds)

	for i := 0; i < rounds; i++ {
		epID := fmt.Sprintf("%016x", 0xc000+i)
		hostname := fmt.Sprintf("app-%d", i)

		p.rememberEndpoint(epID, endpointFingerprint{
			MAC:  fmt.Sprintf("02:42:ac:11:%02x:%02x", i/256, i%256),
			IPv4: "192.168.99.95",
		}, dhcpHostname{name: hostname})

		before := p.orphanedLeaseReleaseFailures.Load()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
				NetworkID: netID, EndpointID: epID,
			}); err != nil {
				t.Errorf("DeleteEndpoint: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			p.spawnOrphanRelease(dispositionManager(t, epID))
		}()
		wg.Wait()
		p.orphanReleases.Wait()

		outcomes[i] = outcome{
			reserved: tombstoneFor(p, netID, hostname),
			released: p.orphanedLeaseReleaseFailures.Load() > before,
		}
	}

	both, neither := 0, 0
	for i, o := range outcomes {
		switch {
		case o.reserved && o.released:
			both++
			t.Errorf("round %d: the lease was reserved for the restart AND handed back to the "+
				"server — this is #800", i)
		case !o.reserved && !o.released:
			neither++
			t.Errorf("round %d: neither reserved nor released — the lease is stranded until it "+
				"expires and no tombstone carries the address", i)
		}
	}

	// WHAT THIS TEST DOES NOT COVER, stated because the fixture decides
	// it and not the property.
	//
	// The winner is not controlled here, and in practice it is not even
	// close: spawnOrphanRelease claims in its caller's goroutine, while
	// DeleteEndpoint loads the network options and looks the link up
	// before reaching its claim. Measured at 0/32 rounds reserved. So
	// this test exercises the RELEASED arm and asserts the invariant
	// across it; it would pass unchanged against an arbitration
	// hard-wired to always release.
	//
	// The reserved arm is carried by
	// TestOrphanRelease_StandsDownWhenATombstoneReservesTheLease, which
	// pins that ordering deterministically. Neither test covers the
	// verdict space alone. Biasing the race with a sleep would make the
	// distribution look better and prove nothing more, which is why
	// there isn't one.
	reserved := 0
	for _, o := range outcomes {
		if o.reserved {
			reserved++
		}
	}
	t.Logf("%d/%d rounds reserved, %d released, %d both, %d neither",
		reserved, rounds, rounds-reserved-both-neither, both, neither)

	if reserved+both+neither == rounds {
		t.Errorf("no round reached the reclaim (%d reserved, %d both, %d neither of %d): "+
			"the invariant held over an empty domain", reserved, both, neither, rounds)
	}
}

// The claim is idempotent for a repeated caller and refusing for the
// other one. spawnOrphanRelease's own sync.Once already covers the
// repeat on that side, so this states the property the arbitration
// promises rather than leaving it to be inferred from the one caller
// that happens not to need it.
func TestClaimEndpointDisposition_FirstWinsAndRepeatsAreIdempotent(t *testing.T) {
	p := newTestPlugin(t)

	if !p.claimEndpointDisposition("ep-1", dispositionReserved) {
		t.Fatal("first claim must win")
	}
	if !p.claimEndpointDisposition("ep-1", dispositionReserved) {
		t.Error("the same disposition claimed twice must still read as granted")
	}
	if p.claimEndpointDisposition("ep-1", dispositionReleased) {
		t.Error("the other disposition must be refused once one is decided")
	}
	if got, ok := p.endpointDispositionOf("ep-1"); !ok || got != dispositionReserved {
		t.Errorf("disposition = (%v, %v), want (reserved, true)", got, ok)
	}

	// A different endpoint is a different decision.
	if !p.claimEndpointDisposition("ep-2", dispositionReleased) {
		t.Error("an unrelated endpoint must not inherit ep-1's decision")
	}

	// A nil plugin and an empty endpoint ID both degrade to "granted",
	// so the unit-test managers built without a plugin keep the
	// pre-#800 behaviour rather than silently losing their reclaim.
	var nilPlugin *Plugin
	if !nilPlugin.claimEndpointDisposition("ep-3", dispositionReleased) {
		t.Error("a nil plugin must not suppress the reclaim")
	}
	if !p.claimEndpointDisposition("", dispositionReleased) {
		t.Error("an empty endpoint ID must not suppress the reclaim")
	}
}
