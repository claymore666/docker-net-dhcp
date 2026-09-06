package lease

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// TestAV6ManagerAcquiresThroughEveryPort is the v6 counterpart of the v4
// acquisition test, and it drives the WHOLE ring: the DHCPv6 transport, the
// Neighbor Discovery port, the duplicate address detection interlock, the
// timers and the journal.
//
// EVERY ASSERTION IS ON WHAT LEFT THE HOST OR ON WHAT THE CALLER WAS TOLD, and
// never on the manager's own counters — the fake server decodes the payload it
// was handed, so lastSent6 reads the bytes.
func TestAV6ManagerAcquiresThroughEveryPort(t *testing.T) {
	r := newRig6(t, testParams6(), answerNormally6(t))

	e := r.acquire6(t)
	if e.Family != FamilyV6 {
		t.Errorf("the event names family %s, want %s", e.Family, FamilyV6)
	}
	if got := e.Lease.Addr.String(); got != test6Addr+"/128" {
		t.Errorf("acquired %s, want %s/128 — RFC 9915 §21.6 carries an address and no prefix length", got, test6Addr)
	}
	if !sameBytes(e.Lease.ServerDUID, test6ServerDUID) {
		t.Errorf("the lease names the server %x, want %x", e.Lease.ServerDUID, test6ServerDUID)
	}
	if e.Lease.ServerID.IsValid() {
		t.Error("the v6 lease carries a v4 ServerID; exactly one of the two is set per family")
	}
	if e.Lease.IAID != test6IAID {
		t.Errorf("the lease names IAID %d, want %d", e.Lease.IAID, test6IAID)
	}
	if len(e.Lease.DNS) != 1 || e.Lease.DNS[0].String() != test6DNS {
		t.Errorf("the lease carries DNS %v, want %s", e.Lease.DNS, test6DNS)
	}
	if len(e.Lease.DomainSearch) != 1 || e.Lease.DomainSearch[0] != test6Search {
		t.Errorf("the lease carries the search list %v, want %s", e.Lease.DomainSearch, test6Search)
	}
	if e.DAD != proto.DADPassed {
		t.Errorf("the Acquired event reports duplicate address detection as %s, want %s: RFC 9915 §18.2.10.1 leaves no window in which a v6 caller holds an unchecked address", e.DAD, proto.DADPassed)
	}
	if e.ACD != proto.ACDIdle {
		t.Errorf("a v6 event reports the RFC 5227 phase %s; that check is not run here", e.ACD)
	}

	// The wall-clock deadlines are ordered the way the protocol orders them.
	if !e.Lease.Renew.Before(e.Lease.Rebind) {
		t.Errorf("T1 %s is not before T2 %s", e.Lease.Renew, e.Lease.Rebind)
	}
	if !e.Lease.Rebind.Before(e.Lease.Expire) {
		t.Errorf("T2 %s is not before the valid lifetime %s", e.Lease.Rebind, e.Lease.Expire)
	}
	if !e.Lease.Preferred.Equal(e.Lease.Valid) {
		t.Errorf("preferred %s and valid %s differ; the fixture sends 300 for both", e.Lease.Preferred, e.Lease.Valid)
	}

	// The exchange that produced it, read off the wire.
	sent := r.server.sentMessages()
	if len(sent) < 2 {
		t.Fatalf("the server saw %d message(s), want a Solicit and a Request", len(sent))
	}
	if sent[0].Type != wire.MsgSolicit || sent[1].Type != wire.MsgRequest6 {
		t.Fatalf("the exchange was %s then %s, want SOLICIT then REQUEST", sent[0].Type, sent[1].Type)
	}
	// Every message went to All_DHCP_Relay_Agents_and_Servers: RFC 9915
	// removed RFC 3315's Server Unicast option, so there is no unicast
	// destination for a v6 client to use.
	for i, d := range r.server.destinations() {
		if d.Addr != wire.AllDHCPRelayAgentsAndServers {
			t.Errorf("message %d went to %s, want %s", i, d.Addr, wire.AllDHCPRelayAgentsAndServers)
		}
	}
}

