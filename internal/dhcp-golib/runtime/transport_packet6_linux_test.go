//go:build linux

package runtime

import (
	"bytes"
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/lease"
)

// testTransportV6 is a PacketTransportV6 with no socket behind it.
//
// deliver reads the source address and the counters and nothing else, so the
// descriptor is the one field that can be left out — and leaving it out is
// what makes these rows run in the pure suite instead of costing a network
// namespace. The channel is real, because being delivered or not is half of
// what each row asserts.
func testTransportV6(t *testing.T, src netip.Addr, depth int) *PacketTransportV6 {
	t.Helper()
	return &PacketTransportV6{src: src, inbound: make(chan lease.Inbound, depth)}
}

// TestTheV6TransportCountsEachRefusalApart is the transport's own refusal
// table, and it is the shape TestTheNDSocketMakesTheFourChecksItOwnsAndCountsEachApart
// already has for NDStats.
//
// THREE OF THE FOUR REFUSALS HAD NO OBSERVER AT ALL before this row existed.
// TransportStatsV6.ZeroChecksum and .BadChecksum are exported, are documented
// as the reason "a server sending unchecksummed datagrams produces a client
// that never acquires and has something to point at", and nothing in the tree
// ever drove either above zero — MEASURED: folding both arms into
// t.skipped.Add(1) left the whole package green, netns proofs included,
// because the two refusals themselves are closed one ring down at
// ParseIPv6UDP and the transport's job is only to say WHICH refusal it was.
//
// AND THAT IS THE WHOLE POINT OF SEPARATE COUNTERS. "Nothing on this link was
// for us" and "a reply for us arrived and was thrown away" are opposite facts
// about a client that is not acquiring: the first sends a reader to the
// server, the second to the path between them. One counter holding both
// reports the second as the first.
func TestTheV6TransportCountsEachRefusalApart(t *testing.T) {
	ours, err := ParseIPv6UDP(capV6Advertise)
	if err != nil {
		t.Fatalf("the captured Advertise does not parse: %v", err)
	}
	// The address the capture was actually sent to. Read out of the frame
	// rather than written down here, so the fixture and the subject cannot
	// disagree about which client this reply is for.
	us := ours.Dst
	them := netip.MustParseAddr("fe80::aabb:ccff:fe11:2233")

	mutate := func(frame []byte, f func([]byte)) []byte {
		c := bytes.Clone(frame)
		f(c)
		return c
	}
	// A well-formed Reply for ANOTHER client on the same link: same server,
	// same ports, a complete checksum over its own pseudo-header, and a
	// destination that is not ours. Built rather than mutated, because a
	// mutated destination with the capture's checksum would be refused for
	// the checksum and the row would prove nothing about the destination.
	foreign, err := BuildIPv6UDP(ours.Src, them, ServerPort6, ClientPort6, dhcpHopLimit, ours.Payload)
	if err != nil {
		t.Fatalf("building another client's reply: %v", err)
	}

	for _, tc := range []struct {
		name      string
		frame     []byte
		delivered bool
		counter   func(TransportStatsV6) uint64
	}{
		{
			name:      "the captured Advertise, addressed to this client",
			frame:     capV6Advertise,
			delivered: true,
		},
		{
			// RFC 8200 section 8.1: "IPv6 receivers must discard UDP packets
			// containing a zero checksum and should log the error." Discarded
			// AND logged, in the only form this library has for a log.
			name:    "a reply for this client carrying a zero checksum",
			frame:   mutate(capV6Advertise, func(b []byte) { b[40+6], b[40+7] = 0, 0 }),
			counter: func(s TransportStatsV6) uint64 { return s.ZeroChecksum },
		},
		{
			name:    "a reply for this client whose checksum was corrupted",
			frame:   mutate(capV6Advertise, func(b []byte) { b[40+6] ^= 0xFF }),
			counter: func(s TransportStatsV6) uint64 { return s.BadChecksum },
		},
		{
			// The finding this narrowing came from: on a shared bridge every
			// other container's Advertise and Reply satisfies ParseIPv6UDP's
			// two port checks, and used to be carried to ring 1 to be
			// discarded there on the Client Identifier.
			name:    "another client's reply on the same link",
			frame:   foreign,
			counter: func(s TransportStatsV6) uint64 { return s.Foreign },
		},
		{
			// Ordinary link traffic. On a shared link nearly all of it.
			name:    "a Router Advertisement",
			frame:   capV6RouterAdvert,
			counter: func(s TransportStatsV6) uint64 { return s.Skipped },
		},
		{
			// A frame that is not IPv6 at all, which is what an ETH_P_IPV6
			// socket must never see and what a transport bound to the wrong
			// EtherType would see nothing but.
			name:    "sixty octets of nothing",
			frame:   make([]byte, 60),
			counter: func(s TransportStatsV6) uint64 { return s.Skipped },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := testTransportV6(t, us, 4)
			tr.deliver(tc.frame)

			st := tr.Stats()
			if st.Reads != 1 {
				t.Errorf("TransportStatsV6.Reads = %d after one frame; Reads is a barrier and every frame must move it", st.Reads)
			}
			refused := st.Skipped + st.ZeroChecksum + st.BadChecksum + st.Foreign
			select {
			case in := <-tr.Received():
				if !tc.delivered {
					t.Fatalf("a frame this transport must discard was delivered: %d octet(s) from %s", len(in.Payload), in.From)
				}
				if !bytes.Equal(in.Payload, ours.Payload) {
					t.Errorf("the delivered payload is %d octet(s), the capture's is %d", len(in.Payload), len(ours.Payload))
				}
				if in.From != ours.Src {
					t.Errorf("the delivered datagram says it came from %s, the capture says %s", in.From, ours.Src)
				}
				if refused != 0 {
					t.Errorf("an accepted frame moved a refusal counter: %+v", st)
				}
				return
			default:
			}
			if tc.delivered {
				t.Fatalf("the frame was not delivered: %+v", st)
			}
			if got := tc.counter(st); got != 1 {
				t.Errorf("the refusal did not land on its own counter: %+v", st)
			}
			if refused != 1 {
				t.Errorf("one frame moved %d refusal counters: %+v", refused, st)
			}
			if st.Dropped != 0 {
				t.Errorf("TransportStatsV6.Dropped = %d; nothing was queued, so nothing can have been dropped", st.Dropped)
			}
		})
	}
}

