// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"context"
	"fmt"
	"time"

	"github.com/vishvananda/netlink"
)

// AwaitLinkByIndex polls for a netlink Link by index until it appears,
// ctx is cancelled, or interval-paced retries exhaust. Synchronous
// because the async form leaked a goroutine per call; it surfaces the
// last attempt's error alongside the deadline because a bare "context
// deadline exceeded" hid a persistent failure in production for weeks
// (#317).
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
