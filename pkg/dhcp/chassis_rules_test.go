// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
	"github.com/vishvananda/netns"
)

func testMAC(t *testing.T) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC("02:42:c0:a8:63:07")
	if err != nil {
		t.Fatalf("parse MAC: %v", err)
	}
	return mac
}

// TestTranslate_AStopIsNotALeaseLoss is the seam's single most
// dangerous rule, driven directly.
//
// WHY IT IS DANGEROUS. Cancelling a manager makes lease.Manager feed
// EvStop to the machine, the machine drops the binding, and the caller
// hears Lost{ReasonStopped}. That happens on EVERY successful
// CreateEndpoint — the one-shot acquisition manager is cancelled the
// moment it has an address — and on every clean Leave. Reading it as a
// lease loss makes the plugin report a loss for every container that
// started correctly: leases_lost climbs, the audit ledger records a
// loss beside the bind, and the health floor sees faults on a healthy
// run.
//
// It is also invisible. The chassis emits nothing for it, so there is
// no message anywhere to notice; the only observable is a counter
// moving that should not have.
//
// The three other Lost reasons are driven beside it, because "drop
// every Lost" satisfies the first assertion on its own and is the
// mutation this test most needs to catch.
//
// THE SLAAC ROWS ARE THE SAME RULE ONE FIELD FURTHER IN (#818, #819).
// A lease formed from a router advertisement was granted by nobody, so
// its ending is a prefix that stopped being advertised or a valid
// lifetime that ran out, and "leasefail" is the event the plugin turns
// into dhcp_timeouts -- the counter an operator alerts on to mean the
// DHCP SERVER went quiet. It gets its own type instead, which is also
// the only way the plugin can be told WHICH addresses to take off the
// link: "leasefail" carries no lease data at all.
func TestTranslate_AStopIsNotALeaseLoss(t *testing.T) {
	now := time.Now()
	v4 := netip.MustParsePrefix("192.168.99.7/24")
	v6 := netip.MustParsePrefix("2001:db8:1::42/64")

	for _, tc := range []struct {
		name     string
		reason   proto.Reason
		slaac    bool
		wantEmit bool
		wantType string
	}{
		{"the chassis cancelling its own manager", proto.ReasonStopped, false, false, ""},
		{"a server NAK", proto.ReasonNak, false, true, "nak"},
		{"the lease expiring", proto.ReasonExpired, false, true, "leasefail"},
		{"the link going down", proto.ReasonLinkDown, false, true, "leasefail"},

		{"a formed address expiring", proto.ReasonExpired, true, true, "slaac_lost"},
		{"a formed address on a link that went down", proto.ReasonLinkDown, true, true, "slaac_lost"},
		// The stop rule still comes first: a formed lease dropped
		// because this process cancelled its own manager is every
		// successful CreateEndpoint on a slaac network.
		{"a stop on a formed lease", proto.ReasonStopped, true, false, ""},
		// Unreachable and pinned, the way the impossible row in
		// classifyV6Absence's table is: no server NAKs an address it
		// never granted. It states which field decides, and a
		// reordering that read the reason first would send a formed
		// lease's ending to the DHCP counters.
		{"a NAK naming a formed lease", proto.ReasonNak, true, true, "slaac_lost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := lease.Lease{Addr: v4}
			if tc.slaac {
				l = lease.Lease{SLAAC: true, Addr: v6, Addrs: []lease.Addr6{{Addr: v6}}}
			}
			out, emit, _ := translateOne(
				lease.Event{Kind: lease.Lost, Reason: tc.reason, Lease: l}, now, time.Time{}, netip.Prefix{})
			if emit != tc.wantEmit {
				t.Fatalf("Lost{%v}: emit=%v, want %v. A stop reported as a loss makes every "+
					"successful container start look like a lease failure; a real loss "+
					"swallowed makes the plugin silent about an address the container no "+
					"longer holds.", tc.reason, emit, tc.wantEmit)
			}
			if emit && out.Type != tc.wantType {
				t.Errorf("Lost{%v}: emitted %q, want %q", tc.reason, out.Type, tc.wantType)
			}
			if tc.wantType == "slaac_lost" {
				if !out.Data.SLAAC || out.Data.IP != v6.String() {
					t.Errorf("the event carries %+v; the plugin takes the addresses off the "+
						"link from this data and has no other source for them",
						out.Data.Addrs)
				}
			}
		})
	}
}

