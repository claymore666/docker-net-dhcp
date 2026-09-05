// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The router-advertisement capture.
//
// A segment's IPv6 service mode IS the M and O flags in its router
// advertisements, and those live on the wire and nowhere else. The DHCP
// server's log says it advertised; only a frame says WHAT it
// advertised, and the whole point of the mode signature is that
// "stateless" and "managed" produce the same log line and different
// bits. The plugin's own counters cannot answer it either: a counter
// that never moved and a check that never ran read identically, which
// is the #524 fault.
//
// Written as a raw AF_PACKET socket rather than by shelling out to
// tcpdump, for arpcapture.go's three reasons, unchanged: the runner
// image is not guaranteed to carry tcpdump and a test that skips when
// its instrument is missing reports "nothing to see" on the run where
// it matters; a capture file has to be flushed before it can be read,
// which is a race against the assertion; and the frames are wanted as
// values with timestamps, not as text to re-parse.
//
// WHERE IT LISTENS. On the fixture's own bridge, which is the device
// the DHCP server transmits its advertisements OUT of. That is not the
// same answer arpcapture.go reaches, and the difference is worth
// stating rather than copying: there the frames under test are
// originated by a macvlan CHILD, whose transmit path reaches the lower
// device without passing the parent's packet taps, so the parent sees
// only what arrives off the wire. Here the frames are originated by
// this host, on this device, and `dev_queue_xmit_nit` delivers every
// transmit to the ptype_all list -- which is what captureEthertypeBE
// binds to, and the reason it binds to ETH_P_ALL rather than a specific
// protocol.
//
// That reasoning is an argument, so the fixture MEASURES it: its
// contract test opens this same capture on the bridge and on a link the
// server does not advertise on, in the managed mode, and requires
// frames on the first and none on the second. Both verdicts in one run,
// because a capture that sees nothing anywhere makes every "no RA
// arrived" assertion below true by construction.

// RACapture is a running router-advertisement capture on one link.
type RACapture struct {
	t     V6FixtureT
	iface string
	fd    int

	mu     sync.Mutex
	frames []RAFrame
	done   bool
	err    error
}

// StartRACapture begins capturing router advertisements on iface until
// the test ends.
//
// It fails the test rather than skipping if the socket cannot be
// opened, for arpcapture.go's reason: a capture that quietly does not
// run turns every "no advertisement arrived" assertion into a
// tautology, and those assertions are what the no-RA mode is.
func StartRACapture(t V6FixtureT, iface string) *RACapture {
	t.Helper()
	fd, err := openRASocket(iface)
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

// StartRACaptureInNetns begins capturing inside the named network
// namespace. The namespace is entered on a LOCKED thread only for as
// long as the socket takes to open and bind; an AF_PACKET socket
// belongs to the namespace it was created in for the rest of its life,
// so the read loop needs no namespace of its own.
//
// The thread is deliberately NOT unlocked on the error paths: a
// goroutine that failed to restore its namespace must not be handed
// back to the scheduler.
func StartRACaptureInNetns(t V6FixtureT, nsName, iface string) *RACapture {
	t.Helper()

	runtime.LockOSThread()

	origin, err := netns.Get()
	if err != nil {
		t.Fatalf("RA capture: read the current netns: %v", err)
	}
	defer func() { _ = origin.Close() }()

	target, err := netns.GetFromName(nsName)
	if err != nil {
		t.Fatalf("RA capture: open netns %q: %v", nsName, err)
	}
	defer func() { _ = target.Close() }()

	if err := netns.Set(target); err != nil {
		t.Fatalf("RA capture: enter netns %q: %v", nsName, err)
	}

	fd, openErr := openRASocket(iface)

	if err := netns.Set(origin); err != nil {
		if openErr == nil {
			_ = unix.Close(fd)
		}
		t.Fatalf("RA capture: could not return to the original netns: %v", err)
	}
	runtime.UnlockOSThread()

	if openErr != nil {
		t.Fatalf("RA capture in netns %q: %v", nsName, openErr)
	}

	c := &RACapture{t: t, iface: iface + " (netns " + nsName + ")", fd: fd}
	go c.run()
	t.Cleanup(c.Stop)
	return c
}

// openRASocket is the socket half, factored out so the
// namespace-switching caller runs exactly the same code and a fix to
// one cannot miss the other.
func openRASocket(iface string) (int, error) {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return -1, fmt.Errorf("LinkByName %s: %w", iface, err)
	}
	proto := captureEthertypeBE()
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(proto))
	if err != nil {
		return -1, fmt.Errorf("socket(AF_PACKET): %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: proto, Ifindex: link.Attrs().Index}); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bind to %s: %w", iface, err)
	}
	tv := unix.Timeval{Usec: 200_000}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("SO_RCVTIMEO: %w", err)
	}
	return fd, nil
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
		if !ok {
			continue
		}
		f.At = time.Now()
		c.mu.Lock()
		c.frames = append(c.frames, f)
		c.mu.Unlock()
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

// Frames returns every advertisement captured so far.
//
// It fails the test if the read loop died on an error: a capture that
// stopped early is indistinguishable from a quiet segment by looking at
// the result, and this instrument's whole job in the no-RA mode is to
// tell those two apart.
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

// AwaitRAAfter waits until at least one advertisement has been captured
// after since, and returns those. ok is false on timeout, with whatever
// was captured.
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

// Dump writes the whole capture through log, for a failing test.
func (c *RACapture) Dump(log func(string)) {
	frames := c.Frames()
	log(fmt.Sprintf("--- RA capture on %s: %d frame(s) ---", c.iface, len(frames)))
	for _, f := range frames {
		log("  " + f.String())
	}
}
