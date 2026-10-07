// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/runtime"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// gatedSender records every release it is handed and holds the send of the record named gate until open is closed.
type gatedSender struct {
	mu      sync.Mutex
	recs    []lease.Record
	gate    string
	entered chan struct{}
	open    chan struct{}
	once    sync.Once
}

func installGatedSender(t *testing.T, gate string) *gatedSender {
	t.Helper()
	g := &gatedSender{gate: gate, entered: make(chan struct{}), open: make(chan struct{})}
	prev := rtSendRelease
	rtSendRelease = func(rec lease.Record, _ runtime.ReleaseConfig) error {
		g.mu.Lock()
		g.recs = append(g.recs, rec)
		g.mu.Unlock()
		if rec.ID == g.gate {
			g.once.Do(func() { close(g.entered) })
			<-g.open
		}
		return nil
	}
	t.Cleanup(func() { rtSendRelease = prev })
	return g
}

func (g *gatedSender) sent(id string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.recs {
		if r.ID == id {
			n++
		}
	}
	return n
}

func TestDeferredRelease_ALiveClaimWrittenDuringThePassIsNotReleased(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	deadline := time.Now()
	idA := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
	idB := heldRecord(t, p, deferredMAC(0x03), "192.168.99.11/24", deadline)

	var once sync.Once
	var liveID string
	sender.errFor = func(rec lease.Record) error {
		once.Do(func() { liveID = liveRecord(t, p, deferredMAC(0x03), "192.168.99.11/24") })
		return nil
	}

	p.sweepDeferredReleases(deadline.Add(releaseSettle))

	for _, r := range sender.recs {
		if r.ID == idB {
			t.Errorf("record B (192.168.99.11) was released although live record %s held that address before its release went out", liveID)
		}
	}
	if got := len(sender.recs); got != 1 || sender.recs[0].ID != idA {
		t.Errorf("the sender saw %d releases, want only record A's", got)
	}
}

func TestDeferredRelease_AnAcquisitionStartedDuringThePassIsNotReleasedFrom(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	deadline := time.Now()
	heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
	idB := heldRecord(t, p, deferredMAC(0x03), "192.168.99.11/24", deadline)

	var once sync.Once
	sender.errFor = func(lease.Record) error {
		once.Do(func() { liveRecord(t, p, deferredMAC(0x03), "") })
		return nil
	}

	p.sweepDeferredReleases(deadline.Add(releaseSettle))

	for _, r := range sender.recs {
		if r.ID == idB {
			t.Error("record B was released while a new endpoint under its MAC was acquiring an address")
		}
	}
}

func TestDeferredRelease_DeleteNetworkAndTheSweepReleaseARecordOnce(t *testing.T) {
	p, _ := deferredPlugin(t, ReleaseOnRemove)
	deadline := time.Now()
	idA := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
	g := installGatedSender(t, idA)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.sweepDeferredReleases(deadline.Add(releaseSettle)) }()
	<-g.entered
	removed := make(chan struct{})
	go func() { defer wg.Done(); p.releaseNetworkRecords(deferredTestNetwork); close(removed) }()
	select {
	case <-removed:
	case <-time.After(500 * time.Millisecond):
	}
	close(g.open)
	wg.Wait()

	if got := g.sent(idA); got != 1 {
		t.Errorf("the sender saw record A %d times, want 1", got)
	}
	if s := p.releasesSentV4.Load(); s != 1 {
		t.Errorf("releases_sent_v4 = %d, want 1", s)
	}
}

func TestDeferredRelease_DeleteNetworkDoesNotWaitForAnotherNetworksRelease(t *testing.T) {
	p, _ := deferredPlugin(t, ReleaseOnRemove)
	const other = "deferrednet2"
	if err := saveOptions(other, DHCPNetworkOptions{Bridge: "br0", ReleaseLease: ReleaseOnRemove}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	deadline := time.Now()
	idOther := p.recordCreated(other, deferredMAC(0x04), dhcp.ClientIdentity(deferredMAC(0x04)))
	if err := p.records.Observed(idOther, acquired("192.168.99.12/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Retained(idOther, deadline); err != nil {
		t.Fatalf("Retained: %v", err)
	}
	idA := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline.Add(time.Hour))
	g := installGatedSender(t, idOther)

	swept := make(chan struct{})
	go func() { p.sweepDeferredReleases(deadline.Add(releaseSettle)); close(swept) }()
	<-g.entered
	start := time.Now()
	removed := make(chan struct{})
	go func() { p.releaseNetworkRecords(deferredTestNetwork); close(removed) }()
	select {
	case <-removed:
	case <-time.After(2 * time.Second):
		t.Error("DeleteNetwork's release waited on another network's release in flight")
	}
	wall := time.Since(start)
	close(g.open)
	<-swept
	<-removed

	t.Logf("DeleteNetwork wall time while the sweep held another record: %v", wall)
	if got := g.sent(idA); got != 1 {
		t.Errorf("the sender saw this network's record %d times, want 1", got)
	}
}

func TestDeferredRelease_AFailedCloseIsNotSentAgainNextTick(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	deadline := time.Now()
	idA := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)
	// A closed store refuses every append and still reads, as a full state disk does.
	_ = p.records.Close()

	p.sweepDeferredReleases(deadline.Add(releaseSettle))
	p.sweepDeferredReleases(deadline.Add(releaseSettle + ipamSweepInterval))

	if got := sender.callCount(); got != 1 {
		t.Errorf("the sender saw %d releases of record %s across two ticks, want 1", got, idA)
	}
}

