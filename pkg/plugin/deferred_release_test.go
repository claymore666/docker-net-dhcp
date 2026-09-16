// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// deferredTestNetwork is the network every record below is filed under.
const deferredTestNetwork = "deferrednet1"

// acquired6 is `acquired` in the DHCPv6 family: a v6 record refuses a
// v4 lease, which is the fold doing its job and not this helper being
// careful.
func acquired6(addr string, until time.Duration) lease.Event {
	return lease.Event{
		Kind: lease.Acquired,
		Lease: lease.Lease{
			Addr:   netip.MustParsePrefix(addr),
			Expire: time.Now().Add(until),
		},
	}
}

// deferredPlugin is a plugin with a record store, a parent to send
// from, a stubbed wire and one network whose options are on disk.
func deferredPlugin(t *testing.T, value string) (*Plugin, *fakeSender) {
	t.Helper()
	withStateDir(t, t.TempDir())
	hostParent(t, "192.168.99.2/24", "fe80::2/64")
	sender := installSender(t, nil)
	p := withRecords(t, &Plugin{})
	if err := saveOptions(deferredTestNetwork, DHCPNetworkOptions{
		Bridge:       "br0",
		ReleaseLease: value,
		IPv6:         true,
	}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	return p, sender
}

// heldRecord is one endpoint's record after its container stopped: a
// lease on it and the tombstone deadline DeleteEndpoint stamps.
func heldRecord(t *testing.T, p *Plugin, mac net.HardwareAddr, addr string, deadline time.Time) string {
	t.Helper()
	id := p.recordCreated(deferredTestNetwork, mac, dhcp.ClientIdentity(mac))
	if id == "" {
		t.Fatal("no record was created")
	}
	if addr != "" {
		if err := p.records.Observed(id, acquired(addr, time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
	}
	if !deadline.IsZero() {
		if err := p.records.Retained(id, deadline); err != nil {
			t.Fatalf("Retained: %v", err)
		}
	}
	return id
}

// liveRecord is a record of an endpoint that is running: created, a
// lease on it, joined. It is what a restart inside the window leaves
// behind beside the held record.
func liveRecord(t *testing.T, p *Plugin, mac net.HardwareAddr, addr string) string {
	t.Helper()
	id := p.recordCreated(deferredTestNetwork, mac, dhcp.ClientIdentity(mac))
	if id == "" {
		t.Fatal("no record was created")
	}
	if addr != "" {
		if err := p.records.Observed(id, acquired(addr, time.Hour), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
	}
	if err := p.records.Bound(id); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	return id
}

func recordPhase(t *testing.T, p *Plugin, id string) lease.Phase {
	t.Helper()
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	rec, ok := rb.ByID(id)
	if !ok {
		t.Fatalf("record %s is gone from the store", id)
	}
	return rec.Phase
}

// deferredMAC returns a MAC that differs from every other in a test.
func deferredMAC(n byte) net.HardwareAddr {
	return net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, n}
}

// TestDeferredRelease_TheClaimCheckIsKeyedOnTheAddress is the whole of
// what decides whether a held address goes back, driven through the
// sweep so the assertion is the datagram and not the predicate.
//
// THE ROW THAT MATTERS MOST IS "a newer record under the same MAC holds
// a different address". A check keyed on the MAC reads "this identity
// is in use again" and calls the old address claimed; the container
// that came back was given something else, so that address would sit
// leased with nothing left to hand it back and no counter saying so.
// The question the option asks is about the ADDRESS.
//
// Closes defeat rows I1 (the MAC-keyed leak), and the design note's
// rows 2, 3, 4 and 6: a restart inside the window must not have its
// address handed back, and an endpoint id or a container identity
// cannot answer that because both are fresh at every start.
func TestDeferredRelease_TheClaimCheckIsKeyedOnTheAddress(t *testing.T) {
	const held = "192.168.99.10/24"
	for _, tc := range []struct {
		name string
		// beside builds whatever else is in the store next to the held
		// record, which is created first.
		beside   func(t *testing.T, p *Plugin, heldMAC net.HardwareAddr, deadline time.Time)
		wantSent int
		// wantReclaimed is the counter's own row, and it is NARROWER
		// than "nothing was sent": `releases_reclaimed` is the option's
		// promise kept, a container running on the address again. An
		// address closed because a newer held record will decide it,
		// or because an exchange under the same key might be about to
		// be given it, is not that and must not read as that.
		wantReclaimed int32
	}{
		{
			name:     "nothing else in the store",
			beside:   func(*testing.T, *Plugin, net.HardwareAddr, time.Time) {},
			wantSent: 1,
		},
		{
			name: "a restart inherited the MAC and the address",
			beside: func(t *testing.T, p *Plugin, mac net.HardwareAddr, _ time.Time) {
				liveRecord(t, p, mac, held)
			},
			wantSent:      0,
			wantReclaimed: 1,
		},
		{
			name: "a pinned MAC came back on a DIFFERENT address",
			beside: func(t *testing.T, p *Plugin, mac net.HardwareAddr, _ time.Time) {
				liveRecord(t, p, mac, "192.168.99.11/24")
			},
			wantSent: 1,
		},
		{
			name: "another MAC holds the same address",
			beside: func(t *testing.T, p *Plugin, _ net.HardwareAddr, _ time.Time) {
				liveRecord(t, p, deferredMAC(0x33), held)
			},
			wantSent:      0,
			wantReclaimed: 1,
		},
		{
			// Nothing is sent, and nothing is counted as reclaimed: no
			// container has this address, and the exchange in flight
			// may well be given another one. The address is left to
			// expire, which is the one-sided cost the sweep documents,
			// and an operator who read it as a reclaim would be told a
			// restart succeeded that has not happened yet.
			name: "an acquisition under the same MAC is still in flight",
			beside: func(t *testing.T, p *Plugin, mac net.HardwareAddr, _ time.Time) {
				liveRecord(t, p, mac, "")
			},
			wantSent:      0,
			wantReclaimed: 0,
		},
		{
			// One address, two stops, both due on this tick: exactly
			// one datagram, from the newer record, and the older one
			// closed because the newer one decides. Two would tell the
			// server twice and the first would go out while the newer
			// record's own window was still open. Not a reclaim: the
			// address went back, it was not taken back.
			name: "a newer record holds the address and has stopped too",
			beside: func(t *testing.T, p *Plugin, mac net.HardwareAddr, deadline time.Time) {
				heldRecord(t, p, mac, held, deadline)
			},
			wantSent:      1,
			wantReclaimed: 0,
		},
		{
			name: "only a closed record shares the address",
			beside: func(t *testing.T, p *Plugin, mac net.HardwareAddr, _ time.Time) {
				id := liveRecord(t, p, mac, held)
				p.closeRecord(id)
			},
			wantSent: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sender := deferredPlugin(t, ReleaseOnRemove)
			mac := deferredMAC(0x02)
			deadline := time.Now()
			id := heldRecord(t, p, mac, held, deadline)
			tc.beside(t, p, mac, deadline)

			p.sweepDeferredReleases(deadline.Add(releaseSettle))

			if got := sender.callCount(); got != tc.wantSent {
				t.Fatalf("%d release(s) went on the wire, want %d", got, tc.wantSent)
			}
			if got := recordPhase(t, p, id); got != lease.PhaseClosed {
				t.Errorf("the held record is %v after the sweep, want CLOSED: a record left "+
					"RETAINED is swept again on the next tick", got)
			}
			if got := p.releasesReclaimedV4.Load(); got != tc.wantReclaimed {
				t.Errorf("releases_reclaimed_v4 = %d, want %d", got, tc.wantReclaimed)
			}
			if got := p.releasesSentV4.Load(); got != int32(tc.wantSent) {
				t.Errorf("releases_sent_v4 = %d, want %d", got, tc.wantSent)
			}
		})
	}
}

// TestDeferredRelease_TheHeldAddressIsWhatGoesBack reads the record the
// sender was handed, because "a datagram left" says nothing about which
// address the server was asked to free.
func TestDeferredRelease_TheHeldAddressIsWhatGoesBack(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	deadline := time.Now()
	heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
	heldRecord(t, p, deferredMAC(0x03), "192.168.99.11/24", deadline)

	p.sweepDeferredReleases(deadline.Add(releaseSettle))

	got := map[string]bool{}
	for _, rec := range sender.recs {
		addr, ok := rec.Addr()
		if !ok {
			t.Fatalf("a release was built from a record with no address")
		}
		got[addr.String()] = true
	}
	for _, want := range []string{"192.168.99.10", "192.168.99.11"} {
		if !got[want] {
			t.Errorf("no release named %s; the sender saw %v", want, got)
		}
	}
	if len(sender.cfgs) > 0 && sender.cfgs[0].Interface != "br0" {
		t.Errorf("the release left by %q, want the network's own parent br0",
			sender.cfgs[0].Interface)
	}
}

// TestDeferredRelease_NothingLeavesBeforeTheDeadlineAndTheSettle is the
// settle's own observer (defeat row I6, design row 5).
//
// A sweep that fired at the deadline itself could land between a
// tombstone consume and the record that consume opens, and would tell
// the server an address is free that a container is at that moment
// being promised. The rows below are the three states of one clock.
func TestDeferredRelease_NothingLeavesBeforeTheDeadlineAndTheSettle(t *testing.T) {
	for _, tc := range []struct {
		name     string
		at       time.Duration
		wantSent int
	}{
		{name: "before the deadline", at: -time.Second, wantSent: 0},
		{name: "at the deadline", at: 0, wantSent: 0},
		{name: "inside the settle", at: releaseSettle - time.Millisecond, wantSent: 0},
		{name: "after the settle", at: releaseSettle, wantSent: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sender := deferredPlugin(t, ReleaseOnRemove)
			deadline := time.Now()
			id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)

			p.sweepDeferredReleases(deadline.Add(tc.at))

			if got := sender.callCount(); got != tc.wantSent {
				t.Fatalf("%d release(s) at deadline%+v, want %d", got, tc.at, tc.wantSent)
			}
			wantPhase := lease.PhaseRetained
			if tc.wantSent > 0 {
				wantPhase = lease.PhaseClosed
			}
			if got := recordPhase(t, p, id); got != wantPhase {
				t.Errorf("the record is %v, want %v: a record closed before its address went "+
					"back can never be released at all", got, wantPhase)
			}
		})
	}
}

