//go:build linux

package runtime

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"syscall"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
)

// This file tests the AF_PACKET transport against a real link, with no server
// on it. The dnsmasq test covers the exchange; this one covers the two things
// that test cannot reach — the frame this library actually puts on the wire,
// read back by an independent socket, and the refusal that keeps a message the
// transport cannot address from being silently broadcast instead.
//
// It runs in its own user and network namespace for the reasons the dnsmasq
// test's header gives.

func TestPacketTransportOnARealLink(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		transportOnARealLink(t)
		return
	}
	reexecInNamespaces(t)
}

func transportOnARealLink(t *testing.T) {
	mustRun(t, "ip", "link", "add", testClientIf, "type", "veth", "peer", "name", testServerIf)
	mustRun(t, "ip", "link", "set", testServerIf, "up")
	mustRun(t, "ip", "link", "set", testClientIf, "up")

	// The witness: an ordinary packet socket on the OTHER end of the link.
	// Asserting on the transport's own Stats would be asserting on the
	// library's opinion of what it sent.
	peer := peerSocket(t, testServerIf)

	tr, err := NewPacketTransport(testClientIf)
	if err != nil {
		t.Fatalf("NewPacketTransport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	// The three ways a unicast is REFUSED rather than quietly broadcast.
	// Broadcasting a DHCPRELEASE that names this client's binding, because its
	// destination could not be addressed, would put it in front of every
	// server on the link and would look exactly like success.
	for _, c := range []struct {
		what string
		dst  proto.Dest
		want error
	}{
		{"no destination", proto.Dest{Src: netip.MustParseAddr("192.168.99.100")}, ErrNotUnicast},
		{"no source", proto.Dest{Addr: netip.MustParseAddr("192.168.99.1")}, ErrUnicastNoSource},
		{"a peer never heard from", proto.Dest{
			Addr: netip.MustParseAddr("192.168.99.7"),
			Src:  netip.MustParseAddr("192.168.99.100"),
		}, ErrUnicastUnresolved},
	} {
		err = tr.Send(c.dst, []byte("unicast"))
		if !errors.Is(err, c.want) {
			t.Fatalf("unicast with %s: err = %v, want %v", c.what, err, c.want)
		}
	}
	if got := tr.Stats().Sends; got != 0 {
		t.Fatalf("a refused send was counted: sends = %d", got)
	}

	payload := []byte("this is not a DHCP message, and the wire does not care")
	if err := tr.Send(proto.Dest{Broadcast: true}, payload); err != nil {
		t.Fatalf("broadcast send: %v", err)
	}

	frame, class := awaitFrameToServerPort(t, peer)
	if class != syscall.PACKET_BROADCAST {
		t.Fatalf("the broadcast arrived as packet class %d, want PACKET_BROADCAST (%d)", class, syscall.PACKET_BROADCAST)
	}

	// RFC 2131 section 4.1: source address 0, broadcast destination. RFC 919
	// for the address itself. These are the fields a client with no address
	// has no other way to set, and they are why this transport exists.
	if got := netip.AddrFrom4([4]byte(frame[12:16])).String(); got != "0.0.0.0" {
		t.Fatalf("source address = %s, want 0.0.0.0", got)
	}
	if got := netip.AddrFrom4([4]byte(frame[16:20])).String(); got != "255.255.255.255" {
		t.Fatalf("destination address = %s, want the broadcast address", got)
	}
	if got := frame[8]; got != 1 {
		t.Fatalf("TTL = %d, want 1: a link-local broadcast must not be forwarded", got)
	}
	if got := binary.BigEndian.Uint16(frame[20:22]); got != ClientPort {
		t.Fatalf("source port = %d, want %d", got, ClientPort)
	}
	total := int(binary.BigEndian.Uint16(frame[2:4]))
	if got := frame[28:total]; !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
	// The witness parses the checksums we computed. This is the half of
	// BuildIPv4UDP that a golden-bytes test cannot reach: those bytes were
	// checked against a table, these were checked by the kernel's own idea of
	// a well-formed IPv4 datagram plus an independent verification here.
	if got := checksum(frame[:20]); got != 0 {
		t.Fatalf("IPv4 header checksum does not verify: %#04x", got)
	}
	var s4, d4 [4]byte
	copy(s4[:], frame[12:16])
	copy(d4[:], frame[16:20])
	if got := udpChecksumVerify(s4, d4, frame[20:total]); got != 0 {
		t.Fatalf("UDP checksum does not verify: %#04x", got)
	}

	if got := tr.Stats().Sends; got != 1 {
		t.Fatalf("sends = %d, want 1", got)
	}

	// ------------------------------------------ the two counters, driven --
	//
	// Skipped and Absent are the transport's answers to "what did you throw
	// away, and what could you not check". Both survived mutation on
	// 2026-08-29 — the increments could be deleted and nothing went red —
	// because nothing in the suite had ever put a frame on a real link that
	// exercised either. A counter nobody drives is a counter nobody can trust
	// when it finally moves.
	notForUs, err := BuildIPv4UDP(
		netip.MustParseAddr("192.168.99.1"), netip.MustParseAddr("255.255.255.255"),
		ServerPort, 9999, 1, 1, []byte("someone else's traffic"))
	if err != nil {
		t.Fatalf("BuildIPv4UDP: %v", err)
	}
	noChecksum, err := BuildIPv4UDP(
		netip.MustParseAddr("192.168.99.1"), netip.MustParseAddr("255.255.255.255"),
		ServerPort, ClientPort, 2, 1, []byte("no checksum here"))
	if err != nil {
		t.Fatalf("BuildIPv4UDP: %v", err)
	}
	// RFC 768's "not computed". Zeroing the field is the whole mutation.
	binary.BigEndian.PutUint16(noChecksum[26:28], 0)

	injectFrames(t, testServerIf, notForUs, noChecksum)

	// The barrier is Reads, which the transport bumps only after a frame has
	// been fully classified. Spinning on the counters under test instead would
	// turn a broken counter into an infinite spin rather than a failed
	// assertion — MEASURED 2026-08-29: written that way, both counter mutants
	// below came back as 200-second HANGS, and a hang is not a kill.
	for tr.Stats().Reads < 2 {
		runtime.Gosched()
	}
	st := tr.Stats()
	if st.Skipped != 1 {
		t.Fatalf("skipped = %d, want exactly the one frame for another port", st.Skipped)
	}
	if st.Absent != 1 {
		t.Fatalf("absent = %d, want exactly the one frame with no checksum", st.Absent)
	}
	if st.Uncompleted != 0 {
		t.Fatalf("uncompleted = %d, want 0: every frame here was built with a completed checksum", st.Uncompleted)
	}
	if st.Dropped != 0 {
		t.Fatalf("dropped = %d, want 0: two frames do not fill the channel", st.Dropped)
	}

	// ------------------------------------------------- the unicast, driven --
	//
	// 192.168.99.1 has now been HEARD from — noChecksum above parsed as a
	// datagram to the client port — so the destination this transport refused
	// three assertions ago is now resolvable, with no ARP anywhere. That
	// before-and-after is the whole of the no-ARP design: the only unicast RFC
	// 2131 gives a client with a lease in hand is the DHCPRELEASE of section
	// 4.4.4, addressed to the server that just answered it.
	srvIface, err := net.InterfaceByName(testServerIf)
	if err != nil {
		t.Fatalf("%s: %v", testServerIf, err)
	}
	if hw, ok := tr.peerHardwareAddr(netip.MustParseAddr("192.168.99.1")); !ok {
		t.Fatal("the transport did not learn the sender's hardware address from a frame it accepted")
	} else if hw.String() != srvIface.HardwareAddr.String() {
		t.Fatalf("learned hardware address %s, the sender's is %s", hw, srvIface.HardwareAddr)
	}

	const releasedFrom = "192.168.99.100"
	uni := []byte("this stands in for a DHCPRELEASE")
	if err := tr.Send(proto.Dest{
		Addr: netip.MustParseAddr("192.168.99.1"),
		Src:  netip.MustParseAddr(releasedFrom),
	}, uni); err != nil {
		t.Fatalf("unicast send to a peer that has been heard from: %v", err)
	}

	uframe, uclass := awaitFrameToServerPort(t, peer)
	// The assertion that separates a unicast from a broadcast wearing unicast
	// IP addresses. A frame sent to the broadcast MAC arrives here too, and
	// every IP-level assertion below would pass on it.
	if uclass != syscall.PACKET_HOST {
		t.Fatalf("the unicast arrived as packet class %d, want PACKET_HOST (%d): it was not addressed to this host at the link layer",
			uclass, syscall.PACKET_HOST)
	}
	if got := netip.AddrFrom4([4]byte(uframe[12:16])).String(); got != releasedFrom {
		t.Fatalf("unicast source = %s, want %s: RFC 2131 section 4.4.4 sends the DHCPRELEASE from the released address", got, releasedFrom)
	}
	if got := netip.AddrFrom4([4]byte(uframe[16:20])).String(); got != "192.168.99.1" {
		t.Fatalf("unicast destination = %s, want the server", got)
	}
	if got := uframe[8]; got != 64 {
		t.Fatalf("unicast TTL = %d, want 64: only the link-local broadcast is capped at 1", got)
	}
	if got := checksum(uframe[:20]); got != 0 {
		t.Fatalf("unicast IPv4 header checksum does not verify: %#04x", got)
	}
	var us4, ud4 [4]byte
	copy(us4[:], uframe[12:16])
	copy(ud4[:], uframe[16:20])
	utotal := int(binary.BigEndian.Uint16(uframe[2:4]))
	if got := udpChecksumVerify(us4, ud4, uframe[20:utotal]); got != 0 {
		t.Fatalf("unicast UDP checksum does not verify: %#04x", got)
	}
	if got := uframe[28:utotal]; !bytes.Equal(got, uni) {
		t.Fatalf("unicast payload = %q, want %q", got, uni)
	}
	if got := tr.Stats().Sends; got != 2 {
		t.Fatalf("sends = %d, want the broadcast and the unicast", got)
	}
}

// TestPacketTransportFollowsAPeerToANewHardwareAddress drives the property the
// peer map's own comment states and nothing held: the LAST frame from an IPv4
// source wins.
//
// It is not a detail of a map literal. A server that comes back on a different
// NIC keeps its address and changes its hardware address, and a transport that
// learned write-once would go on unicasting every DHCPRELEASE to a machine
// that is no longer there — silently, because nothing answers a release.
// MEASURED 2026-09-02 by review: with the map made write-once, the whole
// runtime suite stayed green.
//
// The peer's hardware address is really changed rather than faked, because the
// witness for "the unicast followed it" is the kernel's own packet class:
// PACKET_HOST on the far end means the frame was addressed to the address that
// interface has NOW.
func TestPacketTransportFollowsAPeerToANewHardwareAddress(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		transportFollowsAPeer(t)
		return
	}
	reexecInNamespaces(t)
}

func transportFollowsAPeer(t *testing.T) {
	mustRun(t, "ip", "link", "add", testClientIf, "type", "veth", "peer", "name", testServerIf)
	mustRun(t, "ip", "link", "set", testServerIf, "up")
	mustRun(t, "ip", "link", "set", testClientIf, "up")

	tr, err := NewPacketTransport(testClientIf)
	if err != nil {
		t.Fatalf("NewPacketTransport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	const peerIP = "192.168.99.1"
	source := netip.MustParseAddr(peerIP)

	// Round one: whatever hardware address the peer interface happens to have.
	injectFromPeer(t, peerIP)
	for tr.Stats().Reads < 1 {
		runtime.Gosched()
	}
	firstHW, ok := tr.peerHardwareAddr(source)
	if !ok {
		t.Fatal("nothing was learned from a frame the transport accepted")
	}
	if got := hardwareAddrOf(t, testServerIf); firstHW.String() != got.String() {
		t.Fatalf("learned %s, the sender's address is %s", firstHW, got)
	}

	// The peer moves. Same IPv4 address, different NIC.
	mustRun(t, "ip", "link", "set", testServerIf, "down")
	mustRun(t, "ip", "link", "set", testServerIf, "address", "02:00:00:00:99:01")
	mustRun(t, "ip", "link", "set", testServerIf, "up")
	secondWant := hardwareAddrOf(t, testServerIf)
	if secondWant.String() == firstHW.String() {
		t.Fatalf("the peer's hardware address did not change (%s); this test would measure nothing", secondWant)
	}

	witness := peerSocket(t, testServerIf)
	injectFromPeer(t, peerIP)
	for tr.Stats().Reads < 2 {
		runtime.Gosched()
	}
	secondHW, ok := tr.peerHardwareAddr(source)
	if !ok {
		t.Fatal("the peer entry disappeared after the second frame")
	}
	if secondHW.String() != secondWant.String() {
		t.Fatalf("the peer moved to %s and the transport still holds %s: a write-once map sends every DHCPRELEASE to a machine that is no longer there",
			secondWant, secondHW)
	}

	// The consequence, on the wire: a unicast now reaches the peer at its new
	// address. Reading the map alone would assert on the transport's own
	// bookkeeping; PACKET_HOST is the kernel on the far end agreeing.
	if err := tr.Send(proto.Dest{
		Addr: source,
		Src:  netip.MustParseAddr("192.168.99.100"),
	}, []byte("this stands in for a DHCPRELEASE")); err != nil {
		t.Fatalf("unicast to the moved peer: %v", err)
	}
	if _, class := awaitFrameToServerPort(t, witness); class != syscall.PACKET_HOST {
		t.Fatalf("the unicast arrived as packet class %d, want PACKET_HOST (%d): it was addressed to the hardware address the peer no longer has",
			class, syscall.PACKET_HOST)
	}
}

// injectFromPeer puts one well-formed reply to the client port on the link,
// from ifName's CURRENT hardware address.
func injectFromPeer(t *testing.T, srcIP string) {
	t.Helper()
	frame, err := BuildIPv4UDP(
		netip.MustParseAddr(srcIP), netip.MustParseAddr("255.255.255.255"),
		ServerPort, ClientPort, 1, 1, []byte("a reply"))
	if err != nil {
		t.Fatalf("BuildIPv4UDP: %v", err)
	}
	injectFrames(t, testServerIf, frame)
}

func hardwareAddrOf(t *testing.T, ifName string) net.HardwareAddr {
	t.Helper()
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		t.Fatalf("%s: %v", ifName, err)
	}
	return iface.HardwareAddr
}

// TestPacketTransportDropsWhenTheConsumerStalls drives the one path in the
// reader that throws a valid DHCP reply away.
//
// It exists because that path shares its shape with the "not for us" path and
// used to share its counter: a stalled manager and a segment full of other
// people's traffic reported as the same number. Nothing else in the suite
// reaches it — the manager always drains — and a counter nobody drives is a
// counter nobody can trust when it finally moves.
func TestPacketTransportDropsWhenTheConsumerStalls(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		transportDropsWhenStalled(t)
		return
	}
	reexecInNamespaces(t)
}

func transportDropsWhenStalled(t *testing.T) {
	mustRun(t, "ip", "link", "add", testClientIf, "type", "veth", "peer", "name", testServerIf)
	mustRun(t, "ip", "link", "set", testServerIf, "up")
	mustRun(t, "ip", "link", "set", testClientIf, "up")

	tr, err := NewPacketTransport(testClientIf)
	if err != nil {
		t.Fatalf("NewPacketTransport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	// Nothing consumes tr.Received() DURING a round: this test is the stalled
	// consumer. It drains between rounds so the next round starts from an
	// empty channel.
	//
	// It runs the cycle 40 times because one cycle is one sample of a race.
	// The bumped-on-arrival mutant — the shape that produced the flake this
	// test was written after — is caught by the invariant assertion below,
	// but only when the last frame's classification loses the race.
	//
	// The count is MEASURED against that mutant, not chosen: 16 of 30 at one
	// cycle, 10 of 12 at eight, 12 of 12 at forty. Eight was picked first on
	// the arithmetic of independent trials and the arithmetic was wrong — the
	// cycles are correlated, because a warm reader wins the race more often
	// than a cold one. Forty cycles cost about a second. Detection is a rate,
	// not a guarantee: this catches the mutant, it does not prove it cannot
	// slip through.
	const sent = inboundBuffer + 8
	const rounds = 40
	var consumed uint64

	for round := 1; round <= rounds; round++ {
		injectReplies(t, testServerIf, sent)

		// The barrier is Reads, spun on rather than slept on. It is exact
		// because the transport bumps it only after the frame has been
		// classified: an earlier version of this test span on it while it was
		// bumped on ARRIVAL and read Dropped as 7 of 8, one instant too early.
		// MEASURED 2026-08-29 by the mutation harness, whose control-before
		// check caught the flake on the fourth run of a test that had passed
		// twice. If a frame never arrives the test hangs into go test's own
		// timeout instead of passing early on a guess. No duration appears in
		// this file — see the T2 gate.
		want := uint64(round) * uint64(sent)
		for tr.Stats().Reads < want {
			runtime.Gosched()
		}

		st := tr.Stats()
		if st.Reads != want {
			t.Fatalf("round %d: reads = %d, want %d", round, st.Reads, want)
		}
		// The invariant Reads bumps last in order to make true, asserted
		// rather than assumed: every frame read was skipped, dropped, queued
		// or already taken.
		if got := st.Skipped + st.Dropped + consumed + uint64(len(tr.Received())); got != st.Reads {
			t.Fatalf("round %d: %d frames accounted for, %d read", round, got, st.Reads)
		}
		if st.Skipped != 0 {
			t.Fatalf("round %d: skipped = %d, want 0: every frame was a well-formed reply to the client port",
				round, st.Skipped)
		}
		if wantDropped := uint64(round) * uint64(sent-inboundBuffer); st.Dropped != wantDropped {
			t.Fatalf("round %d: dropped = %d, want %d (%d sent per round, %d fit in the channel)",
				round, st.Dropped, wantDropped, sent, inboundBuffer)
		}
		if got := len(tr.Received()); got != inboundBuffer {
			t.Fatalf("round %d: %d replies are queued, want the channel full at %d", round, got, inboundBuffer)
		}

		// Drain, so the next round measures a fresh fill rather than a channel
		// that was already full.
		for len(tr.Received()) > 0 {
			<-tr.Received()
			consumed++
		}
	}
}

// injectReplies puts n well-formed DHCP replies on the link from ifName. They
// are built by this package's own BuildIPv4UDP, which is what makes them
// acceptable to the parser under test; what is being driven here is the
// reader's queueing, not the codec.
func injectReplies(t *testing.T, ifName string, n int) {
	t.Helper()
	frames := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		frame, err := BuildIPv4UDP(
			netip.MustParseAddr("192.168.99.1"),
			netip.MustParseAddr("255.255.255.255"),
			ServerPort, ClientPort, uint16(i), 1, []byte{byte(i)})
		if err != nil {
			t.Fatalf("BuildIPv4UDP: %v", err)
		}
		frames = append(frames, frame)
	}
	injectFrames(t, ifName, frames...)
}

// injectFrames broadcasts raw IPv4 frames from ifName.
func injectFrames(t *testing.T, ifName string, frames ...[]byte) {
	t.Helper()
	fd := peerSocket(t, ifName)
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		t.Fatalf("%s: %v", ifName, err)
	}
	lla := &syscall.SockaddrLinklayer{
		Protocol: htons(ethPIP),
		Ifindex:  iface.Index,
		Halen:    6,
	}
	copy(lla.Addr[:], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	for i, frame := range frames {
		if err := syscall.Sendto(fd, frame, 0, lla); err != nil {
			t.Fatalf("sendto frame %d: %v", i, err)
		}
	}
}

// peerSocket opens a blocking packet socket on ifName. It is deliberately NOT
// the transport under test: a bug in the transport's own reader would
// otherwise hide a bug in its writer.
func peerSocket(t *testing.T, ifName string) int {
	t.Helper()
	iface, err := net.InterfaceByName(ifName)
	if err != nil {
		t.Fatalf("%s: %v", ifName, err)
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, int(htons(ethPIP)))
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrLinklayer{Protocol: htons(ethPIP), Ifindex: iface.Index}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return fd
}

// awaitFrameToServerPort blocks until a UDP frame addressed to the server port
// arrives, and returns it with the LINK-LAYER CLASS the kernel put it in.
//
// The class is the only thing on this socket that can tell a frame addressed
// to this host from one addressed to the broadcast address or to somebody
// else: SOCK_DGRAM strips the Ethernet header, so the destination MAC is not
// in the bytes. recvfrom's sockaddr carries it as Pkttype — PACKET_HOST,
// PACKET_BROADCAST or PACKET_OTHERHOST — and an AF_PACKET tap sees a frame
// whatever its destination, so "it arrived" is not by itself evidence that it
// was addressed correctly.
//
// There is no deadline here on purpose: go test's own timeout is the backstop,
// and a duration in this file would be a guess that either flakes or hides a
// hang. See the T2 gate.
func awaitFrameToServerPort(t *testing.T, fd int) ([]byte, uint8) {
	t.Helper()
	buf := make([]byte, maxFrame)
	for {
		n, from, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			t.Fatalf("read on the witness socket: %v", err)
		}
		f := buf[:n]
		if len(f) < 28 || f[0]>>4 != ipv4Version || f[9] != protoUDP {
			continue
		}
		if binary.BigEndian.Uint16(f[22:24]) != ServerPort {
			continue
		}
		ll, ok := from.(*syscall.SockaddrLinklayer)
		if !ok {
			t.Fatalf("the witness socket returned a %T, not a link-layer address", from)
		}
		return append([]byte(nil), f...), ll.Pkttype
	}
}
