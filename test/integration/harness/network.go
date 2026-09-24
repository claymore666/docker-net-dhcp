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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// CreateNetwork creates a plugin network of mode bridge, macvlan or ipvlan on the harness's parent for that mode (#556), with cleanup, and returns its ID.
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

	// The span includes the plugin's preflight DHCP probe, an 8 s budget that returns on the first OFFER (#368).
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

// AssertParentFreeOfOtherKind fails if parent carries a child of the kind that cannot coexist with mode. The kernel
// refuses the plugin's LinkAdd for an ipvlan child under a macvlan parent, or the reverse, with "device or resource
// busy" (#556).
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
	links, err := util.DumpResult(netlink.LinkList())
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

// CreateNetworkIPAM creates a network with this plugin as both network and IPAM driver (#110). A separate helper, since
// the `--ipam-driver null` shape does not change (D19); an empty subnet is the untyped case the driver answers with 0.0.0.0/0.
func CreateNetworkIPAM(t *testing.T, ctx context.Context, name, mode, subnet string, ipamOpts map[string]string, extraOpts map[string]string) string {
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

	ipam := &network.IPAM{Driver: DriverName, Options: ipamOpts}
	if subnet != "" {
		ipam.Config = []network.IPAMConfig{{Subnet: subnet}}
	}

	createStart := time.Now()
	res, err := cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver:  DriverName,
		IPAM:    ipam,
		Options: opts,
	})
	EndPhase(t, PhaseNetworkCreate, createStart)
	if err != nil {
		t.Fatalf("NetworkCreate(%s, mode=%s, ipam-driver=%s, subnet=%q, ipam-opts=%v, opts=%v): %v",
			name, mode, DriverName, subnet, ipamOpts, opts, err)
	}
	t.Cleanup(func() {
		removeStart := time.Now()
		err := cli.NetworkRemove(context.Background(), res.ID)
		EndPhase(t, PhaseNetworkRemove, removeStart)
		if err != nil && !isNotFound(err) {
			t.Logf("WARN: NetworkRemove(%s): %v", res.ID, err)
		}
	})
	return res.ID
}

// CreateNetworkIPAMErr returns the daemon's error for a create that must be refused, and registers no cleanup (#110).
func CreateNetworkIPAMErr(ctx context.Context, name, mode, subnet string, ipamOpts, extraOpts map[string]string) error {
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

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
	ipam := &network.IPAM{Driver: DriverName, Options: ipamOpts}
	if subnet != "" {
		ipam.Config = []network.IPAMConfig{{Subnet: subnet}}
	}
	res, err := cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver:  DriverName,
		IPAM:    ipam,
		Options: opts,
	})
	if err == nil {
		_ = cli.NetworkRemove(context.Background(), res.ID)
		return nil
	}
	return err
}