// TestTranslate_ARenewalIsNotCountedTwice pins the coalescing rule.
//
// The library emits Renewed and Changed from ONE action batch when a
// renewal comes back with different contents. Both carry the same
// DHCPACK. Emitting "renew" for each counts one renewal twice — in
// leases_renewed and as two rows in the audit ledger — and an operator
// reading the ledger sees a renewal storm that did not happen.
//
// Driven on both sides of the window, because a coalescer that
// swallowed EVERY Changed would pass the first half alone, and the
// second half is a real re-acquisition: a NAK followed by a different
// address, which the plugin must apply.
func TestTranslate_ARenewalIsNotCountedTwice(t *testing.T) {
	now := time.Now()
	l := lease.Lease{Addr: netip.MustParsePrefix("192.168.99.7/24")}

	_, emit, renewedAt := translateOne(lease.Event{Kind: lease.Renewed, Lease: l}, now, time.Time{}, netip.Prefix{})
	if !emit {
		t.Fatal("a Renewed emitted nothing; the plugin would never see a renewal at all")
	}

	if _, emit, _ := translateOne(
		lease.Event{Kind: lease.Changed, Lease: l},
		now.Add(coalesceWindow/2), renewedAt, netip.Prefix{}); emit {
		t.Error("the Changed that accompanied the same DHCPACK was emitted as a second renewal")
	}

	if _, emit, _ := translateOne(
		lease.Event{Kind: lease.Changed, Lease: l},
		now.Add(10*coalesceWindow), renewedAt, netip.Prefix{}); !emit {
		t.Error("a Changed well outside the window was swallowed. That is a re-acquisition on a " +
			"different address — a NAK and a new lease — and the container is left configured " +
			"with the old one.")
	}
}

