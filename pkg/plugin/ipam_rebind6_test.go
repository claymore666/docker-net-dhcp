// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const s2Addr6 = "2001:db8::10/64"

func s2Created6(t *testing.T, p *Plugin, mac net.HardwareAddr, addr string, until time.Duration) string {
	t.Helper()
	id := p.recordCreated6(ipamTestNetwork, mac, dhcp.Identity6{DUID: append([]byte{0, 3, 0, 1}, mac...), IAID: 1})
	if id == "" {
		t.Fatal("recordCreated6 wrote nothing")
	}
	if addr != "" {
		if err := p.records.Observed(id, acquired6(addr, until), nil); err != nil {
			t.Fatalf("Observed: %v", err)
		}
	}
	return id
}

func s2Tombstone6(t *testing.T, p *Plugin, mac net.HardwareAddr, addr string) string {
	t.Helper()
	id := s2Created6(t, p, mac, addr, time.Hour)
	if err := p.records.Bound(id); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	p.recordRetained(id, time.Now().Add(tombstoneTTL))
	return id
}

func s2Lines(t *testing.T, journal string) []lease.RecordEvent {
	t.Helper()
	raw, err := os.ReadFile(journal)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	var out []lease.RecordEvent
	for _, l := range bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n")) {
		if len(l) == 0 {
			continue
		}
		var ev lease.RecordEvent
		if err := json.Unmarshal(l, &ev); err != nil {
			t.Fatalf("journal line %q: %v", l, err)
		}
		out = append(out, ev)
	}
	return out
}

type s2Line struct {
	id string
	op lease.RecordOp
}

func s2Tail(t *testing.T, journal string, from int, want ...s2Line) []lease.RecordEvent {
	t.Helper()
	tail := s2Lines(t, journal)[from:]
	var got []s2Line
	for _, ev := range tail {
		got = append(got, s2Line{ev.ID, ev.Op})
	}
	if len(got) != len(want) {
		t.Fatalf("the journal gained %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the journal gained %v, want %v", got, want)
		}
	}
	return tail
}

func s2Pair(t *testing.T, p *Plugin, id4, id6 string, mac net.HardwareAddr, phase lease.Phase) time.Time {
	t.Helper()
	r4, r6 := f0Rec(t, p, id4), f0Rec(t, p, id6)
	for _, r := range []lease.Record{r4, r6} {
		if r.Phase != phase {
			t.Fatalf("record %s (%s) is %v, want %v", r.ID, r.Scope, r.Phase, phase)
		}
		if !bytes.Equal(r.CHAddr, mac) {
			t.Fatalf("record %s (%s) is on %v, want %v: the two halves no longer share a hardware address",
				r.ID, r.Scope, net.HardwareAddr(r.CHAddr), mac)
		}
	}
	if !r4.Deadline.Equal(r6.Deadline) {
		t.Fatalf("the v4 tombstone runs to %v and the v6 one to %v: two clock reads, so one half can "+
			"expire while the other is still offered", r4.Deadline, r6.Deadline)
	}
	return r4.Deadline
}

func s2ReserveLink(t *testing.T) {
	t.Helper()
	prev := ipamAddReserveLink
	ipamAddReserveLink = func(_ *Plugin, _ context.Context, _, _, _ string, _ DHCPNetworkOptions, _ net.HardwareAddr) (func(), error) {
		return func() {}, nil
	}
	t.Cleanup(func() { ipamAddReserveLink = prev })
}

func s2Exchange(t *testing.T, answer *dhcp.Info, fail *error) {
	t.Helper()
	prev := dhcpGetIP
	dhcpGetIP = func(context.Context, string, *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		if *fail != nil {
			return dhcp.Info{}, dhcp.RAObservation{}, *fail
		}
		return *answer, dhcp.RAObservation{}, nil
	}
	t.Cleanup(func() { dhcpGetIP = prev })
}

func s2Reserve(t *testing.T, p *Plugin, b *ipamBinding, opts DHCPNetworkOptions, mac net.HardwareAddr) (*ipamReservation, error) {
	t.Helper()
	return p.ipamReserveAddress(context.Background(), ipamTestNetwork, storedNetwork{Options: opts, Binding: b}, mac, "")
}

