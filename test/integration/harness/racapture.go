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

// A segment's IPv6 mode is the M and O flags of its router advertisements, so the check reads frames: "stateless" and
// "managed" log the same server line. The capture sits on the fixture's bridge, which the server transmits from, and
// dev_queue_xmit_nit delivers every transmit to ptype_all; the fixture's contract test measures it (#942).

// RACapture is a running router-advertisement capture on one link.
type RACapture struct {
	t     V6FixtureT
	iface string
	fd    int

	mu     sync.Mutex
	frames []RAFrame
	done   bool
	err    error
	// seen counts every delivered frame by ethertype, including rejected ones, to tell a silent link from an RA-free one (#942).
	seen map[uint16]int
}

// StartRACapture captures router advertisements on iface until the test ends, failing the test if the socket cannot be opened.
func StartRACapture(t V6FixtureT, iface string) *RACapture {
	t.Helper()
	fd, err := openCaptureSocket(iface)
	if err != nil {
		t.Fatalf("RA capture on %s: %v\n"+
			"  The integration lane runs privileged; if this is EPERM the suite is not root "+
			"and every wire assertion about this segment is worthless.", iface, err)
	}
	c := &RACapture{t: t, iface: iface, fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

// StartRACaptureInNetns captures inside the named network namespace.
func StartRACaptureInNetns(t V6FixtureT, nsName, iface string) *RACapture {
	t.Helper()
	fd := openCaptureSocketInNetns(t.Fatalf, "RA capture", nsName, iface)
	c := &RACapture{t: t, iface: iface + " (netns " + nsName + ")", fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

func (c *RACapture) run() {
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
		f, ok := ParseRA(buf[:n])
		c.mu.Lock()
		if n >= ethHeaderLen {
			if c.seen == nil {
				c.seen = map[uint16]int{}
			}
			c.seen[binary.BigEndian.Uint16(buf[12:14])]++
		}
		if ok {
			f.At = time.Now()
			c.frames = append(c.frames, f)
		}
		c.mu.Unlock()
		if !ok {
			continue
		}
	}
}

// Stop ends the capture. Idempotent; registered as a test cleanup.
func (c *RACapture) Stop() {
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	c.mu.Unlock()
	_ = unix.Close(c.fd)
}

// Frames returns every advertisement captured so far, failing the test if the read loop died.
func (c *RACapture) Frames() []RAFrame {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		c.t.Fatalf("RA capture on %s died: %v — every assertion about what was NOT on the wire "+
			"is void for this run", c.iface, c.err)
	}
	return append([]RAFrame(nil), c.frames...)
}

// FramesAfter returns the advertisements captured strictly after since.
func (c *RACapture) FramesAfter(since time.Time) []RAFrame {
	var out []RAFrame
	for _, f := range c.Frames() {
		if f.At.After(since) {
			out = append(out, f)
		}
	}
	return out
}

// AwaitRAAfter waits for at least one advertisement after since; ok is false on timeout.
func (c *RACapture) AwaitRAAfter(since time.Time, within time.Duration) ([]RAFrame, bool) {
	deadline := time.Now().Add(within)
	for {
		got := c.FramesAfter(since)
		if len(got) > 0 {
			return got, true
		}
		if time.Now().After(deadline) {
			return got, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// SeenTally renders the captured frames by ethertype.
func (c *RACapture) SeenTally() string {
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
func (c *RACapture) Dump(log func(string)) {
	frames := c.Frames()
	log(fmt.Sprintf("--- RA capture on %s: %d advertisement(s); %s ---", c.iface, len(frames), c.SeenTally()))
	for _, f := range frames {
		log("  " + f.String())
	}
}
