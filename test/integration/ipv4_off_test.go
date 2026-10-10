// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// A network created with `--ipv4=false` has no IPv4 pool, so the plugin must run no DHCPv4 for it: the segment's own
// server sees no DHCPv4 message, the container holds no IPv4 address and no IPv4 default route, the engine attaches no
// second network for a gateway, and /Plugin.Health says ipv4_off (#1135).

// ipv4OffSettle is how long a DHCPv4 client that should not exist is given to show itself on the wire.
const ipv4OffSettle = 6 * time.Second

type ipv4OffShape struct {
	at       v6Attach
	v6       harness.V6Mode
	ipv6Mode string
	// ipam runs the network with this plugin as its IPAM driver and no --ipv6, which the engine never consults (#1135).
	ipam bool
}

var (
	onV6Ipvlan = v6Attach{mode: "ipvlan", kind: "ipvlan"}
)

func TestIPv4OffBridgeDHCP(t *testing.T) {
	testIPv4Off(t, ipv4OffShape{at: onV6Bridge, v6: harness.V6Managed, ipv6Mode: "dhcp"}, "dh-itest-v4off-bd")
}

func TestIPv4OffBridgeSLAAC(t *testing.T) {
	testIPv4Off(t, ipv4OffShape{at: onV6Bridge, v6: harness.V6SLAAC, ipv6Mode: "slaac"}, "dh-itest-v4off-bs")
}

func TestIPv4OffMacvlanDHCP(t *testing.T) {
	testIPv4Off(t, ipv4OffShape{at: onV6Macvlan, v6: harness.V6Managed, ipv6Mode: "dhcp"}, "dh-itest-v4off-md")
}

func TestIPv4OffIPvlanDHCP(t *testing.T) {
	testIPv4Off(t, ipv4OffShape{at: onV6Ipvlan, v6: harness.V6Managed, ipv6Mode: "dhcp"}, "dh-itest-v4off-id")
}

// TestIPv4OffIPAMWithoutIPv6 settles the open point of #1135: without --ipv6 the engine never asks the IPAM
// driver for anything, so the network reaches the plugin as a driver-mode one. An engine or CLI that refuses the
// pair skips the case with its refusal in the log instead of failing it.
func TestIPv4OffIPAMWithoutIPv6(t *testing.T) {
	testIPv4Off(t, ipv4OffShape{at: onV6Bridge, v6: harness.V6Managed, ipv6Mode: "dhcp", ipam: true}, "dh-itest-v4off-ip")
}

func testIPv4Off(t *testing.T, shape ipv4OffShape, netName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	f := harness.NewV6Fixture(t, shape.v6)
	dumpOnFailure(t, f)
	cli, err := harness.NewDockerClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	opts := map[string]string{"ipv6_mode": shape.ipv6Mode}
	link := "eth0"
	switch shape.at.mode {
	case "bridge":
		opts["bridge"] = f.Bridge()
		link = f.Bridge() + "0"
	default:
		opts["parent"] = f.MacvlanParent()
	}
	if shape.ipam {
		off := false
		opts["mode"] = shape.at.mode
		_, err := cli.NetworkCreate(ctx, netName, docker.NetworkCreateOptions{
			Driver: harness.DriverName, IPAM: &network.IPAM{Driver: harness.DriverName}, Options: opts, EnableIPv4: &off,
		})
		if err != nil {
			t.Fatalf("NetworkCreate with --ipv4=false, this plugin as IPAM driver and no --ipv6 (#1135): %v", err)
		}
		t.Cleanup(func() { _, _ = cli.NetworkRemove(context.Background(), netName, docker.NetworkRemoveOptions{}) })
	} else {
		harness.CreateNetworkIPv4Off(t, ctx, netName, shape.at.mode, opts)
	}

	id := startIPv4OffContainer(t, ctx, cli, netName)
	assertIPv4OffContainer(t, ctx, cli, f, id, netName, link, shape.ipv6Mode)

	// A restart rebuilds the endpoint from the stored options, and the flag has to be among them (#1135).
	restartPluginAndWait(t, ctx, cli)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)
	awaitIPv4OffHealth(t, ctx, cli, ep)
	time.Sleep(ipv4OffSettle)
	assertNoDHCPv4OnTheWire(t, f)
}