// TestDeferredRelease_OnlyAnOnRemoveNetworkIsSwept is the guard's other
// direction, and it is the arm with the worst failure (defeat rows I4
// and I5).
//
// A sweep that ran on every network would hand addresses back on the
// two values that exist precisely not to, and it would CLOSE their
// records -- which is the quieter half of the same fault: Resume walks
// past a CLOSED record, so a container pinned to its MAC that came back
// later would ask the server for nothing and take a fresh address, and
// the only symptom would be an address that changed.
func TestDeferredRelease_OnlyAnOnRemoveNetworkIsSwept(t *testing.T) {
	for _, tc := range []struct {
		value    string
		wantSent int
	}{
		{value: ReleaseNever, wantSent: 0},
		{value: ReleaseOnStop, wantSent: 0},
		{value: "", wantSent: 0},
		{value: ReleaseOnRemove, wantSent: 1},
	} {
		name := tc.value
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			p, sender := deferredPlugin(t, tc.value)
			deadline := time.Now()
			id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)

			p.sweepDeferredReleases(deadline.Add(time.Hour))

			if got := sender.callCount(); got != tc.wantSent {
				t.Fatalf("release_lease=%q sent %d release(s), want %d", tc.value, got, tc.wantSent)
			}
			wantPhase := lease.PhaseRetained
			if tc.wantSent > 0 {
				wantPhase = lease.PhaseClosed
			}
			if got := recordPhase(t, p, id); got != wantPhase {
				t.Errorf("release_lease=%q left the record %v, want %v", tc.value, got, wantPhase)
			}
			if got := p.releasesReclaimedV4.Load(); got != 0 {
				t.Errorf("releases_reclaimed_v4 = %d on release_lease=%q, want 0", got, tc.value)
			}
		})
	}
}