func s2BadPolicy() DHCPNetworkOptions {
	o := f0Options()
	o.DHCPServers = "not-an-address"
	return o
}

// s2Age stands in for the clock runIPAMReserve reads: the pair is re-laid with `left` to run (#960).
func s2Age(t *testing.T, p *Plugin, left time.Duration, ids ...string) {
	t.Helper()
	deadline := time.Now().Add(left)
	for _, id := range ids {
		if err := p.records.Rebound(id, f0Rec(t, p, id).CHAddr); err != nil {
			t.Fatalf("Rebound: %v", err)
		}
		if err := p.records.Retained(id, deadline); err != nil {
			t.Fatalf("Retained: %v", err)
		}
	}
}

func TestIPAMRebind6_BothHalvesMoveTogetherThroughEveryFailedAttempt(t *testing.T) {
	m1 := f0MAC(0x01)
	p, b, sender, journal := f0Fixture(t)
	s2ReserveLink(t)
	id4 := f0Tombstone(t, p, m1, f0Addr)
	id6 := s2Tombstone6(t, p, m1, s2Addr6)
	answer, fail := dhcp.Info{IP: f0Addr, Gateway: "192.168.99.1"}, error(nil)
	s2Exchange(t, &answer, &fail)
	timeout := errors.New("no answer from the server")

	for i, a := range []struct {
		name string
		opts DHCPNetworkOptions
		age  time.Duration
	}{
		{name: "before the exchange", opts: s2BadPolicy()},
		{name: "in the exchange", opts: f0Options()},
		{name: "in the exchange at t0+50s", opts: f0Options(), age: 50 * time.Second},
	} {
		if a.age > 0 {
			s2Age(t, p, tombstoneTTL-a.age, id4, id6)
		}
		mac := f0MAC(byte(0x10 + i))
		fail = timeout
		from := len(s2Lines(t, journal))
		before := time.Now()
		if _, err := s2Reserve(t, p, b, a.opts, mac); err == nil {
			t.Fatalf("%s: the reserve succeeded", a.name)
		}
		after := time.Now()
		tail := s2Tail(t, journal, from,
			s2Line{id4, lease.OpRebind}, s2Line{id6, lease.OpRebind},
			s2Line{id4, lease.OpRetain}, s2Line{id6, lease.OpRetain})
		if !bytes.Equal(tail[0].CHAddr, mac) || !bytes.Equal(tail[1].CHAddr, mac) {
			t.Fatalf("%s: the re-bind lines carry %v and %v, want both on %v", a.name,
				net.HardwareAddr(tail[0].CHAddr), net.HardwareAddr(tail[1].CHAddr), mac)
		}
		if !tail[2].Deadline.Equal(tail[3].Deadline) {
			t.Fatalf("%s: the retain lines carry %v and %v, want one deadline", a.name, tail[2].Deadline, tail[3].Deadline)
		}
		d := s2Pair(t, p, id4, id6, mac, lease.PhaseRetained)
		if d.Before(before.Add(tombstoneTTL)) || d.After(after.Add(tombstoneTTL)) {
			t.Fatalf("%s: the pair runs to %v, want a fresh window from this attempt", a.name, d)
		}
	}

	fail = nil
	last := f0MAC(0x20)
	from := len(s2Lines(t, journal))
	res, err := s2Reserve(t, p, b, f0Options(), last)
	if err != nil {
		t.Fatalf("the reserve failed: %v", err)
	}
	s2Tail(t, journal, from, s2Line{id4, lease.OpRebind}, s2Line{id6, lease.OpRebind})
	if d := s2Pair(t, p, id4, id6, last, lease.PhaseCreated); !d.IsZero() {
		t.Fatalf("a re-bound pair still carries the deadline %v", d)
	}
	if res.record != id4 || res.record6 != id6 {
		t.Fatalf("the reservation carries (%q, %q), want (%q, %q)", res.record, res.record6, id4, id6)
	}
	kept, ok := p.ipamReserves.take(ipamReserveKey(b.PoolID, last))
	if !ok || kept.record != id4 || kept.record6 != id6 {
		t.Fatalf("CreateEndpoint would take (%v, %+v), want both ids", ok, kept)
	}
	if n := sender.callCount(); n != 0 {
		t.Errorf("%d releases went on the wire", n)
	}
}

