package proto

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/wire"
)

func ackMessage(opts wire.Options, yiaddr string) *wire.Message {
	m := &wire.Message{
		Op: wire.BootReply, HType: wire.HTypeEthernet, CHAddr: testCHAddr,
		Options: wire.Options{wire.OptMessageType: {byte(wire.MsgAck)}},
	}
	if yiaddr != "" {
		m.YIAddr = netip.MustParseAddr(yiaddr)
	}
	for c, v := range opts {
		m.Options[c] = v
	}
	return m
}

func TestLeaseFromAck(t *testing.T) {
	m := ackMessage(wire.Options{
		wire.OptSubnetMask:    {255, 255, 255, 0},
		wire.OptLeaseTime:     u32(3600),
		wire.OptServerID:      addr4("192.168.99.1"),
		wire.OptRouter:        append(addr4("192.168.99.1"), addr4("192.168.99.2")...),
		wire.OptDNSServer:     addr4("192.168.99.1"),
		wire.OptDomainName:    []byte("example.test"),
		wire.OptInterfaceMTU:  {0x05, 0xDC},
		wire.OptRenewalTime:   u32(1000),
		wire.OptRebindingTime: u32(2000),
	}, "192.168.99.50")

	l, note, ok := leaseFromAck(m, at(10))
	if !ok {
		t.Fatal("a complete ACK did not produce a lease")
	}
	if note != "" {
		t.Fatalf("unexpected anomaly note %q", note)
	}
	if l.Addr.String() != "192.168.99.50/24" {
		t.Fatalf("addr = %s", l.Addr)
	}
	if l.Start != at(10) {
		t.Fatalf("Start = %s, want the REQUEST send time", l.Start)
	}
	if l.LeaseTime != 3600*Second {
		t.Fatalf("lease time = %s", l.LeaseTime)
	}
	if len(l.Router) != 2 || l.Router[0].String() != "192.168.99.1" {
		t.Fatalf("routers = %v, want both from the concatenated option", l.Router)
	}
	if l.MTU != 1500 || l.Domain != "example.test" {
		t.Fatalf("mtu/domain = %d/%q", l.MTU, l.Domain)
	}
	// Server-supplied T1 and T2 must be used, not the defaults.
	if r, ok := l.RenewAt(); !ok || r != at(1010) {
		t.Fatalf("RenewAt = %v/%v, want the server's T1 at 1010s", r, ok)
	}
	if r, ok := l.RebindAt(); !ok || r != at(2010) {
		t.Fatalf("RebindAt = %v/%v, want the server's T2 at 2010s", r, ok)
	}
}

func TestLeaseDefaultsT1AndT2(t *testing.T) {
	// RFC 2131 section 4.4.5: "T1 defaults to (0.5 * duration_of_lease). T2
	// defaults to (0.875 * duration_of_lease)."
	m := ackMessage(wire.Options{
		wire.OptSubnetMask: {255, 255, 255, 0},
		wire.OptLeaseTime:  u32(800),
	}, "192.168.99.50")
	l, _, ok := leaseFromAck(m, 0)
	if !ok {
		t.Fatal("no lease")
	}
	if r, ok := l.RenewAt(); !ok || r != at(400) {
		t.Fatalf("T1 default = %v/%v, want 0.5 * 800s = 400s", r, ok)
	}
	if r, ok := l.RebindAt(); !ok || r != at(700) {
		t.Fatalf("T2 default = %v/%v, want 0.875 * 800s = 700s", r, ok)
	}
}

func TestLeaseRejectsIncompleteAcks(t *testing.T) {
	cases := []struct {
		name string
		msg  *wire.Message
	}{
		{"no yiaddr", ackMessage(wire.Options{wire.OptLeaseTime: u32(3600)}, "")},
		{"zero yiaddr", ackMessage(wire.Options{wire.OptLeaseTime: u32(3600)}, "0.0.0.0")},
		{"no lease time", ackMessage(wire.Options{wire.OptSubnetMask: {255, 255, 255, 0}}, "192.168.99.50")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := leaseFromAck(tc.msg, 0); ok {
				t.Fatal("an incomplete ACK produced a lease")
			}
		})
	}
}

func TestLeaseWithNoSubnetMaskIsAHostRoute(t *testing.T) {
	// No mask is not "assume /24". Guessing a prefix installs a route over
	// addresses the server never delegated.
	m := ackMessage(wire.Options{wire.OptLeaseTime: u32(3600)}, "192.168.99.50")
	l, _, ok := leaseFromAck(m, 0)
	if !ok {
		t.Fatal("no lease")
	}
	if l.Addr.Bits() != 32 {
		t.Fatalf("prefix = /%d with no subnet mask, want /32", l.Addr.Bits())
	}
}

func TestNonContiguousMaskIsJournalled(t *testing.T) {
	// 255.0.255.0 is not a prefix. The lease is kept as a host route and the
	// anomaly must be visible: a silent /32 is diagnosed months later as "the
	// plugin used the wrong prefix".
	m := ackMessage(wire.Options{
		wire.OptLeaseTime:  u32(3600),
		wire.OptSubnetMask: {255, 0, 255, 0},
	}, "192.168.99.50")
	l, note, ok := leaseFromAck(m, 0)
	if !ok {
		t.Fatal("no lease")
	}
	if l.Addr.Bits() != 32 {
		t.Fatalf("prefix = /%d, want /32", l.Addr.Bits())
	}
	if !strings.Contains(note, "not contiguous") {
		t.Fatalf("note = %q, want it to name the non-contiguous mask", note)
	}
}