// TestDeferredRelease_ANetworkWithNoStoredOptionsIsLeftAlone pins what
// happens when the options cannot be read.
//
// Leaving the record RETAINED is the answer, and the opposite failure
// is the reason: closing it would end an address's last chance of
// going back, on a network whose `release_lease` nobody has read.
func TestDeferredRelease_ANetworkWithNoStoredOptionsIsLeftAlone(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	deadline := time.Now()
	id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
	if err := deleteOptions(deferredTestNetwork); err != nil {
		t.Fatalf("deleteOptions: %v", err)
	}

	p.sweepDeferredReleases(deadline.Add(time.Hour))

	if got := sender.callCount(); got != 0 {
		t.Fatalf("%d release(s) went out for a network whose options could not be read, want 0", got)
	}
	if got := recordPhase(t, p, id); got != lease.PhaseRetained {
		t.Errorf("the record is %v, want RETAINED: a record closed here can never be released", got)
	}
}

// TestDeferredRelease_OneAttemptAndThenTheRecordIsClosed is Q2's answer
// asserted (defeat row I7).
//
// A record left RETAINED after a failed attempt is retried on every
// tick, which on a parent with no IPv6 address is a warning every 15
// seconds for the life of the deployment. One attempt, counted as a
// failure, and the address is left to expire on the server exactly as
// under `never`.
func TestDeferredRelease_OneAttemptAndThenTheRecordIsClosed(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	sender.err = lease.ErrReleaseNoServer
	deadline := time.Now()
	id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)

	for i := 0; i < 3; i++ {
		p.sweepDeferredReleases(deadline.Add(releaseSettle + time.Duration(i)*time.Second))
	}

	if got := sender.callCount(); got != 1 {
		t.Fatalf("the sender was called %d time(s) across three ticks, want 1", got)
	}
	if got := recordPhase(t, p, id); got != lease.PhaseClosed {
		t.Errorf("the record is %v after a failed release, want CLOSED", got)
	}
	if got := p.releaseFailuresV4.Load(); got != 1 {
		t.Errorf("release_failures_v4 = %d, want 1", got)
	}
	if got := p.releasesSentV4.Load(); got != 0 {
		t.Errorf("releases_sent_v4 = %d on a release that did not leave the host, want 0", got)
	}
}

// TestDeferredRelease_ARecordWithNoLeaseIsNotAFailureToInvestigate is
// the design note's row 10: an endpoint that never bound has nothing to
// hand back, and the fold is `on_stop`'s.
func TestDeferredRelease_ARecordWithNoLeaseIsNotAFailureToInvestigate(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	sender.err = lease.ErrReleaseNoAddr
	deadline := time.Now()
	id := heldRecord(t, p, deferredMAC(0x02), "", deadline)

	p.sweepDeferredReleases(deadline.Add(releaseSettle))

	if got := recordPhase(t, p, id); got != lease.PhaseClosed {
		t.Errorf("the record is %v, want CLOSED", got)
	}
	if got := p.releasesSentV4.Load(); got != 0 {
		t.Errorf("releases_sent_v4 = %d for a record that held no address, want 0", got)
	}
	if got := p.releasesReclaimedV4.Load(); got != 0 {
		t.Errorf("releases_reclaimed_v4 = %d, want 0: nothing claimed anything back", got)
	}

	// THE SAME RECORD UNDER THE OTHER FAMILY, and it is here because
	// the family of an address-less record cannot be read off its
	// address: there is no address. It is read off the SCOPE, which
	// carries the `#v6` suffix whether or not the endpoint ever bound.
	// A family taken from the address instead would file every
	// address-less DHCPv6 endpoint under the IPv4 counter, and an
	// operator looking at a v6-only network would see failures on a
	// family it does not run.
	id6 := p.recordCreated6(deferredTestNetwork, deferredMAC(0x03), releaseTestIdentity6())
	if id6 == "" {
		t.Fatal("no v6 record was created")
	}
	if err := p.records.Retained(id6, deadline); err != nil {
		t.Fatalf("Retained: %v", err)
	}

	p.sweepDeferredReleases(deadline.Add(releaseSettle))

	if got := p.releaseFailuresV6.Load(); got != 1 {
		t.Errorf("release_failures_v6 = %d for a DHCPv6 record that never bound, want 1", got)
	}
	if got := p.releaseFailuresV4.Load(); got != 1 {
		t.Errorf("release_failures_v4 = %d, want 1: the IPv4 record above and nothing "+
			"else. A v6 record counted here is the family being read off an address that "+
			"does not exist", got)
	}
	if got := recordPhase(t, p, id6); got != lease.PhaseClosed {
		t.Errorf("the v6 record is %v, want CLOSED", got)
	}
}