// TestBuildParams_NeitherManagerDesyncs is D-1, with the cost of
// getting it wrong MEASURED rather than asserted.
//
// RFC 2131 section 4.4.1 says a client SHOULD wait one to ten seconds
// before its first DISCOVER, to desynchronise a fleet of hosts booting
// together. Neither manager here is a fleet. Each is one container
// asking for one address, started by one `docker run` or one
// `docker start`.
//
// THIS TEST USED TO ASSERT THE OPPOSITE FOR THE JOIN MANAGER, and the
// reason it gave was "it is the one manager of which there may be many
// starting at once, after a plugin restart". That was an argument, and
// it was falsified by a measurement — see
// TestBuildParams_AColdJoinSendsItsFirstPacketAtOnce below, and D-1 in
// the seam note. It is recorded here rather than quietly deleted
// because the shape it belongs to is this repository's most expensive
// one: a claim that survives because nothing ever drove it.
func TestBuildParams_NeitherManagerDesyncs(t *testing.T) {
	mac := testMAC(t)

	for _, tc := range []struct {
		name string
		once bool
	}{
		{"the CreateEndpoint one-shot", true},
		{"the Join manager", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := buildParams(&DHCPClientOptions{MAC: mac}, tc.once)
			if err != nil {
				t.Fatalf("buildParams: %v", err)
			}
			if p.DesyncMin != 0 || p.DesyncMax != 0 {
				t.Errorf("%s desyncs by %v..%v; the library documents both zero as the "+
					"disabling value", tc.name, p.DesyncMin, p.DesyncMax)
			}
		})
	}

	// The equality is the rule, and it is asserted separately from the
	// two values so that a future arm on `once` fails HERE, at the
	// statement that the two managers are the same, rather than only at
	// whichever of them was changed.
	once, err := buildParams(&DHCPClientOptions{MAC: mac}, true)
	if err != nil {
		t.Fatalf("buildParams(once): %v", err)
	}
	persistent, err := buildParams(&DHCPClientOptions{MAC: mac}, false)
	if err != nil {
		t.Fatalf("buildParams(persistent): %v", err)
	}
	if once.DesyncMin != persistent.DesyncMin || once.DesyncMax != persistent.DesyncMax {
		t.Errorf("the two managers desync differently: one-shot %v..%v, Join %v..%v. "+
			"D-1 is that neither of them is the fleet section 4.4.1 is about, and the "+
			"argument for exempting only one of them has already been falsified once.",
			once.DesyncMin, once.DesyncMax, persistent.DesyncMin, persistent.DesyncMax)
	}

	// MEASURED, against the REJECTED shape rather than against the
	// shipped one — the shipped one now waits zero, so measuring it
	// would price nothing.
	//
	// The right question is not "does the delay exceed the budget" --
	// it cannot, the window closes at 10s and so does lease_timeout --
	// but HOW MUCH OF THE BUDGET IS LEFT for the exchange when the
	// first DISCOVER finally goes out. The exchange is four packets,
	// and RFC 2131 section 4.1's own schedule waits 4 seconds before
	// retransmitting the first of them: a draw leaving less than that
	// cannot survive a single dropped packet inside the budget, which
	// is precisely the intermittent failure. Driven through the
	// library's own jitter rather than recomputed here.
	rejected := proto.DefaultParams(mac)
	if rejected.DesyncMin == 0 && rejected.DesyncMax == 0 {
		t.Fatal("proto.DefaultParams no longer desyncs, so the measurement below prices " +
			"nothing and D-1 has become a no-op the chassis is still paying a line for")
	}
	const (
		budget      = 10 * proto.Second
		oneRetrans  = 4 * proto.Second // RFC 2131 4.1's first retransmission
		drawsWanted = 1000
	)
	var tight, n int
	var worst proto.Duration
	for i := 0; i < drawsWanted; i++ {
		d := firstSendDelay(t, rejected, uint64(i)*0x9e3779b97f4a7c15+1)
		n++
		if d > worst {
			worst = d
		}
		if budget-d < oneRetrans {
			tight++
		}
	}
	t.Logf("MEASURED: with the desync left at the library default, the first DISCOVER waits "+
		"up to %.2fs of the %ds lease_timeout, and in %d of %d draws (%.1f%%) less than the "+
		"%ds of RFC 2131 4.1's first retransmission is left for the exchange -- one dropped "+
		"packet and the container start fails",
		float64(worst)/float64(proto.Second), budget/proto.Second, tight, n,
		100*float64(tight)/float64(n), oneRetrans/proto.Second)
	if tight == 0 {
		t.Error("no draw left the exchange short of a retransmission, so this measurement does " +
			"not show what the desync costs and the rule above is unmotivated by it")
	}
}

