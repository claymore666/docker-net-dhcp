//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// ErrNotAllServers is returned for a destination that is not RFC 9915 section
// 7.1's All_DHCP_Relay_Agents_and_Servers.
//
// Section 16: "A client uses multicast to reach all servers or an individual
// server." and section 7.1 names the group. RFC 9915 removed RFC 3315's Server
// Unicast option, so a conformant client has no unicast destination to send
// to — see lease.TransportV6. A destination this transport does not recognise
// is refused rather than sent, because the alternative is a message this
// client believes it delivered arriving nowhere.
var ErrNotAllServers = errors.New("runtime: DHCPv6 send needs All_DHCP_Relay_Agents_and_Servers (ff02::1:2)")

// PacketTransportV6 is the DHCPv6 counterpart of PacketTransport: an
// ETH_P_IPV6 AF_PACKET socket carrying UDP datagrams between the client port
// and the server port.
//
// IT IS AN AF_PACKET SOCKET AND NOT AN ORDINARY UDP ONE, which is the same
// choice PacketTransport makes and for a reason that survives the change of
// family. Binding a UDP socket to port 546 requires an address on the
// interface; the whole point of this client is to run before there is one, and
// RFC 4862 section 5.4's duplicate address detection has to finish before the
// address that a UDP socket would have needed may be used at all. One socket
// shape for both families also means one namespace contract (see NewClient6)
// rather than two.
//
// THE UDP CHECKSUM IS MANDATORY, both ways. RFC 8200 section 8.1: "Unlike IPv4,
// the default behavior when UDP packets are originated by an IPv6 node is that
// the UDP checksum is not optional.  That is, whenever originating a UDP
// packet, an IPv6 node must compute a UDP checksum over the packet and the
// pseudo-header, and, if that computation yields a result of zero, it must be
// changed to hex FFFF for placement in the UDP header.  IPv6 receivers must
// discard UDP packets containing a zero checksum and should log the error." So
// this transport computes one on every send and REFUSES an inbound datagram
// that carries none — where the v4 transport accepts one and counts it. That
// difference is the whole reason ipudp.go's ChecksumAbsent carries the
// sentence saying it is not transferable to IPv6.
//
// IT IS NOT THE SAME AS REFUSING AN UNVERIFIED ONE, and the distinction cost a
// working client to find. Linux hands a locally generated datagram to the wire
// with the pseudo-header sum in the checksum field and the completion deferred
// (CHECKSUM_PARTIAL); MEASURED here, dnsmasq 2.91's Advertise over a veth pair
// arrives that way and a transport that verified strictly discarded all five
// retransmissions of every exchange while reporting nothing but silence to
// ring 1. RFC 8200's MUST is about a checksum field of ZERO, which that is
// not. See ParseIPv6UDP; Stats.Uncompleted is what keeps the acceptance from
// being silent.
//
// BOUNDS:
//
//   - NO BPF FILTER, for PacketTransport's reason. Every IPv6 frame on the
//     link is read and narrowed by ParseIPv6UDP, and Stats.Skipped is the cost.
//
//   - A REPLY FOR ANOTHER CLIENT ON THE LINK IS DISCARDED HERE, at ring 3, on
//     the IPv6 DESTINATION, and counted as Stats.Foreign. ParseIPv6UDP narrows
//     on the two ports and nothing else, so on a shared segment — which is the
//     plugin's bridge and macvlan case, and every container host — every other
//     client's Advertise and Reply satisfies it. RFC 9915 section 18.3.10 says
//     where a server sends one: "the server unicasts the Advertise or Reply
//     message directly to the client using the address in the source address
//     field from the IP datagram in which the original message was received",
//     and that address is this transport's Source() for our own exchanges and
//     somebody else's for theirs. Discarding on it also implements section
//     16's "Clients SHOULD NOT accept multicast messages" without a second
//     rule, because a multicast destination is not this address either.
//
//     RING 1 WOULD HAVE DISCARDED THEM TOO, on the Client Identifier (section
//     16.3: "the contents of the Client Identifier option do not match the
//     client's DUID"), so this is not a correctness fix — it is a cost one,
//     and the cost was real: every foreign exchange took a decode, a counter,
//     a bounded journal entry and a slot in the ring, so on a busy bridge one
//     container's DHCPv6 traffic pushed another container's own history out of
//     its journal. The reading that would make this narrowing WRONG is a
//     server that answers to an address other than the one it received the
//     message from, which section 18.3.10 forbids in the same sentence.
//
//   - AND THE ADDRESS IT NARROWS ON IS READ ONCE, at construction. A kernel
//     that replaced the interface's link-local underneath a running client
//     would leave this transport counting every reply as Foreign rather than
//     acquiring — the same exposure Source() already carries for the sending
//     half, stated here because the receiving half now depends on it too.
//
//   - NO UNICAST DESTINATION AND NO NEIGHBOUR RESOLUTION. Every message goes
//     to ff02::1:2, whose link-layer address is a pure function of the IPv6
//     one (RFC 2464 section 7), so there is nothing to resolve and no peer map
//     — which also removes v4's relay bound rather than reproducing it.
//
//   - THE SOURCE ADDRESS IS THE KERNEL'S, READ AND NOT DERIVED, and read for
//     THE INDEX THIS SOCKET IS ABOUT TO BIND TO rather than for the name a
//     second time. See InterfaceLinkLocal, which carries the argument and the
//     measurement, and the namespace it answers for.
//
//   - THIS HOST'S OWN FRAMES DO NOT COME BACK on this socket, and that is
//     MEASURED rather than inherited from the v4 milestone's answer for
//     ETH_P_ARP. A socket bound to a SPECIFIC EtherType is registered in the
//     kernel's per-protocol list, and outgoing frames are cloned only to
//     sockets bound to ETH_P_ALL — which is why tcpdump shows this client's
//     own Solicits on a capture while this socket, on the same interface at
//     the same moment, read none of them. The measurement is a pair: an
//     ETH_P_IPV6 socket and an ETH_P_ALL socket on one veth, one Router
//     Solicitation sent, zero frames on the first and one frame with
//     PACKET_OUTGOING on the second. Nothing here relies on the absence — the
//     port check would exclude such a frame anyway — but NDSocket's
//     duplicate-address rules would have relied on it, so it is stated where
//     both can see it.
//
//   - NO FRAGMENT REASSEMBLY (see ipv6Upper).
type PacketTransportV6 struct {
	f       *os.File
	ifIndex int
	src     netip.Addr
	hw      net.HardwareAddr

	inbound chan lease.Inbound

	reads   atomic.Uint64
	skipped atomic.Uint64
	sends   atomic.Uint64
	dropped atomic.Uint64

	uncompleted  atomic.Uint64
	zeroChecksum atomic.Uint64
	badChecksum  atomic.Uint64
	foreign      atomic.Uint64

	closeOnce sync.Once
	closed    atomic.Bool
	wg        sync.WaitGroup
}

