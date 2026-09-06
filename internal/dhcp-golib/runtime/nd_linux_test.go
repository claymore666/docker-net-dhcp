//go:build linux

package runtime

import (
	"bytes"
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/wire"
)

// The two hardware addresses the captured frames were sent between. cliMAC is
// this client's; srvMAC is the peer's. Both are read out of the fixture rather
// than invented, so a frame and the address it claims to come from cannot
// drift apart.
var (
	cliMAC = net.HardwareAddr{0x3a, 0x02, 0x5e, 0xe7, 0xdf, 0xe0}
	srvMAC = net.HardwareAddr{0x32, 0xe6, 0xf9, 0x2f, 0xaa, 0x1e}
)

// testNDSocket is an NDSocket with no socket behind it.
//
// Every method under test here reads the hardware address and the counters and
// nothing else, so the descriptor is the one field that can be left out — and
// leaving it out is what makes these rows run in the pure suite instead of
// costing a network namespace. The channels are real, because the drop
// behaviour is one of the things being measured.
func testNDSocket(hw net.HardwareAddr, depth int) *NDSocket {
	return &NDSocket{
		hw:      hw,
		inbound: make(chan lease.NDInbound, depth),
		frames:  make(chan NDFrame, depth),
	}
}

// TestTheNDSocketMakesTheFourChecksItOwnsAndCountsEachApart is defeat rows N-1,
// N-3, N-4 and N-5 in one table.
//
// RFC 4861 section 6.1.2 lists the validity conditions for a Router
// Advertisement, and three of them cannot be checked by the codec because
// their evidence is outside the ICMPv6 body: "The IP Hop Limit field has a
// value of 255, i.e., the packet could not possibly have been forwarded by a
// router", "ICMP Checksum is valid", and "IP Source Address is a link-local
// address". This socket makes those three, and narrows to the four Neighbor
// Discovery types besides.
//
// EACH REFUSAL LANDS ON ITS OWN COUNTER, and that is the point of the table
// rather than a detail of it. "The socket saw nothing" and "the socket saw
// eleven frames and refused all of them" are opposite facts, and one counter
// holding both reports the second as the first — which on a real link is the
// difference between "no router here" and "something is forwarding Neighbor
// Discovery at us".
func TestTheNDSocketMakesTheFourChecksItOwnsAndCountsEachApart(t *testing.T) {
	mutate := func(frame []byte, f func([]byte)) []byte {
		c := bytes.Clone(frame)
		f(c)
		return c
	}
	for _, tc := range []struct {
		name     string
		frame    []byte
		wantOK   bool
		wantType uint8
		counter  func(NDStats) uint64
	}{
		{
			name:     "a real Router Advertisement",
			frame:    capV6RouterAdvert,
			wantOK:   true,
			wantType: wire.ICMPv6RouterAdvert,
		},
		{
			name:     "a real Neighbor Solicitation",
			frame:    capV6NeighborSolicit,
			wantOK:   true,
			wantType: wire.ICMPv6NeighborSolicit,
		},
		{
			name:     "a real Neighbor Advertisement",
			frame:    capV6NeighborAdvert,
			wantOK:   true,
			wantType: wire.ICMPv6NeighborAdvert,
		},
		{
			// A real frame, valid ICMPv6, and not Neighbor Discovery. On a
			// shared link most of what an ETH_P_IPV6 socket reads is like
			// this.
			name:    "an ICMPv6 Destination Unreachable",
			frame:   capV6Unreachable,
			counter: func(s NDStats) uint64 { return s.Skipped },
		},
		{
			name:    "a DHCPv6 datagram",
			frame:   capV6Advertise,
			counter: func(s NDStats) uint64 { return s.Skipped },
		},
		{
			// Section 6.1.2: the hop limit check is what makes a Neighbor
			// Discovery message unforgeable from off-link, so a frame failing
			// it is the attack the check exists for and not noise.
			name:    "a Router Advertisement with hop limit 64",
			frame:   mutate(capV6RouterAdvert, func(b []byte) { b[7] = 64 }),
			counter: func(s NDStats) uint64 { return s.BadHopLimit },
		},
		{
			name:    "a Router Advertisement whose checksum was corrupted",
			frame:   mutate(capV6RouterAdvert, func(b []byte) { b[40+2] ^= 0xFF }),
			counter: func(s NDStats) uint64 { return s.BadChecksum },
		},
		{
			// Section 6.1.2: "Routers must use their link-local address as the
			// source for Router Advertisement and Redirect messages so that
			// hosts can uniquely identify routers."
			name: "a Router Advertisement from a global source address",
			frame: mutate(capV6RouterAdvert, func(b []byte) {
				g := netip.MustParseAddr("fd00:99::1").As16()
				copy(b[8:24], g[:])
				// and re-checksum, so the row is refused for its SOURCE and
				// not for the checksum the edit would otherwise break. The
				// difference between those two is the whole point of the
				// separate counters.
				sum := wire.ICMPv6Checksum(netip.AddrFrom16([16]byte(b[8:24])), netip.AddrFrom16([16]byte(b[24:40])), b[40:])
				b[40+2], b[40+3] = byte(sum>>8), byte(sum)
			}),
			counter: func(s NDStats) uint64 { return s.BadSource },
		},
		{
			// The SAME check must NOT fire on a Neighbor Solicitation from the
			// unspecified address, which RFC 4862 section 5.4.2 requires. A
			// socket that applied the link-local rule to every type would
			// throw away every duplicate-address probe on the link, and the
			// duplicate check would then report every address free.
			name:     "a duplicate-address Neighbor Solicitation from ::",
			frame:    dadSolicitFrame(t, netip.MustParseAddr("fd00:99::1a3")),
			wantOK:   true,
			wantType: wire.ICMPv6NeighborSolicit,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testNDSocket(cliMAC, 4)
			f, ok := s.classify(tc.frame, srvMAC)
			if ok != tc.wantOK {
				t.Fatalf("classify ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK {
				if f.Type != tc.wantType {
					t.Errorf("Type = %d, want %d", f.Type, tc.wantType)
				}
				if st := s.Stats(); st.Skipped+st.BadChecksum+st.BadHopLimit+st.BadSource != 0 {
					t.Errorf("an accepted frame moved a refusal counter: %+v", st)
				}
				return
			}
			if got := tc.counter(s.Stats()); got != 1 {
				t.Errorf("the refusal did not land on its own counter: %+v", s.Stats())
			}
		})
	}
}

