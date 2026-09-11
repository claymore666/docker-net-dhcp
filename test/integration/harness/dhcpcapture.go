// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The client-side DHCP capture.
//
// WHAT IT IS FOR. A counter of renewal requests that went unanswered
// has one failure mode that matters and it is not arithmetic: the
// counter can be right about a wire that was silent for a different
// reason, or can move without a single request having left the host.
// Its own value cannot tell those apart, so the assertion is against
// the requests ON THE WIRE and the counter is compared to them.
//
// WHY NOT THE SERVER LOG. Because there is none to read. dnsmasq
// decides whether to serve a DHCPREQUEST BEFORE it logs it
// (src/rfc2131.c: `case DHCPREQUEST: if (ignore || ...) return 0;`
// precedes every log_packet call), so a request it ignores leaves no
// line; and the outage this test provokes kills the server outright,
// which logs even less. "The server logged nothing" is what a silent
// client and a refused client have in common, which makes it the one
// piece of evidence that cannot settle the question.
//
// WHERE IT LISTENS. The DHCP-server end of the fixture's veth pair,
// which is arpcapture.go's vantage point and for its reason: the
// frames under test are originated by a macvlan CHILD, whose transmit
// path reaches the lower device without passing the parent's packet
// taps, so a capture on the parent sees what arrives off the wire and
// none of what the container puts on it. StartDHCPCapture on the
// fixture picks the right end and the right namespace.

// DHCPCapture is a running capture of client DHCPv4 messages on one
// link.
type DHCPCapture struct {
	t     *testing.T
	iface string
	fd    int

	mu     sync.Mutex
	frames []DHCPClientMessage
	done   bool
	err    error
}

// StartDHCPCapture begins capturing on iface until the test ends.
//
// It fails the test rather than skipping if the socket cannot be
// opened, for arpcapture.go's reason: a capture that quietly does not
// run makes every count it reports a zero, and a zero here would turn
// the bound the caller asserts into a statement about nothing.
func StartDHCPCapture(t *testing.T, iface string) *DHCPCapture {
	t.Helper()
	fd, err := openCaptureSocket(iface)
	if err != nil {
		t.Fatalf("DHCP capture on %s: %v\n"+
			"  The integration lane runs privileged; if this is EPERM the suite is not root "+
			"and every wire assertion about this segment is worthless.", iface, err)
	}
	c := &DHCPCapture{t: t, iface: iface, fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

// StartDHCPCaptureInNetns begins capturing inside the named network
// namespace. The namespace dance and the socket options are
// capturesocket.go's, shared with every other instrument in this
// package.
func StartDHCPCaptureInNetns(t *testing.T, nsName, iface string) *DHCPCapture {
	t.Helper()
	fd := openCaptureSocketInNetns(t.Fatalf, "DHCP capture", nsName, iface)
	c := &DHCPCapture{t: t, iface: iface + " (netns " + nsName + ")", fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

// StartDHCPCapture on the fixture is what a test should call: it puts
// the capture on the segment's only working vantage point without the
// test having to know which namespace the fixture put it in.
//
// It must be called AFTER the fixture is constructed, since the
// namespace does not exist before that, and BEFORE the container
// starts, since a capture opened afterwards has no bind exchange to
// show and cannot say whether it was ever able to see this client.
func (ef *EphemeralFixture) StartDHCPCapture(t *testing.T) *DHCPCapture {
	t.Helper()
	if ef.isolated() {
		return StartDHCPCaptureInNetns(t, ephemeralNetns, ephemeralDhcpVeth)
	}
	return StartDHCPCapture(t, ephemeralDhcpVeth)
}

func (c *DHCPCapture) run() {
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
		m, ok := ParseDHCPv4Request(buf[:n])
		if !ok {
			continue
		}
		m.At = time.Now()
		c.mu.Lock()
		c.frames = append(c.frames, m)
		c.mu.Unlock()
	}
}

// Stop ends the capture. Idempotent; registered as a test cleanup.
func (c *DHCPCapture) Stop() {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	c.mu.Unlock()
	_ = unix.Close(c.fd)
}

// Frames returns every client message captured so far.
//
// It fails the test if the read loop died on an error: a capture that
// stopped early is indistinguishable from a quiet segment by looking at
// the result, and the whole job of this instrument is to tell a client
// that stopped asking apart from one nobody answered.
func (c *DHCPCapture) Frames() []DHCPClientMessage {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		c.t.Fatalf("DHCP capture on %s died: %v — every count taken from it is void for this run",
			c.iface, c.err)
	}
	return append([]DHCPClientMessage(nil), c.frames...)
}

// FramesFrom returns the messages whose BOOTP chaddr is mac.
//
// Keyed on chaddr and not on the ethernet source because chaddr is the
// identity the SERVER keys a lease on, which is the identity the
// counter under test is about.
func (c *DHCPCapture) FramesFrom(mac string) []DHCPClientMessage {
	want, err := net.ParseMAC(mac)
	if err != nil {
		c.t.Fatalf("DHCP capture: bad MAC %q: %v", mac, err)
	}
	var out []DHCPClientMessage
	for _, m := range c.Frames() {
		if m.ClientMAC.String() == want.String() {
			out = append(out, m)
		}
	}
	return out
}

// RenewalRequestsFrom returns the renewal requests mac put on the wire.
func (c *DHCPCapture) RenewalRequestsFrom(mac string) []DHCPClientMessage {
	var out []DHCPClientMessage
	for _, m := range c.FramesFrom(mac) {
		if m.IsRenewalRequest() {
			out = append(out, m)
		}
	}
	return out
}

// AwaitRenewalRequestsFrom waits until mac has put at least n renewal
// requests on the wire, and returns them. ok is false on timeout, with
// whatever was captured.
func (c *DHCPCapture) AwaitRenewalRequestsFrom(mac string, n int, within time.Duration) ([]DHCPClientMessage, bool) {
	deadline := time.Now().Add(within)
	for {
		got := c.RenewalRequestsFrom(mac)
		if len(got) >= n {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(time.Second)
	}
}

// Dump writes the whole capture through log, for a failing test.
func (c *DHCPCapture) Dump(log func(string)) {
	frames := c.Frames()
	log(fmt.Sprintf("--- DHCP capture on %s: %d client message(s) ---", c.iface, len(frames)))
	for _, m := range frames {
		log("  " + m.String())
	}
}
