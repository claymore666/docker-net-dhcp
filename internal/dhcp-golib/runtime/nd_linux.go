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
	"github.com/claymore666/dhcp-golib/wire"
)

// ethPIPv6 is ETH_P_IPV6 in host byte order. syscall does not export it.
const ethPIPv6 = 0x86DD

// ndInboundBuffer is how many frames may sit undelivered on either of this
// socket's two channels before the reader drops them.
//
// It is arpInboundBuffer's size and for arpInboundBuffer's reason: Neighbor
// Discovery arrives continuously because other people are talking, and the
// frame that says an address is taken may be one among many. NDDropped counts
// what does not get through.
const ndInboundBuffer = 64

// ErrNDClosed is returned by Send after Close.
var ErrNDClosed = errors.New("runtime: ND socket closed")

// ErrNDNotMulticast is returned for an ICMPv6 packet whose destination is not
// a multicast address. Every message this library sends on this socket has a
// multicast destination — RFC 4861 section 4.1's Router Solicitation goes to
// ff02::2 and RFC 4862 section 5.4.2's duplicate-address Neighbor Solicitation
// to the solicited-node group of the target — so the link-layer address is a
// pure function of the IPv6 one (RFC 2464 section 7) and no neighbour
// resolution is needed. A unicast destination would need one, and refusing is
// what keeps that absence from being a silent broadcast.
var ErrNDNotMulticast = errors.New("runtime: ND send needs a multicast destination")

// NDFrame is one validated Neighbor Discovery message, with everything about
// it that lives OUTSIDE the ICMPv6 body.
//
// The three fields beyond Body are each load-bearing and none of them can be
// recovered from the body: Src is what RFC 4861 section 6.1.2's link-local
// check is made against and what the checksum was computed over; SenderHW is
// what tells this host's own looped-back frame from another node's (RFC 4862
// section 5.4.3); Dst is section 7.1.1's solicited-node condition.
type NDFrame struct {
	Src, Dst netip.Addr
	SenderHW net.HardwareAddr
	// Type is the ICMPv6 type octet, already narrowed to 133..136.
	Type uint8
	// Body is a copy, not an alias: two consumers read it.
	Body []byte
}

// NDStats is what an NDSocket has seen.
type NDStats struct {
	// Present says there IS an ND socket behind these numbers, for
	// ARPStats.Present's reason.
	Present bool
	// Reads is every ICMPv6 Neighbor Discovery frame delivered to a consumer.
	Reads uint64
	// Sends is every frame that left.
	Sends uint64
	// Skipped is every frame the socket read and did not deliver because it
	// was not one of the four Neighbor Discovery types — on a shared link,
	// most of what an ETH_P_IPV6 socket sees.
	Skipped uint64
	// Own is frames whose link-layer source is this interface's own address:
	// this host's transmissions, arriving back on this socket. They are
	// counted, kept off the lease.ND port, and still handed to the
	// duplicate-address-detection runner, which is the one consumer that has
	// to know they arrived.
	//
	// IT IS ZERO ON LINUX WITH THIS SOCKET, MEASURED, and the counter exists
	// anyway. See NDSocket's BOUNDS for the measurement and for the two cases
	// that can still make it move.
	Own uint64
	// BadChecksum is frames whose ICMPv6 checksum did not verify over RFC
	// 4443 section 2.3's pseudo-header. RFC 4861 section 6.1.2 makes a valid
	// checksum a condition of a valid advertisement; a frame failing it is
	// dropped here and never reaches a decoder.
	BadChecksum uint64
	// BadHopLimit is frames whose IPv6 hop limit was not 255 — RFC 4861
	// section 6.1.2's "the packet could not possibly have been forwarded by a
	// router". Above zero on a real link it means something is routing
	// Neighbor Discovery, which is the attack that check exists for.
	BadHopLimit uint64
	// BadSource is Router Advertisements whose IPv6 source is not a
	// link-local address, section 6.1.2's first check.
	BadSource uint64
	// Dropped is frames thrown away because a consumer had not drained its
	// channel. Above zero it means a duplicate address COULD have gone
	// unseen.
	Dropped uint64
}