// TransportStatsV6 is what a PacketTransportV6 has seen.
//
// It is a second type beside TransportStats and not the same one, because two
// of the six v4 fields do not exist over IPv6 and two of these do not exist
// over IPv4. Reusing TransportStats would leave Uncompleted and Absent
// permanently zero on the v6 path, and a permanently-zero counter reads as
// "this never happens" rather than "this cannot be represented".
type TransportStatsV6 struct {
	// Reads is every frame the socket read.
	Reads uint64
	// Skipped is every frame that was not a DHCPv6 reply for this client: on
	// a shared link, nearly all of it.
	Skipped uint64
	// Sends is every datagram that left.
	Sends uint64
	// Uncompleted counts accepted datagrams whose checksum field held the
	// pseudo-header sum alone — Linux's CHECKSUM_PARTIAL. Those payloads were
	// NOT verified. It is TransportStats.Uncompleted with the same meaning and
	// the same bound; a client leasing from a server on this host shows it on
	// every reply, and a client on a physical link showing it is worth a
	// second look.
	Uncompleted uint64
	// ZeroChecksum counts inbound datagrams DISCARDED for RFC 8200 section
	// 8.1's zero checksum. It is the v6 counterpart of TransportStats.Absent
	// with the opposite verdict — v4 accepts and counts, v6 discards and
	// counts — and it is a counter rather than a silence because a server
	// sending unchecksummed datagrams produces a client that never acquires
	// and has nothing to point at.
	ZeroChecksum uint64
	// BadChecksum counts inbound datagrams whose checksum was present and
	// wrong.
	BadChecksum uint64
	// Foreign counts well-formed DHCPv6 replies addressed to ANOTHER node's
	// link-local address on this link — another container's exchange on the
	// same bridge. They are discarded here rather than carried to ring 1; see
	// this transport's BOUNDS for why the narrowing is a cost fix and not a
	// correctness one, and for the one reading that would make it wrong.
	//
	// It is separate from Skipped because the two answer different questions.
	// Skipped is "this link is busy"; Foreign is "this link has other DHCPv6
	// clients on it", which is the fact a caller debugging a slow acquisition
	// on a shared bridge actually wants, and it is zero on a link with one
	// client rather than permanently zero everywhere.
	Foreign uint64
	// Dropped counts replies that parsed and were then thrown away because
	// the consumer had not drained the channel. Not Skipped, for
	// TransportStats.Dropped's reason.
	Dropped uint64
}

