// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// ipamPhaseCase is one record in one phase, all holding the same
// address in a network of their own, so a lookup keyed on (scope,
// address) sees exactly one candidate per case.
type ipamPhaseCase struct {
	phase lease.Phase
	// answers says whether a RequestAddress replay may be answered from
	// a record in this phase.
	answers bool
	// build writes the events that put a record in this phase and
	// returns its id, or "" when the phase cannot hold an address at all.
	build func(t *testing.T, p *Plugin, scope string, mac net.HardwareAddr) string
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
			// No events at all. There is no record, so there is nothing
			// to hold an address: the case exists so the table covers
			// all eight phases rather than the seven that are reachable.
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

// preChangeLookup is the lookup as it stood before this change: the
// library's index by scope and address, with the caller applying no
// phase test at all.
//
// IT IS HERE TO BE RUN, not to document. The design claimed the lookup
// needed narrowing; the claim is only worth anything if the unnarrowed
// version is shown answering for a phase nothing holds. Deleting this
// function and its test is deleting the evidence for the filter beside
// it.
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

// TestIpamPhaseFilter_ThePreChangeLookupAnswersForDeadRecords is the
// observer, run against the lookup as it was.
//
// A CLOSED record is the phase CreateEndpoint's failure path writes, and
// a RETAINED one is a tombstone whose address the re-bind branch hands
// out through its own deadline. The unnarrowed lookup answers for both.
// That is what makes a daemon-restart replay of a closed endpoint count
// as a replay HIT: ipam_replay_miss, whose whole job is to make that
// visible, never moves, and the endpoint comes back attached to an
// address nothing holds.
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

// TestIpamPhaseFilter_OverEveryPhase is the same table against the
// lookup that ships. Both directions: a phase that must answer and does
// not loses a container its address at a restart, and a phase that must
// not answer and does hands one out that nothing holds.
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
				// The unreachable phase. Nothing holds an address in it,
				// so the assertion is that the filter says so anyway.
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

	// The non-vacuity guard: eight phases, every one of them judged. A
	// phase added to the library and not to this table would otherwise
	// ship with no classification at all.
	for _, ph := range []lease.Phase{
		lease.PhaseUnset, lease.PhaseReserved, lease.PhaseCreated, lease.PhaseJoined,
		lease.PhaseLeft, lease.PhaseRetained, lease.PhaseAdopted, lease.PhaseClosed,
	} {
		if !seen[ph] {
			t.Errorf("%v is not in this table; the filter's classification of it is unread", ph)
		}
	}
}

// TestIpamLiveRecord_NewestWins. A tombstone and the record that
// succeeded it share one address, and Rebuild returns records in
// creation order, so the last answering match is the current one.
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

// TestIpamPhaseFilter_TheSetIsTheLibrarysOwn couples the two
// derivations of one fact, because the looser of the two decides.
//
// ipamRecordPhases and lease.Record.Resume answer the same question --
// which phases can still hold a lease worth claiming -- and the MAC
// guard now asks the library rather than re-deriving it. That leaves
// ipamRecordPhases as the address-keyed half's own copy, and a copy
// that drifts is the defect this test exists for: a phase admitted here
// and refused there would make an address replay answer from a record
// the duplicate guard reads as holding nothing.
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