// TestDeferredRelease_TheV6RecordIsSweptUnderItsOwnNetworksOptions is
// defeat row I8.
//
// A v6 record is filed under `<network id>#v6`, and a network's options
// are filed under the network id alone: the state path refuses anything
// that is not a flat token, so a scope passed where a network id is
// wanted reads NO file and the sweep silently does nothing for every
// DHCPv6 address on the host. The counters are the v6 halves, because a
// dual-stack endpoint's two records are released independently.
func TestDeferredRelease_TheV6RecordIsSweptUnderItsOwnNetworksOptions(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	mac := deferredMAC(0x02)
	deadline := time.Now()

	id6 := p.recordCreated6(deferredTestNetwork, mac, releaseTestIdentity6())
	if id6 == "" {
		t.Fatal("no v6 record was created")
	}
	if err := p.records.Observed(id6, acquired6("2001:db8::10/64", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Retained(id6, deadline); err != nil {
		t.Fatalf("Retained: %v", err)
	}

	p.sweepDeferredReleases(deadline.Add(releaseSettle))

	if got := sender.callCount(); got != 1 {
		t.Fatalf("%d release(s) for a held DHCPv6 address, want 1", got)
	}
	if got := p.releasesSentV6.Load(); got != 1 {
		t.Errorf("releases_sent_v6 = %d, want 1", got)
	}
	if got := p.releasesSentV4.Load(); got != 0 {
		t.Errorf("releases_sent_v4 = %d for a v6-only record, want 0", got)
	}
	if len(sender.cfgs) > 0 && !sender.cfgs[0].Source.Is6() {
		t.Errorf("the v6 release was sent from %v, want a link-local on the parent",
			sender.cfgs[0].Source)
	}
	if got := recordPhase(t, p, id6); got != lease.PhaseClosed {
		t.Errorf("the v6 record is %v after its release, want CLOSED", got)
	}
}

// TestDeferredRelease_TheTickRunsBothPasses is the wiring, and it is a
// test because the two passes are one goroutine's whole job.
//
// A reservation Docker never built an endpoint for is retained by the
// first pass with a deadline; the second pass is what eventually hands
// that address back on an `on_remove` network. A tick that ran only one
// of them would leave either the orphan answering lookups forever or
// every held address held forever, and both are silent.
func TestDeferredRelease_TheTickRunsBothPasses(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	p.ipamReserves = newIPAMReserves()
	now := time.Now()

	// The deferred pass's candidate: due at this tick.
	held := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", now.Add(-releaseSettle))

	// The reservation pass's candidate: reserved long enough ago to be
	// stale, with no endpoint ever created for it.
	reserved := p.recordReserved(deferredTestNetwork, deferredMAC(0x09), dhcp.ClientIdentity([]byte{7}))
	if reserved == "" {
		t.Fatal("no reservation record was created")
	}
	if err := p.records.Observed(reserved, acquired("192.168.99.20/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	key := ipamReserveKey("pool-1", deferredMAC(0x09))
	res, _ := p.ipamReserves.begin(key, now.Add(-2*tombstoneTTL))
	p.ipamReserves.finish(key, res, ipamReservation{
		addr: netip.MustParsePrefix("192.168.99.20/24"), record: reserved,
	}, nil)

	p.sweepRecords(now)

	if got := sender.callCount(); got != 1 {
		t.Fatalf("the deferred pass sent %d release(s) on this tick, want 1", got)
	}
	if got := recordPhase(t, p, held); got != lease.PhaseClosed {
		t.Errorf("the held record is %v, want CLOSED", got)
	}
	if got := recordPhase(t, p, reserved); got != lease.PhaseRetained {
		t.Errorf("the orphaned reservation is %v, want RETAINED: the reservation pass did "+
			"not run on this tick", got)
	}
}

// TestDeferredRelease_APluginRestartInsideTheWindowStillReleases is the
// reason the deadline is written to the record file and not held in a
// timer.
//
// A plugin upgrade, a `docker plugin disable`/`enable`, a daemon restart
// and a crash all end the process while windows are open. A timer dies
// with it and the addresses it was holding are then held forever, which
// is the `never` outcome on a network that asked for the opposite, with
// no counter moving and nothing in the log to read. The deadline is a
// field on an append-only record instead, so the next process rebuilds
// it and the first sweep after it passes sends.
//
// TWO PROCESSES, ONE FILE, and that is what makes this a restart and not
// a second sweep: the record is written through one `dhcp.Records`
// handle under one instance id, that handle is closed, and everything
// after it runs on a second handle under a second id, exactly as the
// plugin's own state directory carries the file across a restart.
func TestDeferredRelease_APluginRestartInsideTheWindowStillReleases(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)
	hostParent(t, "192.168.99.2/24", "fe80::2/64")
	sender := installSender(t, nil)
	if err := saveOptions(deferredTestNetwork, DHCPNetworkOptions{
		Bridge:       "br0",
		ReleaseLease: ReleaseOnRemove,
	}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}

	recordPath := filepath.Join(dir, recordFileName)
	deadline := time.Now().Add(tombstoneTTL)

	before, err := dhcp.OpenRecords(recordPath, "instance-before-restart")
	if err != nil {
		t.Fatalf("OpenRecords: %v", err)
	}
	first := &Plugin{records: before}
	id := heldRecord(t, first, deferredMAC(0x02), "192.168.99.10/24", deadline)

	// THE PARTNER DIRECTION. This second address was already handed back
	// by the process that is about to die, and its record was closed to
	// say so. One attempt is the whole of the rule (#984): a restart
	// re-reads the same append-only file, and if the phase it reads were
	// not what decides, it would send this address back a second time,
	// after some other container may already hold it. A second
	// DHCPRELEASE for an address that has moved on takes it away from
	// whoever has it.
	done := heldRecord(t, first, deferredMAC(0x03), "192.168.99.11/24", deadline)
	first.closeRecord(done)

	if err := before.Close(); err != nil {
		t.Fatalf("closing the first process's records: %v", err)
	}

	// The process that laid the deadline is gone. Nothing in memory
	// remembers this address.
	after, err := dhcp.OpenRecords(recordPath, "instance-after-restart")
	if err != nil {
		t.Fatalf("reopening the records as the next process would: %v", err)
	}
	t.Cleanup(func() { _ = after.Close() })
	p := &Plugin{records: after}
	p.ipamReserves = newIPAMReserves()

	// Still inside the window: a restart does not shorten it.
	p.sweepRecords(deadline.Add(-time.Second))
	if got := sender.callCount(); got != 0 {
		t.Fatalf("%d release(s) went out %s before the deadline, want 0. The restart "+
			"reloaded the deadline and then ignored it", got, time.Second)
	}

	p.sweepRecords(deadline.Add(releaseSettle + time.Second))

	if got := sender.callCount(); got != 1 {
		t.Fatalf("%d release(s) went out after the deadline, want 1. The address was held "+
			"by a process that no longer exists, and the deadline in the record file is "+
			"the only thing that could have sent it", got)
	}
	if got := recordPhase(t, p, id); got != lease.PhaseClosed {
		t.Errorf("the held record is %v, want CLOSED", got)
	}
	if got := recordPhase(t, p, done); got != lease.PhaseClosed {
		t.Errorf("the record closed before the restart is %v, want CLOSED", got)
	}
	for _, rec := range sender.recs {
		addr, ok := rec.Addr()
		if ok && addr.String() == "192.168.99.11" {
			t.Errorf("the restart sent a second DHCPRELEASE for %s, an address this host "+
				"had already handed back before it went down. Whoever holds it now loses "+
				"it", addr)
		}
	}
}

// TestDeferredRelease_AnIpvlanEndpointIsReleasedAtItsDeadline is design
// row 12, and it is a test because the mode changes the record's KEY.
//
// ipvlan L2 slaves all inherit the parent's MAC, so a MAC-derived
// identity could not tell containers apart; the record is keyed on a
// value derived from the endpoint id instead (endpointRecordKey), and
// Docker mints a fresh endpoint id at every start. Two consequences,
// and the second is why this is not the same test as the base case:
// nothing can be reclaimed on an ipvlan network, because nothing can
// present the same key twice, and the same-key arm of the claim check
// can therefore never fire for one. The address must go back at the
// deadline. A check that had reached for "some record with this MAC" on
// a network where every record carries an endpoint-derived key would
// suppress nothing here, and a check that keyed on the PARENT's MAC
// would suppress everything.
func TestDeferredRelease_AnIpvlanEndpointIsReleasedAtItsDeadline(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	deadline := time.Now()

	const (
		oldEndpoint = "aaaaaaaabbbbccccddddeeeeeeeeeeee"
		newEndpoint = "11111111222233334444555555555555"
	)
	oldKey := endpointRecordKey(ModeIPvlan, oldEndpoint, nil)
	newKey := endpointRecordKey(ModeIPvlan, newEndpoint, nil)
	if len(oldKey) == 0 || len(newKey) == 0 {
		t.Fatal("the ipvlan record key is empty, so this test is not driving the ipvlan shape")
	}
	if oldKey.String() == newKey.String() {
		t.Fatalf("two endpoint ids produced one record key (%s); the restart this test "+
			"describes would be indistinguishable from the stop", oldKey)
	}

	id := heldRecord(t, p, oldKey, "192.168.99.10/24", deadline)

	// The container came back. On ipvlan that is a NEW key and a fresh
	// address, and the old address is nobody's.
	liveRecord(t, p, newKey, "192.168.99.11/24")

	p.sweepDeferredReleases(deadline.Add(releaseSettle))

	if got := sender.callCount(); got != 1 {
		t.Fatalf("%d release(s) for an ipvlan endpoint's held address, want 1. ipvlan has "+
			"no restart stability to protect: the returning container cannot present the "+
			"old key, so holding the address keeps it out of the pool for nothing", got)
	}
	if got := p.releasesReclaimedV4.Load(); got != 0 {
		t.Errorf("releases_reclaimed_v4 = %d on an ipvlan network, want 0: nothing on one "+
			"can claim an address back", got)
	}
	if got := recordPhase(t, p, id); got != lease.PhaseClosed {
		t.Errorf("the held record is %v, want CLOSED", got)
	}
}

// TestDeferredRelease_TheTwoFamiliesAreDecidedApart is design row 11.
//
// A dual-stack endpoint is TWO records under two scopes with two
// deadlines, and a restart can be given its old IPv4 address and a
// fresh IPv6 one, or the other way round. A decision taken once for the
// endpoint would then either hand back an address a container is using
// or keep one nobody wants. The observer is which family's datagram
// left and which family's counter moved.
func TestDeferredRelease_TheTwoFamiliesAreDecidedApart(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	mac := deferredMAC(0x02)
	deadline := time.Now()

	v4 := heldRecord(t, p, mac, "192.168.99.10/24", deadline)

	id6 := p.recordCreated6(deferredTestNetwork, mac, releaseTestIdentity6())
	if id6 == "" {
		t.Fatal("no v6 record was created")
	}
	if err := p.records.Observed(id6, acquired6("2001:db8::10/64", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Retained(id6, deadline); err != nil {
		t.Fatalf("Retained: %v", err)
	}

	// The restart got its v4 address back and a different v6 one.
	liveRecord(t, p, mac, "192.168.99.10/24")
	live6 := p.recordCreated6(deferredTestNetwork, mac, releaseTestIdentity6())
	if err := p.records.Observed(live6, acquired6("2001:db8::99/64", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Bound(live6); err != nil {
		t.Fatalf("Bound: %v", err)
	}

	p.sweepDeferredReleases(deadline.Add(releaseSettle))

	if got := sender.callCount(); got != 1 {
		t.Fatalf("%d datagram(s) left the host, want 1: the v6 address is nobody's and the "+
			"v4 address is the running container's", got)
	}
	if got := p.releasesSentV6.Load(); got != 1 {
		t.Errorf("releases_sent_v6 = %d, want 1", got)
	}
	if got := p.releasesSentV4.Load(); got != 0 {
		t.Errorf("releases_sent_v4 = %d, want 0: that address is on a running container", got)
	}
	if got := p.releasesReclaimedV4.Load(); got != 1 {
		t.Errorf("releases_reclaimed_v4 = %d, want 1", got)
	}
	if got := p.releasesReclaimedV6.Load(); got != 0 {
		t.Errorf("releases_reclaimed_v6 = %d, want 0: the v6 address went back, it was not "+
			"taken back", got)
	}
	if got := recordPhase(t, p, v4); got != lease.PhaseClosed {
		t.Errorf("the v4 record is %v, want CLOSED", got)
	}
	if got := recordPhase(t, p, id6); got != lease.PhaseClosed {
		t.Errorf("the v6 record is %v, want CLOSED", got)
	}
}

// TestDeleteNetwork_SaysSoWhenItCannotReadTheOptionsItNeeds is the one
// place where "a later pass can still decide" is false.
//
// Everywhere else an unreadable options file means leave the record
// alone: the file may be readable on the next tick, and a record left
// RETAINED can still be decided then. Here the next tick has nothing to
// read. The options file is deleted moments later in the same handler,
// the tombstones are keyed by this network id and go with it, and the
// record is left holding an address nothing will ever look at again.
//
// Nothing can be sent, and that is not what this test is about. The
// release needs the parent interface and the value of `release_lease`,
// and both were in the file. What it is about is that the operator is
// told, at warning level and not at the debug level the recoverable
// case uses, because the addresses are gone from the pool until the
// server's own lease runs out and nothing else will ever mention them.
func TestDeleteNetwork_SaysSoWhenItCannotReadTheOptionsItNeeds(t *testing.T) {
	const orphan = "deferrednet-no-options"

	p, sender := deferredPlugin(t, ReleaseOnRemove)

	// A held record on a network whose options were never stored, which
	// is what an options file the plugin cannot read looks like from
	// here.
	id := p.recordCreated(orphan, deferredMAC(0x07), dhcp.ClientIdentity(deferredMAC(0x07)))
	if id == "" {
		t.Fatal("no record was created")
	}
	if err := p.records.Observed(id, acquired("192.168.99.30/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Retained(id, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Retained: %v", err)
	}

	out := captureLog(t, func() {
		if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: orphan}); err != nil {
			t.Fatalf("DeleteNetwork: %v", err)
		}
	})

	if got := sender.callCount(); got != 0 {
		t.Fatalf("%d datagram(s) left the host for a network whose release_lease and parent "+
			"interface could not be read, want 0", got)
	}
	if got := recordPhase(t, p, id); got != lease.PhaseRetained {
		t.Errorf("the held record is %v, want RETAINED: nothing decided it, and closing it "+
			"would record a handback that never happened", got)
	}
	if !strings.Contains(out, "could not be read while it was being removed") {
		t.Errorf("the removal said nothing about the addresses it could not hand back. "+
			"They are out of the pool until the server's lease expires and this handler is "+
			"the last thing that could have mentioned them:\n%s", out)
	}
	if !strings.Contains(out, "level=warning") {
		t.Errorf("the line is not at warning level, so it sits with the debug lines the "+
			"recoverable case writes:\n%s", out)
	}
}

// TestDeferredRelease_TheStopSaysTheAddressIsBeingKept is the operator's
// only view of the window while it is open.
//
// On `on_remove` the stop sends nothing and moves no counter, so a
// `docker stop` on this network looks from the outside exactly like a
// stop on a `never` network for the next 60 to 80 seconds. The one
// thing that tells them apart is this line, and the reference documents
// it as such, so its absence is a defect and not a missing nicety. The
// address is in it because an operator reading it is about to go and
// look at the server.
//
// The other two values must NOT print it: `on_stop` has already sent by
// this point and `never` will never send, and a line promising a
// release on either of them is a wrong answer rather than a quiet one.
func TestDeferredRelease_TheStopSaysTheAddressIsBeingKept(t *testing.T) {
	for _, tc := range []struct {
		value    string
		wantLine bool
		wantSent int
	}{
		{value: ReleaseOnRemove, wantLine: true, wantSent: 0},
		{value: ReleaseOnStop, wantLine: false, wantSent: 1},
		{value: ReleaseNever, wantLine: false, wantSent: 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)
			sender := installSender(t, nil)
			m := releasingManager(t, p, tc.value, false)

			out := captureLog(t, func() {
				if err := m.StopForLeave(); err != nil {
					t.Fatalf("StopForLeave: %v", err)
				}
			})

			if got := sender.callCount(); got != tc.wantSent {
				t.Fatalf("release_lease=%q sent %d datagram(s) at the stop, want %d",
					tc.value, got, tc.wantSent)
			}
			const marker = "keeping this endpoint's addresses for the restart window"
			if got := strings.Contains(out, marker); got != tc.wantLine {
				t.Fatalf("release_lease=%q: the stop log says %q: %v, want %v\n%s",
					tc.value, marker, got, tc.wantLine, out)
			}
			if !tc.wantLine {
				return
			}
			if !strings.Contains(out, tombstoneTTL.String()) {
				t.Errorf("the line does not carry the window (%s), so it says an address is "+
					"being kept and not for how long:\n%s", tombstoneTTL, out)
			}
			if !strings.Contains(out, "192.168.99") {
				t.Errorf("the line does not carry the address being kept, which is what an "+
					"operator takes to the server:\n%s", out)
			}
		})
	}
}

// TestDeleteNetwork_ReleasesOnlyTheNetworkBeingRemoved is the other
// half of the network-removal pass, and it is the half with no deadline
// protecting it.
//
// That pass releases whatever is held, whatever the clock says, because
// nothing can claim an address back on a network that is going away.
// The network id is therefore the only thing keeping it off a NEIGHBOUR
// network's held addresses, whose windows are still open and whose
// containers may be about to restart into them. One `docker network rm`
// emptying every other on_remove network's pool is a fault an operator
// would read as the server losing leases.
func TestDeleteNetwork_ReleasesOnlyTheNetworkBeingRemoved(t *testing.T) {
	const neighbour = "deferrednet2"

	p, sender := deferredPlugin(t, ReleaseOnRemove)
	if err := saveOptions(neighbour, DHCPNetworkOptions{
		Bridge:       "br0",
		ReleaseLease: ReleaseOnRemove,
	}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}

	doomed := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", time.Now().Add(time.Hour))

	spared := p.recordCreated(neighbour, deferredMAC(0x03), dhcp.ClientIdentity(deferredMAC(0x03)))
	if spared == "" {
		t.Fatal("no record was created on the neighbour network")
	}
	if err := p.records.Observed(spared, acquired("192.168.99.11/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Retained(spared, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Retained: %v", err)
	}

	if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: deferredTestNetwork}); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}

	if got := sender.callCount(); got != 1 {
		t.Fatalf("%d datagram(s) left the host on one network removal, want 1", got)
	}
	if got := recordPhase(t, p, doomed); got != lease.PhaseClosed {
		t.Errorf("the removed network's held record is %v, want CLOSED", got)
	}
	if got := recordPhase(t, p, spared); got != lease.PhaseRetained {
		t.Errorf("the neighbour network's held record is %v, want RETAINED. Its window is "+
			"still open and its container may be about to restart into that address; "+
			"removing a different network must not touch it", got)
	}
}