// NDSocket is RFC 4861's link access: an AF_PACKET socket on ETH_P_IPV6,
// narrowed in user space to the four Neighbor Discovery message types.
//
// IT IS OPENED IN THE CALLING GOROUTINE'S NETWORK NAMESPACE, which is seam row
// G-8's contract and the same one NewPacketTransport and NewARPSocket carry.
// NewClient6 opens all of this client's sockets together for that reason.
//
// SOCK_DGRAM rather than SOCK_RAW, matching the other two: the kernel supplies
// the Ethernet header on send and strips it on receive. What this type builds
// by hand is the IPv6 header, and it must, because RFC 4443 section 2.3's
// checksum covers the source and destination addresses — see BuildIPv6ICMP.
//
// IT FEEDS TWO CONSUMERS FROM ONE SOCKET, and that is a shape rather than a
// convenience. The lease.ND port carries Router Advertisements to ring 1;
// RFC 4862 section 5.4's duplicate address detection reads Neighbor
// Solicitations and Advertisements and needs the IPv6 source and the sender's
// hardware address, which lease.NDInbound does not carry and should not. Two
// sockets would be two read loops and two answers to "did that frame arrive".
//
// BOUNDS:
//
//   - NO BPF FILTER. Every IPv6 frame on the link is read and narrowed in user
//     space, for the reason PacketTransport and ARPSocket give about their own
//     missing filters: a wrong filter drops the packet you are debugging and
//     is invisible when it does. Stats.Skipped is the cost.
//   - THIS HOST'S OWN FRAMES DO NOT ARRIVE ON THIS SOCKET, MEASURED for
//     ETH_P_IPV6 in this milestone rather than inherited from M6's answer for
//     ETH_P_ARP — the two are different protocol numbers and one measurement
//     does not cover the other, so it was made: an ETH_P_IPV6 socket and an
//     ETH_P_ALL socket on one veth, one Router Solicitation sent, ZERO frames
//     read by the first and one frame marked PACKET_OUTGOING read by the
//     second. A socket bound to a specific EtherType sits in the kernel's
//     per-protocol list and outgoing frames are cloned only to the ETH_P_ALL
//     list; tcpdump is on that list, which is why a capture shows what this
//     socket does not, and why a capture is not a measurement of this socket.
//   - THE OWN-FRAME EXCLUSION IS KEPT ANYWAY, and isOwn is the one predicate
//     both consumers use. Two reasons, and the first is not hypothetical: RFC
//     4862 section 5.4.3 REQUIRES it — "If the solicitation is from the node
//     itself (because the node loops back multicast packets), the solicitation
//     does not indicate the presence of a duplicate address" — and a duplicate
//     address check written to depend on the measurement above would rest a
//     conformance claim on a kernel implementation detail no test here can
//     move. The second is the case the measurement does not cover: a repeater
//     or hairpinning switch that echoes this host's frame back delivers it as
//     an INCOMING frame with this host's address on it, which no
//     PACKET_OUTGOING marking would catch and which the link-layer source
//     does.
//   - NO SEND ADDRESSING BEYOND MULTICAST. See ErrNDNotMulticast.
//   - THE FOUR SECTION 6.1.2 CHECKS MADE HERE are the ones whose evidence is
//     outside the ICMPv6 body: hop limit, checksum, and — for a Router
//     Advertisement — a link-local source. The rest are the codec's, and
//     wire.ErrICMPv6Validity's doc comment draws that line from the other side.
type NDSocket struct {
	f       *os.File
	ifIndex int
	hw      net.HardwareAddr

	inbound chan lease.NDInbound
	frames  chan NDFrame

	reads       atomic.Uint64
	sends       atomic.Uint64
	skipped     atomic.Uint64
	own         atomic.Uint64
	badChecksum atomic.Uint64
	badHopLimit atomic.Uint64
	badSource   atomic.Uint64
	dropped     atomic.Uint64

	closeOnce sync.Once
	closed    atomic.Bool
	wg        sync.WaitGroup
}

// NewNDSocket opens an ETH_P_IPV6 socket bound to ifName.
//
// Non-blocking and handed to os.NewFile for NewPacketTransport's reason: the
// Go runtime poller owns the descriptor, so Close can unblock the reader.
func NewNDSocket(ifName string) (*NDSocket, error) {
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("runtime: interface %q: %w", ifName, err)
	}
	if len(iface.HardwareAddr) != int(wire.ARPHLenEthernet) {
		// Refused rather than padded, for NewARPSocket's reason and one more:
		// the own-frame predicate below compares this address against the
		// sockaddr's, and an address of the wrong length can never match, so
		// every one of this host's own Neighbor Solicitations would read as
		// another node's and every acquisition would decline itself.
		return nil, fmt.Errorf("runtime: interface %q has a %d-octet hardware address; Neighbor Discovery here needs Ethernet's %d",
			ifName, len(iface.HardwareAddr), wire.ARPHLenEthernet)
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

	s := &NDSocket{
		f:       os.NewFile(uintptr(fd), "af_packet_ipv6:"+ifName),
		ifIndex: iface.Index,
		hw:      append(net.HardwareAddr(nil), iface.HardwareAddr...),
		inbound: make(chan lease.NDInbound, ndInboundBuffer),
		frames:  make(chan NDFrame, ndInboundBuffer),
	}
	s.wg.Add(1)
	go s.read()
	return s, nil
}

