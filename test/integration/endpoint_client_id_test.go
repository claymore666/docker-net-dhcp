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

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// createWithClientID creates a container on netName with the per-endpoint client_id driver option; removal is the
// test's cleanup (#219).
func createWithClientID(t *testing.T, ctx context.Context, cli *docker.Client, netName, ctrName, clientID string) string {
	t.Helper()
	create, err := cli.ContainerCreate(ctx, docker.ContainerCreateOptions{
		Config:     &container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		HostConfig: harness.HostConfig(),
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {DriverOpts: map[string]string{"client_id": clientID}},
		}},
		Name: ctrName,
	})
	if err != nil {
		t.Fatalf("ContainerCreate %s: %v", ctrName, err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), create.ID, docker.ContainerRemoveOptions{Force: true})
	})
	return create.ID
}

func waitIPv4(t *testing.T, ctx context.Context, cli *docker.Client, id, netName string) string {
	t.Helper()
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id, docker.ContainerInspectOptions{})
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		if ep := ins.Container.NetworkSettings.Networks[netName]; ep != nil && ep.IPAddress.IsValid() {
			return harness.AddrString(ep.IPAddress)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("container %s got no IPv4 within %v", id[:12], harness.IPAcquisitionBudget)
	return ""
}

func startWithClientID(t *testing.T, ctx context.Context, cli *docker.Client, netName, ctrName, clientID string) (string, string) {
	t.Helper()
	id := createWithClientID(t, ctx, cli, netName, ctrName, clientID)
	if _, err := cli.ContainerStart(ctx, id, docker.ContainerStartOptions{}); err != nil {
		t.Fatalf("ContainerStart %s: %v", ctrName, err)
	}
	return id, waitIPv4(t, ctx, cli, id, netName)
}

// TestEndpointClientID_IpvlanKeepsItsAddressAndRefusesADuplicate checks the per-endpoint client_id on ipvlan: the
// server files the lease under it, the address survives docker restart and a recreate, a second value gets a second
// address, and a duplicate is refused at start (#219).
func TestEndpointClientID_IpvlanKeepsItsAddressAndRefusesADuplicate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const netName = "dh-itest-epcid"
	const idA, idB = "dh-itest-epcid-a", "dh-itest-epcid-b"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := harness.NewDockerClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	harness.CreateNetwork(t, ctx, netName, "ipvlan", nil)

	a, ipA := startWithClientID(t, ctx, cli, netName, "dh-itest-epcid-a1", idA)
	wantHex := "00:" + colonHex([]byte(idA))
	var leases []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(150 * time.Millisecond) {
		if leases, _ = os.ReadFile(fixture.LeaseFile()); strings.Contains(string(leases), wantHex) {
			break
		}
	}
	if !strings.Contains(string(leases), wantHex) {
		t.Fatalf("the server holds no lease under %s:\n%s", wantHex, leases)
	}

	if _, err := cli.ContainerRestart(ctx, a, docker.ContainerRestartOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}
	if got := waitIPv4(t, ctx, cli, a, netName); got != ipA {
		t.Errorf("after docker restart the container has %s, want %s", got, ipA)
	}

	if _, err := cli.ContainerRemove(ctx, a, docker.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	_, ipA2 := startWithClientID(t, ctx, cli, netName, "dh-itest-epcid-a2", idA)
	if ipA2 != ipA {
		t.Errorf("a recreated container with the same client_id has %s, want %s", ipA2, ipA)
	}

	_, ipB := startWithClientID(t, ctx, cli, netName, "dh-itest-epcid-b1", idB)
	if ipB == ipA {
		t.Errorf("two client_id values got the same address %s", ipB)
	}

	dup := createWithClientID(t, ctx, cli, netName, "dh-itest-epcid-b2", idB)
	_, err = cli.ContainerStart(ctx, dup, docker.ContainerStartOptions{})
	if err == nil || !strings.Contains(err.Error(), "already in use") || !strings.Contains(err.Error(), "dh-itest-epcid-b1") {
		t.Errorf("a second container with client_id %s was not refused naming the holder: %v", idB, err)
	}
}
