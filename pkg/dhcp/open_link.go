// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"fmt"
	"net"
)

// The engine renames the link on the move into the sandbox and again for a container-chosen interface name, so four
// attempts, each a netlink round trip and an open with no sleep, survive three renames (#1050).
const openLinkAttempts = 4

// errLinkNameUnstable ends an open whose link never kept its name; the client would be on another link (#1050).
var errLinkNameUnstable = errors.New("dhcp: the link kept being renamed while the client was opened")

// The net package reads the same table net.InterfaceByName resolves in, over netlink on the calling thread; /proc/net
// belongs to the thread group's leader and answers for the wrong namespace on a locked thread (#1050). A var so a test
// can rename a link without root.

// linkNameByIndex returns the current name of the link at index in the calling thread's network namespace.
var linkNameByIndex = func(index int) (string, error) {
	iface, err := net.InterfaceByIndex(index)
	if err != nil {
		return "", err
	}
	return iface.Name, nil
}

// linkIndexByName returns the index of the link currently named name, in the same namespace and table.
var linkIndexByName = func(name string) (int, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return iface.Index, nil
}

// The engine renames the container-side link after the plugin reads its name, and the library resolves the name again
// at the open, so the index, stable for the link's life, identifies it (#1050). Index 0 is a one-shot on an unrenamed
// link: one plain open. Only a name that moved under a failed open is retried; a successful open is kept unless its
// name belongs to another link; a failed namespace read never fails a working open. abandon disposes of a refused
// client on its own thread, since a dropped library client leaks its sockets.

// openOnLink opens a client on the link at index, whatever its name at the open, and returns the name last tried.
func openOnLink[T any](iface string, index int, open func(string) (T, error), abandon func(T)) (T, string, error) {
	var zero T
	name := iface

	for attempt := 1; ; attempt++ {
		if index > 0 {
			if current, err := linkNameByIndex(index); err == nil {
				name = current
			}
		}

		client, err := open(name)
		if err != nil {
			if index > 0 && attempt < openLinkAttempts {
				if current, rerr := linkNameByIndex(index); rerr == nil && current != name {
					continue
				}
			}
			return zero, name, err
		}

		if index <= 0 {
			return client, name, nil
		}

		// The library binds its sockets by index once resolved, so a later rename is harmless; only a name another link
		// took before the open is wrong (#1050).
		opened, err := linkIndexByName(name)
		if err != nil || opened == index {
			return client, name, nil
		}

		abandon(client)

		// A deleted link is reported with the kernel's reason and not retried; only a rename is (#1050).
		if _, rerr := linkNameByIndex(index); rerr != nil {
			return zero, name, rerr
		}

		if attempt >= openLinkAttempts {
			return zero, name, fmt.Errorf("%w: index %d", errLinkNameUnstable, index)
		}
	}
}