// TestTheV6ManagerSolicitsARouterAtStart is design section A.3.3's interlock 1
// seen from ring 2: the Router Solicitation goes out and nothing waits for the
// answer.
func TestTheV6ManagerSolicitsARouterAtStart(t *testing.T) {
	r := newRig6(t, testParams6(), answerNormally6(t))
	r.acquire6(t)

	pkts := r.nd.sentPackets()
	if len(pkts) == 0 {
		t.Fatal("no Router Solicitation went out (RFC 4861 §6.3.7)")
	}
	rs := pkts[0]
	if rs.Dst != wire.AllRoutersMulticast {
		t.Errorf("the Router Solicitation went to %s, want %s", rs.Dst, wire.AllRoutersMulticast)
	}
	if rs.Src.String() != "fe80::e849:4eff:fee5:31ed" {
		t.Errorf("the Router Solicitation's source is %s, want the interface's own link-local address", rs.Src)
	}
	if len(rs.Body) == 0 || rs.Body[0] != wire.ICMPv6RouterSolicit {
		t.Fatalf("the frame is not a Router Solicitation: % x", rs.Body)
	}
	// The lease was acquired without any Router Advertisement arriving, which
	// is interlock 1's whole content.
	if r.mgr.Router().Seen {
		t.Error("the manager reports a router observation with no Router Advertisement delivered")
	}
}

// TestARouterAdvertisementReachesRing1AndTheCaller drives the ND port inbound.
func TestARouterAdvertisementReachesRing1AndTheCaller(t *testing.T) {
	r := newRig6(t, testParams6(), answerNormally6(t))
	r.acquire6(t)

	r.nd.inject(raManagedOther)
	r.journal.waitAppended(t, "the Router Advertisement", func(e proto.JournalEntry6) bool {
		return e.Kind == proto.EvRouterAdvert
	})

	obs := r.mgr.Router()
	if !obs.Seen || !obs.Managed || !obs.Other {
		t.Errorf("the manager reports %+v, want M and O both set", obs)
	}
	if got := r.mgr.Stats().RouterAdvertsSeen; got != 1 {
		t.Errorf("RouterAdvertsSeen = %d, want 1", got)
	}

	// A frame that is not a Router Advertisement is counted and dropped.
	before := r.mgr.Stats()
	r.nd.inject([]byte{136, 0, 0, 0, 0, 0, 0, 0})
	r.nd.inject(raManagedOther)
	r.journal.waitAppended(t, "the second Router Advertisement", func(e proto.JournalEntry6) bool {
		return e.Kind == proto.EvRouterAdvert
	})
	after := r.mgr.Stats()
	if after.NDIgnored != before.NDIgnored+1 {
		t.Errorf("NDIgnored went from %d to %d; a Neighbor Advertisement is not this ring's to read", before.NDIgnored, after.NDIgnored)
	}
	if after.NDSeen != before.NDSeen+2 {
		t.Errorf("NDSeen went from %d to %d, want two more frames", before.NDSeen, after.NDSeen)
	}
}

// raManagedOther is a Router Advertisement with M and O both set.
var raManagedOther = []byte{
	134, 0, 0, 0,
	64, 0xC0, 0x07, 0x08,
	0, 0, 0, 0,
	0, 0, 0, 0,
}

