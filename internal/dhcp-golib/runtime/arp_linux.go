//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/wire"
)

// ethPARP is ETH_P_ARP in host byte order. syscall does not export it.
const ethPARP = 0x0806

// arpInboundBuffer is how many frames may sit undelivered before the reader
// drops them.
//
// LARGER THAN inboundBuffer, and for a reason that is about the link and not
// about taste: a DHCP exchange is a handful of packets that arrive because we
// asked for them, while ARP arrives continuously because other people are
// talking. RFC 5227's probe window is seconds long and a conflicting frame
// inside it may be one among many; dropping it because a burst of unrelated
// ARP filled the channel would turn a detected conflict into an undetected
// one. ARPDropped counts what still gets through this.
const arpInboundBuffer = 64

// ErrARPClosed is returned by Send after Close.
var ErrARPClosed = errors.New("runtime: ARP socket closed")

// ARPSocket is RFC 5227's link access: an AF_PACKET socket on ETH_P_ARP.
//
// IT IS OPENED IN THE CALLING GOROUTINE'S NETWORK NAMESPACE, which is seam row
// G-8's contract and the same one NewPacketTransport carries: the namespace a
// socket is created in is the namespace it stays in, so a caller that wants
// this in a container's netns must have entered it before calling and must not
// let the goroutine wander. runtime.NewClient opens both sockets together for
// exactly that reason.
//
// SOCK_DGRAM rather than SOCK_RAW, matching PacketTransport: the kernel builds
// the Ethernet header on send and strips it on receive, so what crosses this
// type's surface is the ARP packet itself — which is precisely what RFC 826
// defines and what wire.DecodeARP parses. A SOCK_RAW socket would hand ring 2
// fourteen octets of Ethernet header to skip past, and "skip fourteen" is the
// kind of constant that is right until somebody puts a VLAN tag on the link.
//
// BOUNDS:
//
//   - NO FILTER. Every ARP frame on the link is read and handed up, and the
//     relevance decision is ring 1's (proto.Machine.ARPRelevant). On a busy
//     link that is real wasted wakeups, counted as the difference between
//     Stats.ARPSeen and Stats.ARPIgnored. An LSF program is the fix and is not
//     taken here for the reason PacketTransport gives about its own missing
//     filter: a wrong filter drops the packet you are debugging and is
//     invisible when it does.
//   - THIS HOST'S OWN FRAMES COME BACK. A packet socket is delivered copies of
//     what this host sends, so every Probe and Announcement this client
//     broadcasts arrives on this socket a moment later. That is not worked
//     around here, because RFC 5227's rules already handle it correctly — a
//     Probe carries an all-zero sender IP, and an Announcement carries this
//     host's own hardware address, which section 2.4's rule exempts by name.
//     Filtering here instead would move a protocol rule below the ring that
//     owns it and would hide the case section 2.1.1's NOTE about buffered
//     repeaters exists for.
//   - NO SEND ADDRESSING. Every frame this sends is broadcast, because every
//     frame RFC 5227 defines is: section 2.1.1's Probe and section 2.3's
//     Announcement are both "broadcast on the local link".
type ARPSocket struct {
	f       *os.File
	ifIndex int
	hw      net.HardwareAddr

	inbound chan lease.ARPInbound

	reads   atomic.Uint64
	sends   atomic.Uint64
	dropped atomic.Uint64

	closeOnce sync.Once
	closed    atomic.Bool
	wg        sync.WaitGroup
}

// ARPStats is what an ARPSocket has seen.
type ARPStats struct {
	// Present says there IS an ARP socket behind these numbers.
	//
	// It exists because the zero value has to be able to say two different
	// things and could not. Client.ARPStats returns ARPStats{} for a client
	// running with proto.ConflictOff, which has no socket at all — and
	// ARPStats{} is also what a socket that has read nothing reports. Without
	// this field "the off client opened no ARP socket" is not a sentence any
	// test can write, which is how ring 3's off path went unheld through
	// round 1. Only ARPSocket.Stats sets it, so it cannot be true for a
	// client that has no socket.
	Present bool
	// Reads is every frame the socket delivered, this host's own included.
	Reads uint64
	// Sends is every frame that left.
	Sends uint64
	// Dropped is frames thrown away because the manager had not drained the
	// channel. Above zero it means a conflict COULD have been missed, which
	// is why it is a counter and not a debug line.
	Dropped uint64
}

