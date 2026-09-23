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
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// A removed endpoint holds its address for 60 s. A second container claims it under the server's lease identity while
// the server is down, and the plugin is torn down mid-exchange; the retry must get the same address, leased by the
// server to its new MAC. The plugin's counters are not asserted, since their process was torn down (#1047).

// TestIPAMStranded_APluginThatEndsMidRestartGivesTheAddressBack checks that a plugin ended mid-exchange hands the claimed address back (#1047).
func TestIPAMStranded_APluginThatEndsMidRestartGivesTheAddressBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	const netName = "dh-itest-ipam-stranded"
	const firstCtr = "dh-itest-ipam-stranded-a"
	const retryCtr = "dh-itest-ipam-stranded-b"
	// Long enough that the exchange is still waiting on the server, well inside the 60 s window.
	const teardownAfter = 6 * time.Second

	ef := harness.NewEphemeralFixture(t)
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// Registered before the teardown so every later test still finds the plugin enabled.
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer bgCancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil &&
			!strings.Contains(err.Error(), "already enabled") {
			t.Logf("WARN: cleanup PluginEnable: %v", err)
		}
		_ = harness.WaitPluginEnabled(bg, cli, true, 60*time.Second)
	})

	harness.CreateNetworkIPAM(t, ctx, netName, "macvlan", "",
		nil, map[string]string{"parent": harness.EphemeralHostVeth})

	firstID, addr, firstMAC := harness.RunContainer(t, ctx, netName, firstCtr)
	if addr == "" {
		t.Fatal("the first container got no address")
	}
	t.Logf("the address to keep: %s (first hardware address %s)", addr, firstMAC)

	if err := cli.ContainerRemove(ctx, firstID, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove(%s): %v", firstCtr, err)
	}

	windowOpened := time.Now()

	ef.Stop()
	t.Log("the DHCP server is down; the next address request will claim the address and then wait")

	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: retryCtr},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
		nil, retryCtr)
	if err != nil {
		t.Fatalf("ContainerCreate(%s): %v", retryCtr, err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_ = cli.ContainerStop(bg, create.ID, container.StopOptions{})
		_ = cli.ContainerRemove(bg, create.ID, container.RemoveOptions{Force: true})
	})

	// No t.Fatalf off the test goroutine: it would report against whichever test is running.
	startErr := make(chan error, 1)
	go func() {
		startErr <- cli.ContainerStart(context.Background(), create.ID, container.StartOptions{})
	}()

	time.Sleep(teardownAfter)

	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 30*time.Second); err != nil {
		t.Fatalf("the plugin did not reach disabled state: %v", err)
	}
	select {
	case err := <-startErr:
		t.Logf("the interrupted start returned: %v", err)
	case <-ctx.Done():
		t.Fatal("the interrupted container start never returned")
	}

	ef.StartAgain()
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil &&
		!strings.Contains(err.Error(), "already enabled") {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 60*time.Second); err != nil {
		t.Fatalf("the plugin did not come back: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 60*time.Second)

	// After the window closes the retry is a fresh allocation the server may answer with the same address anyway.
	if elapsed := time.Since(windowOpened); elapsed > 45*time.Second {
		t.Fatalf("the plugin took %s to come back, which leaves no room inside the 60 second "+
			"window this test measures; nothing was proved either way", elapsed)
	}

	if err := cli.ContainerStart(ctx, create.ID, container.StartOptions{}); err != nil {
		t.Fatalf("the container could not be started again after the plugin came back: %v.\n"+
			"A record left behind with no endpoint on it still answers for its address and for "+
			"the hardware address it was claimed under, so this is one of the two shapes the "+
			"bug takes.", err)
	}

	again, retryMAC := awaitEndpoint(t, ctx, cli, create.ID, netName)
	t.Logf("the retry came up on %s (hardware address %s)", again, retryMAC)
	if again != addr {
		t.Fatalf("the retry got %s, want the address the removed container left behind, %s.\n"+
			"Inside the window a restarting container keeps its address; a record left behind "+
			"by the plugin process that ended is not one the retry can claim from, so it takes "+
			"a second lease and the first is never handed back.", again, addr)
	}

	// The server's answer is evidence the plugin cannot fake.
	if got := ef.LastACKAddress(retryMAC); got != addr {
		t.Errorf("the DHCP server last acknowledged %q for %s, want %s. The address in Docker's "+
			"answer and the one the server leased must be the same address.", got, retryMAC, addr)
	}
}

// awaitEndpoint returns the endpoint's address and MAC once set; two separate calls fill the endpoint, so an early inspect reads empty.
func awaitEndpoint(t *testing.T, ctx context.Context, cli *docker.Client, id, netName string) (addr, mac string) {
	t.Helper()
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		if ep, ok := ins.NetworkSettings.Networks[netName]; ok && ep.IPAddress != "" {
			return ep.IPAddress, ep.MacAddress
		}
		select {
		case <-ctx.Done():
			t.Fatal("interrupted while waiting for the container's address")
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("the container had no address on %s within %s", netName, harness.IPAcquisitionBudget)
	return "", ""
}
