// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const deferredTestNetwork = "deferrednet1"

func acquired6(addr string, until time.Duration) lease.Event {
	return lease.Event{
		Kind: lease.Acquired,
		Lease: lease.Lease{
			Addr:   netip.MustParsePrefix(addr),
			Expire: time.Now().Add(until),
		},
	}
}

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

func deferredMAC(n byte) net.HardwareAddr {
	return net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, n}
}

func TestDeferredRelease_TheClaimCheckIsKeyedOnTheAddress(t *testing.T) {
	const held = "192.168.99.10/24"
	for _, tc := range []struct {
		name          string
		beside        func(t *testing.T, p *Plugin, heldMAC net.HardwareAddr, deadline time.Time)
		wantSent      int
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
			name: "an acquisition under the same MAC is still in flight",
			beside: func(t *testing.T, p *Plugin, mac net.HardwareAddr, _ time.Time) {
				liveRecord(t, p, mac, "")
			},
			wantSent:      0,
			wantReclaimed: 0,
		},
		{
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

func captureDebugLog(t *testing.T, fn func()) string {
	t.Helper()

	std := log.StandardLogger()
	prevOut, prevLevel := std.Out, std.GetLevel()
	t.Cleanup(func() {
		std.Out = prevOut
		std.SetLevel(prevLevel)
	})

	var buf bytes.Buffer
	std.Out = &buf
	std.SetLevel(log.DebugLevel)
	fn()
	return buf.String()
}

func TestDeferredRelease_TheDocumentedWindowIsTheArithmeticOfItsConstants(t *testing.T) {
	const (
		documentedFloor   = 65 * time.Second
		documentedCeiling = 80 * time.Second
	)

	if got := tombstoneTTL + releaseSettle; got != documentedFloor {
		t.Errorf("the soonest a held address can go back is %v and the documented band "+
			"says %v. tombstoneTTL is %v and releaseSettle is %v; whichever moved, every "+
			"prose statement of this band is now stale, and so is every sentence that "+
			"spells one of the three constants beside its subject. Do not sweep for the "+
			"old number by hand: a grep for the number and its unit misses the sites "+
			"that abbreviate it, parenthesise it, wrap it across a line break or write "+
			"it as a word, and this tree has all four. `bash "+
			"scripts/check-window-constants.sh` derives the band from the constants and "+
			"names every stale sentence with its file and line",
			got, documentedFloor, tombstoneTTL, releaseSettle)
	}
	if got := tombstoneTTL + releaseSettle + ipamSweepInterval; got != documentedCeiling {
		t.Errorf("the latest a held address goes back is %v and the documented band says "+
			"%v. The ceiling is one whole sweep tick past the floor, because a sweep that "+
			"ran just before the settle expired waits a full interval; ipamSweepInterval "+
			"is %v. `bash scripts/check-window-constants.sh` names the sentences this "+
			"falsifies", got, documentedCeiling, ipamSweepInterval)
	}
}

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

func TestDeferredRelease_TheSweepIsQuietAboutAnOptionsFileItMayReadNextTick(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	deadline := time.Now()
	id := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
	if err := deleteOptions(deferredTestNetwork); err != nil {
		t.Fatalf("deleteOptions: %v", err)
	}

	out := captureDebugLog(t, func() {
		p.sweepDeferredReleases(deadline.Add(time.Hour))
	})

	if got := sender.callCount(); got != 0 {
		t.Fatalf("%d release(s) went out, want 0", got)
	}
	if got := recordPhase(t, p, id); got != lease.PhaseRetained {
		t.Errorf("the record is %v, want RETAINED", got)
	}
	for _, loud := range []string{"level=warning", "level=error"} {
		if strings.Contains(out, loud) {
			t.Errorf("the sweep logged at %s for a condition a later tick may resolve. "+
				"This pass runs every %v forever, so that is four an hour for the life of "+
				"the deployment:\n%s", loud, ipamSweepInterval, out)
		}
	}
	if !strings.Contains(out, "leaving the record as it is") {
		t.Errorf("the sweep said nothing at all. Quiet is not silent: an operator asking "+
			"why an address never went back has only this line to find:\n%s", out)
	}
}

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

func TestDeferredRelease_TheTickRunsBothPasses(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	p.ipamReserves = newIPAMReserves()
	now := time.Now()

	held := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", now.Add(-releaseSettle))

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

	done := heldRecord(t, first, deferredMAC(0x03), "192.168.99.11/24", deadline)
	first.closeRecord(done)

	if err := before.Close(); err != nil {
		t.Fatalf("closing the first process's records: %v", err)
	}

	after, err := dhcp.OpenRecords(recordPath, "instance-after-restart")
	if err != nil {
		t.Fatalf("reopening the records as the next process would: %v", err)
	}
	t.Cleanup(func() { _ = after.Close() })
	p := &Plugin{records: after}
	p.ipamReserves = newIPAMReserves()

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

func TestDeleteNetwork_SaysSoWhenItCannotReadTheOptionsItNeeds(t *testing.T) {
	const orphan = "deferrednet-no-options"

	p, sender := deferredPlugin(t, ReleaseOnRemove)

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

func TestDeferredRelease_ARecordLeftJoinedByAMissedStopIsNeverReleased(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)

	stranded := liveRecord(t, p, deferredMAC(0x02), "192.168.99.10/24")

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

func TestDeferredRelease_TheIPAMReservationWithNoEndpointGoesBackAtItsDeadline(t *testing.T) {
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

			p.sweepRecords(now)
			if got := recordPhase(t, p, id); got != lease.PhaseRetained {
				t.Fatalf("after the first tick the orphaned reservation is %v, want RETAINED; "+
					"without a deadline on it there is nothing for the deferred pass to find", got)
			}
			if got := sender.callCount(); got != 0 {
				t.Fatalf("%d release(s) went out on the tick that retained the reservation, "+
					"want 0: the window has not run out yet", got)
			}

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