// TestTheV6ManagerReportsTheStatelessConfiguration drives RFC 9915 §18.2.6
// through the manager: an M=0 O=1 Router Advertisement while soliciting turns
// the exchange into an Information-request, whose Reply is a Configured event
// and NOT a lease.
func TestTheV6ManagerReportsTheStatelessConfiguration(t *testing.T) {
	answered := make(chan struct{}, 1)
	behaviour := func(req *wire.MessageV6, _ int) []*wire.MessageV6 {
		if req.Type != wire.MsgInformationRequest {
			return nil
		}
		search, err := wire.EncodeDomainSearch([]string{test6Search})
		if err != nil {
			t.Errorf("EncodeDomainSearch: %v", err)
			return nil
		}
		dns := netip.MustParseAddr(test6DNS).As16()
		select {
		case answered <- struct{}{}:
		default:
		}
		return []*wire.MessageV6{{
			Type: wire.MsgReply, XID: req.XID,
			Options: wire.OptionsV6{
				optV6(wire.OptV6ClientID, test6DUID),
				optV6(wire.OptV6ServerID, test6ServerDUID),
				optV6(wire.OptV6DNSServers, dns[:]),
				optV6(wire.OptV6DomainList, search),
				// 60 seconds, which is OUTSIDE §21.23's bounds on purpose:
				// IRT_MINIMUM is 600. A fixture inside the bounds cannot
				// tell the reported value from the raw one, and this test
				// is the ring-2 end of that fact.
				optV6(wire.OptV6InfoRefresh, []byte{0, 0, 0, 60}),
			},
		}}
	}
	r := newRig6(t, testParams6(), behaviour)

	// THREE BARRIERS, AND ONLY ONE OF THEM IS settle. Interlock 1 is a rule
	// about the SELECTING6 arm, so all three facts have to hold before the
	// send can be read, and each needs a different barrier.
	//
	// FIRST, THE CLIENT MUST HAVE SOLICITED. newRig6 returns as soon as the
	// manager is running, and the initial Solicit leaves on the machine's own
	// goroutine; an advertisement injected before it is stepped in INIT6,
	// where interlock 1 does not apply, and the Solicit then arrives after it.
	// The observed failure was exactly that: "the last message the server saw
	// is SOLICIT, want INFORMATION-REQUEST".
	//
	// SECOND, THE MACHINE MUST HAVE STEPPED THE ADVERTISEMENT, and settle does
	// not say that: settle fires the marker timer and waits for its Step,
	// while Manager.Run selects between the ND channel and the timer channel,
	// so an advertisement already queued has an even chance of being served
	// after the marker. **A proxy repeats in the same file** — this is the
	// coin toss waitDADStepped was written for, one producer over.
	//
	// THIRD, THE ACTIONS OF THAT STEP MUST HAVE DRAINED, and the journal entry
	// does not say that either: Manager.dispatch appends the entry BEFORE it
	// drains the entry's own actions (see waitSent). settle is the barrier for
	// that half, and it is sound here because the advertisement has already
	// been taken.
	//
	// MEASURED 2026-09-06: with settle alone, 2 failures in 480 runs under an
	// eight-way parallel load, and one of them reddened the
	// suite-domain-unmeasured-module oracle scenario, which has nothing to do
	// with DHCPv6. With the journal barrier added but no waitSent, 1 in 480.
	//
	// None of the three waits for the Information-request itself, which is the
	// point: asserting the send BEFORE reading the event is what turns
	// "interlock 1 does not fire" from a hang on nextEvent into a failure.
	// MEASURED: the mutant that deletes the SELECTING6 arm of the switch was
	// scored HUNG against a wait for the thing it removes.
	r.waitSent(t, wire.MsgSolicit)
	r.nd.inject(raOtherOnly)
	r.journal.waitAppended(t, "the Router Advertisement", func(e proto.JournalEntry6) bool {
		return e.Kind == proto.EvRouterAdvert
	})
	r.settle(t)
	if got := lastSent6(t, r, wire.MsgInformationRequest); got == nil {
		t.Fatal("M=0 O=1 arrived while soliciting and no Information-request left the host: design §A.3.3 interlock 1")
	}

	e := r.nextEvent(t)
	if e.Kind != Configured {
		t.Fatalf("the first event is %s, want configured", e.Kind)
	}
	if e.Lease.Addr.IsValid() {
		t.Errorf("a configured event carries the address %s; §18.2.6's exchange has none", e.Lease.Addr)
	}
	if len(e.Config.DNS) != 1 || e.Config.DNS[0].String() != test6DNS {
		t.Errorf("the configuration carries DNS %v, want %s", e.Config.DNS, test6DNS)
	}
	if len(e.Config.Search) != 1 || e.Config.Search[0] != test6Search {
		t.Errorf("the configuration carries the search list %v, want %s", e.Config.Search, test6Search)
	}
	// The fake clock does not move unless a test moves it, and this one does
	// not, so the instant the configuration names is exactly now + the bound.
	if want := r.clock.Wall().Add(600 * time.Second); !e.Config.Refresh.Equal(want) {
		t.Errorf("the configuration says the client asks again at %s, want %s: the Reply sent 60 seconds and §21.23 says \"A client MUST use the refresh time IRT_MINIMUM if it receives the option with a value less than IRT_MINIMUM.\"",
			e.Config.Refresh, want)
	}
	if got := r.mgr.Stats().ConfiguredEvents; got != 1 {
		t.Errorf("ConfiguredEvents = %d, want 1", got)
	}
	<-answered
	if got := lastSent6(t, r, wire.MsgInformationRequest); got == nil {
		t.Fatal("no Information-request left the host")
	}
}

