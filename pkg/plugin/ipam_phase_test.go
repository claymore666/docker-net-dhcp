// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

type ipamPhaseCase struct {
	phase   lease.Phase
	answers bool
	build   func(t *testing.T, p *Plugin, scope string, mac net.HardwareAddr) string
}

func ipamPhaseCases() []ipamPhaseCase {
	ident := dhcp.ClientIdentity([]byte{9, 9, 9})
	lease4 := func(t *testing.T, p *Plugin, id string) {
		t.Helper()
		if err := p.records.Observed(id, acquired(ipamPhaseAddr, time.Hour), nil); err != nil {
			t.Fatalf("Observed(acquired): %v", err)
		}
	}
	return []ipamPhaseCase{
		{lease.PhaseUnset, false, func(t *testing.T, p *Plugin, scope string, mac net.HardwareAddr) string {
			return ""
		}},
		{lease.PhaseReserved, true, func(t *testing.T, p *Plugin, scope string, mac net.HardwareAddr) string {
			id := p.recordReserved(scope, mac, ident)
			lease4(t, p, id)
			return id
		}},
		{lease.PhaseCreated, true, func(t *testing.T, p *Plugin, scope string, mac net.HardwareAddr) string {
			id := p.recordCreated(scope, mac, ident)
			lease4(t, p, id)
			return id
		}},
		{lease.PhaseJoined, true, func(t *testing.T, p *Plugin, scope string, mac net.HardwareAddr) string {
			id := p.recordCreated(scope, mac, ident)
			lease4(t, p, id)
			if err := p.records.Bound(id); err != nil {
				t.Fatalf("Bound: %v", err)
			}
			return id
		}},
		{lease.PhaseLeft, true, func(t *testing.T, p *Plugin, scope string, mac net.HardwareAddr) string {
			id := p.recordCreated(scope, mac, ident)
			lease4(t, p, id)
			if err := p.records.Bound(id); err != nil {
				t.Fatalf("Bound: %v", err)
			}
			if err := p.records.Left(id); err != nil {
				t.Fatalf("Left: %v", err)
			}
			return id
		}},
		{lease.PhaseAdopted, true, func(t *testing.T, p *Plugin, scope string, mac net.HardwareAddr) string {
			id := newRecordID()
			if err := p.records.Adopted(id, scope, mac, ident); err != nil {
				t.Fatalf("Adopted: %v", err)
			}
			lease4(t, p, id)
			return id
		}},
		{lease.PhaseRetained, false, func(t *testing.T, p *Plugin, scope string, mac net.HardwareAddr) string {
			id := p.recordCreated(scope, mac, ident)
			lease4(t, p, id)
			if err := p.records.Retained(id, time.Now().Add(time.Minute)); err != nil {
				t.Fatalf("Retained: %v", err)
			}
			return id
		}},
		{lease.PhaseClosed, false, func(t *testing.T, p *Plugin, scope string, mac net.HardwareAddr) string {
			id := p.recordCreated(scope, mac, ident)
			lease4(t, p, id)
			if err := p.records.Closed(id); err != nil {
				t.Fatalf("Closed: %v", err)
			}
			return id
		}},
	}
}

const ipamPhaseAddr = "192.168.99.10/24"

// preChangeLookup is the lookup without the phase filter, kept to show it answering for dead records.
func preChangeLookup(rb lease.Rebuilt, networkID string, addr netip.Addr) (lease.Record, bool) {
	matches := rb.ByScopeAddr(networkID, addr)
	if len(matches) == 0 {
		return lease.Record{}, false
	}
	return matches[len(matches)-1], true
}

func ipamPhaseFixture(t *testing.T) (*Plugin, map[lease.Phase]string) {
	t.Helper()
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0a")
	scopes := map[lease.Phase]string{}
	for _, c := range ipamPhaseCases() {
		scope := "net-" + c.phase.String()
		id := c.build(t, p, scope, mac)
		if id == "" {
			continue
		}
		rb, err := p.records.Rebuilt()
		if err != nil {
			t.Fatalf("Rebuilt: %v", err)
		}
		var got lease.Phase
		for _, r := range rb.Records {
			if r.ID == id {
				got = r.Phase
			}
		}
		if got != c.phase {
			t.Fatalf("the fixture for %v built a record in phase %v instead; the table would "+
				"then be judging a phase it does not name", c.phase, got)
		}
		scopes[c.phase] = scope
	}
	return p, scopes
}

