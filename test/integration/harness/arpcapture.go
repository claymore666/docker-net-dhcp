// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Measured on the 2.x lane 2026-09-04 (#901): a capture on the macvlan parent's host end saw the squatter's reply and
// none of the container's Probes, though the server's DHCPDECLINE proved they were sent. A macvlan child's transmits
// reach the lower device without passing the parent's packet taps, so captures listen on the other veth end, inside
// the fixture's namespace. The socket is a raw AF_PACKET one because tcpdump is not guaranteed on the runner.

// ARPFrame is one captured ARP packet, reduced to the fields RFC 5227 uses.
type ARPFrame struct {
	At time.Time
	// Op is 1 for a request, 2 for a reply.
	Op uint16
	// SenderMAC is the ethernet source; RFC 5227 section 2.1 requires SenderHW to equal it.
	SenderMAC net.HardwareAddr
	SenderHW  net.HardwareAddr
	SenderIP  net.IP
	TargetIP  net.IP
}

// IsProbe reports whether this is an RFC 5227 section 2.1.1 Probe: a request with a zero sender and a non-zero target.
// Measured on the 2.x lane 2026-09-04 (#901): a container on a network with no gateway resolves 0.0.0.0 and the kernel
// sends requests with spa and tpa both 0.0.0.0, one a second apart, which the sender test alone read as Probes.
func (f ARPFrame) IsProbe() bool {
	return f.Op == 1 &&
		f.SenderIP != nil && f.SenderIP.Equal(net.IPv4zero) &&
		f.TargetIP != nil && !f.TargetIP.Equal(net.IPv4zero)
}

// IsAnnouncement reports whether this is an RFC 5227 section 2.3 Announcement.
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

// StartARPCapture captures ARP on iface until the test ends, failing the test if the socket cannot open.
func StartARPCapture(t *testing.T, iface string) *ARPCapture {
	t.Helper()
	return startARPCapture(t, "", iface)
}

// StartARPCaptureInNetns captures ARP on iface inside nsName; an AF_PACKET socket keeps its creation namespace (#901).
func StartARPCaptureInNetns(t *testing.T, nsName, iface string) *ARPCapture {
	t.Helper()
	return startARPCapture(t, nsName, iface)
}

func startARPCapture(t *testing.T, nsName, iface string) *ARPCapture {
	t.Helper()

	if nsName != "" {
		return startARPCaptureIn(t, nsName, iface)
	}

	fd, err := openCaptureSocket(iface)
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

// parseARP decodes an ethernet frame carrying IPv4-over-ethernet ARP and drops anything else.
func parseARP(b []byte) (ARPFrame, bool) {
	const (
		ethHdr  = 14
		arpIPv4 = 28
	)
	if len(b) < ethHdr+arpIPv4 {
		return ARPFrame{}, false
	}
	if binary.BigEndian.Uint16(b[12:14]) != 0x0806 {
		return ARPFrame{}, false
	}
	a := b[ethHdr:]
	if binary.BigEndian.Uint16(a[0:2]) != 1 {
		return ARPFrame{}, false
	}
	if binary.BigEndian.Uint16(a[2:4]) != 0x0800 {
		return ARPFrame{}, false
	}
	if a[4] != 6 || a[5] != 4 {
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

// Frames returns everything captured so far, failing the test if the read loop died.
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

// AnnouncementsFrom returns the RFC 5227 section 2.3 Announcements sent by mac.
func (c *ARPCapture) AnnouncementsFrom(mac string) []ARPFrame {
	var out []ARPFrame
	for _, f := range c.FramesFrom(mac) {
		if f.IsAnnouncement() {
			out = append(out, f)
		}
	}
	return out
}

// AwaitProbeFrom waits until mac has sent n Probes and returns them, with ok false on timeout.
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

// startARPCaptureIn opens the socket inside nsName and starts the read loop.
func startARPCaptureIn(t *testing.T, nsName, iface string) *ARPCapture {
	t.Helper()

	fd := openCaptureSocketInNetns(t.Fatalf, "ARP capture", nsName, iface)

	c := &ARPCapture{t: t, iface: iface + " (netns " + nsName + ")", fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

// StartARPCapture captures on the fixture's DHCP-server veth end, the vantage point that sees macvlan transmits (#901).
func (ef *EphemeralFixture) StartARPCapture(t *testing.T) *ARPCapture {
	t.Helper()
	if ef.isolated() {
		return StartARPCaptureInNetns(t, ephemeralNetns, ephemeralDhcpVeth)
	}
	return StartARPCapture(t, ephemeralDhcpVeth)
}
