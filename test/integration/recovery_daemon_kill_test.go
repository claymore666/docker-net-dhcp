// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// TestRecovery_DaemonKilled_LeaseIsReleasedAndReacquired covers the
// daemon death nothing else in the suite reaches: SIGKILL, no shutdown
// sequence, no Leave on any endpoint. Every other way this suite takes
// the daemon or the plugin down is graceful.
//
// WHAT THIS DOES NOT ASSERT, and why that is the finding rather than a
// gap (#480).
//
// The issue this test closes asked for recovered_ok >= 1 after an
// abrupt death — recovery re-adopting an endpoint that outlived the
// daemon. That state does not exist. Measured, six runs, and it is
// Docker's behaviour rather than the runner's:
//
//   - containerd dies with dockerd, the orphaned shims cannot be
//     reattached, and the relaunched daemon removes each sandbox as
//     stale. A restart policy then builds a NEW container and a NEW
//     endpoint, so there is nothing attached for recovery to adopt.
//   - --live-restore does not open the path, it closes it from the
//     other side: the container survives, but so does the plugin
//     process, and recovery only ever runs at plugin startup.
//
// Both readings of the environment are therefore unreachable, and the
// deepest reason is that the plugin never dies abruptly at all. About a
// second after the daemon is killed, the plugin gets a clean SIGTERM
// from the daemon that replaces it, and runs its whole shutdown.
//
// That is what this test asserts instead, and it is worth more than the
// counter would have been: the shutdown really happens, at the DHCP
// server. Both halves are read off the server's log rather than off the
// plugin's opinion of itself.
//
//  1. the pre-death lease is RELEASED — an abrupt daemon death must not
//     burn a pool address until it expires on its own.
//  2. the returned container holds a lease the server actually granted,
//     not merely an address in `docker inspect`.
//
// Address stability is deliberately not asserted in either direction.
// It is not preserved today — new endpoint, new MAC, new address, 6 of
// 6 runs — which follows from Docker rebuilding the endpoint and is
// what #218 would change. Pinning the inequality would fail the day
// that lands; pinning equality would fail today. Both are logged.
//
// **Do not parallelize**, and note this is heavier than the graceful
// restart test: SIGKILL takes every container on the host down outright
// and only a restart policy brings one back.
func TestRecovery_DaemonKilled_LeaseIsReleasedAndReacquired(t *testing.T) {
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

	// Bridge rather than macvlan: the assertions read the DHCP server's
	// log, and the bridge fixture's dnsmasq is the one serving this
	// segment.
	harness.CreateNetwork(t, ctx, netName, "bridge", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	// RestartPolicy=always is what makes the container come back at all
	// after the daemon is killed, so it cannot come from RunContainer.
	// HostConfig() still supplies the init PID 1 every other site gets.
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
		// A fresh client: the one above may already be closed by the
		// cleanup chain, and the one it replaced died with the daemon.
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

	// Wait for the PERSISTENT client, not just the address. The IP
	// appears when CreateEndpoint's one-shot DHCP completes, before Join
	// has started the client that owns the lease — and it is that client
	// which releases it on shutdown. Killing the daemon inside that
	// window would leave nothing to release and make assertion (1)
	// measure the harness rather than the plugin.
	waitLeaseObtained(t, bindW, 30*time.Second)
	bindW.End()
	t.Logf("before the kill: ip=%s mac=%s", ipBefore, macBefore)

	// Counted as a delta: the fixture's log accumulates every test's
	// traffic, so an absolute count of releases says nothing about this
	// endpoint. Keyed on the MAC as well as the verb, so a neighbouring
	// test's release cannot satisfy it.
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

	// (1) The lease the killed daemon's endpoint held must be handed
	// back. The plugin's own view cannot answer this — a counter would
	// only say it believed it released one — so it is read off the
	// server that would otherwise still be holding the address.
	if !waitBridgeLogLines(t, releasesBefore+1, 60*time.Second, "DHCPRELEASE", macBefore) {
		t.Errorf("no DHCPRELEASE for %s (the pre-kill endpoint) within 60s of the daemon being killed: "+
			"the server still holds %s and will until the lease expires. An abrupt daemon death "+
			"must not burn a pool address (#480)", macBefore, ipBefore)
	}

	healthAfter := harness.WaitPluginHealth(t, ctx, cli2, 60*time.Second)
	t.Logf("after the kill: recovered_ok=%d recovery_failed=%d recovery_deferred=%d "+
		"recovery_already_managed=%d tombstones_consumed=%d",
		healthAfter.RecoveredOK, healthAfter.RecoveryFailed, healthAfter.RecoveryDeferred,
		healthAfter.RecoveryAlreadyManaged, healthAfter.TombstonesConsumed)

	// recovery_failed means exactly one thing: a RUNNING container whose
	// renewal client could not be rebuilt. This container is running —
	// asserted above — so zero is the only correct value, whichever path
	// brought it back.
	if healthAfter.RecoveryFailed != 0 {
		t.Errorf("recovery_failed=%d after the daemon was killed: a running container was left "+
			"without a renewal client (#376, #383)", healthAfter.RecoveryFailed)
	}

	ipAfter, macAfter := waitForEndpoint(t, ctx, cli2, id, harness.IPAcquisitionBudget)
	t.Logf("after the kill:  ip=%s mac=%s (address preserved=%v, MAC preserved=%v — "+
		"neither is asserted, see the header)", ipAfter, macAfter, ipAfter == ipBefore, macAfter == macBefore)

	// (2) The address in `docker inspect` is the plugin's word for it.
	// This is the server's: it ACKed that address to that MAC. Without
	// it, a container that came back holding a stale address nothing
	// leased would read exactly like success.
	if !waitBridgeLogLines(t, 1, 30*time.Second, "DHCPACK", macAfter, ipAfter) {
		t.Errorf("no DHCPACK from the server for %s -> %s: the container came back with an "+
			"address the DHCP server never granted it", macAfter, ipAfter)
	}
}

// waitBridgeLogLines polls the bridge fixture's dnsmasq log until at
// least want lines match every substring, or the budget runs out.
//
// Polling rather than a single read: the log is written by another
// process and the events being waited on (a release driven by the
// plugin's shutdown, an ACK for the replacement container) land after
// the docker-side state this test can observe has already settled.
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