// HardwareAddr is the address this socket's interface wears.
func (s *NDSocket) HardwareAddr() net.HardwareAddr {
	return append(net.HardwareAddr(nil), s.hw...)
}

// Send transmits one ICMPv6 packet, from exactly the source its checksum was
// computed over.
func (s *NDSocket) Send(pkt wire.ICMPv6Packet) error {
	if s.closed.Load() {
		return ErrNDClosed
	}
	if !pkt.Dst.Is6() || !pkt.Dst.IsMulticast() {
		return fmt.Errorf("%w: %s", ErrNDNotMulticast, pkt.Dst)
	}
	frame, err := BuildIPv6ICMP(pkt)
	if err != nil {
		return err
	}
	hw := multicastMAC(pkt.Dst)
	lla := &syscall.SockaddrLinklayer{
		Protocol: htons(ethPIPv6),
		Ifindex:  s.ifIndex,
		Halen:    uint8(len(hw)),
	}
	copy(lla.Addr[:], hw)

	rc, err := s.f.SyscallConn()
	if err != nil {
		return fmt.Errorf("runtime: syscallconn: %w", err)
	}
	var serr error
	cerr := rc.Write(func(fd uintptr) bool {
		serr = syscall.Sendto(int(fd), frame, 0, lla)
		return serr != syscall.EAGAIN
	})
	if cerr != nil {
		return fmt.Errorf("runtime: sendto(nd): %w", cerr)
	}
	if serr != nil {
		return fmt.Errorf("runtime: sendto(nd): %w", serr)
	}
	s.sends.Add(1)
	return nil
}

// multicastMAC is RFC 2464 section 7: "An IPv6 packet with a multicast
// destination address DST, consisting of the sixteen octets DST[1] through
// DST[16], is transmitted to the Ethernet multicast address whose first two
// octets are the value 3333 hexadecimal and whose last four octets are the
// last four octets of DST."
func multicastMAC(dst netip.Addr) net.HardwareAddr {
	b := dst.As16()
	return net.HardwareAddr{0x33, 0x33, b[12], b[13], b[14], b[15]}
}

// Received is the lease.ND port: the Neighbor Discovery messages that arrived
// from SOMEBODY ELSE, as ICMPv6 bodies.
//
// This host's own frames are kept off it (see Stats.Own): ring 1 has nothing
// to learn from this client's own Router Solicitation coming back, and every
// event that reaches Step costs a bounded journal entry.
func (s *NDSocket) Received() <-chan lease.NDInbound { return s.inbound }

// Frames is the duplicate-address-detection runner's view: every validated
// Neighbor Discovery frame, this host's own included, with the source address
// and the sender's hardware address that RFC 4862 section 5.4.3 is written
// against.
//
// A consumer that does not drain it makes Stats.Dropped rise; there is no
// consumer-less mode, because a socket whose second channel silently filled
// would report every duplicate as free.
func (s *NDSocket) Frames() <-chan NDFrame { return s.frames }

// Close shuts the socket. Safe to call more than once.
func (s *NDSocket) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		err = s.f.Close()
		s.wg.Wait()
		close(s.inbound)
		close(s.frames)
	})
	return err
}

// Stats reports what the socket has seen.
func (s *NDSocket) Stats() NDStats {
	return NDStats{
		Present:     true,
		Reads:       s.reads.Load(),
		Sends:       s.sends.Load(),
		Skipped:     s.skipped.Load(),
		Own:         s.own.Load(),
		BadChecksum: s.badChecksum.Load(),
		BadHopLimit: s.badHopLimit.Load(),
		BadSource:   s.badSource.Load(),
		Dropped:     s.dropped.Load(),
	}
}