// NewPacketTransportV6 opens an ETH_P_IPV6 AF_PACKET socket bound to ifName.
//
// Non-blocking and handed to os.NewFile for NewPacketTransport's reason, and
// opened in the CALLING GOROUTINE'S NETWORK NAMESPACE, which is the same
// contract NewPacketTransport and NewARPSocket carry.
func NewPacketTransportV6(ifName string) (*PacketTransportV6, error) {
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("runtime: interface %q: %w", ifName, err)
	}
	src, err := interfaceLinkLocal(iface.Index, ifName)
	if err != nil {
		return nil, err
	}

	fd, err := syscall.Socket(syscall.AF_PACKET,
		syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK,
		int(htons(ethPIPv6)))
	if err != nil {
		return nil, fmt.Errorf("runtime: socket(AF_PACKET, ETH_P_IPV6): %w", err)
	}
	sa := &syscall.SockaddrLinklayer{Protocol: htons(ethPIPv6), Ifindex: iface.Index}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("runtime: bind(%s): %w", ifName, err)
	}

	t := &PacketTransportV6{
		f:       os.NewFile(uintptr(fd), "af_packet_ipv6:"+ifName),
		ifIndex: iface.Index,
		src:     src,
		hw:      append(net.HardwareAddr(nil), iface.HardwareAddr...),
		inbound: make(chan lease.Inbound, inboundBuffer),
	}
	t.wg.Add(1)
	go t.read()
	return t, nil
}

// Source is the link-local address this transport puts in every datagram it
// sends, and therefore the address a reply comes back to.
//
// Exported so ONE reading feeds every place that needs it: this transport's
// datagrams, the Router Solicitation's source (lease.Config.LinkLocal) and the
// duplicate-address-detection runner's frames all carry the same address,
// because one fact derived twice gets two answers and the looser derivation
// decides.
func (t *PacketTransportV6) Source() netip.Addr { return t.src }

// HardwareAddr is the address this transport's interface wears.
func (t *PacketTransportV6) HardwareAddr() net.HardwareAddr {
	return append(net.HardwareAddr(nil), t.hw...)
}

// Send builds the IPv6/UDP framing and transmits one payload.
//
// Hop limit 1: ff02::1:2 is link-scoped by construction (RFC 4291 section
// 2.7's scop field is 2, link-local) and must not leave the link. It is v4's
// "TTL 1 rather than 64" for the same reason and by the same argument — a
// relay agent that forwards constructs its own datagram (RFC 9915 section 19).
func (t *PacketTransportV6) Send(dst proto.Dest, payload []byte) error {
	if t.closed.Load() {
		return ErrTransportClosed
	}
	if dst.Addr != AllDHCPRelayAgentsAndServers {
		return fmt.Errorf("%w: got %s", ErrNotAllServers, dst.Addr)
	}
	frame, err := BuildIPv6UDP(t.src, dst.Addr, ClientPort6, ServerPort6, dhcpHopLimit, payload)
	if err != nil {
		return err
	}
	return t.transmit(frame, multicastMAC(dst.Addr))
}

// transmit puts one built frame on the wire, addressed to hw.
func (t *PacketTransportV6) transmit(frame []byte, hw net.HardwareAddr) error {
	lla := &syscall.SockaddrLinklayer{
		Protocol: htons(ethPIPv6),
		Ifindex:  t.ifIndex,
		Halen:    uint8(len(hw)),
	}
	copy(lla.Addr[:], hw)

	rc, err := t.f.SyscallConn()
	if err != nil {
		return fmt.Errorf("runtime: syscallconn: %w", err)
	}
	var serr error
	cerr := rc.Write(func(fd uintptr) bool {
		serr = syscall.Sendto(int(fd), frame, 0, lla)
		return serr != syscall.EAGAIN
	})
	if cerr != nil {
		return fmt.Errorf("runtime: sendto(v6): %w", cerr)
	}
	if serr != nil {
		return fmt.Errorf("runtime: sendto(v6): %w", serr)
	}
	t.sends.Add(1)
	return nil
}