// TestDeferredRelease_OnlyHeldRecordsAreHandedBack is the phase filter,
// and it is asserted through `docker network rm` because that is the one
// caller with no deadline to hide behind.
//
// The deferred sweep is protected twice over: a record that is not
// RETAINED carries no deadline, and a zero deadline is not due. The
// network-removal pass has no such second guard, by design -- nothing
// can claim an address back on a network that is going away, so it
// releases whatever is held whatever the clock says. The phase is
// therefore the only thing standing between it and a JOINED record,
// which is an endpoint whose container is running on the address.
// Releasing that is #524's duplicate assignment with the plugin's own
// fingerprints on it.
//
// A CLOSED record is in the same table for the opposite reason: its
// address has already gone back once, and a second datagram would tell
// the server to free an address it may have handed to somebody else.
func TestDeferredRelease_OnlyHeldRecordsAreHandedBack(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)

	held := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", time.Now().Add(time.Hour))
	live := liveRecord(t, p, deferredMAC(0x03), "192.168.99.11/24")
	closed := heldRecord(t, p, deferredMAC(0x04), "192.168.99.12/24", time.Now().Add(time.Hour))
	p.closeRecord(closed)

	if got := p.releaseNetworkRecords(deferredTestNetwork); got != 1 {
		t.Fatalf("the network-removal pass handed back %d address(es), want 1: only the "+
			"RETAINED record is held by nobody", got)
	}
	if got := sender.callCount(); got != 1 {
		t.Fatalf("%d datagram(s) left the host, want 1", got)
	}
	if got := recordPhase(t, p, held); got != lease.PhaseClosed {
		t.Errorf("the held record is %v, want CLOSED", got)
	}
	if got := recordPhase(t, p, live); got != lease.PhaseJoined {
		t.Errorf("the record of a RUNNING endpoint is %v, want JOINED. Its address was "+
			"handed to the server while a container was using it, which is the duplicate "+
			"assignment the whole option is built to avoid", got)
	}
	if got := recordPhase(t, p, closed); got != lease.PhaseClosed {
		t.Errorf("the already-closed record is %v, want CLOSED", got)
	}
}