func TestIPAMRebind6_AnotherMACsV6TombstoneIsNotPaired(t *testing.T) {
	dual, v4only, next := f0MAC(0x01), f0MAC(0x02), f0MAC(0x03)
	p, _, _, journal := f0Fixture(t)
	id4 := f0Tombstone(t, p, v4only, f0Addr)
	other6 := s2Tombstone6(t, p, dual, s2Addr6)
	before := f0Rec(t, p, other6)

	from := len(s2Lines(t, journal))
	got4, _, _, got6 := p.ipamRebindCandidate(ipamTestNetwork, next)
	if got4 != id4 || got6 != "" {
		t.Fatalf("the re-bind took (%q, %q), want (%q, none): the v6 tombstone is another endpoint's", got4, got6, id4)
	}
	s2Tail(t, journal, from, s2Line{id4, lease.OpRebind})
	if after := f0Rec(t, p, other6); after.Phase != lease.PhaseRetained || !bytes.Equal(after.CHAddr, dual) || !after.Deadline.Equal(before.Deadline) {
		t.Fatalf("the other endpoint's v6 tombstone is %v on %v to %v, want untouched", after.Phase, net.HardwareAddr(after.CHAddr), after.Deadline)
	}
}

func TestIPAMRebind6_TwoV6TombstonesOnOneMACAreAmbiguous(t *testing.T) {
	m1 := f0MAC(0x01)
	p, _, _, journal := f0Fixture(t)
	f0Tombstone(t, p, m1, f0Addr)
	s2Tombstone6(t, p, m1, s2Addr6)
	s2Tombstone6(t, p, m1, "2001:db8::11/64")

	from := len(s2Lines(t, journal))
	if got4, _, _, got6 := p.ipamRebindCandidate(ipamTestNetwork, f0MAC(0x02)); got4 != "" || got6 != "" {
		t.Fatalf("the re-bind took (%q, %q), want neither: which v6 half belongs is a guess", got4, got6)
	}
	s2Tail(t, journal, from)
}

func TestIPAMRebind6_AV4OnlyJournalIsWhatItWas(t *testing.T) {
	m1, m2 := f0MAC(0x01), f0MAC(0x02)
	p, b, _, journal := f0Fixture(t)
	s2ReserveLink(t)
	id4 := f0Tombstone(t, p, m1, f0Addr)
	answer, fail := dhcp.Info{IP: f0Addr, Gateway: "192.168.99.1"}, error(errors.New("no answer"))
	s2Exchange(t, &answer, &fail)

	from := len(s2Lines(t, journal))
	if _, err := s2Reserve(t, p, b, f0Options(), m2); err == nil {
		t.Fatal("the reserve succeeded")
	}
	s2Tail(t, journal, from, s2Line{id4, lease.OpRebind}, s2Line{id4, lease.OpRetain})

	fail = nil
	from = len(s2Lines(t, journal))
	res, err := s2Reserve(t, p, b, f0Options(), f0MAC(0x03))
	if err != nil {
		t.Fatalf("the reserve failed: %v", err)
	}
	s2Tail(t, journal, from, s2Line{id4, lease.OpRebind})
	if res.record != id4 || res.record6 != "" {
		t.Fatalf("the reservation carries (%q, %q), want (%q, none)", res.record, res.record6, id4)
	}
}