// raOtherOnly is M=0, O=1: RFC 4861 §4.2's "other configuration information is
// available via DHCPv6", with no addresses.
var raOtherOnly = []byte{
	134, 0, 0, 0,
	64, 0x40, 0x07, 0x08,
	0, 0, 0, 0,
	0, 0, 0, 0,
}

// TestADuplicateAddressDeclinesAndNeverAcquires is D22's shape at ring 2: the
// caller is never told Acquired for an address the check found in use.
func TestADuplicateAddressDeclinesAndNeverAcquires(t *testing.T) {
	r := newRig6(t, testParams6(), answerNormally6(t))
	r.settleDAD(t, test6Addr, true)

	// settle rather than waitSent: a client that dropped the address without
	// declining it sends nothing, and a barrier waiting for the Decline would
	// HANG on exactly that defect instead of naming it. MEASURED — the mutant
	// that replaces declineAll with a bare restartDiscovery was scored HUNG.
	r.settle(t)
	if _, held := r.mgr.Lease(); held {
		t.Error("the manager holds a lease for an address duplicate address detection found in use")
	}
	dec := findSent6(r, wire.MsgDecline6)
	if dec == nil {
		t.Fatal("no Decline left the host (§18.2.10.1 makes it a MUST)")
	}
	declined, err := dec.Options.IANAs()
	if err != nil || len(declined) != 1 {
		t.Fatalf("the Decline's IA_NA: %v %v", declined, err)
	}
	addrs, err := declined[0].Options.Addrs()
	if err != nil || len(addrs) != 1 || addrs[0].Addr.String() != test6Addr {
		t.Fatalf("the Decline names %v, want the address the check refused, %s", addrs, test6Addr)
	}
}

