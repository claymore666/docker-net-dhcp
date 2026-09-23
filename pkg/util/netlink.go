// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vishvananda/netlink"
)

// The deadline error carries the last attempt's error: a bare deadline hid a persistent failure for weeks (#317).

// AwaitLinkByIndex polls LinkByIndex in the caller's goroutine until the link appears or ctx ends.
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

// vishvananda/netlink v1.3.1 returns ErrDumpInterrupted together with a usable result set (Handle.LinkList and the
// other dump calls): the kernel set NLM_F_DUMP_INTR because the table changed mid-dump, which a suite creating and
// removing macvlan children does by design. Treating it as fatal turned the arm64 lane red on v1.8.0-rc2 (#802).
// Swallowing any other error would turn a real failure into an empty set. scripts/check-netlink-dump-errors.sh
// refuses a dump call that does not go through here.

// DumpResult returns a netlink dump call's results with ErrDumpInterrupted dropped and every other error unchanged.
func DumpResult[T any](v []T, err error) ([]T, error) {
	if err != nil && !errors.Is(err, netlink.ErrDumpInterrupted) {
		return nil, err
	}
	return v, nil
}
