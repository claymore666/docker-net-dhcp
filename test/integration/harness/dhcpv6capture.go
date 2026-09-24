// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// #925's opt-in is the RFC 9915 section 21.20 Reconfigure Accept option, checked on the wire. The capture sits on the
// v6 fixture's bridge, which its dnsmasq binds, so it sees what the server saw; ReconfigureAcceptFindings refuses a
// capture with no client message in it.

// DHCPv6Capture is a running DHCPv6 capture on one link.
type DHCPv6Capture struct {
	t     V6FixtureT
	iface string
	fd    int

	mu     sync.Mutex
	frames []DHCPv6Message
	done   bool
	err    error
	// seen counts every delivered frame by ethertype, including rejected ones, to tell a silent link from a DHCPv6-free one.
	seen map[uint16]int
}

// StartDHCPv6Capture captures DHCPv6 on iface until the test ends, failing the test if the socket cannot be opened.
func StartDHCPv6Capture(t V6FixtureT, iface string) *DHCPv6Capture {
	t.Helper()
	fd, err := openCaptureSocket(iface)
	if err != nil {
		t.Fatalf("DHCPv6 capture on %s: %v\n"+
			"  The integration lane runs privileged; if this is EPERM the suite is not root "+
			"and every wire assertion about this segment is worthless.", iface, err)
	}
	c := &DHCPv6Capture{t: t, iface: iface, fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

// StartDHCPv6CaptureInNetns captures inside the named network namespace.
func StartDHCPv6CaptureInNetns(t V6FixtureT, nsName, iface string) *DHCPv6Capture {
	t.Helper()
	fd := openCaptureSocketInNetns(t.Fatalf, "DHCPv6 capture", nsName, iface)
	c := &DHCPv6Capture{t: t, iface: iface + " (netns " + nsName + ")", fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

// StartDHCPv6Capture captures on the fixture's bridge; call it before the container starts (#925).
func (f *V6Fixture) StartDHCPv6Capture() *DHCPv6Capture {
	f.t.Helper()
	return StartDHCPv6Capture(f.t, V6BridgeName)
}

func (c *DHCPv6Capture) run() {
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
		m, ok := ParseDHCPv6(buf[:n])
		c.mu.Lock()
		if n >= ethHeaderLen {
			if c.seen == nil {
				c.seen = map[uint16]int{}
			}
			c.seen[binary.BigEndian.Uint16(buf[12:14])]++
		}
		if ok {
			m.At = time.Now()
			c.frames = append(c.frames, m)
		}
		c.mu.Unlock()
	}
}

// Stop ends the capture. Idempotent; registered as a test cleanup.
func (c *DHCPv6Capture) Stop() {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	c.mu.Unlock()
	_ = unix.Close(c.fd)
}

// Messages returns every DHCPv6 datagram captured so far, both directions, failing the test if the read loop died.
func (c *DHCPv6Capture) Messages() []DHCPv6Message {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		c.t.Fatalf("DHCPv6 capture on %s died: %v — every statement about what the client "+
			"announced is void for this run", c.iface, c.err)
	}
	return append([]DHCPv6Message(nil), c.frames...)
}

// ClientMessages returns the half of the capture the client sent.
func (c *DHCPv6Capture) ClientMessages() []DHCPv6Message {
	var out []DHCPv6Message
	for _, m := range c.Messages() {
		if m.FromClient {
			out = append(out, m)
		}
	}
	return out
}

// AwaitClientMessages waits until every kind in kinds came from the client at least once, and returns the whole capture;
// ok is false on timeout. Every kind, not a count: four Solicits and no Request is a retransmitting client (#925).
func (c *DHCPv6Capture) AwaitClientMessages(kinds []uint8, within time.Duration) ([]DHCPv6Message, bool) {
	deadline := time.Now().Add(within)
	for {
		got := c.Messages()
		have := map[uint8]bool{}
		for _, m := range got {
			if m.FromClient {
				have[m.Type] = true
			}
		}
		all := true
		for _, k := range kinds {
			if !have[k] {
				all = false
				break
			}
		}
		if all {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// SeenTally renders the captured frames by ethertype.
func (c *DHCPv6Capture) SeenTally() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) == 0 {
		return "no frames of any kind reached the capture"
	}
	keys := make([]int, 0, len(c.seen))
	for k := range c.seen {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	total := 0
	for _, k := range keys {
		n := c.seen[uint16(k)]
		total += n
		parts = append(parts, fmt.Sprintf("ethertype %04x: %d", k, n))
	}
	return fmt.Sprintf("%d frame(s) reached the capture (%s)", total, strings.Join(parts, ", "))
}

// Dump writes the whole capture through log, for a failing test.
func (c *DHCPv6Capture) Dump(log func(string)) {
	msgs := c.Messages()
	log(fmt.Sprintf("--- DHCPv6 capture on %s: %d datagram(s); %s ---", c.iface, len(msgs), c.SeenTally()))
	for _, m := range msgs {
		log("  " + m.String())
	}
}
