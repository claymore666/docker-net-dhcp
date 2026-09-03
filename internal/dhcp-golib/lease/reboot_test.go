package lease

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// Ring 2's half of INIT-REBOOT: the wall-clock lease a previous run left
// behind, converted once into the monotonic one ring 1 works in, and the
// report that says what the client asked for.

const rebootAddr = "192.168.99.77" // deliberately not testYIAddr

// rememberedLease is what a record hands back: wall-clock deadlines, a
// gateway, a server identifier — all the fields Config.Resume deliberately
// ignores except two.
func rememberedLease(clk *fakeClock, addr string, in time.Duration) Lease {
	l := Lease{
		Addr:     netip.MustParsePrefix(addr + "/24"),
		Gateway:  netip.MustParseAddr(testServerID),
		ServerID: netip.MustParseAddr(testServerID),
		Acquired: clk.Wall(),
	}
	if in != 0 {
		l.Expire = clk.Wall().Add(in)
	}
	return l
}

func firstSent(t *testing.T, r *rig) *wire.Message {
	t.Helper()
	sent := r.server.sentMessages()
	if len(sent) == 0 {
		t.Fatal("nothing was sent")
	}
	return sent[0]
}

// TestARememberedLeaseSendsAnInitRebootRequest is the ring-2 end of P-3: a
// Config.Resume, and the FIRST thing on the wire is a DHCPREQUEST for that
// address rather than a DHCPDISCOVER.
func TestARememberedLeaseSendsAnInitRebootRequest(t *testing.T) {
	r := newRig(t, testParams(), answerTheRequestedAddress, Fault{},
		withResume(Lease{
			Addr:     netip.MustParsePrefix(rebootAddr + "/24"),
			ServerID: netip.MustParseAddr(testServerID),
			Expire:   newFakeClock().Wall().Add(time.Hour),
		}))

	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("first event %s, want acquired", ev)
	}
	if got := ev.Lease.Addr.Addr().String(); got != rebootAddr {
		t.Fatalf("bound to %s, want the remembered %s", got, rebootAddr)
	}
	if ev.Requested.String() != rebootAddr {
		t.Fatalf("Event.Requested = %s, want %s", ev.Requested, rebootAddr)
	}

	sent := r.server.sentMessages()
	for _, m := range sent {
		if ty, _ := m.Type(); ty == wire.MsgDiscover {
			t.Fatalf("a DHCPDISCOVER was sent: %v", sent)
		}
	}
	first := firstSent(t, r)
	if ty, _ := first.Type(); ty != wire.MsgRequest {
		t.Fatalf("first message is a %s, want DHCPREQUEST", ty)
	}
	if v, ok := first.Addr4(wire.OptRequestedIP); !ok || v.String() != rebootAddr {
		t.Fatalf("option 50 = %v/%v, want %s", v, ok, rebootAddr)
	}
	// The remembered lease NAMED a server. Passing that on would make section
	// 4.3.2 read this as a SELECTING request.
	if v, ok := first.Addr4(wire.OptServerID); ok {
		t.Fatalf("the remembered lease's server identifier reached the wire: %s", v)
	}
	if first.CIAddr.IsValid() && !first.CIAddr.IsUnspecified() {
		t.Fatalf("ciaddr = %s, want zero", first.CIAddr)
	}
}

// TestARememberedLeaseIsConvertedThroughTheClockBridge is the crossing itself.
//
// The record stores WALL-CLOCK deadlines because a monotonic epoch means
// nothing to the next process; ring 1 compares against a monotonic Instant
// because it cannot import time. This is the one conversion, and it is
// measured by moving the two clocks apart: a lease that expires in an hour of
// wall time must still reboot when the monotonic clock is nowhere near the
// wall clock's numeric value, and a lease that expired an hour ago must not.
func TestARememberedLeaseIsConvertedThroughTheClockBridge(t *testing.T) {
	for _, c := range []struct {
		what   string
		in     time.Duration
		reboot bool
	}{
		{"an hour left", time.Hour, true},
		{"a second left", time.Second, true},
		{"infinite (a zero Expire)", 0, true},
		{"expired an hour ago", -time.Hour, false},
		{"expired a nanosecond ago", -1, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			clk := newFakeClock()
			// Move the monotonic clock away from zero, so a conversion that
			// forgot the bridge and used the raw wall nanoseconds — or the
			// raw monotonic ones — lands in a different century.
			clk.advance(90 * proto.Minute)

			r := newRigOn(t, clk, testParams(), answerTheRequestedAddress, Fault{},
				withResume(rememberedLease(clk, rebootAddr, c.in)))

			ev := r.nextEvent(t)
			if ev.Kind != Acquired {
				t.Fatalf("first event %s, want acquired", ev)
			}
			ty, _ := firstSent(t, r).Type()
			want := wire.MsgDiscover
			if c.reboot {
				want = wire.MsgRequest
			}
			if ty != want {
				t.Fatalf("first message is a %s, want %s", ty, want)
			}
		})
	}
}