func s2Rebound(t *testing.T, p *Plugin, b *ipamBinding, mac net.HardwareAddr) (string, string) {
	t.Helper()
	s2ReserveLink(t)
	id4 := f0Tombstone(t, p, f0MAC(0x01), f0Addr)
	id6 := s2Tombstone6(t, p, f0MAC(0x01), s2Addr6)
	answer, fail := dhcp.Info{IP: f0Addr, Gateway: "192.168.99.1"}, error(nil)
	s2Exchange(t, &answer, &fail)
	res, err := s2Reserve(t, p, b, f0Options(), mac)
	if err != nil || res.record != id4 || res.record6 != id6 {
		t.Fatalf("the re-bind reserve gave (%+v, %v), want both ids", res, err)
	}
	return id4, id6
}

func TestIPAMRebind6_EveryGiveUpEndsBothHalvesOnOneDeadline(t *testing.T) {
	restarted := f0MAC(0x02)
	for _, tc := range []struct {
		name   string
		want6  lease.Phase
		want4  lease.Phase
		giveUp func(t *testing.T, p *Plugin, b *ipamBinding)
	}{
		{
			name: "the sweeper", want4: lease.PhaseRetained, want6: lease.PhaseRetained,
			giveUp: func(t *testing.T, p *Plugin, _ *ipamBinding) {
				if n := p.sweepIPAMReservations(time.Now().Add(tombstoneTTL + time.Second)); n != 1 {
					t.Fatalf("the sweep took %d reservations, want 1", n)
				}
			},
		},
		{
			name: "ReleaseAddress", want4: lease.PhaseRetained, want6: lease.PhaseRetained,
			giveUp: func(t *testing.T, p *Plugin, b *ipamBinding) {
				if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
					t.Fatalf("ReleaseAddress: %v", err)
				}
			},
		},
		{
			name: "CreateEndpoint fails after the take", want4: lease.PhaseRetained, want6: lease.PhaseRetained,
			giveUp: func(t *testing.T, p *Plugin, b *ipamBinding) {
				p.docker = f0Docker()
				if _, err := p.createIPAMEndpoint(context.Background(), time.Now(), f0CreateRequest(restarted, f0Addr), f0Options(), b); err == nil {
					t.Fatal("CreateEndpoint succeeded; this host has the bridge the fixture needs absent")
				}
			},
		},
		{
			name: "require_mac refuses the endpoint", want4: lease.PhaseRetained, want6: lease.PhaseRetained,
			giveUp: func(t *testing.T, p *Plugin, b *ipamBinding) {
				p.ipamDropRefusedReservation(f0CreateRequest(restarted, f0Addr), b)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, sender, journal := f0Fixture(t)
			id4, id6 := s2Rebound(t, p, b, restarted)
			from := len(s2Lines(t, journal))
			before := time.Now()

			tc.giveUp(t, p, b)

			tail := s2Lines(t, journal)[from:]
			var end4, end6 *lease.RecordEvent
			for i := range tail {
				switch {
				case tail[i].ID == id4 && (tail[i].Op == lease.OpRetain || tail[i].Op == lease.OpClose):
					end4 = &tail[i]
				case tail[i].ID == id6 && (tail[i].Op == lease.OpRetain || tail[i].Op == lease.OpClose):
					end6 = &tail[i]
				}
			}
			if end4 == nil || end6 == nil {
				t.Fatalf("the give-up wrote v4=%v v6=%v, want both halves ended", end4 != nil, end6 != nil)
			}
			if r := f0Rec(t, p, id4); r.Phase != tc.want4 {
				t.Fatalf("the v4 record is %v, want %v", r.Phase, tc.want4)
			}
			if r := f0Rec(t, p, id6); r.Phase != tc.want6 {
				t.Fatalf("the v6 record is %v, want %v", r.Phase, tc.want6)
			}
			if tc.want4 == lease.PhaseRetained {
				d := s2Pair(t, p, id4, id6, restarted, lease.PhaseRetained)
				if !end4.Deadline.Equal(end6.Deadline) || d.Before(before) {
					t.Fatalf("the journal carries %v and %v, want one fresh deadline", end4.Deadline, end6.Deadline)
				}
			}
			if n := sender.callCount(); n != 0 {
				t.Errorf("%d releases went on the wire", n)
			}
		})
	}
}