// TestBuildParams_AColdJoinSendsItsFirstPacketAtOnce drives the path
// that the argument for keeping the Join manager's desync never
// considered: a JOINED endpoint whose remembered lease is NOT LIVE.
//
// proto.Machine.beginAcquisition takes the resume BEFORE the desync
// block and returns, so a Join that resumes a live lease never reaches
// the draw at all — which is why every green run in this branch's
// history was blind to it. takeResume returns false for a lease that
// has expired ("a server with no record of a client must stay silent",
// RFC 2131 4.3.2) and falls through to the draw. A container that was
// down over a weekend comes back on exactly that path.
//
// WHAT IT COSTS, MEASURED, run 33785125087. The `Resume`-dropped mutant
// forced every Join onto this path and took two kills that were not the
// one it was aimed at: TestDNSPropagate_OptInWritesResolvConf on shard
// main-3 and TestMTUPropagate_OptInSetsLinkMTU on main-5. The run's own
// dumps show the mechanism is not a slow exchange but SILENCE — the
// fixture logged exactly one DHCP transaction for the container's MAC,
// CreateEndpoint's one-shot, and the plugin logged at teardown
// "Persistent client stopped before it ever held the lease; the
// one-shot's lease is left to expire on the server". The Join manager
// spent the container's entire life inside the draw.
//
// WHY THE ASSERTION IS ON THE PACKET AND NOT ON resolv.conf. The two
// tests that caught it assert on a file appearing within five seconds,
// so they are a race whose outcome is a uniform draw: they fail about
// 60% of the time on this path and pass the rest, which is the shape
// that gets called a flaky harness. The delay before the first packet
// is the thing itself, and it is exact on the library's virtual clock.
func TestBuildParams_AColdJoinSendsItsFirstPacketAtOnce(t *testing.T) {
	mac := testMAC(t)

	p, err := buildParams(&DHCPClientOptions{MAC: mac}, false)
	if err != nil {
		t.Fatalf("buildParams(Join): %v", err)
	}
	// An expired remembered lease: Expire is not after now (0), so
	// takeResume refuses it and the machine acquires from INIT. This is
	// the record a container that was down too long comes back with.
	p.Resume = &proto.Resume{
		Addr:      netip.MustParseAddr("192.168.99.7"),
		Expire:    0,
		HasExpire: true,
	}

	const draws = 256
	var worst proto.Duration
	for i := 0; i < draws; i++ {
		rnd := uint64(i)*0x9e3779b97f4a7c15 + 1
		d, mt := firstSendDelayAndType(t, p, rnd)
		if mt != wire.MsgDiscover {
			t.Fatalf("the first packet on draw %d was %s, not a DHCPDISCOVER: this test is "+
				"not on the path it claims to be on. An expired Resume must be refused by "+
				"takeResume and acquired from INIT (RFC 2131 4.3.2).", i, mt)
		}
		if d > worst {
			worst = d
		}
	}
	if worst != 0 {
		t.Errorf("a Join with an expired remembered lease waited up to %.2fs before its "+
			"first DHCPDISCOVER. That is RFC 2131 4.4.1's fleet desync applied to one "+
			"container, and for the length of it the container runs with Docker's own "+
			"resolv.conf and the link-default MTU on a network that asked for neither "+
			"(propagate_dns / propagate_mtu are applied from the bind event). D-1 says "+
			"both managers send at once.", float64(worst)/float64(proto.Second))
	}
	t.Logf("MEASURED: cold Join, first DHCPDISCOVER at %.2fs over %d entropy draws",
		float64(worst)/float64(proto.Second), draws)

	// The control, and it is what makes the assertion above mean
	// something: restore the library default on this same drive and the
	// delay must come back. Without it, "worst == 0" is equally
	// satisfied by a helper that measures nothing.
	restored := p
	restored.DesyncMin, restored.DesyncMax = proto.DefaultParams(mac).DesyncMin, proto.DefaultParams(mac).DesyncMax
	var delayed int
	var ctlWorst proto.Duration
	for i := 0; i < draws; i++ {
		d, _ := firstSendDelayAndType(t, restored, uint64(i)*0x9e3779b97f4a7c15+1)
		if d > 0 {
			delayed++
		}
		if d > ctlWorst {
			ctlWorst = d
		}
	}
	if delayed != draws {
		t.Errorf("control: with the desync restored, %d of %d draws still sent at once. "+
			"The drive is not reaching the desync block, so the assertion above is not "+
			"measuring the thing it names.", draws-delayed, draws)
	}
	t.Logf("control: with the library default restored, the same cold Join waits up to "+
		"%.2fs before its first DHCPDISCOVER (%d of %d draws delayed)",
		float64(ctlWorst)/float64(proto.Second), delayed, draws)
}

// firstSendDelayAndType is firstSendDelay plus the message type of the
// packet it stopped on. The type is what proves which path the machine
// took: INIT-REBOOT sends a DHCPREQUEST, INIT sends a DHCPDISCOVER, and
// a test that only measured the delay could not tell a refused resume
// from a resume it never had.
func firstSendDelayAndType(t *testing.T, p proto.Params, rnd uint64) (proto.Duration, wire.MessageType) {
	t.Helper()
	m, err := proto.New(p)
	if err != nil {
		t.Fatalf("proto.New: %v", err)
	}
	now := proto.Instant(0)
	ev := proto.Simple(proto.EvStart)
	for step := 0; step < 20; step++ {
		_, acts := m.Step(now, rnd+uint64(step), ev)
		var next proto.Duration
		var armed bool
		for _, a := range acts {
			switch a.Kind {
			case proto.ActSend:
				if a.Msg == nil {
					t.Fatalf("ActSend with no message at %v", now)
				}
				mt, ok := a.Msg.Type()
				if !ok {
					t.Fatalf("the message sent at %v carries no option 53", now)
				}
				return proto.Duration(now), mt
			case proto.ActSetTimer:
				next, armed = a.After, true
			}
		}
		if !armed {
			t.Fatalf("no send and no timer at %v", now)
		}
		now += proto.Instant(next)
		ev = proto.TimerFired(lastTimer(acts))
	}
	t.Fatal("no send within 20 steps")
	return 0, 0
}