// TestNewManagerRefusesTwoResumes. One fact, two derivations, and the looser
// one would decide — see ErrResumeTwice.
func TestNewManagerRefusesTwoResumes(t *testing.T) {
	p := testParams()
	p.Resume = &proto.Resume{Addr: netip.MustParseAddr(rebootAddr)}
	_, err := NewManager(Config{
		Params: p, Transport: newFakeServer(answerNormally), Clock: newFakeClock(),
		Timers: newFakeTimers(), Entropy: &fakeEntropy{},
		Resume: &Lease{Addr: netip.MustParsePrefix(rebootAddr + "/24")},
	})
	if err != ErrResumeTwice {
		t.Fatalf("err = %v, want ErrResumeTwice", err)
	}
	// The control: EITHER alone is accepted, so the refusal is about the pair
	// and not about the field.
	for _, c := range []struct {
		what string
		cfg  func(*Config)
	}{
		{"Params.Resume alone", func(c *Config) { c.Params.Resume = &proto.Resume{Addr: netip.MustParseAddr(rebootAddr)} }},
		{"Config.Resume alone", func(c *Config) { c.Resume = &Lease{Addr: netip.MustParsePrefix(rebootAddr + "/24")} }},
	} {
		cfg := Config{
			Params: testParams(), Transport: newFakeServer(answerNormally), Clock: newFakeClock(),
			Timers: newFakeTimers(), Entropy: &fakeEntropy{},
		}
		c.cfg(&cfg)
		if _, err := NewManager(cfg); err != nil {
			t.Errorf("%s: %v", c.what, err)
		}
	}
}

// TestNewManagerRefusesAResumeThatCannotFillOption50. Refused rather than
// ignored: silently dropping it gives the caller the DHCPDISCOVER it was
// trying to replace, with nothing to read that says why.
func TestNewManagerRefusesAResumeThatCannotFillOption50(t *testing.T) {
	for _, c := range []struct {
		what string
		l    Lease
	}{
		{"an empty Lease", Lease{}},
		{"0.0.0.0", Lease{Addr: netip.MustParsePrefix("0.0.0.0/0")}},
		{"an IPv6 prefix", Lease{Addr: netip.MustParsePrefix("2001:db8::1/64")}},
	} {
		_, err := NewManager(Config{
			Params: testParams(), Transport: newFakeServer(answerNormally), Clock: newFakeClock(),
			Timers: newFakeTimers(), Entropy: &fakeEntropy{}, Resume: &c.l,
		})
		if err != ErrResumeNoAddr {
			t.Errorf("%s: err = %v, want ErrResumeNoAddr", c.what, err)
		}
	}
}

// TestTheAcquiredEventReportsASubstitutedAddress.
//
// RFC 2131 section 4.4.2 accepts a DHCPACK for an INIT-REBOOT request "from
// any server" on the xid alone, so a server is free to answer with a different
// address and a conforming client takes it. The chassis is the one that
// decides whether the container can live with that, and it can only decide if
// it is told; this is the telling.
func TestTheAcquiredEventReportsASubstitutedAddress(t *testing.T) {
	// answerNormally always ACKs testYIAddr, whatever was asked for.
	r := newRig(t, testParams(), answerNormally, Fault{},
		withResume(Lease{
			Addr:   netip.MustParsePrefix(rebootAddr + "/24"),
			Expire: newFakeClock().Wall().Add(time.Hour),
		}))

	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("first event %s, want acquired", ev)
	}
	if ev.Lease.Addr.Addr().String() != testYIAddr {
		t.Fatalf("this fixture no longer substitutes: bound to %s", ev.Lease.Addr)
	}
	if ev.Requested.String() != rebootAddr {
		t.Fatalf("Event.Requested = %s, want the address asked for, %s", ev.Requested, rebootAddr)
	}
	// The comparison a chassis makes, spelled out here so that a change to
	// either field breaks this rather than the chassis.
	if !(ev.Requested.IsValid() && ev.Requested != ev.Lease.Addr.Addr()) {
		t.Fatal("a chassis comparing Requested against the lease cannot see the substitution")
	}
	if !strings.Contains(ev.String(), "asked for") {
		t.Fatalf("the rendered event hides it: %s", ev)
	}
}