func TestIpamPhaseFilter_ThePreChangeLookupAnswersForDeadRecords(t *testing.T) {
	p, scopes := ipamPhaseFixture(t)
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	addr := netip.MustParsePrefix(ipamPhaseAddr).Addr()

	answeredDead := 0
	for _, c := range ipamPhaseCases() {
		scope, built := scopes[c.phase]
		if !built {
			continue
		}
		_, ok := preChangeLookup(rb, scope, addr)
		if !ok {
			t.Errorf("the pre-change lookup does not answer for %v at all; this observer no "+
				"longer reproduces the shape the filter was written for", c.phase)
			continue
		}
		if !c.answers {
			answeredDead++
		}
	}
	if answeredDead == 0 {
		t.Error("the pre-change lookup answered for no dead phase, so the filter beside it " +
			"narrows nothing and this test is measuring the wrong thing")
	}
}

func TestIpamPhaseFilter_OverEveryPhase(t *testing.T) {
	p, scopes := ipamPhaseFixture(t)
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	addr := netip.MustParsePrefix(ipamPhaseAddr).Addr()

	seen := map[lease.Phase]bool{}
	for _, c := range ipamPhaseCases() {
		t.Run(c.phase.String(), func(t *testing.T) {
			seen[c.phase] = true
			scope, built := scopes[c.phase]
			if !built {
				if ipamPhaseAnswers(c.phase) {
					t.Errorf("%v is in the answering set and no record can ever be in it "+
						"holding an address", c.phase)
				}
				return
			}
			_, ok := ipamLiveRecord(rb, scope, addr)
			if ok != c.answers {
				t.Errorf("ipamLiveRecord answers %v for a %v record, want %v", ok, c.phase, c.answers)
			}
			if got := ipamPhaseAnswers(c.phase); got != c.answers {
				t.Errorf("ipamPhaseAnswers(%v) = %v, want %v — the set and the lookup disagree", c.phase, got, c.answers)
			}
		})
	}

	for _, ph := range []lease.Phase{
		lease.PhaseUnset, lease.PhaseReserved, lease.PhaseCreated, lease.PhaseJoined,
		lease.PhaseLeft, lease.PhaseRetained, lease.PhaseAdopted, lease.PhaseClosed,
	} {
		if !seen[ph] {
			t.Errorf("%v is not in this table; the filter's classification of it is unread", ph)
		}
	}
}

func TestIpamLiveRecord_NewestWins(t *testing.T) {
	p := recordingPlugin(t)
	mac, _ := net.ParseMAC("02:42:c0:a8:63:0a")
	ident := dhcp.ClientIdentity([]byte{1})

	old := p.recordCreated("net-1", mac, ident)
	if err := p.records.Observed(old, acquired(ipamPhaseAddr, time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Retained(old, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("Retained: %v", err)
	}
	fresh := p.recordCreated("net-1", mac, ident)
	if err := p.records.Observed(fresh, acquired(ipamPhaseAddr, time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}

	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	got, ok := ipamLiveRecord(rb, "net-1", netip.MustParsePrefix(ipamPhaseAddr).Addr())
	if !ok {
		t.Fatal("no record answered for an address a live endpoint holds")
	}
	if got.ID != fresh {
		t.Errorf("answered with record %q; the live one is %q and the other is its tombstone", got.ID, fresh)
	}
}

func TestIpamPhaseFilter_TheSetIsTheLibrarysOwn(t *testing.T) {
	now := time.Now()
	for _, ph := range lease.AllPhases() {
		rec := lease.Record{
			Phase: ph,
			Held:  true,
			Lease: lease.Lease{
				Addr:   netip.MustParsePrefix(ipamPhaseAddr),
				Expire: now.Add(time.Hour),
			},
		}
		_, resumes := rec.Resume(now)
		if resumes != ipamPhaseAnswers(ph) {
			t.Errorf("%v: the library resumes it = %v, ipamRecordPhases admits it = %v.\n"+
				"The two sets are one fact and they have drifted; the address-keyed lookup "+
				"and the hardware-address guard now disagree about the same record.",
				ph, resumes, ipamPhaseAnswers(ph))
		}
	}
}