// resumeHeld drives a Join whose CreateEndpoint record was never written; a v6 restart keeps its address by its DUID (#1237).
func resumeHeld(t *testing.T, p *Plugin, v6 bool, id, wantAddr string) {
	t.Helper()
	m := &dhcpManager{plugin: p, joinReq: JoinRequest{NetworkID: deferredTestNetwork}}
	m.MacAddress = deferredMAC(0x02)
	if v6 {
		got, _, id6 := m.resumeFromRecord6()
		if got != id {
			t.Fatalf("Join resumed record %q, want the held %q", got, id)
		}
		if want := releaseTestIdentity6(); !bytes.Equal(id6.Bytes(), want.Bytes()) {
			t.Errorf("the restarted container sends DUID %x, want the held record's %x", id6.Bytes(), want.Bytes())
		}
		return
	}
	got, res := m.resumeFromRecord()
	if got != id {
		t.Fatalf("Join resumed record %q, want the held %q", got, id)
	}
	addr := res.Prefer
	if res.Lease != nil {
		addr = res.Lease.Addr.Addr().String()
	}
	if addr != wantAddr {
		t.Errorf("the restarted container is offered %q, want its held %s back", addr, wantAddr)
	}
}

func TestDeferredRelease_AContainerRunningOnAResumedHeldRecordKeepsItsAddress(t *testing.T) {
	for _, tc := range []struct {
		name string
		v6   bool
		addr string
	}{
		{"v4", false, "192.168.99.10"},
		{"v6", true, "2001:db8::10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sender := deferredPlugin(t, ReleaseOnRemove)
			deadline := time.Now()
			var id string
			if tc.v6 {
				id = heldRecord6(t, p, deadline)
			} else {
				id = heldRecord(t, p, deferredMAC(0x02), tc.addr+"/24", deadline)
			}

			resumeHeld(t, p, tc.v6, id, tc.addr)
			if ph := recordPhase(t, p, id); ph != lease.PhaseJoined {
				t.Errorf("the resumed record is %s, want joined: a held record keeps its deadline", ph)
			}
			p.sweepDeferredReleases(deadline.Add(releaseSettle))

			if got := sender.callCount(); got != 0 {
				t.Errorf("the sender saw %d releases of the address a running container resumed", got)
			}
		})
	}
}

func TestDeferredRelease_AResumedHeldRecordWhoseBindCannotBeWrittenIsNotReleased(t *testing.T) {
	for _, tc := range []struct {
		name string
		v6   bool
		addr string
	}{
		{"v4", false, "192.168.99.10"},
		{"v6", true, "2001:db8::10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, sender := deferredPlugin(t, ReleaseOnRemove)
			deadline := time.Now()
			var id string
			if tc.v6 {
				id = heldRecord6(t, p, deadline)
			} else {
				id = heldRecord(t, p, deferredMAC(0x02), tc.addr+"/24", deadline)
			}
			_ = p.records.Close()

			resumeHeld(t, p, tc.v6, id, tc.addr)
			p.sweepDeferredReleases(deadline.Add(releaseSettle))
			if got := sender.callCount(); got != 0 {
				t.Fatalf("the sender saw %d releases of the address a running container resumed", got)
			}

			// The container is removed: its teardown lays a hold, which this store cannot write either.
			p.recordRetained(id, deadline)
			p.sweepDeferredReleases(deadline.Add(releaseSettle))
			if got := sender.callCount(); got != 1 {
				t.Errorf("the sender saw %d releases once the container was gone, want 1", got)
			}
		})
	}
}

func TestDeferredRelease_ARecordResumedDuringThePassIsNotReleased(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	deadline := time.Now()
	idA := heldRecord(t, p, deferredMAC(0x03), "192.168.99.11/24", deadline)
	idB := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", deadline)

	var once sync.Once
	sender.errFor = func(lease.Record) error {
		once.Do(func() { resumeHeld(t, p, false, idB, "192.168.99.10") })
		return nil
	}

	p.sweepDeferredReleases(deadline.Add(releaseSettle))

	if got := len(sender.recs); got != 1 || sender.recs[0].ID != idA {
		t.Errorf("the sender saw %d releases, want only record A's: B was resumed by a running container first", got)
	}
}

func TestDeferredRelease_ARecordClosedAfterThePassReadItIsNotSent(t *testing.T) {
	p, sender := deferredPlugin(t, ReleaseOnRemove)
	idA := heldRecord(t, p, deferredMAC(0x02), "192.168.99.10/24", time.Now())
	opts, err := loadOptions(deferredTestNetwork)
	if err != nil {
		t.Fatalf("loadOptions: %v", err)
	}
	p.closeRecord(idA)

	p.handOneRecordBack(idA, func(lease.Record) bool { return true }, opts, deferredTestNetwork, false)

	if got := sender.callCount(); got != 0 {
		t.Errorf("the sender saw %d releases of a record another pass had already closed", got)
	}
}