// TestARequestedIPIsReportedToo: the same report on the INIT path, which is
// where the plugin's `ip` option lands (Params.RequestedIP, option 50 in the
// DHCPDISCOVER — RFC 2131 section 4.4.1 makes it a MAY).
func TestARequestedIPIsReportedToo(t *testing.T) {
	p := testParams()
	p.RequestedIP = netip.MustParseAddr(rebootAddr)
	r := newRig(t, p, answerNormally, Fault{})

	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("first event %s, want acquired", ev)
	}
	if ev.Requested != p.RequestedIP {
		t.Fatalf("Event.Requested = %s, want %s", ev.Requested, p.RequestedIP)
	}
	first := firstSent(t, r)
	if ty, _ := first.Type(); ty != wire.MsgDiscover {
		t.Fatalf("first message is a %s; RequestedIP is not INIT-REBOOT", ty)
	}
	if v, ok := first.Addr4(wire.OptRequestedIP); !ok || v != p.RequestedIP {
		t.Fatalf("the DHCPDISCOVER's option 50 = %v/%v, want %s", v, ok, p.RequestedIP)
	}
}

// TestAnOrdinaryAcquisitionReportsNoRequest is the preservation control for
// both of the above: a client that asked for nothing must report nothing, not
// the address it was given.
func TestAnOrdinaryAcquisitionReportsNoRequest(t *testing.T) {
	r := newRig(t, testParams(), answerNormally, Fault{})
	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("first event %s, want acquired", ev)
	}
	if ev.Requested.IsValid() {
		t.Fatalf("Event.Requested = %s on an acquisition that asked for nothing", ev.Requested)
	}
	if strings.Contains(ev.String(), "asked for") {
		t.Fatalf("the rendered event invents a request: %s", ev)
	}
}

// ------------------------------------------------------- the record side --

// TestPreferAndResumeAreDisjointAndTotal is the pair's contract, over the
// DERIVED phase set rather than a list typed out here: a record either
// believes its lease is live and CLAIMS the address (INIT-REBOOT), or does not
// and ASKS for it (option 50 in a DHCPDISCOVER). Never both, and never neither
// while it holds a usable address in a phase that still exists.
//
// Written as a product so that adding a phase makes this incomplete rather
// than leaving it quietly passing on the phases that were there before.
func TestPreferAndResumeAreDisjointAndTotal(t *testing.T) {
	phases := AllPhases()
	if len(phases) != 8 {
		t.Fatalf("the domain is %d phase(s), want 8; a constant moved", len(phases))
	}

	live := testRecordLease("192.168.99.100/24")
	dead := testRecordLease("192.168.99.100/24")
	dead.Expire = testNow.Add(-time.Second)
	forever := testRecordLease("192.168.99.100/24")
	forever.Expire = time.Time{}

	var resumes, prefers int
	for _, phase := range phases {
		for _, c := range []struct {
			what  string
			lease Lease
			held  bool
		}{
			{"a live lease", live, true},
			{"a lease that expired a second ago", dead, true},
			{"an infinite lease", forever, true},
			{"a lease that is not held", live, false},
		} {
			t.Run(phase.String()+"/"+c.what, func(t *testing.T) {
				rec := recordAt(t, phase)
				rec.Lease, rec.Held = c.lease, c.held

				_, canResume := rec.Resume(testNow)
				addr, canPrefer := rec.Prefer(testNow)
				if canResume && canPrefer {
					t.Fatal("both: the same address would be claimed and asked for at once")
				}
				if canResume {
					resumes++
				}
				if canPrefer {
					prefers++
					if addr.String() != "192.168.99.100" {
						t.Fatalf("Prefer = %s, want the record's own address", addr)
					}
				}

				// Totality: a record that owns an address, in a phase that
				// still exists, must answer one of the two.
				owns := phase != PhaseUnset && phase != PhaseClosed
				if owns && !canResume && !canPrefer {
					t.Fatalf("neither: %s holds %s and would ask for nothing", phase, rec.Lease.Addr)
				}
				if !owns && (canResume || canPrefer) {
					t.Fatalf("%s answered resume=%v prefer=%v; it holds nothing", phase, canResume, canPrefer)
				}
			})
		}
	}
	// Both halves must actually have fired, or the disjointness above is a
	// statement about an empty set.
	if resumes == 0 || prefers == 0 {
		t.Fatalf("resume fired %d time(s) and prefer %d; one of the two is unreachable and this table proves nothing", resumes, prefers)
	}
}