// TestAV6ConflictIsCountedAndReportedAsOne is the ring-2 half of proto's
// TestADuplicateOnAFreshAcquisitionIsReportedAndNotOnlyJournalled.
//
// THE COUNTER IS THE CLAIM, and it was wrong in two places at once. Stats
// documents DADChecksStarted and DADConflicts as a pair whose difference is
// not "how many passed", and WireCounters documents DADConflicts as the v6
// share of Conflicts, "so their difference is how many came from RFC 5227's
// check rather than RFC 4862's". On the ordinary v6 duplicate — the check runs
// BEFORE the lease is announced, so nothing was ever held — ring 1 emitted no
// action a counter could be derived from, and ring 2's ActFailed arm counted
// only ConflictsDetected. A reader of a record from a client that had just
// declined a duplicate address saw one conflict, attributed to ARP.
//
// The event is asserted beside the two counters because they are separate
// failures: a chassis watching Events() was told nothing at all.
func TestAV6ConflictIsCountedAndReportedAsOne(t *testing.T) {
	r := newRig6(t, testParams6(), answerNormally6(t))
	r.settleDAD(t, test6Addr, true)
	r.settle(t)

	e := r.takeEvent(t)
	if e.Kind != Failed {
		t.Fatalf("the duplicate produced a %s event, want %s", e.Kind, Failed)
	}
	if e.Reason != proto.ReasonConflict {
		t.Errorf("reason %s, want %s", e.Reason, proto.ReasonConflict)
	}

	s := r.mgr.Stats()
	if s.DADChecksStarted != 1 {
		t.Errorf("DADChecksStarted = %d, want 1", s.DADChecksStarted)
	}
	if s.ConflictsDetected != 1 {
		t.Errorf("ConflictsDetected = %d, want 1", s.ConflictsDetected)
	}
	if s.DADConflicts != 1 {
		t.Errorf("DADConflicts = %d, want 1: a v6 conflict counted only in ConflictsDetected reads, through WireCounters, as one that came from RFC 5227's check", s.DADConflicts)
	}
	if s.LeasesLost != 0 {
		t.Errorf("LeasesLost = %d; nothing was ever announced to lose, and a conflict counted in both arms is counted twice", s.LeasesLost)
	}
}

// TestAV6ManagerRefusesTheWrongPorts is the Config validation, as a table.
func TestAV6ManagerRefusesTheWrongPorts(t *testing.T) {
	p := testParams6()
	base := func() Config {
		return Config{
			Params6:     &p,
			TransportV6: newFakeServer6(silent6),
			ND:          newFakeND(),
			Clock:       newFakeClock(),
			Timers:      newFakeTimers(),
			Entropy:     &fakeEntropy{},
		}
	}
	for _, tc := range []struct {
		name  string
		spoil func(*Config)
		want  error
	}{
		{"no v6 transport", func(c *Config) { c.TransportV6 = nil }, ErrNoTransportV6},
		{"no Neighbor Discovery port", func(c *Config) { c.ND = nil }, ErrNoND},
		{"no clock", func(c *Config) { c.Clock = nil }, ErrNoClock},
		{"no timers", func(c *Config) { c.Timers = nil }, ErrNoTimers},
		{"no entropy", func(c *Config) { c.Entropy = nil }, ErrNoEntropy},
		{"a v4 transport beside Params6", func(c *Config) { c.Transport = newFakeServer(answerNormally) }, ErrBothFamilies},
		{"a v4 resume beside Params6", func(c *Config) { c.Resume = &Lease{} }, ErrBothFamilies},
		{
			"a resume with no usable address",
			func(c *Config) { c.Resume6 = &Lease{} },
			ErrResume6NoAddr,
		},
		{
			"a v4 address in the v6 resume",
			func(c *Config) { c.Resume6 = &Lease{Addr: netip.MustParsePrefix("192.168.99.5/24")} },
			ErrResume6NoAddr,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.spoil(&cfg)
			mgr, err := NewManager(cfg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewManager = %v, %v; want %v", mgr, err, tc.want)
			}
			if mgr != nil {
				t.Error("a refused Config still produced a manager")
			}
		})
	}

	t.Run("the control", func(t *testing.T) {
		mgr, err := NewManager(base())
		if err != nil || mgr == nil {
			t.Fatalf("NewManager on a good Config = %v, %v", mgr, err)
		}
		if !mgr.v6() {
			t.Error("a Config with Params6 built a v4 manager")
		}
	})
}