func TestIPAMRebind6_TheReserveExitsEndBothHalves(t *testing.T) {
	m1, mine := f0MAC(0x01), f0MAC(0x02)
	for _, tc := range []struct {
		name         string
		opts         DHCPNetworkOptions
		ack          string
		want4, want6 lease.Phase
	}{
		{name: "the conflict check cannot be read", opts: func() DHCPNetworkOptions {
			o := f0Options()
			o.ConflictCheck = "sometimes"
			return o
		}(), ack: f0Addr, want4: lease.PhaseRetained, want6: lease.PhaseRetained},
		{name: "the ACK is refused", opts: f0Options(), ack: "10.9.9.9/24", want4: lease.PhaseClosed, want6: lease.PhaseRetained},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, sender, journal := f0Fixture(t)
			s2ReserveLink(t)
			id4 := f0Tombstone(t, p, m1, f0Addr)
			id6 := s2Tombstone6(t, p, m1, s2Addr6)
			answer, fail := dhcp.Info{IP: tc.ack}, error(nil)
			s2Exchange(t, &answer, &fail)

			from := len(s2Lines(t, journal))
			if _, err := s2Reserve(t, p, b, tc.opts, mine); err == nil {
				t.Fatal("the reserve succeeded")
			}
			end4, end6 := lease.OpRetain, lease.OpRetain
			if tc.want4 == lease.PhaseClosed {
				end4 = lease.OpClose
			}
			tail := s2Tail(t, journal, from,
				s2Line{id4, lease.OpRebind}, s2Line{id6, lease.OpRebind}, s2Line{id4, end4}, s2Line{id6, end6})
			if r := f0Rec(t, p, id4); r.Phase != tc.want4 {
				t.Fatalf("the v4 record is %v, want %v", r.Phase, tc.want4)
			}
			if tc.want4 == lease.PhaseRetained {
				s2Pair(t, p, id4, id6, mine, lease.PhaseRetained)
			} else if r := f0Rec(t, p, id6); r.Phase != tc.want6 || !tail[3].Deadline.After(time.Now()) {
				t.Fatalf("the v6 record is %v to %v, want retained: its lease was never refused", r.Phase, r.Deadline)
			}
			if n := sender.callCount(); n != 0 {
				t.Errorf("%d releases went on the wire", n)
			}
		})
	}
}

func TestIPAMRebind6_TheRestartRuleReadsBothScopes(t *testing.T) {
	stranded, running, lone, split := f0MAC(0x02), f0MAC(0x03), f0MAC(0x04), f0MAC(0x05)
	p, b, _, journal := f0Fixture(t)
	id4, id6 := s2Rebound(t, p, b, stranded)
	run4 := f0Created(t, p, running, f0Addr2)
	run6 := s2Created6(t, p, running, "2001:db8::13/64", time.Hour)
	lone6 := s2Created6(t, p, lone, "2001:db8::14/64", time.Hour)
	split4 := f0Created(t, p, split, "192.168.99.12/24")

	f0Reopen(t, p, journal, "proc-2")
	split6 := s2Created6(t, p, split, "2001:db8::15/64", time.Hour)
	from := len(s2Lines(t, journal))
	now := time.Now()

	if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, []net.HardwareAddr{running}, now); n != 4 {
		t.Fatalf("the rule gave up %d records, want 4: the stranded pair, the lone v6 record and the split v4 one", n)
	}
	tail := s2Tail(t, journal, from,
		s2Line{id4, lease.OpRetain}, s2Line{id6, lease.OpRetain},
		s2Line{split4, lease.OpRetain}, s2Line{lone6, lease.OpRetain})
	if !tail[0].Deadline.Equal(tail[1].Deadline) || !tail[0].Deadline.Equal(now.Add(tombstoneTTL)) {
		t.Fatalf("the stranded pair was retained to %v and %v, want both %v", tail[0].Deadline, tail[1].Deadline, now.Add(tombstoneTTL))
	}
	s2Pair(t, p, id4, id6, stranded, lease.PhaseRetained)
	s2Pair(t, p, run4, run6, running, lease.PhaseCreated)
	if r := f0Rec(t, p, split6); r.Phase != lease.PhaseCreated {
		t.Fatalf("the v6 record this process wrote is %v, want created: it is a start in flight here", r.Phase)
	}

	from = len(s2Lines(t, journal))
	if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, []net.HardwareAddr{running}, now); n != 0 {
		t.Fatalf("a second run gave up %d records, want 0", n)
	}
	s2Tail(t, journal, from)
}