// TestDeferredRelease_ARecordLeftJoinedByAMissedStopIsNeverReleased is
// design row 13, and it is the BOUND on the option rather than a
// feature of it.
//
// If the plugin is not running when a container stops, Docker's
// DeleteEndpoint never reaches it. The record stays JOINED, no deadline
// is ever written on it, and no window opens. `on_stop` misses the same
// stop for the same reason. What this test pins is that the sweep does
// not try to repair it: a JOINED record is a record the plugin believes
// a container is using, and a background pass that released those would
// hand back every address on the host after any restart that arrives
// before recovery has adopted the running containers (design row 15).
// The cost of the bound is one address left to expire; the cost of
// crossing it is every address on the host.
func TestDeferredRelease_ARecordLeftJoinedByAMissedStopIsNeverReleased(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)

	stranded := liveRecord(t, p, deferredMAC(0x02), "192.168.99.10/24")

	// Far past any deadline this record could have carried, had one
	// ever been written on it.
	p.sweepRecords(time.Now().Add(24 * time.Hour))

	if got := sender.callCount(); got != 0 {
		t.Fatalf("%d datagram(s) left the host for a record still marked JOINED, want 0. "+
			"A JOINED record is one the plugin believes a container is using; sweeping "+
			"those hands back live addresses after every restart", got)
	}
	if got := recordPhase(t, p, stranded); got != lease.PhaseJoined {
		t.Errorf("the record is %v, want JOINED: the sweep rewrote a record it has no "+
			"deadline for", got)
	}
}

