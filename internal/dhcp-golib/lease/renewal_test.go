package lease

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// TestManagerAnnouncesRenewal is seam gap G-3 at the manager's edge: the ACK
// that extends a lease reaches the caller as Renewed, and as Renewed ALONE
// when nothing in the lease changed.
//
// The renewal here changes nothing at all — the fake server answers the
// renewal DHCPREQUEST with the same lease it gave at acquisition — which is
// the case a Changed-only design cannot report and a chassis has to log.
func TestManagerAnnouncesRenewal(t *testing.T) {
	r := newRig(t, testParams(), answerNormally, Fault{})

	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s, want acquired", ev)
	}
	if !r.timers.waitArmed(proto.TimerRenew) {
		t.Fatal("no renewal timer was armed after acquisition")
	}
	first, _ := r.mgr.Lease()

	// Move the clock to T1 before firing it. The deadline the renewal earns
	// is measured from the moment its DHCPREQUEST is sent (RFC 2131 4.4.5),
	// so a renewal at a clock that never moved earns the same deadline it
	// already had — and the assertion below, which is the only one that can
	// tell a renewal from a no-op, would be unfalsifiable.
	renewAt, ok := r.timers.armedAt(proto.TimerRenew)
	if !ok {
		t.Fatal("the renewal timer is not armed")
	}
	r.clock.advance(renewAt)

	r.timers.fire(proto.TimerRenew)
	ev := r.nextEvent(t)
	if ev.Kind != Renewed {
		t.Fatalf("event after T1 is %s, want renewed", ev)
	}
	if ev.Lease.Addr != first.Addr {
		t.Fatalf("renewed onto %s, want the same address %s", ev.Lease.Addr, first.Addr)
	}
	if !ev.Lease.Expire.After(first.Expire) {
		t.Fatalf("the renewed lease expires at %s, no later than the old %s; a renewal that does not move the deadline is not one",
			ev.Lease.Expire, first.Expire)
	}

	// Lease() returns the renewed one, not the one it replaced.
	held, ok := r.mgr.Lease()
	if !ok || !held.Expire.Equal(ev.Lease.Expire) {
		t.Fatalf("Lease() = %v, %v; want the renewed lease", held, ok)
	}

	s := r.mgr.Stats()
	if s.RenewalsSent == 0 {
		t.Fatal("Stats.RenewalsSent did not move")
	}
	if s.RenewalsCompleted != 1 {
		t.Fatalf("Stats.RenewalsCompleted = %d, want 1", s.RenewalsCompleted)
	}
	if s.LeasesAcquired != 1 {
		t.Fatalf("Stats.LeasesAcquired = %d; a renewal is not an acquisition", s.LeasesAcquired)
	}
	if s.LeasesLost != 0 {
		t.Fatalf("Stats.LeasesLost = %d; nothing was lost", s.LeasesLost)
	}
}

// TestRenewalsSentCountsRenewalsOnly is the control on the counter above: the
// DHCPREQUEST of an acquisition must not be counted as a renewal. The two are
// told apart by 'ciaddr' (RFC 2131 Table 5), so this drives the acquisition
// alone and asserts the counter stayed at zero.
func TestRenewalsSentCountsRenewalsOnly(t *testing.T) {
	r := newRig(t, testParams(), answerNormally, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s, want acquired", ev)
	}
	if s := r.mgr.Stats(); s.RenewalsSent != 0 {
		t.Fatalf("Stats.RenewalsSent = %d after an acquisition that sent one DHCPREQUEST in SELECTING", s.RenewalsSent)
	}
}

