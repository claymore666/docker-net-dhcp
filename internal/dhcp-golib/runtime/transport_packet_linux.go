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

// ethPIP is ETH_P_IP in host byte order. syscall does not export it.
const ethPIP = 0x0800

// inboundBuffer is how many parsed replies may sit undelivered before the
// reader drops them. Small on purpose: a DHCP exchange is a handful of packets,
// and a manager further behind than this has a problem a bigger buffer hides.
const inboundBuffer = 16

// maxFrame is the read buffer, sized so a jumbo frame carrying something else
// cannot be truncated into a DIFFERENT valid-looking frame.
const maxFrame = 9216

// broadcastMAC is the link-layer destination for every message this milestone
// sends. RFC 2131 section 4.1: a client with no configured address must
// broadcast at the link layer too — unicasting to an address the client does
// not yet have relies on it accepting a frame for an address it does not own.
var broadcastMAC = net.HardwareAddr{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

var (
	// ErrTransportClosed is returned by Send after Close.
	ErrTransportClosed = errors.New("runtime: transport closed")
	// ErrUnicastUnresolved is returned for a unicast Dest whose link-layer
	// address this transport has not seen. See sendUnicast.
	ErrUnicastUnresolved = errors.New("runtime: unicast send needs a resolved link-layer address")
	// ErrUnicastNoSource is returned for a unicast Dest carrying no source
	// address. RFC 2131 section 4.4.4's DHCPRELEASE is sent FROM the released
	// address; there is no address on the interface to fall back to.
	ErrUnicastNoSource = errors.New("runtime: unicast send needs a source address")
	// ErrNotUnicast is returned for a unicast Dest that is not a usable IPv4
	// destination.
	ErrNotUnicast = errors.New("runtime: unicast send needs an IPv4 destination")
)

// PacketTransport is the raw AF_PACKET transport.
//
// SOCK_DGRAM rather than SOCK_RAW: the kernel supplies the Ethernet header on
// send and strips it on receive, so we need not know the link's header format.
// The IP and UDP headers are still ours to build (see ipudp.go).
//
// BOUNDS:
//
//   - No BPF filter. Every IPv4 frame on the link is read and filtered in user
//     space by ParseIPv4UDP — real wasted wakeups on a busy link, measured as
//     Stats.Skipped. An LSF program is the fix; not at M1, because a wrong
//     filter drops the packet you are debugging and is invisible when it does.
//   - No ARP. A unicast is sent to the link-layer address the peer was last
//     HEARD from (see peers), never resolved. A destination never heard from
//     is refused with ErrUnicastUnresolved rather than broadcast anyway.
//   - THE RELAY CASE, and it is a real hole rather than a caveat. The map is
//     keyed on the datagram's IPv4 SOURCE; the lookup key is the server
//     identifier ring 1 read out of the reply. On a link with no relay agent
//     those are the same address and RFC 2131 section 4.4.4's DHCPRELEASE can
//     be addressed. Behind a relay they are not: RFC 1542 section 5.4 has the
//     relay forward the reply itself, so the source is the relay's address,
//     nothing is ever learned for the server identifier, and EVERY release on
//     that link is refused. The refusal is correct — it never mis-delivers,
//     and it is journalled and counted — but a client behind a relay cannot
//     release at all, and that is not a case this transport handles.
//     INFERRED, from RFC 1542: no relay has been run against this code.
//     RENEWING arrives in a later milestone, where an address on the
//     interface makes an ordinary UDP socket possible and this whole
//     mechanism goes away.
//   - No fragment reassembly (see ParseIPv4UDP).
type PacketTransport struct {
	f       *os.File
	ifIndex int
	src     netip.Addr

	inbound chan lease.Inbound

	// peers is the link-layer address each IPv4 source has been HEARD from,
	// which is what stands in for ARP. It is written by the reader goroutine
	// and read by Send from the caller's, so it is locked.
	//
	// KEYED ON THE DATAGRAM'S SOURCE, which is not always the address a
	// release is sent to — see the relay bound on PacketTransport.
	//
	// The LAST frame wins: a learned address is not an authenticated one, and
	// anything on the link answering from that IP moves the entry. That is the
	// same trust an unauthenticated DHCP client already places in whatever
	// answered its broadcast, and it is stated rather than implied. Moving is
	// also the behaviour the legitimate case needs — a server that came back
	// on a different NIC — so it is driven by
	// TestPacketTransportFollowsAPeerToANewHardwareAddress rather than left as
	// a property of a map literal.
	peerMu sync.Mutex
	peers  map[netip.Addr]net.HardwareAddr

	ident atomic.Uint32

	// Without skipped, a transport that sees nothing and one that sees
	// everything and rejects all of it are the same silence. reads is bumped
	// LAST, in deliver: see the invariant there.
	reads   atomic.Uint64
	skipped atomic.Uint64
	sends   atomic.Uint64

	// The two ways a payload reaches us unverified, counted apart because they
	// have different diagnoses: see ChecksumState.
	uncompleted atomic.Uint64
	absent      atomic.Uint64
	dropped     atomic.Uint64

	closeOnce sync.Once
	closed    atomic.Bool
	wg        sync.WaitGroup
}

// TransportStats is what a PacketTransport has seen.
type TransportStats struct {
	Reads   uint64
	Skipped uint64
	Sends   uint64
	// Uncompleted counts accepted datagrams whose UDP checksum field held the
	// pseudo-header sum (CHECKSUM_PARTIAL); Absent counts those carrying no
	// checksum at all (RFC 768's zero). Neither payload was verified — see
	// acceptUDPChecksum — and counting them is what keeps that from being
	// silent.
	//
	// Neither says WHERE the sender is. The
	// expectation runs the other way and only as an expectation: a client
	// leasing from a server on this host shows Uncompleted on every reply.
	Uncompleted uint64
	Absent      uint64
	// Dropped counts DHCP replies that parsed and were then thrown away
	// because the consumer had not drained the inbound channel. Not Skipped:
	// see the drop site.
	Dropped uint64
}

// NewPacketTransport opens an AF_PACKET socket bound to ifName.
//
// NON-BLOCKING, handed to os.NewFile so the Go runtime poller owns it: a
// blocking raw socket read cannot be interrupted by closing the fd from
// another goroutine, and working around that ends in a leaked goroutine or a
// use-after-free on an fd number the runtime has reused. With the poller,
// Close unblocks the reader.
//
// For the same reason Send never calls f.Fd(), which would put the descriptor
// back into blocking mode and remove it from the poller. SyscallConn keeps it.
func NewPacketTransport(ifName string) (*PacketTransport, error) {
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("runtime: interface %q: %w", ifName, err)
	}

	fd, err := syscall.Socket(syscall.AF_PACKET,
		syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK,
		int(htons(ethPIP)))
	if err != nil {
		return nil, fmt.Errorf("runtime: socket(AF_PACKET): %w", err)
	}

	sa := &syscall.SockaddrLinklayer{
		Protocol: htons(ethPIP),
		Ifindex:  iface.Index,
	}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("runtime: bind(%s): %w", ifName, err)
	}

	t := &PacketTransport{
		f:       os.NewFile(uintptr(fd), "af_packet:"+ifName),
		ifIndex: iface.Index,
		src:     netip.AddrFrom4([4]byte{0, 0, 0, 0}),
		inbound: make(chan lease.Inbound, inboundBuffer),
		peers:   make(map[netip.Addr]net.HardwareAddr),
	}
	t.wg.Add(1)
	go t.read()
	return t, nil
}

