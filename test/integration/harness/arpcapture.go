// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The parent-link ARP capture.
//
// RFC 5227's whole mechanism is ARP frames on the segment, and every
// question worth asking about it — did a Probe go out, from which MAC,
// for which address, before or after the container was told its address
// — is answerable from the wire and from nowhere else. The plugin's own
// counters cannot answer it: a counter that never moved and a check that
// never ran read identically, which is the #524 fault itself.
//
// Written as a raw AF_PACKET socket rather than by shelling out to
// tcpdump, for three reasons. The runner image is not guaranteed to
// carry tcpdump, and a test that skips when its instrument is missing
// is a test that reports "nothing to see" on the run where it matters.
// A capture file has to be flushed before it can be read, which is a
// race against the assertion. And the frames are wanted as values with
// timestamps, not as text to re-parse.
//
// WHERE IT LISTENS, AND WHY NOT ON THE PARENT. The obvious vantage
// point is the macvlan parent, and it is the wrong one. MEASURED on the
// beta lane 2026-09-04: a capture bound to the parent veth's host end
// saw the SQUATTER's reply arriving and not one of the container's own
// Probes, on a run where the DHCPDECLINE in the server log proves the
// Probes were sent and answered. A macvlan child's transmit path
// reaches the lower device without passing the parent's packet taps, so
// the parent sees what comes IN off the wire and nothing its children
// put ON it.
//
// That is not a cosmetic difference. It makes "no Probe from the
// container's MAC on the parent" TRUE whether the plugin probed or not,
// which is the one assertion conflict_check=off rests on — a gate with
// one possible verdict, dressed as wire evidence.
//
// So the vantage point is the OTHER end of the veth pair, inside the
// fixture's namespace, where the squatter and the DHCP server already
// live. Everything the parent transmits arrives there, including every
// frame its macvlan children originate. It costs a namespace switch to
// open the socket, which StartARPCaptureInNetns does on a locked
// thread; the socket stays bound to that namespace afterwards, so the
// read loop runs anywhere.

// ARPFrame is one captured ARP packet, reduced to what RFC 5227 turns
// on.
type ARPFrame struct {
	At time.Time
	// Op is 1 for a request, 2 for a reply.
	Op uint16
	// SenderMAC is the ethernet source, which is the identity a Probe
	// is attributable to. RFC 5227 section 2.1 requires the ARP sender
	// hardware address to be the same, and SenderHW carries that one so
	// a test can assert they agree rather than assume it.
	SenderMAC net.HardwareAddr
	SenderHW  net.HardwareAddr
	SenderIP  net.IP
	TargetIP  net.IP
}

// IsProbe reports whether this is an RFC 5227 section 2.1.1 ARP Probe:
// a request with an ALL-ZERO sender protocol address and the address
// being probed as its TARGET. The zero sender is what stops a probe
// disturbing the address's real owner; the non-zero target is what
// makes it a question about an address at all.
//
// BOTH halves are load-bearing, and the second was missing. MEASURED on
// the beta lane 2026-09-04: a container whose network has no gateway
// resolves 0.0.0.0, and the kernel emits an ordinary ARP Request whose
// sender protocol address `inet_select_addr` could not fill either --
// spa 0.0.0.0, tpa 0.0.0.0, three of them a second apart, which is the
// neighbour retransmission schedule and reads exactly like section
// 2.1.1's. With only the sender test, `conflict_check=off` failed on a
// plugin that had correctly sent nothing: the instrument reported the
// kernel's traffic as the plugin's.
//
// This is why it is a method on the frame with its own table test
// rather than a predicate spelled out at each call site: a predicate
// too loose fails a correct build, and one too tight passes a broken
// one, and neither is visible in the caller.
func (f ARPFrame) IsProbe() bool {
	return f.Op == 1 &&
		f.SenderIP != nil && f.SenderIP.Equal(net.IPv4zero) &&
		f.TargetIP != nil && !f.TargetIP.Equal(net.IPv4zero)
}