// TestNakCountersSeparateTheWireFromTheMachine is seam gap G-9's NAK half.
//
// Two counters, because their DIFFERENCE is the diagnostic: a DHCPNAK
// discarded for a stale xid is invisible in either number alone, and on a LAN
// with two DHCP servers it is the number that explains the behaviour.
//
// THE TEST HAS TO KEEP A TRANSACTION OPEN. Review round 4 measured this test's
// first two shapes and found both vacuous: disabling proto's xid guard left
// this package GREEN. The reason was not the barrier — it was the state. In
// BOUND the machine discards every inbound message before it ever looks at an
// xid ("message in BOUND with no transaction open"), so a stale NAK injected
// there costs nothing whatever the guard does. Firing T1 first puts the
// machine in RENEWING, where a DHCPNAK for the open transaction WOULD end the
// lease, and the xid is then the only thing standing between the foreign NAK
// and the loss. MEASURED: with the guard disabled this test now fails.
func TestNakCountersSeparateTheWireFromTheMachine(t *testing.T) {
	r := newRig(t, testParams(), answerTheAcquisitionThenGoSilent, Fault{})
	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s, want acquired", ev)
	}

	// Into RENEWING, and stay there: the server answers no renewal.
	if !r.timers.waitArmed(proto.TimerRenew) {
		t.Fatal("no renewal timer was armed")
	}
	at, ok := r.timers.armedAt(proto.TimerRenew)
	if !ok {
		t.Fatal("the renewal timer is not armed")
	}
	r.clock.advance(at)
	r.timers.fire(proto.TimerRenew)
	r.packets.waitRecorded(t, "the renewal request", func(c CapturedPacket) bool {
		if c.Dir != DirOut || c.Msg == nil {
			return false
		}
		mt, ok := c.Msg.Type()
		return ok && mt == wire.MsgRequest && c.Msg.CIAddr.IsValid() && !c.Msg.CIAddr.IsUnspecified()
	})

	// A DHCPNAK for a transaction this client never had.
	stale := nakFor(&wire.Message{XID: 0xDEADBEEF, CHAddr: testCHAddr})
	raw, err := wire.Encode(stale)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	go r.server.injectRaw(raw)
	r.journal.waitAppended(t, "the stale DHCPNAK", func(e proto.JournalEntry) bool {
		return e.Kind == proto.EvReceived && bytes.Equal(e.Raw, raw)
	})

	// A SECOND NAK, AND ITS JOURNAL ENTRY, ARE THE BARRIER FOR THE FIRST.
	//
	// waitAppended's own doc says why: an entry is appended after Step but
	// BEFORE that Step's actions drain, so the first NAK's entry proves the
	// machine saw it and not that it did nothing about it. The entry for a
	// LATER event is appended only after the earlier event's actions have
	// drained, so it is the point at which "nothing happened" is a fact
	// rather than a head start.
	second := nakFor(&wire.Message{XID: 0xFEEDFACE, CHAddr: testCHAddr})
	raw2, err := wire.Encode(second)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	go r.server.injectRaw(raw2)
	r.journal.waitAppended(t, "the second stale DHCPNAK", func(e proto.JournalEntry) bool {
		return e.Kind == proto.EvReceived && bytes.Equal(e.Raw, raw2)
	})

	s := r.mgr.Stats()
	if s.NaksSeen != 2 {
		t.Fatalf("Stats.NaksSeen = %d, want 2: both NAKs were on the wire whatever the machine did with them", s.NaksSeen)
	}
	if s.NaksAccepted != 0 {
		t.Fatalf("Stats.NaksAccepted = %d, want 0: a NAK for a foreign transaction cost this client nothing", s.NaksAccepted)
	}
	if _, ok := r.mgr.Lease(); !ok {
		t.Fatal("a DHCPNAK with a foreign xid ended the lease")
	}
	if s.LeasesLost != 0 {
		t.Fatalf("Stats.LeasesLost = %d, want 0", s.LeasesLost)
	}
}

// TestManagerLeaseCarriesTheRouteAndSearchList is P-5 at the manager's edge:
// what a server sends in options 121 and 119 reaches the caller, and the
// gateway is the one RFC 3442 says to use.
func TestManagerLeaseCarriesTheRouteAndSearchList(t *testing.T) {
	behaviour := func(req *wire.Message, n int) []*wire.Message {
		out := answerNormally(req, n)
		for _, m := range out {
			if t, ok := m.Type(); !ok || t != wire.MsgAck {
				continue
			}
			// Option 3 says one gateway, option 121 says another. RFC 3442
			// makes ignoring option 3 a MUST, so the caller must see 121's.
			m.Options[wire.OptRouter] = addr4("192.168.99.254")
			m.Options[wire.OptClasslessStaticRte] = append(
				append([]byte{0}, addr4("192.168.99.1")...),
				append([]byte{24, 10, 0, 0}, addr4("192.168.99.2")...)...)
			m.Options[wire.OptDomainSearch] = []byte{
				3, 'e', 'n', 'g', 3, 'l', 'a', 'n', 0, 0xC0, 0x04,
			}
		}
		return out
	}
	r := newRig(t, testParams(), behaviour, Fault{})
	ev := r.nextEvent(t)
	if ev.Kind != Acquired {
		t.Fatalf("first event is %s, want acquired", ev)
	}
	if ev.Lease.Gateway != netip.MustParseAddr("192.168.99.1") {
		t.Fatalf("gateway = %s, want option 121's default route and not option 3's 192.168.99.254", ev.Lease.Gateway)
	}
	if len(ev.Lease.Routes) != 2 {
		t.Fatalf("routes = %v, want two", ev.Lease.Routes)
	}
	want := []string{"eng.lan", "lan"}
	if len(ev.Lease.DomainSearch) != 2 || ev.Lease.DomainSearch[0] != want[0] || ev.Lease.DomainSearch[1] != want[1] {
		t.Fatalf("domain search = %q, want %q", ev.Lease.DomainSearch, want)
	}
}

