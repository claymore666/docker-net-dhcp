//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
)

// TestErrors_ParentDown drives validateParentForChild's down-state
// branch. Brings the host veth admin-down, attempts to create a
// macvlan network on it, asserts the plugin rejects the create
// with ErrParentDown wrapping. Restores the link to UP in
// t.Cleanup so subsequent tests still see a healthy parent.
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

// TestErrors_ParentIsBridge drives validateParentForChild's
// disallowed-type branch. Creates a transient Linux bridge in the
// host netns, points the plugin at it as a macvlan parent, asserts
// rejection. macvlan over a bridge is something the kernel itself
// would happily allow but produces nonsensical behaviour for our
// use case (broadcast loops, MAC learning conflicts), so the
// plugin refuses up-front.
func TestErrors_ParentIsBridge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Linux IFNAMSIZ caps interface names at 15 characters + NUL.
	// "dh-itest-br" is 11; fits with room for a suffix.
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

// TestErrors_DriverOptIPMalformed drives the resolveExplicitV4
// driver-opt validation branch in CreateEndpoint: an endpoint-level
// `ip=` driver-opt that doesn't parse as a bare IPv4 must be
// rejected with ErrIPAM wrapping.
//
// The conflict path (Interface.Address from --ip vs driver-opt ip)
// is unreachable through the docker API on a null-IPAM network —
// libnetwork rejects --ip with "user specified IP address is
// supported only when connecting to networks with user configured
// subnets", before the plugin's CreateEndpoint sees it. The
// driver-opt path is the one we can exercise end-to-end here, and
// it covers the same parsing helper.
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

	_, createErr := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}},
		&container.HostConfig{},
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
	if createErr == nil {
		t.Fatalf("expected ContainerCreate to fail on malformed driver-opt ip, got success")
	}
	if !strings.Contains(strings.ToLower(createErr.Error()), "invalid driver-opt ip") {
		t.Errorf("error missing expected substring 'invalid driver-opt ip'\nactual: %s", createErr.Error())
	} else {
		t.Logf("✓ malformed driver-opt ip rejected: %s", createErr.Error())
	}
}
