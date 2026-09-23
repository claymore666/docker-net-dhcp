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
)

// Recovery cannot re-adopt an endpoint after a SIGKILLed daemon (six runs, #480): containerd dies with dockerd, the
// relaunched daemon removes each sandbox as stale and a restart policy builds a new container and endpoint; with
// --live-restore the plugin survives too, and recovery runs only at plugin startup. About a second after the kill the
// plugin gets a clean SIGTERM from the replacement daemon. Since v1.9.0 no DHCPRELEASE is sent without release_lease
// (#800, #962); the DHCPACK for the returned container is asserted first as the positive control, and the address
// changes with the new MAC in 6 of 6 runs until #218, so it is logged, not asserted.
// Do not parallelize: SIGKILL takes every container on the host down.

// TestRecovery_DaemonKilled_LeaseIsHeldUntilItExpires checks that a SIGKILLed daemon's endpoint releases nothing and its container is ACKed again on return (#480, #800).
func TestRecovery_DaemonKilled_LeaseIsHeldUntilItExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	netName := "dh-itest-daemon-kill-net"
	ctrName := "dh-itest-daemon-kill-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// The bridge fixture's dnsmasq serves this segment, and the assertions read its log.
	harness.CreateNetwork(t, ctx, netName, "bridge", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	// RestartPolicy=always brings the container back after the kill; HostConfig() still supplies the init PID 1.
	hostCfg := harness.HostConfig()
	hostCfg.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyAlways}
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: ctrName,
		},
		hostCfg,
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
		},
		nil,
		ctrName,
	)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	id := create.ID
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		// The client above may be closed by the cleanup chain.
		bgCli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
		if err != nil {
			return
		}
		defer bgCli.Close()
		_, _ = bgCli.ContainerUpdate(bg, id, container.UpdateConfig{
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		})
		_ = bgCli.ContainerStop(bg, id, container.StopOptions{})
		_ = bgCli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	ipBefore, macBefore := waitForEndpoint(t, ctx, cli, id, harness.IPAcquisitionBudget)

	// The IP appears when CreateEndpoint's one-shot completes, before Join starts the client that owns the lease; an
	// absence needs its subject to have existed (#800).
	waitLeaseObtained(t, bindW, 30*time.Second)
	bindW.End()
	t.Logf("before the kill: ip=%s mac=%s", ipBefore, macBefore)

	// A delta keyed on the MAC, since the fixture's log accumulates every test's traffic.
	releasesBefore := fixture.CountBridgeLogLines("DHCPRELEASE", macBefore)

	harness.KillDockerDaemon(t, ctx)
	killedAt := time.Now()

	_ = cli.Close()
	cli2, err := waitDaemonReady(ctx, 120*time.Second)
	if err != nil {
		t.Fatalf("daemon did not come back after being killed: %v", err)
	}
	defer cli2.Close()
	t.Logf("daemon answering again %v after the kill", time.Since(killedAt).Round(time.Millisecond))

	if err := waitContainerRunning(ctx, cli2, id, 90*time.Second); err != nil {
		t.Fatalf("container did not come back after the daemon was killed: %v", err)
	}

	healthAfter := harness.WaitPluginHealth(t, ctx, cli2, 60*time.Second)
	t.Logf("after the kill: recovered_ok=%d recovery_failed=%d recovery_deferred=%d "+
		"recovery_already_managed=%d tombstones_consumed=%d",
		healthAfter.RecoveredOK, healthAfter.RecoveryFailed, healthAfter.RecoveryDeferred,
		healthAfter.RecoveryAlreadyManaged, healthAfter.TombstonesConsumed)

	// recovery_failed counts a running container whose renewal client could not be rebuilt, and this one is running.
	if healthAfter.RecoveryFailed != 0 {
		t.Errorf("recovery_failed=%d after the daemon was killed: a running container was left "+
			"without a renewal client (#376, #383)", healthAfter.RecoveryFailed)
	}

	ipAfter, macAfter := waitForEndpoint(t, ctx, cli2, id, harness.IPAcquisitionBudget)
	t.Logf("after the kill:  ip=%s mac=%s (address preserved=%v, MAC preserved=%v — "+
		"neither is asserted, see the header)", ipAfter, macAfter, ipAfter == ipBefore, macAfter == macBefore)

	// The server ACKed that address to that MAC, which also proves the log is present, current and matched before the
	// absence below is read (#480).
	if !waitBridgeLogLines(t, 1, 30*time.Second, "DHCPACK", macAfter, ipAfter) {
		t.Fatalf("no DHCPACK from the server for %s -> %s: the container came back with an "+
			"address the DHCP server never granted it. Nothing below this line can be "+
			"trusted — the release check that follows would read 0 whether or not the "+
			"plugin released", macAfter, ipAfter)
	}

	// Before #800 the release arrived about a second after the kill, and the window here spans the whole restart-and-rebind
	// cycle, so no extra wait is added (#800).
	if got := fixture.CountBridgeLogLines("DHCPRELEASE", macBefore) - releasesBefore; got != 0 {
		t.Errorf("%d DHCPRELEASE(s) for %s after the daemon was killed, want 0: since #800 "+
			"nothing releases a lease on a network that does not set release_lease, and this "+
			"one does not. The address %s must stay leased until it expires, the same as a "+
			"machine that was powered off abruptly",
			got, macBefore, ipBefore)
	}
}

// waitBridgeLogLines polls the bridge fixture's dnsmasq log until at least want lines match every substring or the budget runs out.
func waitBridgeLogLines(t *testing.T, want int, budget time.Duration, substrings ...string) bool {
	t.Helper()
	deadline := time.Now().Add(budget)
	last := 0
	for {
		last = fixture.CountBridgeLogLines(substrings...)
		if last >= want {
			return true
		}
		if time.Now().After(deadline) {
			t.Logf("waited %v for %d line(s) matching %v; saw %d",
				budget, want, strings.Join(substrings, "+"), last)
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}
