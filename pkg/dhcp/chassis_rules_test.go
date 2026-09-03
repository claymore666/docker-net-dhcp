// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
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
func TestTranslate_AStopIsNotALeaseLoss(t *testing.T) {
	now := time.Now()

	for _, tc := range []struct {
		name     string
		reason   proto.Reason
		wantEmit bool
		wantType string
	}{
		{"the chassis cancelling its own manager", proto.ReasonStopped, false, ""},
		{"a server NAK", proto.ReasonNak, true, "nak"},
		{"the lease expiring", proto.ReasonExpired, true, "leasefail"},
		{"the link going down", proto.ReasonLinkDown, true, "leasefail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, emit, _ := translateOne(
				lease.Event{Kind: lease.Lost, Reason: tc.reason}, now, time.Time{})
			if emit != tc.wantEmit {
				t.Fatalf("Lost{%v}: emit=%v, want %v. A stop reported as a loss makes every "+
					"successful container start look like a lease failure; a real loss "+
					"swallowed makes the plugin silent about an address the container no "+
					"longer holds.", tc.reason, emit, tc.wantEmit)
			}
			if emit && out.Type != tc.wantType {
				t.Errorf("Lost{%v}: emitted %q, want %q", tc.reason, out.Type, tc.wantType)
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

	_, emit, renewedAt := translateOne(lease.Event{Kind: lease.Renewed, Lease: l}, now, time.Time{})
	if !emit {
		t.Fatal("a Renewed emitted nothing; the plugin would never see a renewal at all")
	}

	if _, emit, _ := translateOne(
		lease.Event{Kind: lease.Changed, Lease: l},
		now.Add(coalesceWindow/2), renewedAt); emit {
		t.Error("the Changed that accompanied the same DHCPACK was emitted as a second renewal")
	}

	if _, emit, _ := translateOne(
		lease.Event{Kind: lease.Changed, Lease: l},
		now.Add(10*coalesceWindow), renewedAt); !emit {
		t.Error("a Changed well outside the window was swallowed. That is a re-acquisition on a " +
			"different address — a NAK and a new lease — and the container is left configured " +
			"with the old one.")
	}
}

// TestBuildParams_TheAcquisitionManagerDoesNotDesync is D-1, with the
// cost of getting it wrong MEASURED rather than asserted.
//
// RFC 2131 section 4.4.1 says a client SHOULD wait one to ten seconds
// before its first DISCOVER, to desynchronise a fleet of hosts booting
// together. The CreateEndpoint one-shot is one container asking for one
// address inside a `docker run`, against a lease_timeout that defaults
// to ten seconds. The delay does not spread load there; it eats the
// budget, and the resulting failure is INTERMITTENT — the shape that
// gets diagnosed as a flaky harness and skipped rather than fixed.
//
// The Join manager keeps the desync: it is not on anyone's critical
// path, and it is the one manager of which there may be many starting
// at once, after a plugin restart.
func TestBuildParams_TheAcquisitionManagerDoesNotDesync(t *testing.T) {
	mac := testMAC(t)

	once, err := buildParams(&DHCPClientOptions{MAC: mac}, true)
	if err != nil {
		t.Fatalf("buildParams(once): %v", err)
	}
	if once.DesyncMin != 0 || once.DesyncMax != 0 {
		t.Errorf("the CreateEndpoint one-shot desyncs by %v..%v; the library documents both "+
			"zero as the disabling value", once.DesyncMin, once.DesyncMax)
	}

	persistent, err := buildParams(&DHCPClientOptions{MAC: mac}, false)
	if err != nil {
		t.Fatalf("buildParams(persistent): %v", err)
	}
	if persistent.DesyncMin == 0 && persistent.DesyncMax == 0 {
		t.Error("the Join manager's desync was disabled too. Nothing on the CreateEndpoint " +
			"deadline is served by that, and it is the manager of which a plugin restart " +
			"starts many at once — the case section 4.4.1 is actually about.")
	}

	// MEASURED, and the reason this is not an argument.
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
	const (
		budget      = 10 * proto.Second
		oneRetrans  = 4 * proto.Second // RFC 2131 4.1's first retransmission
		drawsWanted = 1000
	)
	var tight, n int
	var worst proto.Duration
	for i := 0; i < drawsWanted; i++ {
		d := firstSendDelay(t, persistent, uint64(i)*0x9e3779b97f4a7c15+1)
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