// Received is the stream of DHCPv6 payloads addressed to the client port.
func (t *PacketTransportV6) Received() <-chan lease.Inbound { return t.inbound }

// Close shuts the socket. Safe to call more than once.
func (t *PacketTransportV6) Close() error {
	var err error
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		err = t.f.Close()
		t.wg.Wait()
		close(t.inbound)
	})
	return err
}

// Stats reports what the socket has seen.
func (t *PacketTransportV6) Stats() TransportStatsV6 {
	return TransportStatsV6{
		Reads:        t.reads.Load(),
		Skipped:      t.skipped.Load(),
		Sends:        t.sends.Load(),
		Uncompleted:  t.uncompleted.Load(),
		ZeroChecksum: t.zeroChecksum.Load(),
		BadChecksum:  t.badChecksum.Load(),
		Foreign:      t.foreign.Load(),
		Dropped:      t.dropped.Load(),
	}
}

// read is the receive loop. See PacketTransport.read for why it goes through
// SyscallConn.
//
// It uses Recvfrom rather than f.Read even though nothing here needs the
// sender's link-layer address, because f.Read on this socket would still have
// to take the descriptor off the runtime poller to be interruptible. The
// sockaddr is read and discarded; there is no peer map to feed, because there
// is no unicast destination to address.
func (t *PacketTransportV6) read() {
	defer t.wg.Done()
	buf := make([]byte, maxFrame)
	rc, err := t.f.SyscallConn()
	if err != nil {
		t.fail(fmt.Errorf("runtime: syscallconn: %w", err))
		return
	}
	for {
		var (
			n    int
			rerr error
		)
		cerr := rc.Read(func(fd uintptr) bool {
			n, _, rerr = syscall.Recvfrom(int(fd), buf, 0)
			return rerr != syscall.EAGAIN
		})
		if err := firstErr(cerr, rerr); err != nil {
			if t.closed.Load() {
				return
			}
			t.fail(fmt.Errorf("runtime: v6 read: %w", err))
			return
		}
		t.deliver(buf[:n])
	}
}

// fail reports a read error on the port, best-effort. A read error on a live
// socket is reported and not swallowed, for PacketTransport.read's reason: an
// interface going away is exactly this.
func (t *PacketTransportV6) fail(err error) {
	select {
	case t.inbound <- lease.Inbound{Err: err}:
	default:
	}
}

// deliver classifies one frame and, if it is a reply for us, queues it.
//
// The read counter is bumped in a DEFER, LAST, which makes Reads a barrier for
// PacketTransport.deliver's reason.
func (t *PacketTransportV6) deliver(frame []byte) {
	defer t.reads.Add(1)
	dg, perr := ParseIPv6UDP(frame)
	if perr != nil {
		// The two checksum refusals are counted apart from everything else:
		// "nothing on this link was for us" and "a reply for us arrived and
		// was discarded" are opposite facts, and RFC 8200 section 8.1's
		// discard is the one a v4 reader would not expect.
		switch {
		case errors.Is(perr, ErrZeroChecksum6):
			t.zeroChecksum.Add(1)
		case errors.Is(perr, ErrBadChecksum):
			t.badChecksum.Add(1)
		default:
			t.skipped.Add(1)
		}
		return
	}
	if dg.Dst != t.src {
		// Another client's reply on a shared link. See BOUNDS.
		t.foreign.Add(1)
		return
	}
	if !dg.Checksum.Verified() {
		// Accepted and counted, and counted only for a datagram that is
		// actually ours: the only unverified state ParseIPv6UDP returns is
		// ChecksumUncompleted, and the zero-checksum one is a refusal handled
		// above.
		t.uncompleted.Add(1)
	}

	// The payload aliases buf, which the next read overwrites.
	p := make([]byte, len(dg.Payload))
	copy(p, dg.Payload)

	select {
	case t.inbound <- lease.Inbound{Payload: p, From: dg.Src}:
	default:
		t.dropped.Add(1)
	}
}