// Send builds the IPv4/UDP framing and transmits one payload.
func (t *PacketTransport) Send(dst proto.Dest, payload []byte) error {
	if t.closed.Load() {
		return ErrTransportClosed
	}
	if !dst.Broadcast {
		return t.sendUnicast(dst, payload)
	}

	// TTL 1 rather than 64: a broadcast to 255.255.255.255 is link-local by
	// definition (RFC 919) and must not be forwarded. A relay agent that needs
	// to forward it constructs its own datagram.
	frame, err := BuildIPv4UDP(t.src, netip.AddrFrom4([4]byte{255, 255, 255, 255}),
		ClientPort, ServerPort, uint16(t.ident.Add(1)), 1, payload)
	if err != nil {
		return err
	}
	return t.transmit(frame, broadcastMAC)
}

// sendUnicast transmits one payload to a single host, from a single address.
//
// RFC 2131 section 4.4.4 makes the DHCPRELEASE a unicast, and section 4.4.6's
// message identifies the binding by the address being given back, so the
// datagram must also come FROM that address — both halves arrive in dst,
// because nothing below ring 1 knows the client's address: the kernel has none
// on this interface.
//
// The link-layer destination is the address the peer was last heard from, not
// a resolved one, and dst.Addr is therefore refused unless something has
// already been heard from exactly that address — which behind a relay agent is
// never the server identifier. See the relay bound on PacketTransport.
//
// A destination never heard from is refused rather than broadcast:
// broadcasting a message addressed to one server would put a DHCPRELEASE
// naming this client's binding in front of every server on the link.
//
// TTL 64, not the broadcast path's 1: a unicast to the server identifier is an
// ordinary datagram and is not link-local by definition.
func (t *PacketTransport) sendUnicast(dst proto.Dest, payload []byte) error {
	if !dst.Addr.Is4() || dst.Addr.IsUnspecified() {
		return fmt.Errorf("%w: %s", ErrNotUnicast, dst.Addr)
	}
	if !dst.Src.Is4() || dst.Src.IsUnspecified() {
		return fmt.Errorf("%w: %s", ErrUnicastNoSource, dst)
	}
	hw, ok := t.peerHardwareAddr(dst.Addr)
	if !ok {
		return fmt.Errorf("%w: %s has not been heard from and this transport sends no ARP", ErrUnicastUnresolved, dst.Addr)
	}
	frame, err := BuildIPv4UDP(dst.Src, dst.Addr,
		ClientPort, ServerPort, uint16(t.ident.Add(1)), 64, payload)
	if err != nil {
		return err
	}
	return t.transmit(frame, hw)
}

