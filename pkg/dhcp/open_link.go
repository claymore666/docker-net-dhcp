// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"fmt"
	"net"
)

// openLinkAttempts caps how often one client open is retried when the
// link is renamed under it.
//
// THE RETRY IS NOT A WAIT. Every attempt is a netlink round trip and an
// open, with no sleep anywhere, so the whole of this costs microseconds
// and cannot eat the attach budget. Four is chosen against what the
// engine does: it moves the link into the sandbox namespace and renames
// it, and it renames it once more where a container asked for an
// interface name of its own. Two renames arriving inside one open is
// already the pathological case; four attempts survive three.
const openLinkAttempts = 4

// errLinkNameUnstable is the honest end of a link whose name never
// stood still long enough to open a client on it. It is not a fallback:
// returning the client anyway would install a renewal client on a link
// that belongs to somebody else.
var errLinkNameUnstable = errors.New("dhcp: the link kept being renamed while the client was opened")

// linkNameByIndex answers what the link at index is called NOW, in the
// network namespace of the calling thread.
//
// net and not netlink-the-library, deliberately: this is the exact
// table the library resolves the name in (net.InterfaceByName), read in
// the opposite direction, so the two cannot disagree about what a name
// means. It is a netlink RTM_GETLINK under the hood, opened on the
// calling thread, and NOT a read of /proc/net, which belongs to the
// thread group's leader and would answer for the wrong namespace on a
// locked thread.
//
// A var so a test can rename a link at a chosen instant without root.
var linkNameByIndex = func(index int) (string, error) {
	iface, err := net.InterfaceByIndex(index)
	if err != nil {
		return "", err
	}
	return iface.Name, nil
}

// linkIndexByName answers which link currently carries name, same
// namespace and same table as linkNameByIndex.
var linkIndexByName = func(name string) (int, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return iface.Index, nil
}

// openOnLink opens a client on the link identified by index, whatever
// that link is called at the instant of the open.
//
// WHY THE INDEX AND NOT THE NAME. The name a container-side link has
// when the plugin reads it is not the name it has when the client is
// opened: the engine moves the link into the sandbox namespace and
// renames it (to eth0, or to the name the container asked for), and the
// library resolves the name a second time, inside the namespace, at the
// open. Every read of the name is therefore already stale when it is
// used, and narrowing the gap only makes the failure rarer. The index
// does not change for the life of the link, so it is asked at the
// instant the name is needed, and asked again to check what was opened.
//
// The name still comes in and is still used: index 0 means the caller
// has no index to offer (the one-shot acquisitions, whose link is in
// this namespace and is not being renamed by anybody), and then this is
// one plain open on the caller's name, which is what it always was.
//
// WHAT IT PROMISES. Either a client running on the link at index, or
// the error that stopped it, once, with nothing retried that a retry
// cannot change:
//
//   - an index that no longer resolves is a link that is gone, so the
//     open's own error stands and nothing is retried;
//   - an open that failed is retried ONLY when the link's name moved
//     under it, which is the one failure a second attempt can fix;
//   - an open that succeeded is kept unless the name it opened turns
//     out to belong to another link, which is the one way a successful
//     open is wrong;
//   - a namespace read that fails never turns a working open into a
//     failure: the caller's name carries the attempt, exactly as
//     before this existed.
//
// Generic over the client type because the v4 and v6 families open the
// same way and a second hand-written copy is where they start to
// differ. abandon disposes of a client this function will not return:
// dropping a library client without running it leaks its sockets, and
// it must be dropped on the thread that made it, inside the namespace
// it was made in, which is where this whole function runs.
//
// Returns the name it last attempted, so the caller's error text names
// the link the open actually tried and not the one it was asked about.
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

		// WHAT WAS OPENED, NOT WHAT WAS RENAMED. The question is
		// whether the name this client resolved belongs to the link it
		// was meant for, and a rename that happens AFTER the open is
		// not that question: the library's sockets are bound to the
		// interface by index once resolved, so a link renamed under a
		// running client keeps leasing. Asking "does this name still
		// point at my link" answers the one case that is wrong -- the
		// old name was taken by another link before the open reached
		// it -- and leaves the harmless case alone.
		opened, err := linkIndexByName(name)
		if err != nil || opened == index {
			return client, name, nil
		}

		abandon(client)

		// A LINK THAT IS GONE IS NOT A LINK THAT KEEPS MOVING. The
		// open succeeded on somebody else's link, which happens both
		// when the rename freed the name and when the link was
		// DELETED and the name was taken after it. The two end
		// differently and an operator acts on them differently, so
		// the index is asked once more: a link that no longer
		// resolves is reported with the kernel's reason for it, and
		// nothing is retried, because nothing about it changes.
		if _, rerr := linkNameByIndex(index); rerr != nil {
			return zero, name, rerr
		}

		if attempt >= openLinkAttempts {
			return zero, name, fmt.Errorf("%w: index %d", errLinkNameUnstable, index)
		}
	}
}
