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

// Renewal requests are counted on the wire because dnsmasq logs no request it ignores: in src/rfc2131.c,
// `case DHCPREQUEST: if (ignore || ...) return 0;` precedes every log_packet call. The capture sits on the server end
// of the veth: a macvlan child's transmit path bypasses the parent's packet taps (#940).

// DHCPCapture is a running capture of client DHCPv4 messages on one link.
type DHCPCapture struct {
	t     *testing.T
	iface string
	fd    int

	mu     sync.Mutex
	frames []DHCPClientMessage
	done   bool
	err    error
}

// StartDHCPCapture captures on iface until the test ends, failing the test if the socket cannot be opened.
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

// StartDHCPCaptureInNetns captures inside the named network namespace.
func StartDHCPCaptureInNetns(t *testing.T, nsName, iface string) *DHCPCapture {
	t.Helper()
	fd := openCaptureSocketInNetns(t.Fatalf, "DHCP capture", nsName, iface)
	c := &DHCPCapture{t: t, iface: iface + " (netns " + nsName + ")", fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

// StartDHCPCapture captures on the fixture's server end; call it after the fixture exists and before the container starts (#940).
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

// Frames returns every client message captured so far, failing the test if the read loop died.
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

// FramesFrom returns the messages whose BOOTP chaddr, the server's lease key, is mac.
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

// AwaitRenewalRequestsFrom waits for at least n renewal requests from mac; ok is false on timeout.
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