// TestDeferredRelease_TheIPAMReservationWithNoEndpointGoesBackAtItsDeadline
// closes the escape v2.1.1 documented (#962, #984 Q6).
//
// In IPAM mode the driver can answer RequestAddress and never see a
// CreateEndpoint for it: the daemon rolled the create back, or the
// container never started. The address is leased from the real server
// and nothing on the host holds it, and until this change nothing ever
// handed it back on ANY value of `release_lease` -- that is the sentence
// the release notes and the reference carry, and it is why it is worth a
// test of its own rather than a line in the wiring test above.
//
// The two passes do it between them, which is why both ticks are here:
// the reservation pass retains the orphan with a deadline, and the
// deferred pass hands it back when that deadline passes. A test that ran
// one tick would see the retention and call it the feature.
//
// This arm is proven here and not in the integration lane because the
// lane's IPAM shape cannot produce a reservation without an endpoint on
// demand: the harness drives Docker, and Docker only skips the create
// when something else has already failed. The lane proves the ordinary
// IPAM-mode release instead
// (TestReleaseLease_OnRemoveHandsAnIPAMAddressBackToo).
func TestDeferredRelease_TheIPAMReservationWithNoEndpointGoesBackAtItsDeadline(t *testing.T) {
	for _, tc := range []struct {
		value    string
		wantSent int
	}{
		{value: ReleaseOnRemove, wantSent: 1},
		// The control, and the sentence the docs still carry for these
		// two: on `never` and `on_stop` the orphan stays held and the
		// address waits out the server's lease.
		{value: ReleaseOnStop, wantSent: 0},
		{value: ReleaseNever, wantSent: 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			p, sender := deferredPlugin(t, tc.value)
			p.ipamReserves = newIPAMReserves()
			now := time.Now()

			id := p.recordReserved(deferredTestNetwork, deferredMAC(0x09), dhcp.ClientIdentity([]byte{7}))
			if id == "" {
				t.Fatal("no reservation record was created")
			}
			if err := p.records.Observed(id, acquired("192.168.99.20/24", time.Hour), nil); err != nil {
				t.Fatalf("Observed: %v", err)
			}
			key := ipamReserveKey("pool-1", deferredMAC(0x09))
			res, _ := p.ipamReserves.begin(key, now.Add(-2*tombstoneTTL))
			p.ipamReserves.finish(key, res, ipamReservation{
				addr: netip.MustParsePrefix("192.168.99.20/24"), record: id,
			}, nil)

			// Tick one: the reservation pass finds the orphan and gives
			// it the same restart window a stopped container gets.
			p.sweepRecords(now)
			if got := recordPhase(t, p, id); got != lease.PhaseRetained {
				t.Fatalf("after the first tick the orphaned reservation is %v, want RETAINED; "+
					"without a deadline on it there is nothing for the deferred pass to find", got)
			}
			if got := sender.callCount(); got != 0 {
				t.Fatalf("%d release(s) went out on the tick that retained the reservation, "+
					"want 0: the window has not run out yet", got)
			}

			// Tick two: past the deadline the reservation pass wrote.
			p.sweepRecords(now.Add(tombstoneTTL + releaseSettle + time.Second))

			if got := sender.callCount(); got != tc.wantSent {
				t.Fatalf("release_lease=%q: %d release(s) for an address reserved for an "+
					"endpoint Docker never created, want %d", tc.value, got, tc.wantSent)
			}
			wantPhase := lease.PhaseRetained
			if tc.wantSent > 0 {
				wantPhase = lease.PhaseClosed
			}
			if got := recordPhase(t, p, id); got != wantPhase {
				t.Errorf("the reservation record is %v, want %v", got, wantPhase)
			}
		})
	}
}