// TestRenewedEventKindRendersItself: EventKind.String is what a chassis logs,
// and an unnamed kind renders as a number nobody can grep for.
func TestRenewedEventKindRendersItself(t *testing.T) {
	if got := Renewed.String(); got != "renewed" {
		t.Fatalf("Renewed.String() = %q", got)
	}
	e := Event{Kind: Renewed, Lease: Lease{Addr: netip.MustParsePrefix("192.168.99.50/24")}}
	if got := e.String(); got == "" || got[:7] != "renewed" {
		t.Fatalf("Event.String() = %q, want it to begin with the kind", got)
	}
}

// TestNakDuringRenewalEndsTheHeldLease is where the netns test's question
// about a DHCPNAK is answered deterministically: at the moment the manager
// announces the loss, it holds nothing.
//
// The netns test cannot ask this. Its event channel is eight deep, so the
// client is free to complete the post-NAK re-acquisition before the reader
// gets to the next line, and a Lease() read there answers a question about
// NOW rather than about what the DHCPNAK cost. Here the server goes silent
// after the refusal, so nothing can re-acquire behind the assertion.
func TestNakDuringRenewalEndsTheHeldLease(t *testing.T) {
	r := newRig(t, testParams(), nakTheRenewalThenGoSilent, Fault{})

	if ev := r.nextEvent(t); ev.Kind != Acquired {
		t.Fatalf("first event is %s, want acquired", ev)
	}
	held, ok := r.mgr.Lease()
	if !ok {
		t.Fatal("nothing held after the acquisition")
	}
	if !r.timers.waitArmed(proto.TimerRenew) {
		t.Fatal("no renewal timer was armed after acquisition")
	}
	renewAt, ok := r.timers.armedAt(proto.TimerRenew)
	if !ok {
		t.Fatal("the renewal timer is not armed")
	}
	r.clock.advance(renewAt)
	r.timers.fire(proto.TimerRenew)

	lost := r.nextEvent(t)
	if lost.Kind != Lost || lost.Reason != proto.ReasonNak {
		t.Fatalf("event after the DHCPNAK is %s, want lost/nak", lost)
	}
	if lost.Lease.Addr != held.Addr {
		t.Fatalf("the lost lease names %s, want the address that was held, %s", lost.Lease.Addr, held.Addr)
	}
	if l, ok := r.mgr.Lease(); ok {
		t.Fatalf("the manager still reports holding %s while announcing that it lost it", l.Addr)
	}
	if s := r.mgr.Stats(); s.LeasesLost != 1 {
		t.Fatalf("Stats.LeasesLost = %d, want 1: it is the counter the Lost event reports", s.LeasesLost)
	}

	// THE NAK COUNTERS BELONG TO THE NEXT EVENT, so the next event is the
	// barrier. One Step produces ActLeaseLost and then ActFailed, in that
	// order because a caller tears the interface down when it sees the loss;
	// NaksAccepted is bumped in the second of those. Reading it here without
	// waiting was review round 4's blocking finding: it made the arbiter go
	// red under load, in the false-red direction, roughly once in a hundred.
	failed := r.nextEvent(t)
	if failed.Kind != Failed || failed.Reason != proto.ReasonNak {
		t.Fatalf("event after the loss is %s, want failed/nak", failed)
	}

	s := r.mgr.Stats()
	if s.NaksSeen != 1 || s.NaksAccepted != 1 {
		t.Fatalf("NAK counters = %d seen / %d accepted, want 1 and 1: this NAK was on the wire AND cost the lease",
			s.NaksSeen, s.NaksAccepted)
	}
	if s.LeasesLost != 1 {
		t.Fatalf("Stats.LeasesLost = %d, want 1", s.LeasesLost)
	}
}