// isOwn reports whether a frame's link-layer source is this interface's own
// address — this host's transmission, whether the kernel handed it back on the
// way out or a repeater on the link echoed it in.
//
// ONE PREDICATE, TWO CONSUMERS. The port uses it to keep this client's own
// Router Solicitations away from ring 1; the duplicate-address-detection
// runner uses it for RFC 4862 section 5.4.3's "If the solicitation is from the
// node itself ... the solicitation does not indicate the presence of a
// duplicate address". Two spellings of it would be one fact derived twice, and
// the looser of the two would decide whether this client declines every
// address it is offered.
//
// A LENGTH MISMATCH IS NOT OWN, which is the safe direction and is worth
// saying because the other reading is available. Reporting a frame of unknown
// provenance as this host's own would silence a real duplicate; reporting this
// host's own as a stranger's would decline an address that is free. Both are
// wrong, and NewNDSocket refuses an interface whose address is not six octets
// precisely so this branch cannot be reached by a whole interface at once.
func (s *NDSocket) isOwn(hw net.HardwareAddr) bool {
	if len(hw) != len(s.hw) {
		return false
	}
	for i := range hw {
		if hw[i] != s.hw[i] {
			return false
		}
	}
	return true
}

// read is the receive loop. See PacketTransport.read for why it goes through
// SyscallConn, and why it uses recvfrom rather than f.Read: the sender's
// link-layer address survives only in the sockaddr, and here it is the field
// that tells this host's own duplicate-address probe from a real duplicate.
func (s *NDSocket) read() {
	defer s.wg.Done()
	buf := make([]byte, maxFrame)
	rc, err := s.f.SyscallConn()
	if err != nil {
		s.fail(fmt.Errorf("runtime: syscallconn: %w", err))
		return
	}
	for {
		var (
			n    int
			from syscall.Sockaddr
			rerr error
		)
		cerr := rc.Read(func(fd uintptr) bool {
			n, from, rerr = syscall.Recvfrom(int(fd), buf, 0)
			return rerr != syscall.EAGAIN
		})
		if err := firstErr(cerr, rerr); err != nil {
			if s.closed.Load() {
				return
			}
			s.fail(fmt.Errorf("runtime: nd read: %w", err))
			return
		}
		s.deliver(buf[:n], senderHardwareAddr(from))
	}
}

// deliver validates one frame and hands it to the consumers that want it.
//
// The read counter is bumped LAST, in a defer, for PacketTransport.deliver's
// reason: it makes Reads a barrier a test can wait on.
func (s *NDSocket) deliver(frame []byte, hw net.HardwareAddr) {
	f, ok := s.classify(frame, hw)
	if !ok {
		return
	}
	defer s.reads.Add(1)

	if !s.isOwn(hw) {
		select {
		case s.inbound <- lease.NDInbound{Frame: append([]byte(nil), f.Body...)}:
		default:
			s.dropped.Add(1)
		}
	}
	select {
	case s.frames <- f:
	default:
		s.dropped.Add(1)
	}
}

// classify applies the RFC 4861 section 6.1.2 checks whose evidence is outside
// the ICMPv6 body, and narrows to the four Neighbor Discovery types.
//
// It is a method rather than a function so each refusal lands on its own
// counter: "the socket saw nothing" and "the socket saw eleven frames and
// refused all of them" are opposite facts and one counter holding both reports
// the second as the first.
func (s *NDSocket) classify(frame []byte, hw net.HardwareAddr) (NDFrame, bool) {
	pf, err := ParseIPv6ICMP(frame)
	if err != nil {
		s.skipped.Add(1)
		return NDFrame{}, false
	}
	typ := pf.Body[0]
	if typ < wire.ICMPv6RouterSolicit || typ > wire.ICMPv6NeighborAdvert {
		s.skipped.Add(1)
		return NDFrame{}, false
	}
	if pf.HopLimit != NDHopLimit {
		s.badHopLimit.Add(1)
		return NDFrame{}, false
	}
	if !wire.VerifyICMPv6Checksum(pf.Src, pf.Dst, pf.Body) {
		s.badChecksum.Add(1)
		return NDFrame{}, false
	}
	if typ == wire.ICMPv6RouterAdvert && !pf.Src.IsLinkLocalUnicast() {
		// Section 6.1.2's first check: "IP Source Address is a link-local
		// address.  Routers must use their link-local address as the source
		// for Router Advertisement and Redirect messages so that hosts can
		// uniquely identify routers."
		s.badSource.Add(1)
		return NDFrame{}, false
	}
	if s.isOwn(hw) {
		s.own.Add(1)
	}
	return NDFrame{
		Src:      pf.Src,
		Dst:      pf.Dst,
		SenderHW: append(net.HardwareAddr(nil), hw...),
		Type:     typ,
		// A copy: the frame aliases the read buffer, which the next read
		// overwrites, and two consumers hold it.
		Body: append([]byte(nil), pf.Body...),
	}, true
}

// fail reports a read error on the port, best-effort, for ARPSocket.fail's
// reason.
func (s *NDSocket) fail(err error) {
	select {
	case s.inbound <- lease.NDInbound{Err: err}:
	default:
	}
}
