// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// Whether recovery or the tombstone path preserves the address depends on whether dockerd's graceful shutdown ran
// Leave first, so the test asserts that one of the two ran (#386); both branches of RestartDockerDaemon shut down
// gracefully, and the abrupt case is #480. The test stops every container on the runner for about 5 to 15s.
// Do not parallelize: restarting the daemon drops every docker connection on the host.

// TestRecovery_DaemonRestart_PreservesContainer checks that a daemon restart brings the container back with its IP and MAC (#386).
func TestRecovery_DaemonRestart_PreservesContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	netName := "dh-itest-daemon-restart-net"
	ctrName := "dh-itest-daemon-restart-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// Closed before the restart, which respawns the plugin and resets every counter, so the post-restart reads are
	// absolute (#405).
	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	// RunContainer takes no RestartPolicy; HostConfig still supplies the init PID 1 that spares docker stop's 10s grace (#367).
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
		if err == nil {
			defer bgCli.Close()
			// So the cleanup container does not restart between Stop and Remove.
			_, _ = bgCli.ContainerUpdate(bg, id, container.UpdateConfig{
				RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
			})
			_ = bgCli.ContainerStop(bg, id, container.StopOptions{})
			_ = bgCli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
		}
	})
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	ipBefore, macBefore := waitForEndpoint(t, ctx, cli, id, harness.IPAcquisitionBudget)
	t.Logf("before restart: ip=%s mac=%s", ipBefore, macBefore)

	// The IP appears when CreateEndpoint's one-shot completes, before Join starts the persistent client; a daemon kill in
	// that window can bring the endpoint back on a different IP on slower hardware. No DHCPRELEASE is sent without
	// release_lease since #800 (#962), and whether the wait is still needed has not been re-measured.
	waitLeaseObtained(t, bindW, 30*time.Second)
	// The daemon restart below ends the process this window measured.
	bindW.End()

	// The plugin ID does not change, so the mark survives the restart; dumped only on failure.
	logMark := harness.MarkPluginLog(t, ctx)
	harness.DumpPluginLogOnFailure(t, ctx, logMark, "the daemon was restarted")

	harness.RestartDockerDaemon(t, ctx)

	// The pre-restart cli's connection is dead.
	_ = cli.Close()
	cli2, err := waitDaemonReady(ctx, 60*time.Second)
	if err != nil {
		t.Fatalf("daemon did not return: %v", err)
	}
	defer cli2.Close()

	// dockerd briefly reports 'restarting' while it reattaches to the running container.
	if err := waitContainerRunning(ctx, cli2, id, 30*time.Second); err != nil {
		t.Fatalf("container not running after daemon restart: %v", err)
	}

	// The new socket answering marks the end of the synchronous recovery walk, not of any rebuild: the walk spawns
	// each rebuild and recovered_ok moves only after it returns (pkg/plugin/plugin.go:recoverOneEndpoint, #376).
	healthAfter := harness.WaitPluginHealth(t, ctx, cli2, 30*time.Second)

	// No counter window: the restart ends the plugin instance, so reads either side cannot be tied to one instance (#405).
	const preserved = "one of the two paths that preserve the address to have run " +
		"(recovered_ok >= 1 or tombstones_consumed >= 1)"
	// The switch below is the assertion, so the wait's verdict is not read twice.
	waited, _ := harness.AwaitRecoveryRebuildOn(t, ctx, cli2, preserved,
		func(h *harness.HealthResponse) bool { return h.RecoveredOK >= 1 || h.TombstonesConsumed >= 1 })
	if waited != nil {
		healthAfter = waited
	}
	t.Logf("after restart: recovered_ok=%d tombstones_consumed=%d recovery_failed=%d recovery_deferred=%d recovery_aborted_container_gone=%d",
		healthAfter.RecoveredOK, healthAfter.TombstonesConsumed, healthAfter.RecoveryFailed,
		healthAfter.RecoveryDeferred, healthAfter.RecoveryAbortedContainerGone)

	// Before tombstones_consumed, an address surviving by neither path read as success, the blind spot #383 hid in; the
	// counters are the respawned instance's own (#386).
	switch {
	case healthAfter.RecoveredOK >= 1 && healthAfter.TombstonesConsumed >= 1:
		// Possible with more than one endpoint in play.
		t.Logf("address preserved by BOTH paths (recovered_ok=%d, tombstones_consumed=%d)",
			healthAfter.RecoveredOK, healthAfter.TombstonesConsumed)
	case healthAfter.RecoveredOK >= 1:
		t.Log("address preserved by recovery re-adopting the live endpoint")
	case healthAfter.TombstonesConsumed >= 1:
		t.Log("address preserved by CreateEndpoint replaying the tombstone")
	default:
		t.Errorf("neither path fired after the daemon restart, and neither had within the budget "+
			"the plugin's own timeouts allow. Either the address did not actually survive (the "+
			"assertions below will say), or it survived by a mechanism this test does not model "+
			"— and an unmodelled mechanism is not something to pass on (#386).\n  %s",
			harness.RecoveryRebuildFailure(preserved, healthAfter))
	}
	if waited == nil {
		t.Errorf("the plugin answered once after the restart and then not again for the whole " +
			"wait, so the counters judged above are the first reachable read and not the last. " +
			"A plugin that stops answering during recovery is a fault in its own right, and it " +
			"is not the one the arms above name")
	}

	// recovery_failed is sound only since both benign events were split out: #383's NetworkList timeout during the
	// plugin respawn, now recovery_deferred, which reached 1 on every run, and #376's exited container, now
	// recovery_aborted_container_gone. This container is running, so zero is the only correct value.
	if healthAfter.RecoveryFailed != 0 {
		t.Errorf("recovery_failed=%d after daemon restart: a running container was left without a renewal client (#376, #383)",
			healthAfter.RecoveryFailed)
	}

	// On the tombstone path the endpoint can briefly show no IP after State.Running flips.
	ipAfter, macAfter := waitForEndpoint(t, ctx, cli2, id, harness.IPAcquisitionBudget)
	t.Logf("after restart:  ip=%s mac=%s", ipAfter, macAfter)
	if ipAfter != ipBefore {
		t.Errorf("IP changed across daemon restart: before=%s after=%s", ipBefore, ipAfter)
	}
	if macAfter != macBefore {
		t.Errorf("MAC changed across daemon restart: before=%s after=%s", macBefore, macAfter)
	}
}