// TestStatsAreCurrentWhenAnEventIsReceived pins the contract Stats documents,
// for EVERY event kind: the counters an event reports are bumped before it is
// emitted, so a caller that reads Stats on receipt sees that event accounted
// for.
//
// It exists because review round 4 held this branch on a test that assumed
// MORE than that — it read a counter belonging to the NEXT event — and the
// arbiter went red under load. This test states the contract in the caller's
// terms — read Stats the moment the event arrives, see the event in it — but
// it is NOT what holds the order: measured, it lets a bump moved below its
// emit through on an idle box. TestEveryCounterIsBumpedBeforeItsEmit, below,
// is the observer; this one is the description.
//
// One rig drives all five kinds in order: acquire, renew onto a lease whose
// router has moved (Renewed then Changed), then a refused renewal (Lost then
// Failed).
func TestStatsAreCurrentWhenAnEventIsReceived(t *testing.T) {
	r := newRig(t, testParams(), renewalChangesThenNak(), Fault{})

	renew := func() {
		t.Helper()
		if !r.timers.waitArmed(proto.TimerRenew) {
			t.Fatal("no renewal timer was armed")
		}
		at, ok := r.timers.armedAt(proto.TimerRenew)
		if !ok {
			t.Fatal("the renewal timer is not armed")
		}
		r.clock.advance(at)
		r.timers.fire(proto.TimerRenew)
	}

	steps := []struct {
		kind EventKind
		// want reports what is WRONG, or "" when the counters this kind
		// reports are current.
		want func(Stats, Event) string
		// then drives whatever produces the next event.
		then func()
	}{
		{
			kind: Acquired,
			want: func(s Stats, _ Event) string {
				if s.LeasesAcquired < 1 {
					return "LeasesAcquired is 0"
				}
				return ""
			},
			then: renew,
		},
		{
			kind: Renewed,
			want: func(s Stats, _ Event) string {
				if s.RenewalsCompleted < 1 {
					return "RenewalsCompleted is 0"
				}
				if s.RenewalsSent < 1 {
					return "RenewalsSent is 0"
				}
				return ""
			},
		},
		{
			kind: Changed,
			// Changed reports no counter of its own. What it reports is the
			// lease, and that is current at receipt for the same reason: the
			// manager writes it under the mutex before it emits.
			want: func(_ Stats, ev Event) string {
				held, ok := r.mgr.Lease()
				if !ok {
					return "the manager holds nothing while announcing a changed lease"
				}
				if held.Gateway != ev.Lease.Gateway {
					return "Lease() gateway " + held.Gateway.String() + " is not the event's " + ev.Lease.Gateway.String()
				}
				return ""
			},
			then: renew,
		},
		{
			kind: Lost,
			want: func(s Stats, _ Event) string {
				if s.LeasesLost < 1 {
					return "LeasesLost is 0"
				}
				return ""
			},
		},
		{
			kind: Failed,
			want: func(s Stats, _ Event) string {
				if s.AcquireFailures < 1 {
					return "AcquireFailures is 0"
				}
				if s.NaksAccepted < 1 {
					return "NaksAccepted is 0"
				}
				return ""
			},
		},
	}

	for i, step := range steps {
		ev := r.nextEvent(t)
		// Read the counters FIRST, before anything else can advance them:
		// the question is what the caller sees at the moment the event
		// arrives, not what it sees a moment later.
		s := r.mgr.Stats()
		if ev.Kind != step.kind {
			t.Fatalf("event %d is %s, want %s", i, ev, step.kind)
		}
		if bad := step.want(s, ev); bad != "" {
			t.Fatalf("at the %s event, %s: the counters an event reports must be current when it is received", step.kind, bad)
		}
		if step.then != nil {
			step.then()
		}
	}
}