func TestNonContiguousMaskReachesTheJournal(t *testing.T) {
	// The note is only worth producing if it actually gets recorded. Driven
	// through the machine so the wiring is measured, not the helper's return
	// value.
	m := machineIn(t, StateRequesting)
	req := &wire.Message{XID: m.xid, CHAddr: testCHAddr}
	ack := ackFor(req, "192.168.99.50", "192.168.99.1", 3600)
	ack.Options[wire.OptSubnetMask] = []byte{255, 0, 255, 0}

	_, acts := m.Step(at(5), 1, received(t, ack))
	found := false
	for _, a := range acts {
		if a.Kind == ActJournal && strings.Contains(a.Note, "not contiguous") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the mask anomaly never reached the journal: %v", RenderActions(acts))
	}
}

func TestMaskBits(t *testing.T) {
	cases := []struct {
		mask string
		bits int
		ok   bool
	}{
		{"0.0.0.0", 0, true},
		{"128.0.0.0", 1, true},
		{"255.0.0.0", 8, true},
		{"255.255.255.0", 24, true},
		{"255.255.255.252", 30, true},
		{"255.255.255.255", 32, true},
		{"255.0.255.0", 0, false},
		{"255.255.0.1", 0, false},
		{"0.0.0.1", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.mask, func(t *testing.T) {
			got, ok := maskBits(netip.MustParseAddr(tc.mask))
			if ok != tc.ok {
				t.Fatalf("maskBits(%s) ok = %v, want %v", tc.mask, ok, tc.ok)
			}
			if ok && got != tc.bits {
				t.Fatalf("maskBits(%s) = %d, want %d", tc.mask, got, tc.bits)
			}
		})
	}
}

func TestInfiniteLease(t *testing.T) {
	// RFC 2132 section 9.2's lease time is a 32-bit value and 0xFFFFFFFF is
	// the infinite lease. It must not become an expiry 136 years out that a
	// caller then arms a timer for.
	m := ackMessage(wire.Options{
		wire.OptLeaseTime:  u32(InfiniteSeconds),
		wire.OptSubnetMask: {255, 255, 255, 0},
	}, "192.168.99.50")
	l, _, ok := leaseFromAck(m, at(10))
	if !ok {
		t.Fatal("no lease")
	}
	if !l.LeaseTime.IsInfinite() {
		t.Fatalf("lease time = %s, want infinite", l.LeaseTime)
	}
	if _, has := l.Expire(); has {
		t.Fatal("an infinite lease reports an expiry")
	}
	if _, has := l.RenewAt(); has {
		t.Fatal("an infinite lease reports a renewal time")
	}
	if _, has := l.RebindAt(); has {
		t.Fatal("an infinite lease reports a rebinding time")
	}
}

func TestLeaseEqualIgnoresStartAndOptions(t *testing.T) {
	// A renewal of the same address produces a new Start and an identical
	// configuration. Comparing Start would make every renewal look like a
	// change and churn the interface.
	base := Lease{
		Addr:     netip.MustParsePrefix("192.168.99.50/24"),
		ServerID: netip.MustParseAddr("192.168.99.1"),
		DNS:      []netip.Addr{netip.MustParseAddr("192.168.99.1")},
		Domain:   "example.test", MTU: 1500,
		Start: at(1), LeaseTime: 3600 * Second,
		Options: wire.Options{wire.OptMessageType: {5}},
	}
	renewed := base
	renewed.Start = at(4000)
	renewed.Options = wire.Options{wire.OptMessageType: {5}, wire.OptHostName: []byte("x")}
	if !base.Equal(renewed) {
		t.Fatal("a renewal of the same address reads as a changed lease")
	}

	// And the opposite direction: everything Equal DOES compare must be able
	// to make it false. A comparison that ignored one of these would pass the
	// test above for the wrong reason.
	for _, tc := range []struct {
		name string
		mut  func(*Lease)
	}{
		{"address", func(l *Lease) { l.Addr = netip.MustParsePrefix("192.168.99.51/24") }},
		{"prefix length", func(l *Lease) { l.Addr = netip.MustParsePrefix("192.168.99.50/25") }},
		{"server id", func(l *Lease) { l.ServerID = netip.MustParseAddr("192.168.99.2") }},
		{"domain", func(l *Lease) { l.Domain = "other.test" }},
		{"mtu", func(l *Lease) { l.MTU = 9000 }},
		{"dns", func(l *Lease) { l.DNS = []netip.Addr{netip.MustParseAddr("8.8.8.8")} }},
		{"dns count", func(l *Lease) { l.DNS = nil }},
		{"router", func(l *Lease) { l.Router = []netip.Addr{netip.MustParseAddr("10.0.0.1")} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := base
			tc.mut(&other)
			if base.Equal(other) {
				t.Fatalf("a changed %s reads as the same lease", tc.name)
			}
		})
	}
}