// dadSolicitFrame builds the frame a node running RFC 4862 section 5.4's
// duplicate address detection puts on the link, as an IPv6 packet.
func dadSolicitFrame(t *testing.T, target netip.Addr) []byte {
	t.Helper()
	pkt, err := wire.EncodeDADNeighborSolicit(target)
	if err != nil {
		t.Fatalf("EncodeDADNeighborSolicit: %v", err)
	}
	frame, err := BuildIPv6ICMP(pkt)
	if err != nil {
		t.Fatalf("BuildIPv6ICMP: %v", err)
	}
	return frame
}

// TestThisHostsOwnFrameIsCountedAndKeptOffTheLeasePort drives the branch that
// nothing on this link can reach.
//
// NDSocket's BOUNDS record the measurement: a socket bound to ETH_P_IPV6 does
// not read back this host's own transmissions, so no netns proof in this
// package can make Own move. The branch is kept because RFC 4862 section 5.4.3
// requires it and because a repeater on the link would deliver such a frame as
// an ordinary incoming one — and a branch that only exists is a branch that
// only compiles. This is where it runs.
//
// TWO ASSERTIONS AND THE SECOND IS THE ONE THAT MATTERS. The frame is still
// delivered to Frames(), because the duplicate-address runner has to see it in
// order to ignore it for the right reason; it is NOT delivered to the lease.ND
// port, because ring 1 has nothing to learn from this client's own Router
// Solicitation coming back and every event that reaches Step costs a bounded
// journal entry.
func TestThisHostsOwnFrameIsCountedAndKeptOffTheLeasePort(t *testing.T) {
	s := testNDSocket(cliMAC, 4)

	s.deliver(capV6NeighborAdvert, cliMAC)
	if st := s.Stats(); st.Own != 1 || st.Reads != 1 {
		t.Fatalf("Own = %d, Reads = %d, want 1 and 1: %+v", st.Own, st.Reads, st)
	}
	select {
	case in := <-s.inbound:
		t.Errorf("this host's own frame reached the lease.ND port: % x", in.Frame)
	default:
	}
	select {
	case f := <-s.frames:
		if f.Type != wire.ICMPv6NeighborAdvert {
			t.Errorf("Frames() delivered type %d", f.Type)
		}
	default:
		t.Errorf("this host's own frame did not reach the duplicate-address runner, which has to see it to ignore it")
	}

	// The control: the same frame from somebody else goes to both.
	s2 := testNDSocket(cliMAC, 4)
	s2.deliver(capV6NeighborAdvert, srvMAC)
	if st := s2.Stats(); st.Own != 0 {
		t.Errorf("Own = %d on a frame from another node", st.Own)
	}
	if len(s2.inbound) != 1 || len(s2.frames) != 1 {
		t.Errorf("another node's frame reached %d consumer(s) on the port and %d on the runner, want 1 and 1", len(s2.inbound), len(s2.frames))
	}
}

