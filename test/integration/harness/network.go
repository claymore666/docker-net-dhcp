// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
)

// CreateNetwork drives `docker network create` with the plugin and
// the per-test options. Registers a t.Cleanup to delete it. Returns
// the network ID.
//
// mode is "bridge", "macvlan", or "ipvlan". The harness picks the
// attachment point for the mode: HostVeth for macvlan, IpvlanParent
// for ipvlan (each its own netdev on one L2 segment — see #556 on the
// constants), BridgeName for bridge. Pass parent= or bridge= in
// extraOpts only to override that deliberately.
func CreateNetwork(t *testing.T, ctx context.Context, name, mode string, extraOpts map[string]string) string {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	opts := map[string]string{"mode": mode}
	switch mode {
	case "macvlan":
		opts["parent"] = HostVeth
	case "ipvlan":
		opts["parent"] = IpvlanParent
	case "bridge":
		opts["bridge"] = BridgeName
	}
	for k, v := range extraOpts {
		opts[k] = v
	}
	if parent := opts["parent"]; parent != "" && (mode == "macvlan" || mode == "ipvlan") {
		AssertParentFreeOfOtherKind(t, parent, mode)
	}

	// This span covers the plugin's whole CreateNetwork RPC, which
	// includes the preflight DHCP probe — an 8s budget that should
	// return on the first OFFER (#368).
	createStart := time.Now()
	res, err := cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver:  DriverName,
		IPAM:    &network.IPAM{Driver: "null"},
		Options: opts,
	})
	EndPhase(t, PhaseNetworkCreate, createStart)
	if err != nil {
		t.Fatalf("NetworkCreate(%s, mode=%s, opts=%v): %v", name, mode, opts, err)
	}
	t.Cleanup(func() {
		// Use a fresh context so a parent ctx-cancel during a
		// failure doesn't skip cleanup.
		removeStart := time.Now()
		err := cli.NetworkRemove(context.Background(), res.ID)
		EndPhase(t, PhaseNetworkRemove, removeStart)
		if err != nil && !isNotFound(err) {
			t.Logf("WARN: NetworkRemove(%s): %v", res.ID, err)
		}
	})
	return res.ID
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "No such network")
}

// AssertParentFreeOfOtherKind fails the test if parent already carries
// a child of the kind that cannot coexist with mode — an ipvlan child
// under a macvlan network's parent or the reverse. The kernel would
// refuse the plugin's LinkAdd with "device or resource busy", from deep
// inside a netlink call, and that reads as a plugin fault: it cost an
// investigation an hour before #556 named it. With dedicated parents
// this must never fire; if it does, some test left a child of the wrong
// kind on a parent that is not its own, and the message names both.
func AssertParentFreeOfOtherKind(t *testing.T, parent, mode string) {
	t.Helper()
	other := map[string]string{"macvlan": "ipvlan", "ipvlan": "macvlan"}[mode]
	if other == "" {
		return
	}
	parentLink, err := netlink.LinkByName(parent)
	if err != nil {
		t.Fatalf("parent %s for a %s network does not exist: %v", parent, mode, err)
	}
	links, err := netlink.LinkList()
	if err != nil {
		t.Fatalf("LinkList: %v", err)
	}
	for _, l := range links {
		if l.Attrs().ParentIndex == parentLink.Attrs().Index && l.Type() == other {
			t.Fatalf("fixture invariant broken (#556): %s network asked for parent %s, "+
				"but it already carries %s child %s — a parent is a macvlan port or an "+
				"ipvlan port, never both, and the kernel would refuse the plugin with EBUSY. "+
				"Some earlier test left that child behind on a parent that is not its kind's.",
				mode, parent, other, l.Attrs().Name)
		}
	}
}
