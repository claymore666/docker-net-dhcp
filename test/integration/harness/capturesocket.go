// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"runtime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The capture socket, shared by every wire instrument in this package.
//
// arpcapture.go, racapture.go and dhcpcapture.go ask three questions of
// three protocols and open the SAME socket to answer them: AF_PACKET,
// SOCK_RAW, ETH_P_ALL, bound to one link, with a receive timeout so the
// read loop can notice it has been stopped. Each of the first two
// carried its own copy and its own comment saying the opener was
// "factored out so the namespace-switching caller runs exactly the same
// code and a fix to one cannot miss the other" -- which is the right
// rule stated twice and applied within one file each time. The third
// instrument is where that stops: one opener, one namespace dance, and
// a change to the socket options reaches all three.

// openCaptureSocket opens the packet socket and binds it to iface.
func openCaptureSocket(iface string) (int, error) {
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

// captureEthertypeBE is htons(ETH_P_ALL), and it is not ETH_P_ARP for a
// reason worth the extra frames.
//
// A packet socket bound to a SPECIFIC protocol is fed from
// `ptype_base`, which the receive path consults; the TRANSMIT path
// (`dev_queue_xmit_nit`) delivers only to `ptype_all`. So an ETH_P_ARP
// socket sees what arrives on the link and nothing the host sends out
// of it. MEASURED on the 2.x lane 2026-09-04: the squatter's ARP
// Request was missing from the capture while the reply to it was
// present -- which would have made the positive control in the
// conflict_check=off case unsatisfiable, and every absence beneath it
// unreadable.
//
// The cost is that each parser now has to reject frames of every other
// shape itself. On a fixture link carrying one DHCP exchange and a ping
// that is a handful of packets, and the alternative is an instrument
// that cannot see half the wire.
func captureEthertypeBE() uint16 {
	const ethPALL = 0x0003
	return uint16(ethPALL&0xff)<<8 | uint16(ethPALL>>8)
}

// openCaptureSocketInNetns opens the capture socket inside nsName.
//
// The namespace is entered on a LOCKED thread only for as long as the
// socket takes to open and bind, and the thread is put back before this
// returns. An AF_PACKET socket belongs to the namespace it was created
// in for the rest of its life, so the read loop needs no namespace of
// its own -- which is the property that makes this safe to call from a
// test whose other goroutines must stay in the host namespace.
//
// runtime.LockOSThread is not optional here and the thread is
// deliberately NOT unlocked on the error paths: a goroutine that failed
// to restore its namespace must not be handed back to the scheduler,
// and letting the locked thread die with the goroutine is the only way
// to guarantee that. fatalf is the test's Fatalf, which ends the
// goroutine, so the thread dies with it.
//
// It FAILS rather than skips when the namespace cannot be entered: a
// capture that quietly did not run turns every "nothing was on the
// wire" assertion into a tautology.
func openCaptureSocketInNetns(fatalf func(string, ...any), what, nsName, iface string) int {
	runtime.LockOSThread()

	origin, err := netns.Get()
	if err != nil {
		fatalf("%s: read the current netns: %v", what, err)
		return -1
	}
	defer func() { _ = origin.Close() }()

	target, err := netns.GetFromName(nsName)
	if err != nil {
		fatalf("%s: open netns %q: %v\n"+
			"  Without it the capture would run in the host namespace, where a macvlan child's\n"+
			"  transmits are invisible and every absence assertion is vacuous.", what, nsName, err)
		return -1
	}
	defer func() { _ = target.Close() }()

	if err := netns.Set(target); err != nil {
		fatalf("%s: enter netns %q: %v", what, nsName, err)
		return -1
	}

	fd, openErr := openCaptureSocket(iface)

	if err := netns.Set(origin); err != nil {
		// The thread stays locked and is never returned to the pool.
		if openErr == nil {
			_ = unix.Close(fd)
		}
		fatalf("%s: could not return to the original netns: %v", what, err)
		return -1
	}
	runtime.UnlockOSThread()

	if openErr != nil {
		fatalf("%s in netns %q: %v", what, nsName, openErr)
		return -1
	}
	return fd
}
