// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// openContainerNetNS opens the network namespace of pid, having first
// confirmed that pid still belongs to ctrID.
//
// WHY THIS IS NOT netns.GetFromPath("/proc/<pid>/ns/net")
//
// It is the same hazard #688 closed for resolv.conf, reaching a much
// larger sink. The plugin runs in the host PID namespace, so a PID that
// has been recycled names an arbitrary host task; a container that exits
// inside the attach window (awaitTimeout + attachDaemonBusyGrace = 70s)
// leaves its PID free for one. What the plugin then does with the
// namespace it opened is not one file: the netlink handle built from it
// carries every address, MTU and route change the manager makes, with
// CAP_NET_ADMIN, and dhcpcd is spawned into it as root.
//
// openContainerProc's own comment already names the rule this function
// exists to follow -- "re-deriving the path as a string afterwards would
// reopen the window the check just closed". Opening ns/net relative to
// the directory fd it returns is what keeps the check and the use on the
// same task: procfs invalidates a /proc/<pid> dentry when the task
// exits, so this openat either reaches the process that passed the
// cgroup check or fails with ESRCH.
//
// The returned handle is an open file descriptor. It is the caller's to
// close, and passing IT onward -- rather than a path to re-resolve -- is
// the second half of the fix: two independent resolutions of the same
// string can disagree, and the one inside DHCPClient.Start used to.
func openContainerNetNS(pid int, ctrID string) (netns.NsHandle, error) {
	procDir, err := openContainerProc(pid, ctrID)
	if err != nil {
		return netns.None(), err
	}
	defer procDir.Close()

	fd, err := unix.Openat(int(procDir.Fd()), "ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return netns.None(), fmt.Errorf("open network namespace of pid %d: %w", pid, err)
	}
	return netns.NsHandle(fd), nil
}

// awaitContainerNetNS polls openContainerNetNS until it succeeds, ctx is
// cancelled, or interval-paced retries exhaust.
//
// Polling here is strictly better than polling a path, which is what
// this replaced: every attempt re-runs the identity check, so a wait
// that spans a container exit cannot end by attaching to whatever turned
// up at that PID. It waits for THIS container rather than for something,
// anything, to appear.
//
// Like the other Await helpers, the deadline error carries the last
// attempt's cause. That is the #317 lesson and it applies with extra
// force here: the identity refusal and a slow-starting container both
// look like a timeout otherwise, and only one of them is an attack.
func awaitContainerNetNS(ctx context.Context, pid int, ctrID string, interval time.Duration) (netns.NsHandle, error) {
	var lastErr error
	for {
		ns, err := openContainerNetNS(pid, ctrID)
		if err == nil {
			return ns, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return netns.None(), fmt.Errorf("%w (last attempt: %w)", ctx.Err(), lastErr)
		case <-time.After(interval):
		}
	}
}