// TestATombstoneAsksRatherThanClaims is the note's section 4 rule, alone.
//
// A RETAINED record's address was GIVEN UP. The server may have handed it to
// someone else, and RFC 2131 section 4.3.2 has a server with no record of the
// client answer an INIT-REBOOT DHCPREQUEST with silence — so claiming it costs
// a retransmission budget before the DHCPDISCOVER that should have gone first.
func TestATombstoneAsksRatherThanClaims(t *testing.T) {
	rec := recordAt(t, PhaseRetained)
	rec.Lease, rec.Held = testRecordLease("192.168.99.100/24"), true

	if _, ok := rec.Resume(testNow); ok {
		t.Fatal("a tombstone offered its address as an INIT-REBOOT claim")
	}
	addr, ok := rec.Prefer(testNow)
	if !ok {
		t.Fatal("a tombstone offered nothing at all; the address is a preference, not a secret")
	}
	if addr.String() != "192.168.99.100" {
		t.Fatalf("Prefer = %s", addr)
	}

	// The control: the same record BEFORE the tombstone claims it.
	joined := recordAt(t, PhaseJoined)
	joined.Lease, joined.Held = testRecordLease("192.168.99.100/24"), true
	if _, ok := joined.Resume(testNow); !ok {
		t.Fatal("a joined record with a live lease no longer resumes, so the RETAINED refusal above measures nothing")
	}
}

// TestSnapshotParamsClonesTheResume. Params is a value with four slices and
// now ONE POINTER; a shallow copy leaves the record saying what the caller's
// memory holds now rather than what the manager ran with, and a replay then
// replays a configuration that never ran.
func TestSnapshotParamsClonesTheResume(t *testing.T) {
	p := proto.DefaultParams(testMAC)
	p.Resume = &proto.Resume{Addr: netip.MustParseAddr("192.168.99.100"), Expire: 42, HasExpire: true}

	snap := SnapshotParams(p)
	if snap.Resume == p.Resume {
		t.Fatal("the snapshot shares the caller's Resume pointer")
	}
	p.Resume.Addr = netip.MustParseAddr("192.168.99.200")
	p.Resume.HasExpire = false
	if snap.Resume.Addr.String() != "192.168.99.100" || !snap.Resume.HasExpire {
		t.Fatalf("the caller moved the snapshot: %+v", *snap.Resume)
	}

	// A nil Resume stays nil rather than becoming a pointer to a zero value,
	// which would make every snapshotted Params look like it carried one.
	if got := SnapshotParams(proto.DefaultParams(testMAC)); got.Resume != nil {
		t.Fatalf("SnapshotParams invented a Resume: %+v", *got.Resume)
	}
}