func TestIPAMRebind6_ReleaseAddressAfterTheEndpointTombstoneWritesNothing(t *testing.T) {
	restarted := f0MAC(0x02)
	p, b, _, journal := f0Fixture(t)
	id4, id6 := s2Rebound(t, p, b, restarted)

	from := len(s2Lines(t, journal))
	p.retainRecordFor(ipamTestNetwork, restarted)
	s2Tail(t, journal, from, s2Line{id4, lease.OpRetain}, s2Line{id6, lease.OpRetain})
	d := s2Pair(t, p, id4, id6, restarted, lease.PhaseRetained)

	from = len(s2Lines(t, journal))
	if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
		t.Fatalf("ReleaseAddress: %v", err)
	}
	s2Tail(t, journal, from)
	if got := s2Pair(t, p, id4, id6, restarted, lease.PhaseRetained); !got.Equal(d) {
		t.Fatalf("the pair's deadline moved from %v to %v", d, got)
	}
}

func TestIPAMRebind6_ReleaseAddressWithNoPoolLeavesThePairAlone(t *testing.T) {
	restarted := f0MAC(0x02)
	p, b, _, journal := f0Fixture(t)
	id4, id6 := s2Rebound(t, p, b, restarted)

	from := len(s2Lines(t, journal))
	if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: "", Address: "2001:db8::10"}); err != nil {
		t.Fatalf("ReleaseAddress: %v", err)
	}
	s2Tail(t, journal, from)
	s2Pair(t, p, id4, id6, restarted, lease.PhaseCreated)
}

func s2Expired(t *testing.T, p *Plugin, mac net.HardwareAddr) (string, string) {
	t.Helper()
	id4 := p.recordCreated(ipamTestNetwork, mac, dhcp.ClientIdentity(mac))
	if err := p.records.Observed(id4, acquired(f0Addr, -time.Minute), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	id6 := s2Created6(t, p, mac, s2Addr6, -time.Minute)
	deadline := time.Now().Add(tombstoneTTL)
	for _, id := range []string{id4, id6} {
		if err := p.records.Bound(id); err != nil {
			t.Fatalf("Bound: %v", err)
		}
		p.recordRetained(id, deadline)
	}
	return id4, id6
}

func TestIPAMRebind6_GapA_AnExpiredReBoundLeaseIsClosedAndNeverReleased(t *testing.T) {
	first, restarted := f0MAC(0x01), f0MAC(0x02)
	p, b, sender, journal := f0Fixture(t)
	id4, id6 := s2Expired(t, p, first)
	if got4, _, _, got6 := p.ipamRebindCandidate(ipamTestNetwork, restarted); got4 != id4 || got6 != id6 {
		t.Fatalf("the re-bind took (%q, %q), want (%q, %q)", got4, got6, id4, id6)
	}
	f0Reservation(p, b, restarted, id4, f0Addr, nil)
	now := time.Now()

	p.docker = f0Docker()
	if _, err := p.createIPAMEndpoint(context.Background(), time.Now(), f0CreateRequest(restarted, f0Addr), f0Options(), b); err == nil {
		t.Fatal("CreateEndpoint succeeded; this host has the bridge the fixture needs absent")
	}
	s2Pair(t, p, id4, id6, restarted, lease.PhaseClosed)
	if n := f0Tombstones(t, p, now); n != 0 {
		t.Errorf("tombstones = %d, want 0", n)
	}
	if n := p.sweepDeferredReleases(now.Add(tombstoneTTL + releaseSettle)); n != 0 || sender.callCount() != 0 {
		t.Errorf("the sweep handed back %d and sent %d, want neither: the leases expired at the server", n, sender.callCount())
	}
	f0Reopen(t, p, journal, "proc-2")
	if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, nil, time.Now()); n != 0 {
		t.Errorf("the restart rule found %d stranded records, want 0", n)
	}
}

