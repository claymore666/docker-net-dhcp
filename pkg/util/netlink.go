package util

import (
	"context"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// AwaitNetNS polls for a netns at path until it appears, ctx is cancelled,
// or interval-paced retries exhaust. Synchronous to avoid leaking a
// poller goroutine on ctx-cancel (the previous form did, and each leaked
// goroutine kept hammering netns.GetFromPath forever).
func AwaitNetNS(ctx context.Context, path string, interval time.Duration) (netns.NsHandle, error) {
	var dummy netns.NsHandle
	for {
		ns, err := netns.GetFromPath(path)
		if err == nil {
			return ns, nil
		}
		select {
		case <-ctx.Done():
			return dummy, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// AwaitLinkByIndex polls for a netlink Link by index until it appears,
// ctx is cancelled, or interval-paced retries exhaust. Synchronous for
// the same reason as AwaitNetNS.
func AwaitLinkByIndex(ctx context.Context, handle *netlink.Handle, index int, interval time.Duration) (netlink.Link, error) {
	for {
		link, err := handle.LinkByIndex(index)
		if err == nil {
			return link, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}
