// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"context"
	"fmt"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// AwaitNetNS polls for a netns at path until it appears, ctx is cancelled,
// or interval-paced retries exhaust. Synchronous to avoid leaking a
// poller goroutine on ctx-cancel (the previous form did, and each leaked
// goroutine kept hammering netns.GetFromPath forever).
//
// On ctx expiry the returned error wraps ctx.Err() (so errors.Is against
// context.DeadlineExceeded keeps working) and carries the last attempt's
// underlying error. A bare "context deadline exceeded" hid a persistent
// EACCES for weeks in production — the netns open needs ptrace access to
// the target process, and a permission failure retried to the deadline is
// indistinguishable from a startup race without this (#317).
func AwaitNetNS(ctx context.Context, path string, interval time.Duration) (netns.NsHandle, error) {
	var dummy netns.NsHandle
	var lastErr error
	for {
		ns, err := netns.GetFromPath(path)
		if err == nil {
			return ns, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return dummy, fmt.Errorf("%w (last attempt: %w)", ctx.Err(), lastErr)
		case <-time.After(interval):
		}
	}
}

// AwaitLinkByIndex polls for a netlink Link by index until it appears,
// ctx is cancelled, or interval-paced retries exhaust. Synchronous for
// the same reason as AwaitNetNS; surfaces the last attempt's error for
// the same reason too.
func AwaitLinkByIndex(ctx context.Context, handle *netlink.Handle, index int, interval time.Duration) (netlink.Link, error) {
	var lastErr error
	for {
		link, err := handle.LinkByIndex(index)
		if err == nil {
			return link, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last attempt: %w)", ctx.Err(), lastErr)
		case <-time.After(interval):
		}
	}
}