// TestTheAdoptThenJoinSequenceConfirmsAtJoin is the IPAM path's shape, end to
// end across the two rings: an address is taken over with NO WIRE at all
// (OpAdopt is a fold, and a fold has no transport), and the confirmation that
// it is really this client's happens at Join, as an INIT-REBOOT DHCPREQUEST.
//
// The point of splitting it that way is that the address is known — and can be
// answered to the container runtime — before any server has been asked.
func TestTheAdoptThenJoinSequenceConfirmsAtJoin(t *testing.T) {
	adopted := testRecordLease(rebootAddr + "/24")
	adopted.Expire = testNow.Add(time.Hour)

	var rec Record
	for i, ev := range []RecordEvent{
		{Op: OpAdopt, Scope: "net-a", Family: FamilyV4, CHAddr: testMAC, Identity: testIdentity},
		{Op: OpLease, Kind: Acquired, Lease: &adopted},
		{Op: OpBind},
	} {
		ev.ID, ev.Seq = "rec-1", uint64(i+1)
		next, err := Fold(rec, ev)
		if err != nil {
			t.Fatalf("event %d (%s): %v", i+1, ev.Op, err)
		}
		rec = next
	}
	if rec.Phase != PhaseJoined {
		t.Fatalf("phase = %s, want joined", rec.Phase)
	}

	resume, ok := rec.Resume(testNow)
	if !ok {
		t.Fatal("the adopted record offers nothing to confirm")
	}
	if _, ok := rec.Prefer(testNow); ok {
		t.Fatal("the adopted record wants to ask AND to claim")
	}

	// The clock the manager runs on is the one whose wall time the record's
	// deadline is expressed in.
	clk := newFakeClock()
	clk.wall = testNow
	r := newRigOn(t, clk, testParams(), answerTheRequestedAddress, Fault{}, withResume(resume))

	ev := r.nextEvent(t)
	if ev.Kind != Acquired || ev.Lease.Addr.Addr().String() != rebootAddr {
		t.Fatalf("first event %s, want the adopted address confirmed", ev)
	}
	first := firstSent(t, r)
	if ty, _ := first.Type(); ty != wire.MsgRequest {
		t.Fatalf("the confirmation at Join is a %s, want DHCPREQUEST", ty)
	}
	if v, ok := first.Addr4(wire.OptRequestedIP); !ok || v.String() != rebootAddr {
		t.Fatalf("option 50 = %v/%v, want the adopted %s", v, ok, rebootAddr)
	}
	for _, m := range r.server.sentMessages() {
		if ty, _ := m.Type(); ty == wire.MsgDiscover {
			t.Fatal("a DHCPDISCOVER was sent; the adopted address was not confirmed, it was re-acquired")
		}
	}
}

// TestNeitherHalfOffersAnAddressThatOption50CannotCarry is the survivor M5-af
// found: Prefer's own family guard had no test, because every record fixture
// in this package holds an IPv4 lease.
//
// Option 50 is four bytes. An address that is not IPv4 cannot be put in one,
// and a record is not the place that discovers it.
//
// THE TWO HALVES ARE NOT SYMMETRIC, and this test says so rather than hiding
// it. Prefer refuses such an address itself. Resume does NOT — it hands the
// whole Lease back, and the refusal happens one ring later, at NewManager,
// as ErrResumeNoAddr. Both fail closed and neither can reach the wire; the
// asymmetry is recorded here so that a later round changes it deliberately
// rather than by accident.
func TestNeitherHalfOffersAnAddressThatOption50CannotCarry(t *testing.T) {
	for _, c := range []struct {
		what string
		pfx  string
	}{
		{"an IPv6 lease", "2001:db8::1/64"},
		{"the unspecified address", "0.0.0.0/0"},
	} {
		t.Run(c.what, func(t *testing.T) {
			rec := recordAt(t, PhaseRetained)
			rec.Lease, rec.Held = testRecordLease(c.pfx), true

			if addr, ok := rec.Prefer(testNow); ok {
				t.Fatalf("Prefer offered %s, which cannot fill a four-byte option 50", addr)
			}

			// The other half, at the ring boundary: a joined record does hand
			// such a lease back, and the manager is what refuses it.
			joined := recordAt(t, PhaseJoined)
			joined.Lease, joined.Held = testRecordLease(c.pfx), true
			// Asserted, not tolerated: this is the CURRENT answer, pinned so
			// that changing it is a deliberate edit here rather than a silent
			// drift. A round that gives Resume the same guard makes this line
			// fail, and the comment above is what it should update.
			resume, ok := joined.Resume(testNow)
			if !ok {
				t.Fatal("Resume has grown a family guard of its own: update the asymmetry this test documents, above")
			}
			_, err := NewManager(Config{
				Params: testParams(), Transport: newFakeServer(answerNormally), Clock: newFakeClock(),
				Timers: newFakeTimers(), Entropy: &fakeEntropy{}, Resume: &resume,
			})
			if err != ErrResumeNoAddr {
				t.Fatalf("NewManager accepted %s: err = %v, want ErrResumeNoAddr", resume.Addr, err)
			}
		})
	}
}