// transmit puts one built frame on the wire, addressed to hw.
func (t *PacketTransport) transmit(frame []byte, hw net.HardwareAddr) error {
	lla := &syscall.SockaddrLinklayer{
		Protocol: htons(ethPIP),
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
		// Returning false parks the goroutine on the poller and retries when
		// the socket is writable. Any other error is final.
		return serr != syscall.EAGAIN
	})
	if cerr != nil {
		return fmt.Errorf("runtime: sendto: %w", cerr)
	}
	if serr != nil {
		return fmt.Errorf("runtime: sendto: %w", serr)
	}
	t.sends.Add(1)
	return nil
}

// peerHardwareAddr returns the link-layer address src was last heard from.
func (t *PacketTransport) peerHardwareAddr(src netip.Addr) (net.HardwareAddr, bool) {
	t.peerMu.Lock()
	defer t.peerMu.Unlock()
	hw, ok := t.peers[src]
	return hw, ok
}

// learnPeer records the link-layer address an IPv4 source was heard from.
func (t *PacketTransport) learnPeer(src netip.Addr, hw net.HardwareAddr) {
	if !src.Is4() || src.IsUnspecified() || len(hw) == 0 {
		return
	}
	t.peerMu.Lock()
	t.peers[src] = append(net.HardwareAddr(nil), hw...)
	t.peerMu.Unlock()
}

// Received is the stream of DHCP payloads addressed to the client port.
func (t *PacketTransport) Received() <-chan lease.Inbound { return t.inbound }

