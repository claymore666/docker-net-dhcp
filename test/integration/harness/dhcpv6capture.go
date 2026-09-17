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

// The DHCPv6 capture.
//
// WHAT IT IS FOR. #925's opt-in is RFC 9915 section 21.20's Reconfigure
// Accept option, and an option is on the wire or it is not. The plugin
// already has a unit test that reads AcceptReconfigure off the Params6
// it builds (pkg/dhcp/v6mode_test.go); that is the plugin's intention,
// and this is the outcome. The two are different claims, and the ways
// the second can be false while the first is true -- an encoder that
// drops the option, a library condition the plugin does not meet, a
// client that never reached the link at all -- are exactly the ways a
// feature ships broken behind a green test.
//
// Written as a raw AF_PACKET socket for racapture.go's three reasons,
// unchanged: the runner image is not guaranteed to carry tcpdump and a
// test that skips when its instrument is missing reports "nothing to
// see" on the run where it matters; a capture file has to be flushed
// before it can be read, which is a race against the assertion; and the
// frames are wanted as values with timestamps, not as text to re-parse.
//
// WHERE IT LISTENS, and what is INFERRED about it. On the v6 fixture's
// own bridge, which is the device its dnsmasq binds and therefore the
// device on which the client's Solicit is delivered locally -- that
// delivery is how the server receives the message at all, so a capture
// there sees what the server saw. That is an argument and not a
// measurement, and it is why ReconfigureAcceptFindings refuses a
// capture with no client message in it instead of passing: if the
// inference is wrong, this instrument says so loudly and names which
// half it saw, rather than making every assertion about the client's
// options true by taking none of them.

// DHCPv6Capture is a running DHCPv6 capture on one link.
type DHCPv6Capture struct {
	t     V6FixtureT
	iface string
	fd    int

	mu     sync.Mutex
	frames []DHCPv6Message
	done   bool
	err    error
	// seen counts every frame the socket delivered, by ethertype,
	// including the ones ParseDHCPv6 rejects. Kept for racapture.go's
	// reason: "the capture saw nothing at all" and "the capture saw the
	// segment's traffic and no DHCPv6 in it" are different findings
	// that otherwise produce the same message.
	seen map[uint16]int
}

// StartDHCPv6Capture begins capturing DHCPv6 on iface until the test
// ends.
//
// It fails the test rather than skipping if the socket cannot be
// opened, for racapture.go's reason: a capture that quietly does not
// run turns every statement about what the client announced into a
// statement about an empty set.
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

// StartDHCPv6CaptureInNetns begins capturing inside the named network
// namespace. The namespace dance and the socket options are
// capturesocket.go's, shared with every other instrument in this
// package.
func StartDHCPv6CaptureInNetns(t V6FixtureT, nsName, iface string) *DHCPv6Capture {
	t.Helper()
	fd := openCaptureSocketInNetns(t.Fatalf, "DHCPv6 capture", nsName, iface)
	c := &DHCPv6Capture{t: t, iface: iface + " (netns " + nsName + ")", fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

// StartDHCPv6Capture on the fixture is what a test should call: it puts
// the capture on the segment without the test having to know which
// device the fixture built.
//
// It must be called BEFORE the container starts. A capture opened
// afterwards has no Solicit to show and cannot say whether it was ever
// able to see this client, which is the one thing it is for.
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

// Messages returns every DHCPv6 datagram captured so far, both
// directions.
//
// It fails the test if the read loop died on an error: a capture that
// stopped early is indistinguishable from a quiet segment by looking at
// the result, and telling those apart is this instrument's job.
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

// AwaitClientMessages waits until every message kind in kinds has been
// captured from the client at least once, and returns the whole
// capture. ok is false on timeout, with whatever was captured.
//
// EVERY KIND AND NOT A COUNT, because the question is whether the
// exchange got as far as each of section 21.20's announcing messages. A
// count would be satisfied by four Solicits and no Request, which is
// the retransmitting client and precisely the run whose Request nobody
// checked.
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

// SeenTally renders every frame the capture took, by ethertype, so a
// failure message can say whether the link was silent or merely
// DHCPv6-free.
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