// firstSendDelay drives one machine from EvStart to its first send and
// returns the simulated delay before it.
func firstSendDelay(t *testing.T, p proto.Params, rnd uint64) proto.Duration {
	t.Helper()
	m, err := proto.New(p)
	if err != nil {
		t.Fatalf("proto.New: %v", err)
	}
	now := proto.Instant(0)
	ev := proto.Simple(proto.EvStart)
	for step := 0; step < 20; step++ {
		_, acts := m.Step(now, rnd+uint64(step), ev)
		var next proto.Duration
		var armed bool
		for _, a := range acts {
			switch a.Kind {
			case proto.ActSend:
				return proto.Duration(now)
			case proto.ActSetTimer:
				next, armed = a.After, true
			}
		}
		if !armed {
			t.Fatalf("no send and no timer at %v", now)
		}
		now += proto.Instant(next)
		ev = proto.TimerFired(lastTimer(acts))
	}
	t.Fatal("no send within 20 steps")
	return 0
}

// TestBuildParams_TheVendorClassDefaultIsTheChassisS is seam D-2, and
// the distinction the existing integration test cannot make.
//
// An empty vendor_class has always meant option 60 = "docker-net-dhcp"
// on the wire. An empty proto.Params.VendorClass means NO OPTION 60 AT
// ALL. Both leave a container on the untagged pool, so every test that
// asserts on the address it got passes either way; what differs is
// what a server keyed on the class sees, and that is only visible in
// the server's own log.
func TestBuildParams_TheVendorClassDefaultIsTheChassisS(t *testing.T) {
	mac := testMAC(t)

	p, err := buildParams(&DHCPClientOptions{MAC: mac}, true)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if p.VendorClass != VendorID {
		t.Errorf("with no vendor_class set, option 60 is %q, want %q. Empty means the option "+
			"is not sent at all, which no test asserting on an address can tell apart from "+
			"the default being sent.", p.VendorClass, VendorID)
	}

	p, err = buildParams(&DHCPClientOptions{MAC: mac, VendorClass: "acme"}, true)
	if err != nil {
		t.Fatalf("buildParams(override): %v", err)
	}
	if p.VendorClass != "acme" {
		t.Errorf("the operator's vendor_class was replaced by %q", p.VendorClass)
	}
}

// TestClientIdentity_CarriesTheTypeByte is seam D-3/D10.
//
// RFC 2132 section 9.14 makes option 61 a type byte followed by the
// value; type 0 is "not a hardware address / opaque". The library
// sends Params.ClientID verbatim, so the byte is the chassis's to add.
// Dropping it does not fail anything visible: the client simply has a
// different identity, the server files a second binding, and the
// container gets an address that is not the one the record remembers.
//
// The record stores what this returns for the same reason — a record
// of a different client than the server has is a record that only
// disagrees when a restart needs it.
func TestClientIdentity_CarriesTheTypeByte(t *testing.T) {
	got := ClientIdentity([]byte{0xde, 0xad})
	want := []byte{0x00, 0xde, 0xad}
	if len(got) != len(want) {
		t.Fatalf("ClientIdentity gave %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ClientIdentity gave %v, want %v", got, want)
		}
	}

	// Empty is not "type 0 and nothing": it is no option 61 at all,
	// and the server then keys on the chaddr. A lone type byte would
	// be a third identity, distinct from both.
	if n := len(ClientIdentity(nil)); n != 0 {
		t.Errorf("ClientIdentity(nil) is %d byte(s); an empty client-id must send no option 61", n)
	}
}