// Close shuts the socket. Safe to call more than once.
func (t *PacketTransport) Close() error {
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
func (t *PacketTransport) Stats() TransportStats {
	return TransportStats{
		Reads:       t.reads.Load(),
		Skipped:     t.skipped.Load(),
		Sends:       t.sends.Load(),
		Uncompleted: t.uncompleted.Load(),
		Absent:      t.absent.Load(),
		Dropped:     t.dropped.Load(),
	}
}

// read is the receive loop.
//
// It goes through SyscallConn and recvfrom rather than f.Read, and the reason
// is the sockaddr: on a SOCK_DGRAM AF_PACKET socket the kernel strips the
// Ethernet header, so the sender's link-layer address survives ONLY in the
// sockaddr recvfrom fills in. f.Read discards it, and without it the transport
// cannot address a unicast to the server that just answered — which, with no
// ARP here, means it cannot send RFC 2131 section 4.4.4's DHCPRELEASE at all.
//
// SyscallConn keeps the descriptor on the runtime poller, which is what makes
// Close able to unblock this loop; f.Fd() would take it off, and a blocking
// raw-socket read cannot be interrupted by closing the fd from another
// goroutine. Returning false from the callback parks on the poller and retries
// when the socket is readable.
func (t *PacketTransport) read() {
	defer t.wg.Done()
	buf := make([]byte, maxFrame)
	rc, err := t.f.SyscallConn()
	if err != nil {
		select {
		case t.inbound <- lease.Inbound{Err: fmt.Errorf("runtime: syscallconn: %w", err)}:
		default:
		}
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
			if t.closed.Load() {
				return
			}
			// A read error on a live socket is reported, not swallowed: an
			// interface going away is exactly this, and it must reach the
			// machine as an event rather than as a silence.
			select {
			case t.inbound <- lease.Inbound{Err: fmt.Errorf("runtime: read: %w", err)}:
			default:
			}
			return
		}
		t.deliver(buf[:n], senderHardwareAddr(from))
	}
}

// firstErr returns the first non-nil of the two, so the poller's error and the
// syscall's are not conflated into one and neither is dropped.
func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// senderHardwareAddr extracts the sender's link-layer address from the
// sockaddr recvfrom filled in, or nil when there is none to extract.
func senderHardwareAddr(sa syscall.Sockaddr) net.HardwareAddr {
	ll, ok := sa.(*syscall.SockaddrLinklayer)
	if !ok {
		return nil
	}
	if int(ll.Halen) == 0 || int(ll.Halen) > len(ll.Addr) {
		return nil
	}
	return append(net.HardwareAddr(nil), ll.Addr[:ll.Halen]...)
}

// deliver classifies one frame and, if it is a reply for us, queues it.
//
// The read counter is bumped in a DEFER, LAST, which makes Reads a barrier:
// seeing Reads reach N means those N frames have each been skipped, dropped or
// queued. Bumping it on arrival instead left the last frame classified a few
// instructions later, a race a test cannot wait out.
// TestPacketTransportDropsWhenTheConsumerStalls.
func (t *PacketTransport) deliver(frame []byte, hw net.HardwareAddr) {
	defer t.reads.Add(1)
	dg, perr := ParseIPv4UDP(frame)
	if perr != nil {
		// Not for us — on a shared link, most of what arrives — so counted
		// rather than reported.
		t.skipped.Add(1)
		return
	}
	if !dg.Checksum.Verified() {
		// Accepted, and counted: the payload was NOT verified. See
		// acceptUDPChecksum. A client leasing from a server on the same
		// host shows Uncompleted on every reply, which is normal; a client
		// on a physical link showing either state is worth a second look.
		switch dg.Checksum {
		case ChecksumUncompleted:
			t.uncompleted.Add(1)
		case ChecksumAbsent:
			t.absent.Add(1)
		}
	}
	// Learned HERE and not on arrival: only a frame that parsed as an IPv4 UDP
	// datagram for the client port has an IPv4 source to key on, and only
	// something that answered a DHCP broadcast has any claim to be the server.
	t.learnPeer(dg.Src, hw)

	// The payload aliases buf, which the next read overwrites.
	p := make([]byte, len(dg.Payload))
	copy(p, dg.Payload)

	select {
	case t.inbound <- lease.Inbound{Payload: p, From: dg.Src}:
	default:
		// The consumer is behind. Blocking here would stall the reader and
		// lose packets in the kernel instead, where nothing can count them.
		//
		// Counted APART from Skipped: a frame that was not for us and a DHCP
		// reply we threw away are opposite facts, and one counter holding both
		// reports the second as the first. Dropped above zero means a stalled
		// manager and a retransmission that need not have happened.
		t.dropped.Add(1)
	}
}

// htons converts to network byte order. The AF_PACKET protocol field is
// big-endian even in a struct the kernel otherwise reads natively.
func htons(v uint16) uint16 { return v<<8 | v>>8 }