// TestDeleteNetwork_HandsHeldAddressesBackBeforeTheOptionsGo is defeat
// row I3, and the ORDER is the whole assertion.
//
// The release reads the network's `release_lease` and its parent
// interface from the stored options, and the same handler deletes that
// file. Moving the release below the delete leaves a handler that reads
// nothing, decides nothing, sends nothing and logs nothing: the
// addresses leak in silence and no counter moves. The deadline has not
// passed here, deliberately -- on a network that is going away nothing
// can claim an address back, so there is nothing to wait for.
func TestDeleteNetwork_HandsHeldAddressesBackBeforeTheOptionsGo(t *testing.T) {
	for _, tc := range []struct {
		value    string
		wantSent int
	}{
		{value: ReleaseOnRemove, wantSent: 1},
		{value: ReleaseOnStop, wantSent: 0},
		{value: ReleaseNever, wantSent: 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			p, sender := deferredPlugin(t, tc.value)
			id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", time.Now().Add(time.Hour))

			if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: deferredTestNetwork}); err != nil {
				t.Fatalf("DeleteNetwork: %v", err)
			}

			if got := sender.callCount(); got != tc.wantSent {
				t.Fatalf("release_lease=%q: %d release(s) at network removal, want %d",
					tc.value, got, tc.wantSent)
			}
			if _, err := loadOptions(deferredTestNetwork); err == nil {
				t.Fatal("the stored options survived DeleteNetwork, so this test cannot tell " +
					"a release that read them from one that ran after they were gone")
			}
			wantPhase := lease.PhaseRetained
			if tc.wantSent > 0 {
				wantPhase = lease.PhaseClosed
			}
			if got := recordPhase(t, p, id); got != wantPhase {
				t.Errorf("the held record is %v, want %v", got, wantPhase)
			}
		})
	}
}