// TestClientIDPayload_IsClientIdentitysInverse.
//
// A re-bind reads the identity a record carries and has to put it back
// on the wire; DHCPClientOptions.ClientID is the payload without the
// type byte, so one form has to be derived from the other and a
// derivation that drifts sends a client-id no record describes.
func TestClientIDPayload_IsClientIdentitysInverse(t *testing.T) {
	payload := []byte{0xde, 0xad, 0xbe, 0xef}
	got, ok := ClientIDPayload(ClientIdentity(payload))
	if !ok {
		t.Fatal("the chassis's own identity was refused by the inverse of the function that built it")
	}
	if len(got) != len(payload) {
		t.Fatalf("round trip gave %v, want %v", got, payload)
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("round trip gave %v, want %v", got, payload)
		}
	}

	for _, c := range []struct {
		name     string
		identity []byte
	}{
		{"nothing", nil},
		{"a type byte with no payload", []byte{0x00}},
		{"a shape this chassis does not write", []byte{0xff, 1, 2}},
	} {
		if _, ok := ClientIDPayload(c.identity); ok {
			t.Errorf("%s was accepted; trimming its first byte would put a value on the wire "+
				"that no record says, which is the failure the type byte exists to prevent", c.name)
		}
	}
}

// TestBuildParams_TheBroadcastFlagReachesTheWire is the fifth seam rule,
// and the only one of the five that was already broken when it was
// written.
//
// WHAT IT PINS. proto.DefaultParams sets Params.Broadcast TRUE, and the
// library's own doc comment gives the reason: the BROADCAST flag of RFC
// 2131 section 2 exists for "a client that cannot receive unicast IP
// datagrams until its protocol software has been configured with an IP
// address", and ring 3 is a raw AF_PACKET socket on an unconfigured
// interface for EVERY mode -- there is one transport, chosen nowhere.
// The library states the consequence of clearing it too: "a client that
// works against servers ignoring the flag and hangs against those
// honouring it".
//
// WHY IT BROKE. buildParams ended with `p.Broadcast = opts.Broadcast`,
// and both plugin call sites passed `mode == ModeIPvlan`. That
// expression is correct 1.x code: under dhcpcd it ADDED the flag for
// ipvlan, whose slaves share the parent MAC, on top of dhcpcd's own
// behaviour (#243). Carried across the seam onto a default of true it
// INVERTED -- bridge and macvlan cleared a flag their transport
// requires. Nothing failed, because the option's name and value were
// unchanged on both sides of the swap; only the meaning of `false`
// moved.
//
// WHY NO FIXTURE RUN COULD HAVE DECIDED IT. The brief asks for this row
// to be settled by one measured fixture run. It cannot be. dnsmasq and
// Kea both answer an unconfigured client whether or not the flag is
// set, so the suite is green either way -- and it WAS green, on every
// mode, with the flag cleared. A measurement whose two arms produce the
// same observation is not a measurement of that variable. The oracle
// here is the library's documented contract with its own transport, so
// the test drives the flag onto the wire instead.
func TestBuildParams_TheBroadcastFlagReachesTheWire(t *testing.T) {
	mac := testMAC(t)

	for _, tc := range []struct {
		name string
		once bool
	}{
		{"the CreateEndpoint one-shot", true},
		{"the Join manager", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := buildParams(&DHCPClientOptions{MAC: mac}, tc.once)
			if err != nil {
				t.Fatalf("buildParams: %v", err)
			}
			if !p.Broadcast {
				t.Fatal("the chassis cleared Params.Broadcast. The library sets it true in " +
					"DefaultParams for a raw-socket client and documents that clearing it " +
					"hangs against any server that honours the flag; the fixture cannot " +
					"see the difference, so nothing downstream of here will catch this.")
			}
		})
	}

	// The field is not the wire. Driven through the machine so that a
	// library that stopped honouring Params.Broadcast is caught here
	// rather than in production: the one-shot has no desync, so EvStart
	// sends the DISCOVER in the same step.
	p, err := buildParams(&DHCPClientOptions{MAC: mac}, true)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	m, err := proto.New(p)
	if err != nil {
		t.Fatalf("proto.New: %v", err)
	}
	_, acts := m.Step(0, 0, proto.Simple(proto.EvStart))

	var sent int
	for _, a := range acts {
		if a.Kind != proto.ActSend || a.Msg == nil {
			continue
		}
		sent++
		mt, _ := a.Msg.Type()
		if a.Msg.Flags&wire.FlagBroadcast == 0 {
			t.Errorf("%s went out with flags %#04x; the BROADCAST bit (%#04x) is clear, so a "+
				"server honouring it will unicast the reply to an address this client does "+
				"not have yet", mt, a.Msg.Flags, wire.FlagBroadcast)
		}
	}
	if sent == 0 {
		t.Fatal("EvStart emitted no ActSend, so the flag assertion above judged nothing. " +
			"The one-shot's desync is zero and the DISCOVER is supposed to go out in this " +
			"same step; a test that passes here having sent nothing is the failure this " +
			"repository keeps meeting.")
	}
}

