// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// The rename of a host-side veth is two kernel calls, and between them
// the generated `dh-<12 hex>` name resolves to nothing: the kernel
// refuses an altname equal to the link's current name and refuses a
// rename onto the link's own altname (MEASURED in a user namespace on
// 6.12.107), so the old name can only be put back after the rename has
// freed it. These cells drive a reader INTO that window and require it
// to see the link (#1051).
//
// The window is opened from inside the rename seam rather than by
// sleeping: the fake table drops the name, releases the reader, and the
// altname goes on only once the reader has answered or a bound has
// passed. Without the guard the reader answers from inside the window
// every time.

// windowTable is a link table keyed by every name that resolves,
// altnames included, which is what netlink.LinkByName does.
type windowTable struct {
	mu      sync.Mutex
	link    *netlink.Veth
	names   map[string]bool
	deleted []string
}

func newWindowTable(generated string) *windowTable {
	return &windowTable{
		link:  &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: generated, Index: 11}},
		names: map[string]bool{generated: true},
	}
}

func (w *windowTable) byName(name string) (netlink.Link, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.names[name] {
		return nil, netlink.LinkNotFoundError{}
	}
	return w.link, nil
}

// driveTheRenameWindow runs one rename with probe racing it, and
// releases probe at the instant the generated name has stopped
// resolving. The rename waits for the probe to answer before it puts
// the name back, bounded so a probe the guard is holding cannot wedge
// it: that bound is reached in exactly the arm where the guard works.
func driveTheRenameWindow(t *testing.T, w *windowTable, generated string, probe func()) {
	t.Helper()

	windowOpen := make(chan struct{})
	probeDone := make(chan struct{})

	prevBy, prevSet, prevAlt, prevDel := nlLinkByName, nlLinkSetName, nlLinkAddAltName, nlLinkDel
	t.Cleanup(func() {
		nlLinkByName, nlLinkSetName, nlLinkAddAltName, nlLinkDel = prevBy, prevSet, prevAlt, prevDel
	})

	nlLinkByName = func(name string) (netlink.Link, error) { return w.byName(name) }
	nlLinkSetName = func(_ netlink.Link, name string) error {
		w.mu.Lock()
		delete(w.names, w.link.Attrs().Name)
		w.link.LinkAttrs.Name = name
		w.names[name] = true
		w.mu.Unlock()

		close(windowOpen)
		select {
		case <-probeDone:
		case <-time.After(300 * time.Millisecond):
		}
		return nil
	}
	nlLinkAddAltName = func(_ netlink.Link, name string) error {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.names[name] = true
		return nil
	}
	nlLinkDel = func(l netlink.Link) error {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.deleted = append(w.deleted, l.Attrs().Name)
		return nil
	}

	go func() {
		defer close(probeDone)
		<-windowOpen
		probe()
	}()

	m := newDHCPManager(nil, JoinRequest{
		NetworkID:  windowNetID,
		EndpointID: windowEndpointID,
	}, DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0", HostIfname: HostIfnameContainerName}).
		withPlugin(&Plugin{})
	m.renameHostLink(true, "/web", "web-host")

	select {
	case <-probeDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("the reader never answered: it is still inside the rename of %q", generated)
	}
}

const (
	windowNetID      = "net-rename-window"
	windowEndpointID = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	windowHostName   = "dh-a1b2c3d4e5f6"
)

func TestDeleteEndpoint_ARenameInFlightIsNotAFinishedTeardown(t *testing.T) {
	p := deleteEndpointPlugin(t, windowNetID, DHCPNetworkOptions{Bridge: "br0", Mode: ModeBridge})
	w := newWindowTable(windowHostName)

	var delErr error
	driveTheRenameWindow(t, w, windowHostName, func() {
		delErr = p.DeleteEndpoint(context.Background(), DeleteEndpointRequest{
			NetworkID:  windowNetID,
			EndpointID: windowEndpointID,
		})
	})

	if delErr != nil {
		t.Fatalf("DeleteEndpoint during a rename returned %v, want nil", delErr)
	}
	w.mu.Lock()
	deleted := append([]string(nil), w.deleted...)
	w.mu.Unlock()
	if len(deleted) != 1 {
		t.Fatalf("the teardown deleted %v, want exactly one link. A delete that lands between the "+
			"rename and the altname is told the host has no link called %q, reads that as a teardown "+
			"that already happened and returns nil: the veth stays on the bridge for the life of the "+
			"host, silently, which is the failure the altname exists to prevent (#1051)",
			deleted, windowHostName)
	}
}

func TestEndpointOperInfo_ARenameInFlightStillHasAHostVeth(t *testing.T) {
	p := deleteEndpointPlugin(t, windowNetID, DHCPNetworkOptions{Bridge: "br0", Mode: ModeBridge})
	w := newWindowTable(windowHostName)

	var (
		info    InfoResponse
		infoErr error
	)
	driveTheRenameWindow(t, w, windowHostName, func() {
		info, infoErr = p.EndpointOperInfo(context.Background(), InfoRequest{
			NetworkID:  windowNetID,
			EndpointID: windowEndpointID,
		})
	})

	if infoErr != nil {
		t.Fatalf("EndpointOperInfo during a rename returned %v, want the link. This call is not "+
			"serialised with any attach, so a `docker network inspect --verbose` on a busy host is "+
			"told the endpoint has no host veth while it is being renamed (#1051)", infoErr)
	}
	if got := info.Value["veth_host"]; got != "web" {
		t.Errorf("veth_host = %v, want web: the name the link carries once the rename it read "+
			"through has finished", got)
	}
}

// The window is as narrow as two kernel calls only while nothing else
// runs inside it. Work added between them -- a retry, a second lookup,
// a counter read -- widens it for every reader the guard cannot cover:
// an operator's `ip link`, the suite, another process. This cell is
// that bound, over what reaches the kernel; a pure delay between the
// two calls is not visible here and is the part this bound gives up.
func TestRenameHostLink_NothingReachesTheKernelBetweenTheTwoCalls(t *testing.T) {
	w := newWindowTable(windowHostName)

	var (
		mu    sync.Mutex
		order []string
	)
	record := func(op string) {
		mu.Lock()
		order = append(order, op)
		mu.Unlock()
	}

	prevBy, prevSet, prevAlt := nlLinkByName, nlLinkSetName, nlLinkAddAltName
	t.Cleanup(func() { nlLinkByName, nlLinkSetName, nlLinkAddAltName = prevBy, prevSet, prevAlt })

	nlLinkByName = func(name string) (netlink.Link, error) {
		record("lookup")
		return w.byName(name)
	}
	nlLinkSetName = func(_ netlink.Link, name string) error {
		record("rename")
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.names, w.link.Attrs().Name)
		w.link.LinkAttrs.Name = name
		w.names[name] = true
		return nil
	}
	nlLinkAddAltName = func(_ netlink.Link, name string) error {
		record("altname")
		w.mu.Lock()
		defer w.mu.Unlock()
		w.names[name] = true
		return nil
	}

	m := newDHCPManager(nil, JoinRequest{
		NetworkID:  windowNetID,
		EndpointID: windowEndpointID,
	}, DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0", HostIfname: HostIfnameContainerName}).
		withPlugin(&Plugin{})
	m.renameHostLink(true, "/web", "web-host")

	mu.Lock()
	defer mu.Unlock()
	for i, op := range order {
		if op != "rename" {
			continue
		}
		if i+1 >= len(order) || order[i+1] != "altname" {
			t.Fatalf("the kernel calls went %v: the generated name %q is gone from the rename until "+
				"the altname, and everything between them is time in which an operator's `ip link`, "+
				"the suite and another process are told this host has no such link (#1051)",
				order, windowHostName)
		}
		return
	}
	t.Fatalf("this cell saw no rename at all, in %v: it asserts nothing until renameHostLink asks "+
		"the kernel for a name again", order)
}
