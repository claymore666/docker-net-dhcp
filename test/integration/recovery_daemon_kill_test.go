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

// TestRecovery_DaemonKilled_LeaseIsHeldUntilItExpires covers the
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
// THE ASSERTION INVERTED IN #800, and this is the point of the test.
//
// Until v1.9.0 that shutdown released the pre-death lease, and this
// test asserted the release arrived, on the reasoning that "an abrupt
// daemon death must not burn a pool address until it expires on its
// own". That reasoning is now rejected at the product level: a machine
// that is powered off abruptly does not hand its address back either,
// and the address staying leased until it expires is what a lease IS.
// The plugin sends no DHCPRELEASE on any path, so what is asserted here
// is the absence of one.
//
// The old assertion's premise was not wrong about the cost — the
// address really is held for the remainder of the lease, and the
// container really does come back on a different one. It was wrong
// about the remedy. The address is only stranded because Docker
// rebuilds the endpoint with a new MAC; give the endpoint a stable MAC
// (#218) and the returned container asks for the address it already
// holds and is given it, which is exactly how a rebooted machine keeps
// its address. Releasing papered over that with a protocol message the
// LAN does not require.
//
// Because the subject is now an absence, ORDER MATTERS: the DHCPACK for
// the returned container is asserted FIRST and is the positive control.
// It proves, in this same run and from this same file, that the log is
// readable, current, and being matched — the three things a bare "zero
// releases" cannot distinguish itself from. The matcher's ability to
// recognise a DHCPRELEASE at all is driven separately, against a canned
// log, in harness.TestCountBridgeLogLines_SeesADHCPRELEASE.
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
	// has started the client that owns the lease. Killing the daemon
	// inside that window would mean no renewal client was ever running,
	// so "nothing released" would be true of a shape the assertion below
	// is not about — it would measure the harness rather than the
	// plugin. This is the same reason the wait was here before #800
	// inverted the assertion; an absence needs its subject to have
	// existed even more than a presence does.
	waitLeaseObtained(t, bindW, 30*time.Second)
	bindW.End()
	t.Logf("before the kill: ip=%s mac=%s", ipBefore, macBefore)

	// Counted as a delta: the fixture's log accumulates every test's
	// traffic, so an absolute count of releases says nothing about this
	// endpoint. Keyed on the MAC as well as the verb, so a neighbouring
	// test's release cannot be attributed here — which now matters in
	// the opposite direction, since a stray match would FAIL this test
	// rather than satisfy it.
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

	// (1) POSITIVE CONTROL, and the assertion that the container really
	// came back on the wire. The address in `docker inspect` is the
	// plugin's word for it; this is the server's — it ACKed that address
	// to that MAC. Without it, a container that came back holding a
	// stale address nothing leased would read exactly like success.
	//
	// It runs before (2) on purpose. (2) is an absence, and an absence
	// read from a log that is missing, stale, or no longer matched reads
	// as a pass. This line fails in all three of those cases.
	if !waitBridgeLogLines(t, 1, 30*time.Second, "DHCPACK", macAfter, ipAfter) {
		t.Fatalf("no DHCPACK from the server for %s -> %s: the container came back with an "+
			"address the DHCP server never granted it. Nothing below this line can be "+
			"trusted — the release check that follows would read 0 whether or not the "+
			"plugin released", macAfter, ipAfter)
	}

	// (2) The lease the killed daemon's endpoint held must NOT have been
	// handed back (#800). The plugin's own view cannot answer this — a
	// counter would only say what it believed — so it is read off the
	// server that is still holding the address.
	//
	// No extra wait is needed and none is added: the release used to
	// arrive about a second after the kill, driven by the SIGTERM the
	// replacement daemon sends the plugin, and by this point the window
	// has been open for the whole daemon-restart-and-rebind cycle —
	// tens of seconds. A sleep here would be the weakening this repo
	// treats as a bug report, not a fix.
	if got := fixture.CountBridgeLogLines("DHCPRELEASE", macBefore) - releasesBefore; got != 0 {
		t.Errorf("%d DHCPRELEASE(s) for %s after the daemon was killed, want 0: since #800 "+
			"nothing this plugin runs releases a lease on any path. The address %s must stay "+
			"leased until it expires, the same as a machine that was powered off abruptly",
			got, macBefore, ipBefore)
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