// TestBuildParams_Option81NeedsTheNameAtConstruction is the reason the
// attach still waits for the name on a register_dns network (#961).
//
// register_dns asks the server to register the container in DNS, which
// RFC 4702 encodes as option 81, and option 81 is built HERE from the
// name the client is constructed with. There is no setter for it: a
// client started with no name carries no option 81 for its whole life,
// and the late SetHostname that #961 added would put the name in option
// 12 instead -- a different option, which section 3.1 forbids beside
// option 81 and which asks the server for nothing at all. Nothing in
// the tree reads option 81, so the swap would be invisible.
//
// The second half is the drive. Without it this is a test that
// register_dns works, which it was before #961 too.
func TestBuildParams_Option81NeedsTheNameAtConstruction(t *testing.T) {
	mac := testMAC(t)

	p, err := buildParams(&DHCPClientOptions{MAC: mac, Hostname: "web1", FQDN: "both"}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if p.FQDN.Name != "web1" {
		t.Errorf("option 81 carries %q, want %q: register_dns is the opt-in to a DNS registration and "+
			"this is the only place the name reaches it", p.FQDN.Name, "web1")
	}

	p, err = buildParams(&DHCPClientOptions{MAC: mac, FQDN: "both"}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if p.FQDN.Name != "" {
		t.Errorf("option 81 carries %q for a client built with no name, want it absent. If this ever "+
			"became a name the attach could fill in afterwards, the pre-start wait on a register_dns "+
			"network in pkg/plugin could go away with it", p.FQDN.Name)
	}
}

// TestSetHostname_RefusesWhatItCannotSend covers the two answers this
// chassis gives without asking the library (#961).
//
// Both are silent if they are not refusals. A v6 client sends no name
// option at all in this library, and a client whose socket was never
// opened has no manager to queue the request on; either one returning
// nil would tell the attach the server had been told, and the attach
// counts that as a name delivered.
func TestSetHostname_RefusesWhatItCannotSend(t *testing.T) {
	mac := testMAC(t)

	ns, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get: %v", err)
	}
	defer func() { _ = ns.Close() }()

	v6, err := NewDHCPClient("eth0", &DHCPClientOptions{
		MAC:                mac,
		V6:                 true,
		NetNS:              &ns,
		HonorRouterAdverts: true,
		Identity6:          Identity6{DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xc0, 0xa8, 0x63, 0x07}, IAID: 0xc0a86307},
	})
	if err != nil {
		t.Fatalf("NewDHCPClient v6: %v", err)
	}
	err = v6.SetHostname("web1")
	if !errors.Is(err, lease.ErrHostnameV6) {
		t.Errorf("SetHostname on a v6 client = %v, want lease.ErrHostnameV6: this library sends no name "+
			"option for DHCPv6, so a nil here is a name the caller believes went out", err)
	}

	v4, err := NewDHCPClient("eth0", &DHCPClientOptions{MAC: mac})
	if err != nil {
		t.Fatalf("NewDHCPClient v4: %v", err)
	}
	if err := v4.SetHostname("web1"); !errors.Is(err, ErrNoRunningClient) {
		t.Errorf("SetHostname before Start = %v, want ErrNoRunningClient: nothing is opened until "+
			"Start, so the name would be dropped by a client that does not exist yet", err)
	}
}
