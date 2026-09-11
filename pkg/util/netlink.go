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

// DumpResult is what a netlink dump call returns, with the one error
// that is not a failure removed.
//
// In vishvananda/netlink v1.3.1 every dump-style call — LinkList,
// AddrList, RouteList and their filtered and *WithOptions forms —
// returns ErrDumpInterrupted TOGETHER WITH A USABLE RESULT SET.
// link_linux.go:2419-2436 bails early only on an error that is not the
// sentinel; otherwise it parses the messages and hands them back
// alongside it. The sentinel means the kernel set NLM_F_DUMP_INTR
// because the table changed mid-dump, which is what a suite creating
// and tearing down macvlan children generates by design.
//
// Treating it as fatal cost the arm64 lane a red on the v1.8.0-rc2 tag
// (#802) and is the fail-open half of the mode-collision guard in
// childLinkKind. Every OTHER error is returned unchanged: a helper
// that swallowed them would turn a real netlink failure into an empty
// result set, which is the same blindness pointing the other way.
//
// Used as `util.DumpResult(netlink.LinkList())` — the dump call's two
// results are the two parameters — so no call site has to spell the
// sentinel, and scripts/check-netlink-dump-errors.sh refuses a dump
// call that does not go through here.
func DumpResult[T any](v []T, err error) ([]T, error) {
	if err != nil && !errors.Is(err, netlink.ErrDumpInterrupted) {
		return nil, err
	}
	return v, nil
}