// NewARPSocket opens an ETH_P_ARP socket bound to ifName.
//
// Non-blocking and handed to os.NewFile for the reason NewPacketTransport
// gives: the Go runtime poller owns the descriptor, so Close can unblock the
// reader.
func NewARPSocket(ifName string) (*ARPSocket, error) {
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("runtime: interface %q: %w", ifName, err)
	}
	if len(iface.HardwareAddr) != int(wire.ARPHLenEthernet) {
		// Refused rather than padded or truncated. RFC 5227 section 2.1.1
		// makes the sender hardware address of a Probe a MUST, and a Probe
		// carrying six octets of something that is not this interface's
		// address is answered to nobody — indistinguishable, from here, from
		// an address that is free.
		return nil, fmt.Errorf("runtime: interface %q has a %d-octet hardware address; RFC 5227 needs Ethernet's %d",
			ifName, len(iface.HardwareAddr), wire.ARPHLenEthernet)
	}

	fd, err := syscall.Socket(syscall.AF_PACKET,
		syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK,
		int(htons(ethPARP)))
	if err != nil {
		return nil, fmt.Errorf("runtime: socket(AF_PACKET, ETH_P_ARP): %w", err)
	}

	sa := &syscall.SockaddrLinklayer{
		Protocol: htons(ethPARP),
		Ifindex:  iface.Index,
	}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("runtime: bind(%s): %w", ifName, err)
	}

	s := &ARPSocket{
		f:       os.NewFile(uintptr(fd), "af_packet_arp:"+ifName),
		ifIndex: iface.Index,
		hw:      append(net.HardwareAddr(nil), iface.HardwareAddr...),
		inbound: make(chan lease.ARPInbound, arpInboundBuffer),
	}
	s.wg.Add(1)
	go s.read()
	return s, nil
}

// HardwareAddr is the address this socket's interface wears, which is the one
// RFC 5227 requires in the 'sender hardware address' of every packet it
// defines.
func (s *ARPSocket) HardwareAddr() net.HardwareAddr {
	return append(net.HardwareAddr(nil), s.hw...)
}

// Send broadcasts one ARP packet.
func (s *ARPSocket) Send(frame []byte) error {
	if s.closed.Load() {
		return ErrARPClosed
	}
	lla := &syscall.SockaddrLinklayer{
		Protocol: htons(ethPARP),
		Ifindex:  s.ifIndex,
		Halen:    uint8(len(broadcastMAC)),
	}
	copy(lla.Addr[:], broadcastMAC)

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
		return fmt.Errorf("runtime: sendto(arp): %w", cerr)
	}
	if serr != nil {
		return fmt.Errorf("runtime: sendto(arp): %w", serr)
	}
	s.sends.Add(1)
	return nil
}

// Received is the stream of ARP frames.
func (s *ARPSocket) Received() <-chan lease.ARPInbound { return s.inbound }

// Close shuts the socket. Safe to call more than once.
func (s *ARPSocket) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		err = s.f.Close()
		s.wg.Wait()
		close(s.inbound)
	})
	return err
}

// Stats reports what the socket has seen.
func (s *ARPSocket) Stats() ARPStats {
	return ARPStats{
		Present: true,
		Reads:   s.reads.Load(),
		Sends:   s.sends.Load(),
		Dropped: s.dropped.Load(),
	}
}

// read is the receive loop. See PacketTransport.read for why it goes through
// SyscallConn.
//
// It does NOT need the sockaddr: an ARP packet carries the sender's hardware
// address in its own 'ar$sha' field, which is the field every RFC 5227 rule is
// written against. Reading the link-layer source instead would be a second
// answer to the same question, and section 2.1.1's NOTE about repeaters is a
// warning that the two can disagree.
func (s *ARPSocket) read() {
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
			rerr error
		)
		cerr := rc.Read(func(fd uintptr) bool {
			n, _, rerr = syscall.Recvfrom(int(fd), buf, 0)
			return rerr != syscall.EAGAIN
		})
		if err := firstErr(cerr, rerr); err != nil {
			if s.closed.Load() {
				return
			}
			s.fail(fmt.Errorf("runtime: arp read: %w", err))
			return
		}
		// The frame aliases buf, which the next read overwrites.
		f := make([]byte, n)
		copy(f, buf[:n])
		s.reads.Add(1)
		select {
		case s.inbound <- lease.ARPInbound{Frame: f}:
		default:
			// Counted, never blocked on: stalling this reader loses frames in
			// the kernel instead, where nothing can count them. Above zero
			// this number means a conflict may have gone unseen.
			s.dropped.Add(1)
		}
	}
}

// fail reports a read error, best-effort.
//
// Best-effort because the alternative is blocking a goroutine that is on its
// way out on a consumer that may already have stopped. The manager treats a
// closed ARP stream as the end of section 2.4's detection and says so in the
// journal, so the error being dropped costs a reason, not the fact.
func (s *ARPSocket) fail(err error) {
	select {
	case s.inbound <- lease.ARPInbound{Err: err}:
	default:
	}
}