// TestEveryCounterIsBumpedBeforeItsEmit is the deterministic half of the
// contract above, and it exists because the runtime half cannot be made
// deterministic.
//
// MEASURED, round 4: four mutants that move a counter bump BELOW its emit
// (LeasesAcquired, RenewalsCompleted, LeasesLost, the NAK pair) all SURVIVE
// TestStatsAreCurrentWhenAnEventIsReceived at -count=20 on an idle box, and
// all four die under a scheduling probe that spins 5 ms after every emit. That
// is the shape of the defect that round fixed: the window between "the caller
// has the event" and "the manager executed the next instruction" is a few
// nanoseconds wide, so a test that races it observes the property only when
// the box is busy — which is the wrong way round for an observer.
//
// The property is about the ORDER OF TWO STATEMENTS, so it is checked where it
// is decidable: in the source. For every case arm of drain's action switch that
// emits an event, no counter may be written after the emit.
//
// M4 CHANGED HOW "AFTER" IS DECIDED. Until here this walked the arm's TOP-LEVEL
// statements and compared list indexes, so a counter write nested inside the
// same statement as the emit — a closure called in place, a defer, a bare block
// — was invisible: M3's review round 5 built exactly that mutant (R1) and it
// survived this test while dying under the probe. Positions replace indexes:
// the boundary is the END of the last emit CALL in the arm, and every counter
// write anywhere in the arm's subtree is compared against it. A write inside
// the emit's own ARGUMENTS is correctly not flagged, because arguments are
// evaluated before the call.
//
// Deferred and spawned writes are flagged wherever they appear, because their
// position says nothing about when they run.
func TestEveryCounterIsBumpedBeforeItsEmit(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "manager.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing manager.go: %v", err)
	}

	var drain *ast.FuncDecl
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if ok && fn.Name.Name == "drain" {
			drain = fn
		}
	}
	if drain == nil {
		t.Fatal("manager.go declares no drain method: this test is looking at the wrong code")
	}

	emitting := 0
	ast.Inspect(drain, func(n ast.Node) bool {
		clause, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		last := lastCallEnd(clause, "emit")
		if !last.IsValid() {
			return true
		}
		emitting++
		for _, w := range counterWrites(clause) {
			switch {
			case w.deferred:
				t.Errorf("%s: %s is written by a %s statement in an arm that emits; a deferred write runs after the emit whatever its position. See Stats.",
					fset.Position(w.pos), w.name, w.how)
			case w.pos > last:
				t.Errorf("%s: %s is written AFTER the emit above it; every counter an event reports must be current when the event is received. See Stats.",
					fset.Position(w.pos), w.name)
			}
		}
		return true
	})

	// The walk above is vacuous if it matched nothing: five event kinds, five
	// arms that emit.
	if emitting != 5 {
		t.Fatalf("drain has %d case arm(s) that emit, want 5 (one per event kind): the walk is not seeing what it thinks it is", emitting)
	}
}

// lastCallEnd returns the position just past the last call to a method of this
// name anywhere under n, or token.NoPos when there is none.
//
// The END of the call and not its start: a counter write inside the call's own
// arguments is evaluated BEFORE the call runs, so it is not a write after the
// emit and must not be reported as one.
func lastCallEnd(n ast.Node, name string) token.Pos {
	last := token.NoPos
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name && call.End() > last {
			last = call.End()
		}
		return true
	})
	return last
}

// counterRef is one write to mg.stats, where it is, and whether its execution
// is detached from its position.
type counterRef struct {
	name     string
	pos      token.Pos
	deferred bool
	how      string
}

// counterWrites finds every write to a counter anywhere under n.
//
// Three spellings count, because a gate keyed on one spelling only enforces
// that spelling: the bump helper, a direct increment under the mutex, and any
// assignment to a field of mg.stats. Anything that touches mg.stats is a write
// as far as this test is concerned; there is no reason to read a counter down
// here, and a false positive costs a rewrite while a false negative costs the
// property.
//
// The walk is over the whole subtree rather than over a statement list, which
// is what makes a write nested inside another statement visible.
func counterWrites(n ast.Node) []counterRef {
	type span struct {
		from, to token.Pos
		how      string
	}
	var detached []span
	ast.Inspect(n, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.DeferStmt:
			detached = append(detached, span{v.Pos(), v.End(), "defer"})
		case *ast.GoStmt:
			detached = append(detached, span{v.Pos(), v.End(), "go"})
		}
		return true
	})
	detachedAt := func(p token.Pos) (bool, string) {
		for _, s := range detached {
			if p >= s.from && p < s.to {
				return true, s.how
			}
		}
		return false, ""
	}

	var out []counterRef
	add := func(name string, pos token.Pos) {
		d, how := detachedAt(pos)
		out = append(out, counterRef{name: name, pos: pos, deferred: d, how: how})
	}
	note := func(sel *ast.SelectorExpr) {
		if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "stats" {
			add("stats."+sel.Sel.Name, sel.Pos())
		}
	}
	ast.Inspect(n, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CallExpr:
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "bump" {
				add("a counter (via bump)", v.Pos())
			}
		case *ast.IncDecStmt:
			if sel, ok := v.X.(*ast.SelectorExpr); ok {
				note(sel)
			}
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok {
					note(sel)
				}
			}
		}
		return true
	})
	return out
}