func startIPv4OffContainer(t *testing.T, ctx context.Context, cli *docker.Client, netName string) string {
	t.Helper()
	ctrName := netName + "-ctr"
	create, err := cli.ContainerCreate(ctx, docker.ContainerCreateOptions{
		Config:           &container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		HostConfig:       harness.HostConfig(),
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
		Name:             ctrName,
	})
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), create.ID, docker.ContainerRemoveOptions{Force: true})
	})
	if _, err := cli.ContainerStart(ctx, create.ID, docker.ContainerStartOptions{}); err != nil {
		t.Fatalf("a container on a --ipv4=false network did not start: %v", err)
	}
	return create.ID
}

func assertIPv4OffContainer(t *testing.T, ctx context.Context, cli *docker.Client, f *harness.V6Fixture, id, netName, link, ipv6Mode string) {
	t.Helper()
	if ipv6Mode == "slaac" {
		awaitContainerV6(t, ctx, id, v6SegmentPrefix(t), slaacAddrBudget())
	} else if linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget) == "" {
		t.Fatalf("no global IPv6 appeared on %s: the endpoint of a --ipv4=false network carries its IPv6 address alone", link)
	}

	ins, err := cli.ContainerInspect(ctx, id, docker.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	nets := ins.Container.NetworkSettings.Networks
	if len(nets) != 1 {
		t.Errorf("the container is on %d networks, want only %s: with no gateway named the engine attaches its gateway bridge",
			len(nets), netName)
	}
	if ep := nets[netName]; ep == nil || ep.IPAddress.IsValid() {
		t.Errorf("docker inspect shows an IPv4 address %v on a --ipv4=false network", ep)
	}
	if routes := harness.ExecOutput(t, ctx, id, "ip", "-4", "route", "show"); strings.Contains(routes, "default") {
		t.Errorf("the container has an IPv4 default route:\n%s", routes)
	}
	for _, l := range strings.Split(strings.TrimSpace(harness.ExecOutput(t, ctx, id, "ip", "-4", "-o", "addr", "show")), "\n") {
		if l != "" && !strings.Contains(l, " lo ") {
			t.Errorf("the container holds an IPv4 address beyond lo: %s", l)
		}
	}

	ep := harness.EndpointShortID(t, ctx, cli, id, netName)
	awaitIPv4OffHealth(t, ctx, cli, ep)
	time.Sleep(ipv4OffSettle)
	assertNoDHCPv4OnTheWire(t, f)
}

func awaitIPv4OffHealth(t *testing.T, ctx context.Context, cli *docker.Client, ep string) {
	t.Helper()
	harness.WaitPluginHealthFor(t, ctx, cli, 30*time.Second, "the endpoint to read ipv4_off with no address",
		func(h *harness.HealthResponse) bool {
			e, ok := endpointView(h, ep)
			return ok && e.LeaseState == "ipv4_off" && e.Address == ""
		})
}

// The fixture is this test's own segment, so any DHCPv4 message in its server's log is the plugin's.
func assertNoDHCPv4OnTheWire(t *testing.T, f *harness.V6Fixture) {
	t.Helper()
	if n := f.CountDHCPv4Messages(); n > 0 {
		t.Errorf("the segment's server logged %d DHCPv4 messages on a network that has no IPv4", n)
	}
}

func restartPluginAndWait(t *testing.T, ctx context.Context, cli *docker.Client) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = cli.PluginEnable(context.Background(), harness.PluginRef, docker.PluginEnableOptions{Timeout: 30})
	})
	if _, err := cli.PluginDisable(ctx, harness.PluginRef, docker.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 30*time.Second); err != nil {
		t.Fatalf("plugin did not reach disabled state: %v", err)
	}
	if _, err := cli.PluginEnable(ctx, harness.PluginRef, docker.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("plugin did not re-enable: %v", err)
	}
}

// TestIPv4OffIPv6OffRefused checks that --ipv4=false with no ipv6_mode is refused at create, with the remedy named.
func TestIPv4OffIPv6OffRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cli, err := harness.NewDockerClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	const netName = "dh-itest-v4off-refused"
	off := false
	_, err = cli.NetworkCreate(ctx, netName, docker.NetworkCreateOptions{
		Driver: harness.DriverName, IPAM: &network.IPAM{Driver: "null"}, EnableIPv4: &off,
		Options: map[string]string{"mode": "macvlan", "parent": harness.HostVeth},
	})
	t.Cleanup(func() { _, _ = cli.NetworkRemove(context.Background(), netName, docker.NetworkRemoveOptions{}) })
	if err == nil {
		t.Fatal("--ipv4=false with ipv6_mode off was accepted: an endpoint would have no address at all")
	}
	if !strings.Contains(err.Error(), "ipv6_mode") {
		t.Errorf("the refusal does not name the remedy -o ipv6_mode=: %v", err)
	}
}