// IsAnnouncement reports whether this is an RFC 5227 section 2.3 ARP
// Announcement: a request whose sender and target protocol addresses
// are both the address being claimed.
func (f ARPFrame) IsAnnouncement() bool {
	return f.Op == 1 && f.SenderIP != nil && f.TargetIP != nil &&
		!f.SenderIP.Equal(net.IPv4zero) && f.SenderIP.Equal(f.TargetIP)
}

func (f ARPFrame) String() string {
	kind := "request"
	if f.Op == 2 {
		kind = "reply"
	}
	switch {
	case f.IsProbe():
		kind = "PROBE"
	case f.IsAnnouncement():
		kind = "ANNOUNCE"
	}
	return fmt.Sprintf("%s %s src=%s spa=%s tpa=%s",
		f.At.Format("15:04:05.000"), kind, f.SenderMAC, f.SenderIP, f.TargetIP)
}

// ARPCapture is a running capture on one link.
type ARPCapture struct {
	t     *testing.T
	iface string
	fd    int

	mu     sync.Mutex
	frames []ARPFrame
	done   bool
	err    error
}

// StartARPCapture begins capturing ARP on iface until the test ends.
//
// It fails the test rather than skipping if the socket cannot be
// opened. A capture that quietly does not run turns every "no probe was
// sent" assertion below into a tautology, and those assertions are the
// ones carrying conflict_check=off.
func StartARPCapture(t *testing.T, iface string) *ARPCapture {
	t.Helper()
	return startARPCapture(t, "", iface)
}

// StartARPCaptureInNetns begins capturing ARP on iface inside the named
// network namespace.
//
// The namespace is entered on a LOCKED thread only for as long as the
// socket takes to open and bind, and the thread is put back before this
// returns. An AF_PACKET socket belongs to the namespace it was created
// in for the rest of its life, so the read loop needs no namespace of
// its own -- which is the property that makes this safe to call from a
// test whose other goroutines must stay in the host namespace.
//
// It FAILS rather than skips when the namespace cannot be entered, for
// the reason in StartARPCapture: a capture that quietly did not run
// turns every "nothing was on the wire" assertion into a tautology.
func StartARPCaptureInNetns(t *testing.T, nsName, iface string) *ARPCapture {
	t.Helper()
	return startARPCapture(t, nsName, iface)
}

func startARPCapture(t *testing.T, nsName, iface string) *ARPCapture {
	t.Helper()

	if nsName != "" {
		return startARPCaptureIn(t, nsName, iface)
	}

	fd, _, err := openARPSocket(iface)
	if err != nil {
		t.Fatalf("ARP capture: %v\n"+
			"  The integration lane runs privileged; if this is EPERM the suite is not root "+
			"and every wire assertion in this file is worthless.", err)
	}

	c := &ARPCapture{t: t, iface: iface, fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

func (c *ARPCapture) run() {
	buf := make([]byte, 2048)
	for {
		c.mu.Lock()
		stop := c.done
		c.mu.Unlock()
		if stop {
			return
		}
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK || err == unix.EINTR {
				continue
			}
			c.mu.Lock()
			if !c.done && c.err == nil {
				c.err = err
			}
			c.mu.Unlock()
			return
		}
		f, ok := parseARP(buf[:n])
		if !ok {
			continue
		}
		f.At = time.Now()
		c.mu.Lock()
		c.frames = append(c.frames, f)
		c.mu.Unlock()
	}
}

// parseARP decodes an ethernet frame carrying IPv4-over-ethernet ARP.
// Anything else is dropped: this instrument answers questions about
// RFC 5227, and RFC 5227 is that one shape.
func parseARP(b []byte) (ARPFrame, bool) {
	const (
		ethHdr  = 14
		arpIPv4 = 28
	)
	if len(b) < ethHdr+arpIPv4 {
		return ARPFrame{}, false
	}
	// The socket is ETH_P_ALL (see captureEthertypeBE), so everything on
	// the link arrives here and the ethertype filter is ours to apply.
	if binary.BigEndian.Uint16(b[12:14]) != 0x0806 {
		return ARPFrame{}, false
	}
	a := b[ethHdr:]
	if binary.BigEndian.Uint16(a[0:2]) != 1 { // hardware type ethernet
		return ARPFrame{}, false
	}
	if binary.BigEndian.Uint16(a[2:4]) != 0x0800 { // protocol type IPv4
		return ARPFrame{}, false
	}
	if a[4] != 6 || a[5] != 4 { // hardware/protocol address lengths
		return ARPFrame{}, false
	}
	return ARPFrame{
		Op:        binary.BigEndian.Uint16(a[6:8]),
		SenderMAC: net.HardwareAddr(append([]byte(nil), b[6:12]...)),
		SenderHW:  net.HardwareAddr(append([]byte(nil), a[8:14]...)),
		SenderIP:  net.IP(append([]byte(nil), a[14:18]...)),
		TargetIP:  net.IP(append([]byte(nil), a[24:28]...)),
	}, true
}

// Stop ends the capture. Idempotent; registered as a test cleanup.
func (c *ARPCapture) Stop() {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	c.mu.Unlock()
	_ = unix.Close(c.fd)
}

// Frames returns everything captured so far.
//
// It fails the test if the read loop died on an error, because a
// capture that stopped early is indistinguishable from a quiet segment
// by looking at the result.
func (c *ARPCapture) Frames() []ARPFrame {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		c.t.Fatalf("ARP capture on %s died: %v — every assertion about what was NOT on the wire "+
			"is void for this run", c.iface, c.err)
	}
	return append([]ARPFrame(nil), c.frames...)
}