// TestAnUncompletedChecksumIsCountedOnlyForThisClient pins the ORDER of the
// two things deliver does to an accepted datagram.
//
// Uncompleted is documented as "accepted datagrams whose checksum field held
// the pseudo-header sum alone", and a client on a link with other DHCPv6
// clients would otherwise count every one of THEIR locally generated replies
// as an unchecked payload of its own — which is the counter a reader consults
// to decide whether the frames it acted on were verified.
func TestAnUncompletedChecksumIsCountedOnlyForThisClient(t *testing.T) {
	us := netip.MustParseAddr("fe80::1")
	them := netip.MustParseAddr("fe80::2")
	srv := netip.MustParseAddr("fe80::ff")

	// A datagram in Linux's CHECKSUM_PARTIAL state: the folded pseudo-header
	// sum in the checksum field, the completion deferred. Built here because
	// it is what a veth actually carries and what no capture can be edited
	// into without recomputing the very field under test.
	partial := func(dst netip.Addr) []byte {
		frame, err := BuildIPv6UDP(srv, dst, ServerPort6, ClientPort6, dhcpHopLimit, []byte{0x07, 0x11, 0x22, 0x33})
		if err != nil {
			t.Fatalf("BuildIPv6UDP: %v", err)
		}
		u := frame[40:]
		sum := pseudoHeaderSum6(srv.As16(), dst.As16(), len(u))
		u[6], u[7] = byte(sum>>8), byte(sum)
		return frame
	}

	tr := testTransportV6(t, us, 4)
	tr.deliver(partial(them))
	if st := tr.Stats(); st.Uncompleted != 0 {
		t.Errorf("TransportStatsV6.Uncompleted = %d after another client's unverified reply; the counter is about payloads this client acted on", st.Uncompleted)
	}
	tr.deliver(partial(us))
	if st := tr.Stats(); st.Uncompleted != 1 {
		t.Errorf("TransportStatsV6.Uncompleted = %d after this client's own unverified reply, want 1 — %+v", st.Uncompleted, st)
	}
}