// TestAResumedV6LeaseConfirms is design section A.3.3's interlock 3 through
// ring 2, and it is where the wall-clock crossing is checked: the record's
// deadlines become the remaining lifetimes ring 1 works in.
func TestAResumedV6LeaseConfirms(t *testing.T) {
	clk := newFakeClock()
	now := clk.Wall()
	remembered := Lease{
		Addr:       netip.MustParsePrefix(test6Addr + "/128"),
		ServerDUID: append([]byte(nil), test6ServerDUID...),
		IAID:       test6IAID,
		Preferred:  now.Add(120e9),
		Valid:      now.Add(240e9),
		Expire:     now.Add(240e9),
		Renew:      now.Add(60e9),
		Rebind:     now.Add(180e9),
	}
	r := newRig6On(t, clk, testParams6(), answerNormally6(t), withResume6(remembered))

	r.waitSent(t, wire.MsgConfirm)
	conf := findSent6(r, wire.MsgConfirm)
	if conf == nil {
		t.Fatal("no Confirm left the host (§18.2.12)")
	}
	if findSent6(r, wire.MsgSolicit) != nil {
		t.Error("a Solicit went out beside the Confirm; a client with remembered addresses confirms them")
	}
	if _, ok := conf.Options.First(wire.OptV6ServerID); ok {
		t.Error("the Confirm carries a Server Identifier; §18.2.3 lists none")
	}
	ias, err := conf.Options.IANAs()
	if err != nil || len(ias) != 1 {
		t.Fatalf("the Confirm's IA_NA: %v %v", ias, err)
	}
	addrs, err := ias[0].Options.Addrs()
	if err != nil || len(addrs) != 1 || addrs[0].Addr.String() != test6Addr {
		t.Fatalf("the Confirm asks about %v, want the remembered %s", addrs, test6Addr)
	}
}

// findSent6 returns the first message of this type the server saw, or nil.
func findSent6(r *rig6, want wire.MessageTypeV6) *wire.MessageV6 {
	for _, m := range r.server.sentMessages() {
		if m.Type == want {
			return m
		}
	}
	return nil
}

func sameBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTheConfiguredRunnerIsAskedAndItsAnswerIsTheMachines drives the whole of
// the DADRunner seam in one pass, with NOTHING in the test answering the
// machine.
//
// EVERY OTHER v6 TEST IN THIS PACKAGE CALLS ReportDADResult BY HAND, which is
// the shape M7b left behind and is exactly why this row exists: a manager
// wired to real sockets and left alone would ask nobody, and every one of
// those tests would still pass. rig6.acquire6's own comment says so — "the DAD
// result is fed by the test because ring 3 is not built yet". Here the answer
// comes from the port, and the test only watches.
func TestTheConfiguredRunnerIsAskedAndItsAnswerIsTheMachines(t *testing.T) {
	dad := &fakeDAD{}
	r := newRig6(t, testParams6(), answerNormally6(t), withDAD(dad))

	// THREE BARRIERS AND NOT ONE READ, because the read this test wants is the
	// one every mutant here turns into a hang. waitDADRequested says the
	// machine asked (and stops on the three ways it can fail to); answered
	// says the runner's verdict is in the manager's request channel, or that
	// there was no runner call to wait for; and waitDADStepped says the
	// manager has stepped it. rig6.settle's own comment carries the general
	// version of this argument, fakeDAD.answered and rig6.waitDADStepped the
	// two measurements — including why a count of settles is not a barrier at
	// all here. takeEvent settles for the drain.
	r.waitDADRequested(t, test6Addr)
	dad.answered()
	r.waitDADStepped(t)

	e := r.takeEvent(t)
	if e.Kind != Acquired {
		t.Fatalf("the first event is %s, want acquired; the machine's last Steps were:\n%s", e.Kind, r.tail6(6))
	}

	// THE ADDRESS IS ASSERTED, not just the count. A runner asked about the
	// wrong address would still answer, the machine would still journal an
	// EvDADResult it did not want, and the acquisition would then fail on ring
	// 1's deadline rather than here — a slower and much less legible failure.
	asked := dad.asked()
	if len(asked) != 1 || asked[0].String() != test6Addr {
		t.Fatalf("the runner was asked about %v, want exactly [%s]", asked, test6Addr)
	}
	if got := r.mgr.Stats().DADChecksStarted; got != 1 {
		t.Errorf("DADChecksStarted = %d, want 1", got)
	}
	if _, held := r.mgr.Lease(); !held {
		t.Error("no lease is held after the runner reported the address free")
	}
}