// FramesFrom returns the frames whose ethernet source is mac.
func (c *ARPCapture) FramesFrom(mac string) []ARPFrame {
	want, err := net.ParseMAC(mac)
	if err != nil {
		c.t.Fatalf("ARP capture: bad MAC %q: %v", mac, err)
	}
	var out []ARPFrame
	for _, f := range c.Frames() {
		if f.SenderMAC.String() == want.String() {
			out = append(out, f)
		}
	}
	return out
}

// ProbesFrom returns the RFC 5227 section 2.1.1 Probes sent by mac.
func (c *ARPCapture) ProbesFrom(mac string) []ARPFrame {
	var out []ARPFrame
	for _, f := range c.FramesFrom(mac) {
		if f.IsProbe() {
			out = append(out, f)
		}
	}
	return out
}

// AnnouncementsFrom returns the RFC 5227 section 2.3 Announcements sent
// by mac.
func (c *ARPCapture) AnnouncementsFrom(mac string) []ARPFrame {
	var out []ARPFrame
	for _, f := range c.FramesFrom(mac) {
		if f.IsAnnouncement() {
			out = append(out, f)
		}
	}
	return out
}

// AwaitProbeFrom waits until mac has sent at least n Probes, and
// returns them. ok is false on timeout, with whatever was captured.
func (c *ARPCapture) AwaitProbeFrom(mac string, n int, within time.Duration) ([]ARPFrame, bool) {
	deadline := time.Now().Add(within)
	for {
		got := c.ProbesFrom(mac)
		if len(got) >= n {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Dump writes the whole capture through log, for a failing test.
func (c *ARPCapture) Dump(log func(string)) {
	frames := c.Frames()
	log(fmt.Sprintf("--- ARP capture on %s: %d frame(s) ---", c.iface, len(frames)))
	for _, f := range frames {
		log("  " + f.String())
	}
}

// startARPCaptureIn opens and binds the socket inside nsName.
//
// runtime.LockOSThread is not optional here and the thread is
// deliberately NOT unlocked on the error paths: a goroutine that failed
// to restore its namespace must not be handed back to the scheduler,
// and letting the locked thread die with the goroutine is the only way
// to guarantee that. On the success path the original namespace is
// restored and the lock released, in that order.
func startARPCaptureIn(t *testing.T, nsName, iface string) *ARPCapture {
	t.Helper()

	runtime.LockOSThread()

	origin, err := netns.Get()
	if err != nil {
		t.Fatalf("ARP capture: read the current netns: %v", err)
	}
	defer func() { _ = origin.Close() }()

	target, err := netns.GetFromName(nsName)
	if err != nil {
		t.Fatalf("ARP capture: open netns %q: %v\n"+
			"  Without it the capture would run in the host namespace, where a macvlan child's\n"+
			"  transmits are invisible and every absence assertion is vacuous.", nsName, err)
	}
	defer func() { _ = target.Close() }()

	if err := netns.Set(target); err != nil {
		t.Fatalf("ARP capture: enter netns %q: %v", nsName, err)
	}

	fd, ifname, openErr := openARPSocket(iface)

	if err := netns.Set(origin); err != nil {
		// The thread stays locked and is never returned to the pool.
		if openErr == nil {
			_ = unix.Close(fd)
		}
		t.Fatalf("ARP capture: could not return to the original netns: %v", err)
	}
	runtime.UnlockOSThread()

	if openErr != nil {
		t.Fatalf("ARP capture in netns %q: %v", nsName, openErr)
	}

	c := &ARPCapture{t: t, iface: ifname + " (netns " + nsName + ")", fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

// openARPSocket is the socket half of StartARPCapture, factored out so
// the namespace-switching caller runs exactly the same code and a fix
// to one cannot miss the other.
func openARPSocket(iface string) (int, string, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return -1, iface, fmt.Errorf("LinkByName %s: %w", iface, err)
	}
	proto := captureEthertypeBE()
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return -1, iface, fmt.Errorf("socket(AF_PACKET): %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: link.Attrs().Index}); err != nil {
		_ = unix.Close(fd)
		return -1, iface, fmt.Errorf("bind to %s: %w", iface, err)
	}
	tv := unix.Timeval{Usec: 200_000}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return -1, iface, fmt.Errorf("SO_RCVTIMEO: %w", err)
	}
	return fd, iface, nil
}

// captureEthertypeBE is htons(ETH_P_ALL), and it is not ETH_P_ARP for a
// reason worth the extra frames.
//
// A packet socket bound to a SPECIFIC protocol is fed from
// `ptype_base`, which the receive path consults; the TRANSMIT path
// (`dev_queue_xmit_nit`) delivers only to `ptype_all`. So an ETH_P_ARP
// socket sees what arrives on the link and nothing the host sends out
// of it. MEASURED on the beta lane 2026-09-04: the squatter's ARP
// Request was missing from the capture while the reply to it was
// present -- which would have made the positive control in the
// conflict_check=off case unsatisfiable, and every absence beneath it
// unreadable.
//
// The cost is that parseARP now has to reject non-ARP frames itself.
// On a fixture link carrying one DHCP exchange and a ping that is a
// handful of packets, and the alternative is an instrument that cannot
// see half the wire.
func captureEthertypeBE() uint16 {
	const ethPALL = 0x0003
	return uint16(ethPALL&0xff)<<8 | uint16(ethPALL>>8)
}

// StartARPCapture on the fixture is what a test should call: it puts
// the capture on the segment's ONLY working vantage point without the
// test having to know which namespace the fixture put it in.
//
// The link is always the DHCP-server end of the veth pair -- the same
// end the squatter and the server sit on -- because that is where a
// macvlan child's transmits are visible; see the vantage-point
// paragraph at the top of this file. Which namespace that end lives in
// depends on the backend (Kea is namespaced, dnsmasq is not), and that
// is the only difference between the two branches below.
//
// It must be called AFTER the fixture is constructed, since the
// namespace does not exist before that.
func (ef *EphemeralFixture) StartARPCapture(t *testing.T) *ARPCapture {
	t.Helper()
	if ef.isolated() {
		return StartARPCaptureInNetns(t, ephemeralNetns, ephemeralDhcpVeth)
	}
	return StartARPCapture(t, ephemeralDhcpVeth)
}
