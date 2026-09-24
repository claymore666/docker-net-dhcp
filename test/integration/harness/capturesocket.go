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

// openCaptureSocket opens the AF_PACKET, SOCK_RAW, ETH_P_ALL socket every wire instrument shares, bound to iface (#940).
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

// captureEthertypeBE is htons(ETH_P_ALL). A socket bound to one protocol is fed from ptype_base on receive only;
// dev_queue_xmit_nit delivers transmitted frames to ptype_all. Measured on the 2.x lane 2026-09-04: an ETH_P_ARP
// capture missed the squatter's ARP Request and kept the reply (#882, #940).
func captureEthertypeBE() uint16 {
	const ethPALL = 0x0003
	return uint16(ethPALL&0xff)<<8 | uint16(ethPALL>>8)
}

// openCaptureSocketInNetns opens the capture socket inside nsName on a locked thread and restores the thread; an
// AF_PACKET socket stays in the netns it was created in. On error the thread stays locked and dies with the goroutine
// through fatalf, since it may still be in the wrong netns (#940).
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