// TestTheConfiguredRunnersDuplicateVerdictDeclines is the other verdict
// through the same seam.
//
// It is a separate row rather than a table because the two answers exercise
// different halves: the row above shows the seam CARRIES a verdict, and this
// one shows it carries THE verdict. A runner whose answer were ignored — or a
// seam that reported free regardless — would pass the row above and lose the
// only check RFC 9915 section 18.2.10.1 makes a MUST.
func TestTheConfiguredRunnersDuplicateVerdictDeclines(t *testing.T) {
	dad := &fakeDAD{verdict: true}
	r := newRig6(t, testParams6(), answerNormally6(t), withDAD(dad))

	// The same three barriers as above, and then a READ. A client that took
	// the verdict and dropped the address without declining it sends nothing,
	// so waiting for the Decline would hang on that defect instead of naming
	// it — which is the argument TestADuplicateAddressDeclinesAndNeverAcquires
	// makes about the same message.
	r.waitDADRequested(t, test6Addr)
	dad.answered()
	r.waitDADStepped(t)
	// And one settle for the DRAIN: waitDADStepped returns before the Step's
	// actions have run, and the Decline read below is one of them.
	r.settle(t)

	if _, held := r.mgr.Lease(); held {
		t.Error("the manager holds a lease for an address the runner reported in use")
	}
	if findSent6(r, wire.MsgDecline6) == nil {
		t.Fatalf("no Decline left the host after the runner reported a duplicate (RFC 9915 section 18.2.10.1 makes it a MUST); the machine's last Steps were:\n%s", r.tail6(6))
	}
	if asked := dad.asked(); len(asked) != 1 || asked[0].String() != test6Addr {
		t.Fatalf("the runner was asked about %v, want exactly [%s]", asked, test6Addr)
	}
}

// TestWithoutARunnerTheCallerStillOwesTheResult is the preservation control.
//
// The seam is a WIDENING of Config, and a widening needs a row saying the old
// shape still behaves: every caller built before lease.DADRunner existed
// passes no runner and answers by hand, and this is the test that would fail
// if the new arm had swallowed that path. It also pins the difference where a
// reader will find it — the two journal notes are not the same sentence, and
// the one written here names who owes the answer.
func TestWithoutARunnerTheCallerStillOwesTheResult(t *testing.T) {
	r := newRig6(t, testParams6(), answerNormally6(t))

	// The caller answers, exactly as every v6 test in this package did before
	// the port existed, and the machine acquires. settleDAD is the barrier for
	// both halves: it returns only once the request has been journalled AND
	// the answer stepped, so the snapshot below is taken after both.
	r.settleDAD(t, test6Addr, false)
	if e := r.takeEvent(t); e.Kind != Acquired {
		t.Fatalf("the first event is %s, want acquired", e.Kind)
	}

	if got := r.mgr.Stats().DADChecksStarted; got != 1 {
		t.Errorf("DADChecksStarted = %d, want 1; the request is counted with or without a runner", got)
	}

	var note string
	for _, e := range r.mgr.Journal6() {
		if e.Note && strings.Contains(e.Reason, "duplicate address detection") {
			note = e.Reason
		}
	}
	if !strings.Contains(note, "no runner configured, the caller owes the result") {
		t.Fatalf("the journal note for a runner-less client reads %q; a reader cannot tell it from a client whose runner was asked", note)
	}
}
