// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// dnsmasq hashes the client identity across the whole range, so an unreserved high address drew .89 and .12 on one
// commit; the address is reserved by MAC, since initialDHCPHostname returns "" before the endpoint is bound (#425).

// TestStaticIP_DriverOpt checks that `--driver-opt ip=<addr>` is requested as option 50 and leased by the server.
func TestStaticIP_DriverOpt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const netName = "dh-itest-staticip"
	ctrName := harness.StaticTestHostname
	wantIP := harness.StaticTestIP

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// harness.RunContainer takes no per-endpoint DriverOpts.
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: harness.TestImage,
			Cmd:   []string{"sleep", "infinity"},
			// The hostname only makes the dnsmasq log readable; the reservation keys on the MAC (#425).
			Hostname: harness.StaticTestHostname,
		},
		harness.HostConfig(),
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netName: {
					DriverOpts: map[string]string{"ip": wantIP},
					// Must match the fixture's --dhcp-host reservation (#425).
					MacAddress: harness.StaticTestMAC,
				},
			},
		},
		nil,
		ctrName,
	)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	id := create.ID
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerStop(bg, id, container.StopOptions{})
		_ = cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
	})

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var gotIP string
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				gotIP = ep.IPAddress
			}
		}
		if gotIP != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if gotIP == "" {
		t.Fatalf("container never got an IP within %v", harness.IPAcquisitionBudget)
	}
	if gotIP != wantIP {
		t.Errorf("static-IP driver-opt was ignored: requested %s, got %s", wantIP, gotIP)
	}

	// Docker's view cannot tell a reserved address from one free by luck, which passed three runs before failing twice (#425).
	assertServerLeasedTo(t, wantIP, harness.StaticTestMAC)

	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, wantIP) {
		t.Errorf("eth0 inside container does not show requested IP %q\nactual:\n%s", wantIP, out)
	}
}

// assertServerLeasedTo requires a DHCPACK in dnsmasq's log handing ip to mac.
func assertServerLeasedTo(t *testing.T, ip, mac string) {
	t.Helper()

	data, err := os.ReadFile(fixture.DnsmasqLog())
	if err != nil {
		t.Fatalf("read dnsmasq log %s: %v — cannot confirm the server leased %s to %s, "+
			"and an unreadable log is not evidence of anything",
			fixture.DnsmasqLog(), err, ip, mac)
	}

	ok, acks := harness.ACKedTo(data, ip, mac)
	if ok {
		return
	}
	if len(acks) == 0 {
		t.Errorf("dnsmasq never ACKed %s at all, yet Docker reports the container holds it", ip)
		return
	}
	t.Errorf("dnsmasq ACKed %s but never to the reserved MAC %s — the --dhcp-host "+
		"reservation is not in effect and this test is back to competing with the "+
		"dynamic pool.\nACKs seen:\n\t%s", ip, mac, strings.Join(acks, "\n\t"))
}