func TestIPAMRebind6_GapA_KeyingOnTheReboundMarkWouldReleaseAnExpiredLease(t *testing.T) {
	first, restarted := f0MAC(0x01), f0MAC(0x02)
	p, _, sender, _ := f0Fixture(t)
	id4, id6 := s2Expired(t, p, first)
	if got4, _, _, _ := p.ipamRebindCandidate(ipamTestNetwork, restarted); got4 != id4 {
		t.Fatalf("the re-bind took %q, want %q", got4, id4)
	}
	now := time.Now()

	p.ipamGiveUpAttempt(id4, true, now)

	s2Pair(t, p, id4, id6, restarted, lease.PhaseRetained)
	n := p.sweepDeferredReleases(now.Add(tombstoneTTL + releaseSettle))
	t.Logf("retained on the rebound mark: swept=%d sent=%d", n, sender.callCount())
	if n == 0 || sender.callCount() == 0 {
		t.Fatalf("the sweep handed back %d and sent %d; this test measures that a retained expired lease "+
			"goes on the wire, and it no longer does", n, sender.callCount())
	}
}

func TestIPAMRebind6_GapB_ASecondProcessCannotRunTheRuleOnALiveJournal(t *testing.T) {
	running, pending := f0MAC(0x01), f0MAC(0x02)
	p, _, _, journal := f0Fixture(t)
	run4 := f0Created(t, p, running, f0Addr)
	run6 := s2Created6(t, p, running, s2Addr6, time.Hour)
	pend4 := f0Created(t, p, pending, f0Addr2)

	second, err := dhcp.OpenRecords(journal, "proc-2")
	if err == nil {
		_ = second.Close()
		t.Fatal("a second process opened the journal while the first holds it")
	}
	if !errors.Is(err, dhcp.ErrRecordsLocked) {
		t.Fatalf("the second open failed with %v, want ErrRecordsLocked", err)
	}

	f0Reopen(t, p, journal, "proc-2")
	from := len(s2Lines(t, journal))
	if n := p.giveUpStrandedIPAMRecords(ipamTestNetwork, []net.HardwareAddr{running}, time.Now()); n != 1 {
		t.Fatalf("the rule gave up %d records, want 1", n)
	}
	s2Tail(t, journal, from, s2Line{pend4, lease.OpRetain})
	s2Pair(t, p, run4, run6, running, lease.PhaseCreated)
}

func TestIPAMRebind6_EveryGiveUpGoesThroughTheOneCore(t *testing.T) {
	const core = "func (p *Plugin) ipamEndRecord("
	for _, file := range []string{"ipam_reserve.go", "ipam.go", "ipam_endpoint.go", "require_mac.go"} {
		body, err := os.ReadFile(filepath.Join(".", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, fn := range strings.Split(string(body), "\nfunc ") {
			if strings.HasPrefix("func "+fn, core) {
				continue
			}
			for _, call := range []string{"p.recordRetained(", "p.closeRecord("} {
				if strings.Contains(fn, call) {
					t.Errorf("%s calls %s outside ipamEndRecord, in %q: that exit ends one family and "+
						"leaves the other half of the endpoint behind", file, call, strings.SplitN(fn, "\n", 2)[0])
				}
			}
		}
	}
}

func TestIPAMRebind6_AnEndedV6HalfIsNotEndedAgain(t *testing.T) {
	restarted := f0MAC(0x02)
	p, b, _, journal := f0Fixture(t)
	id4, id6 := s2Rebound(t, p, b, restarted)
	if err := p.records.Closed(id6); err != nil {
		t.Fatalf("Closed: %v", err)
	}

	from := len(s2Lines(t, journal))
	if err := p.ReleaseAddress(ReleaseAddressRequest{PoolID: b.PoolID, Address: "192.168.99.10"}); err != nil {
		t.Fatalf("ReleaseAddress: %v", err)
	}
	s2Tail(t, journal, from, s2Line{id4, lease.OpRetain})
}