// TestTheNDSocketDropsWhenAConsumerStalls drives the bound the Dropped counter
// exists for.
//
// A shared link carries Neighbor Discovery continuously and both channels are
// bounded, so a consumer that stops draining loses frames. Blocking instead
// would stall the reader and lose them in the kernel, where nothing can count
// them. Dropped above zero means a duplicate address COULD have gone unseen,
// which is why it is a number and not a log line.
func TestTheNDSocketDropsWhenAConsumerStalls(t *testing.T) {
	s := testNDSocket(cliMAC, 1)
	for range 3 {
		s.deliver(capV6RouterAdvert, srvMAC)
	}
	st := s.Stats()
	if st.Reads != 3 {
		t.Fatalf("Reads = %d, want 3", st.Reads)
	}
	// Two channels, each holding one, so the second and third frame are
	// dropped twice over: once per channel.
	if st.Dropped != 4 {
		t.Errorf("Dropped = %d, want 4 (two frames × two stalled channels)", st.Dropped)
	}
	if st.Skipped != 0 {
		t.Errorf("Skipped = %d; a dropped frame is not a skipped one and merging them reports a stalled consumer as an empty link", st.Skipped)
	}
}

// TestTheMulticastMACIsRFC2464s pins the link-layer address every message this
// socket sends is addressed to.
//
// RFC 2464 section 7: "An IPv6 packet with a multicast destination address DST,
// consisting of the sixteen octets DST[1] through DST[16], is transmitted to
// the Ethernet multicast address whose first two octets are the value 3333
// hexadecimal and whose last four octets are the last four octets of DST."
//
// It is worth a row of its own because it is the reason this transport needs
// no neighbour resolution at all: the mapping is a pure function, so there is
// nothing to look up and nothing to get stale. Get it wrong and every frame is
// sent to a group nobody is in — which on a switch looks exactly like a link
// that is up and silent.
func TestTheMulticastMACIsRFC2464s(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want net.HardwareAddr
	}{
		{"ff02::1:2", net.HardwareAddr{0x33, 0x33, 0x00, 0x01, 0x00, 0x02}},
		{"ff02::1", net.HardwareAddr{0x33, 0x33, 0x00, 0x00, 0x00, 0x01}},
		{"ff02::2", net.HardwareAddr{0x33, 0x33, 0x00, 0x00, 0x00, 0x02}},
		{"ff02::1:ffe7:dfe0", net.HardwareAddr{0x33, 0x33, 0xff, 0xe7, 0xdf, 0xe0}},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			got := multicastMAC(netip.MustParseAddr(tc.addr))
			if !bytes.Equal(got, tc.want) {
				t.Errorf("multicastMAC(%s) = %s, want %s", tc.addr, got, tc.want)
			}
		})
	}
}

// TestTheNDSocketRefusesAUnicastDestination drives the refusal that stands in
// for the neighbour resolution this socket does not do.
//
// Every message this library sends on it is multicast by the RFC's own
// instruction, so the link-layer address is a pure function of the IPv6 one. A
// unicast destination would need an address this socket has never learned, and
// the alternatives to refusing are both worse: guessing, or broadcasting a
// message addressed to one node.
func TestTheNDSocketRefusesAUnicastDestination(t *testing.T) {
	s := testNDSocket(cliMAC, 1)
	pkt, err := wire.EncodeDADNeighborSolicit(netip.MustParseAddr("fd00:99::1a3"))
	if err != nil {
		t.Fatalf("EncodeDADNeighborSolicit: %v", err)
	}
	pkt.Dst = netip.MustParseAddr("fd00:99::1")
	if err := s.Send(pkt); !errors.Is(err, ErrNDNotMulticast) {
		t.Fatalf("Send err = %v, want ErrNDNotMulticast", err)
	}
}
