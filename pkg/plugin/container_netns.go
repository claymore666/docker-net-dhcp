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

// openContainerNetNS opens pid's ns/net through the /proc/<pid> fd that passed the
// cgroup check; procfs fails that openat with ESRCH once the task exits, so a recycled
// PID cannot be reached, and the caller passes the fd on instead of a path (#688).
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

// awaitContainerNetNS retries openContainerNetNS, identity check included, and its
// deadline error carries the last attempt's cause (#317).
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