// A window, not a bare baseline, so the wait fails if the plugin restarts underneath it (#405).

// waitLeaseObtained polls Plugin.Health until leases_obtained moves past the window's baseline.
func waitLeaseObtained(t *testing.T, w *harness.CounterWindow, budget time.Duration) {
	t.Helper()
	baseline := w.Before().LeasesObtained
	last, ok := w.Await(budget, func(now, before *harness.HealthResponse) bool {
		return now.LeasesObtained > before.LeasesObtained
	})
	if !ok {
		t.Fatalf("persistent DHCP client did not bind within %v (leases_obtained stuck at %d)", budget, last.LeasesObtained)
	}
	t.Logf("persistent DHCP client bound (leases_obtained %d -> %d)", baseline, last.LeasesObtained)
}

// waitForEndpoint mirrors RunContainer's polling loop for an already-started container.
func waitForEndpoint(t *testing.T, ctx context.Context, cli *docker.Client, id string, budget time.Duration) (ipv4, mac string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				return ep.IPAddress, ep.MacAddress
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("container did not get an IP within %v", budget)
	return
}

// waitDaemonReady polls Ping on a fresh client until the daemon responds, which takes about 5 to 15s after a restart.
func waitDaemonReady(ctx context.Context, budget time.Duration) (*docker.Client, error) {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, perr := cli.Ping(pingCtx)
			cancel()
			if perr == nil {
				return cli, nil
			}
			_ = cli.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, context.DeadlineExceeded
}

// waitContainerRunning polls ContainerInspect until State.Running is true.
func waitContainerRunning(ctx context.Context, cli *docker.Client, id string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var lastState string
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err == nil && ins.State != nil {
			if ins.State.Running {
				return nil
			}
			lastState = ins.State.Status
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	return &timeoutError{op: "container running", state: lastState}
}

type timeoutError struct{ op, state string }

func (e *timeoutError) Error() string {
	if e.state == "" {
		return e.op + " timed out"
	}
	return e.op + " timed out (last state: " + e.state + ")"
}
