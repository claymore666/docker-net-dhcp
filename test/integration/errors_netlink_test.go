// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
)

// TestErrors_ParentDown checks that a macvlan network on an admin-down parent is refused with ErrParentDown.
func TestErrors_ParentDown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	link, err := netlink.LinkByName(harness.HostVeth)
	if err != nil {
		t.Fatalf("LinkByName(%s): %v", harness.HostVeth, err)
	}
	if err := netlink.LinkSetDown(link); err != nil {
		t.Fatalf("LinkSetDown(%s): %v", harness.HostVeth, err)
	}
	t.Cleanup(func() {
		if err := netlink.LinkSetUp(link); err != nil {
			t.Logf("WARN: failed to restore %s to UP: %v", harness.HostVeth, err)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	netName := "dh-itest-err-parent-down"
	res, createErr := cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver: harness.DriverName,
		IPAM:   &network.IPAM{Driver: "null"},
		Options: map[string]string{
			"mode":   "macvlan",
			"parent": harness.HostVeth,
		},
	})
	if createErr == nil {
		_ = cli.NetworkRemove(context.Background(), res.ID)
		t.Fatalf("expected NetworkCreate to fail with parent-down error, got success")
	}
	if !strings.Contains(strings.ToLower(createErr.Error()), "parent interface is down") {
		t.Errorf("error missing expected substring 'parent interface is down'\nactual: %s", createErr.Error())
	} else {
		t.Logf("✓ parent-down rejected: %s", createErr.Error())
	}
}

// The kernel allows macvlan over a bridge, but it loops broadcasts and confuses MAC learning, so the plugin refuses it.

// TestErrors_ParentIsBridge checks that a Linux bridge is refused as a macvlan parent.
func TestErrors_ParentIsBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// IFNAMSIZ caps interface names at 15 characters.
	const brName = "dh-itest-br"
	br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: brName}}
	if err := netlink.LinkAdd(br); err != nil {
		t.Fatalf("LinkAdd(%s bridge): %v", brName, err)
	}
	t.Cleanup(func() {
		if l, err := netlink.LinkByName(brName); err == nil {
			_ = netlink.LinkDel(l)
		}
	})
	if err := netlink.LinkSetUp(br); err != nil {
		t.Fatalf("LinkSetUp(%s): %v", brName, err)
	}

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	netName := "dh-itest-err-parent-bridge"
	res, createErr := cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver: harness.DriverName,
		IPAM:   &network.IPAM{Driver: "null"},
		Options: map[string]string{
			"mode":   "macvlan",
			"parent": brName,
		},
	})
	if createErr == nil {
		_ = cli.NetworkRemove(context.Background(), res.ID)
		t.Fatalf("expected NetworkCreate to fail when parent is a bridge, got success")
	}
	if !strings.Contains(strings.ToLower(createErr.Error()), "unsuitable for macvlan") {
		t.Errorf("error missing expected substring 'unsuitable for macvlan'\nactual: %s", createErr.Error())
	} else {
		t.Logf("✓ parent-is-bridge rejected: %s", createErr.Error())
	}
}

// libnetwork refuses --ip on a null-IPAM network before CreateEndpoint, so only the driver-opt path is reachable here.

// TestErrors_DriverOptIPMalformed checks that an endpoint `ip=` driver-opt that is not a bare IPv4 is refused with ErrIPAM.
func TestErrors_DriverOptIPMalformed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-err-droptip"
	ctrName := "dh-itest-err-droptip-ctr"
	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	created, createErr := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}},
		harness.HostConfig(),
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netName: {
					DriverOpts: map[string]string{"ip": "not-a-valid-ip"},
				},
			},
		},
		nil,
		ctrName,
	)
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerRemove(bg, ctrName, container.RemoveOptions{Force: true})
	})

	// libnetwork may defer CreateEndpoint from ContainerCreate to ContainerStart, so either call can carry the refusal.
	var failure string
	if createErr != nil {
		failure = createErr.Error()
	} else if startErr := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); startErr != nil {
		failure = startErr.Error()
	}
	if failure == "" {
		t.Fatalf("expected ContainerCreate or ContainerStart to fail on malformed driver-opt ip, both succeeded")
	}
	if !strings.Contains(strings.ToLower(failure), "invalid driver-opt ip") {
		t.Errorf("error missing expected substring 'invalid driver-opt ip'\nactual: %s", failure)
	} else {
		t.Logf("✓ malformed driver-opt ip rejected: %s", failure)
	}
}
