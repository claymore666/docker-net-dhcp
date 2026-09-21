// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"os"
	"testing"
	"time"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// TestSetupClient_TheClientIsOpenedOnTheLinkAndNotOnlyOnItsName closes
// the hole the re-read left open (#1050).
//
// The re-read by index makes the name current at the instant the plugin
// reads it, and the name is resolved AGAIN, inside the namespace, when
// the client is opened. On a host where the sandbox key route carries
// the attach the open lands early in the container start, so the
// engine's rename falls between those two resolutions and the open
// fails on a name the kernel no longer has. The index is the one thing
// about the link that does not change, so it travels with the name and
// the open resolves the current name from it.
//
// WHAT THIS DRIVES: that the index of the link the attach LOCATED is
// what the client is opened with. What happens with that index on the
// far side is openOnLink's own subject, in pkg/dhcp, where a rename can
// be landed at the instant of the open without a namespace or a
// capability.
func TestSetupClient_TheClientIsOpenedOnTheLinkAndNotOnlyOnItsName(t *testing.T) {
	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"net-1": {Containers: map[string]dNetwork.EndpointResource{
				"ctr-1": {EndpointID: "ep-abcdef"},
			}},
		},
		containerResult: map[string]dContainer.InspectResponse{
			"ctr-1": {
				ContainerJSONBase: &dContainer.ContainerJSONBase{State: &dContainer.State{Pid: os.Getpid()}},
				Config:            &dContainer.Config{Hostname: "ctr-1"},
			},
		},
	}
	m, _ := daemonFreeManager(t, docker)

	// The re-read answers with the name the engine has just given the
	// link, on the index the located link had. Both halves matter: the
	// name proves the drive reaches the open path, the index is what
	// the assertion is about.
	const renamed = "eth0"
	var locatedIndex int
	prevByIndex := nlLinkByIndex
	nlLinkByIndex = func(_ *netlink.Handle, index int) (netlink.Link, error) {
		locatedIndex = index
		return &netlink.Device{LinkAttrs: netlink.LinkAttrs{
			Index:        index,
			Name:         renamed,
			HardwareAddr: m.MacAddress,
		}}, nil
	}
	t.Cleanup(func() { nlLinkByIndex = prevByIndex })

	var (
		openedName  string
		openedIndex int
		opens       int
	)
	prevNew := newDHCPClient
	newDHCPClient = func(iface string, opts *dhcp.DHCPClientOptions) (*dhcp.DHCPClient, error) {
		opens++
		openedName, openedIndex = iface, opts.LinkIndex
		return prevNew(iface, opts)
	}
	t.Cleanup(func() { newDHCPClient = prevNew })
	withStartedClient(t, func() {})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if opens != 1 {
		t.Fatalf("the attach prepared %d clients, want one: the drive is not measuring the open", opens)
	}
	if locatedIndex == 0 {
		t.Fatal("the attach re-read no link index at all, so there is nothing for the open to resolve " +
			"the current name from")
	}
	if openedIndex != locatedIndex {
		t.Errorf("the client was opened with index %d and the attach located link %d: an index that is "+
			"not the located link's resolves some other link, which is worse than resolving a stale "+
			"name", openedIndex, locatedIndex)
	}
	if openedName != renamed {
		t.Errorf("the client was opened on %q, want %q: the name is still read as late as it can be, "+
			"and the index is what covers the rest", openedName, renamed)
	}
}
