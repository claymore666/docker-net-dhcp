// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// TestRecovery_DaemonRestart_PreservesContainer is the integration
// counterpart to Phase D step 9 of the manual smoke test: bounce the
// whole docker daemon (harness.RestartDockerDaemon — systemctl on
// systemd hosts, supervised-dockerd signal on containerized runners)
// while a plugin-managed container is attached, and verify that
//
//   - the daemon comes back up (no hang on plugin re-enable — the
//     historical upstream failure mode this fork modernized away from)
//   - the container is running again (RestartPolicy=always so docker
//     brings it back once the daemon is up)
//   - the IP and MAC are preserved across the restart
//
// Deliberately NOT asserted: Plugin.Health.recovered_ok ≥ 1 on its
// own. Whether recovery or the tombstone path runs depends on whether
// dockerd's graceful shutdown ran Leave on the container's endpoint
// before going down. If it did, the post-restart container goes
// through CreateEndpoint+tombstone (recovered_ok stays 0,
// tombstones_consumed > 0); if it didn't, recoverEndpoints rebuilds the
// manager (recovered_ok > 0). Both yield the same user-visible
// invariant — same IP and MAC — so that is what is asserted.
//
// What IS asserted is that one of the two ran (#386). "The address
// survived by neither path" used to be indistinguishable from success,
// because recovered_ok=0 was the expected reading for the tombstone
// case and nothing observed the tombstone case positively.
//
// Note which branch of RestartDockerDaemon CI takes — always the
// containerized one, see harness/daemon.go. Both branches shut the
// daemon down gracefully, so both are expected to land on the tombstone
// path here; the abrupt-death case that would force recovery is #480.
//
// **Do not parallelize.** Restarting the daemon drops every docker
// connection on the host, including those of any other test running
// concurrently. Per the rule documented in test/integration/README.md
// the suite is serial; this test relies on that.
//
// **Side effects on the runner host.** This test stops every container
// on the runner briefly (whatever `--restart=always` they have decides
// whether they come back). Anything else running on the same docker
// daemon will see ~5–15s of unavailability. The runner is configured
// for this; on a shared dev box, run with care.
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

	// Baseline before the container exists: leases_obtained is
	// cumulative across the (serial) suite, so the wait below is
	// relative to this snapshot.
	//
	// This window covers only the pre-restart bind and is closed before
	// the daemon goes down. It deliberately does NOT span the restart:
	// docker respawns the plugin with the daemon, so every counter is
	// reset, which is exactly why the post-restart assertions further
	// down are absolute rather than deltas (#405).
	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	// We can't use harness.RunContainer because it doesn't take a
	// RestartPolicy. Inlining keeps the harness API stable — but the
	// HostConfig still comes from the harness so this site keeps the
	// init PID 1 that spares every teardown docker stop's 10s grace
	// (#367).
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
		// Use a fresh client because the one captured above may have
		// been Close()'d by the t.Cleanup chain ordering.
		bgCli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
		if err == nil {
			defer bgCli.Close()
			// Override RestartPolicy so the cleanup container
			// doesn't auto-restart between Stop and Remove.
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

	// The IP above appears as soon as CreateEndpoint's one-shot DHCP
	// completes — *before* Join has started the persistent client. If
	// the daemon goes down inside that window the post-restart endpoint
	// can come back with a *different* IP, failing the IP-stability
	// assertion below for reasons that have nothing to do with restart
	// recovery. Fast hosts never see this window; slower runner-class
	// hardware does. Wait for the persistent client's first "bound"
	// event (leases_obtained) before pulling the daemon down.
	//
	// The mechanism written here until #800 was that no client existed
	// to RELEASE the lease on shutdown. That is now false outright:
	// nothing this plugin runs sends a DHCPRELEASE, on any path. What
	// survives is the window itself — an endpoint whose Join has not
	// finished is not the steady state this test is about — so the wait
	// stays. Whether it is still LOAD-BEARING has not been re-measured
	// since the release paths went; deleting it needs that measurement,
	// not this comment.
	waitLeaseObtained(t, bindW, 30*time.Second)
	// Done with this window while the plugin it measured is still the
	// one running; the daemon restart below ends that process.
	bindW.End()

	harness.RestartDockerDaemon(t, ctx)

	// The pre-restart cli's TCP connection is dead. Build a new one.
	_ = cli.Close()
	cli2, err := waitDaemonReady(ctx, 60*time.Second)
	if err != nil {
		t.Fatalf("daemon did not return: %v", err)
	}
	defer cli2.Close()

	// containerd-shim keeps the container running across the daemon
	// restart, but ContainerInspect briefly returns 'restarting' as
	// dockerd reattaches. Poll until State.Running.
	if err := waitContainerRunning(ctx, cli2, id, 30*time.Second); err != nil {
		t.Fatalf("container not running after daemon restart: %v", err)
	}

	// Plugin.Health socket is replaced when the plugin process is
	// respawned by docker. Poll until the new socket answers — that
	// signals plugin enable + recovery have completed (recovery is
	// synchronous inside NewPlugin before the socket starts
	// listening, see pkg/plugin/plugin.go).
	healthAfter := harness.WaitPluginHealth(t, ctx, cli2, 30*time.Second)
	t.Logf("after restart: recovered_ok=%d tombstones_consumed=%d recovery_failed=%d recovery_deferred=%d recovery_aborted_container_gone=%d",
		healthAfter.RecoveredOK, healthAfter.TombstonesConsumed, healthAfter.RecoveryFailed,
		healthAfter.RecoveryDeferred, healthAfter.RecoveryAbortedContainerGone)

	// Which path preserved the address (#386).
	//
	// Either is legitimate and which one runs depends on whether
	// dockerd's graceful shutdown drove Leave before going down, so
	// neither can be demanded on its own. What CAN be demanded is that
	// one of them ran: the IP/MAC assertions below pass if the address
	// survived, and until tombstones_consumed existed there was no way
	// to distinguish "the tombstone path preserved it" from "it survived
	// for a reason this test does not model". That third state read as
	// success, which is the same shape of blind spot #383 hid in — its
	// IP/MAC assertions passed throughout while recovery was failing on
	// every single run.
	//
	// The counters are absolute rather than a delta on purpose: the
	// plugin process is respawned by the daemon restart, so these are
	// the new instance's own numbers and already scoped to this event.
	switch {
	case healthAfter.RecoveredOK >= 1 && healthAfter.TombstonesConsumed >= 1:
		// Possible with more than one endpoint in play; not an error,
		// but say so rather than silently picking one.
		t.Logf("address preserved by BOTH paths (recovered_ok=%d, tombstones_consumed=%d)",
			healthAfter.RecoveredOK, healthAfter.TombstonesConsumed)
	case healthAfter.RecoveredOK >= 1:
		t.Log("address preserved by recovery re-adopting the live endpoint")
	case healthAfter.TombstonesConsumed >= 1:
		t.Log("address preserved by CreateEndpoint replaying the tombstone")
	default:
		t.Errorf("neither path fired after the daemon restart: recovered_ok=0 and "+
			"tombstones_consumed=0, yet the address assertions below are what decide "+
			"this test. Either the address did not actually survive (the assertions "+
			"below will say), or it survived by a mechanism this test does not model "+
			"— and an unmodelled mechanism is not something to pass on (#386). "+
			"recovery_deferred=%d recovery_aborted_container_gone=%d",
			healthAfter.RecoveryDeferred, healthAfter.RecoveryAbortedContainerGone)
	}

	//
	// recovery_failed IS asserted, and that assertion only became sound
	// once BOTH benign events were split out of it. Each was independently
	// enough to make a strict check here flaky by construction:
	//
	//   #383 — recovery's first NetworkList hit the Docker client's 2s
	//   timeout because docker respawns the plugin during its own
	//   startup, and recovery was then abandoned for every network. This
	//   counter reached 1 on every single run of this test. It hid for
	//   so long because the IP/MAC assertions below still passed,
	//   carried by the tombstone path. Now counted as recovery_deferred.
	//
	//   #376 — a container that had merely exited before recovery
	//   reached it. Now counted as recovery_aborted_container_gone.
	//
	// What is left means exactly one thing: a RUNNING container whose
	// renewal client could not be rebuilt. This container is running
	// (asserted above), so the only correct value is zero.
	//
	// Non-zero recovery_deferred here is expected and fine — it means the
	// daemon was not ready and the retry did its job.
	if healthAfter.RecoveryFailed != 0 {
		t.Errorf("recovery_failed=%d after daemon restart: a running container was left without a renewal client (#376, #383)",
			healthAfter.RecoveryFailed)
	}

	// In the tombstone-path case (graceful shutdown ran Leave) the
	// container goes through a fresh CreateEndpoint+dhcpcd on the
	// way back up; the endpoint can briefly show no IP after
	// State.Running flips. Poll on the endpoint, not just the state.
	ipAfter, macAfter := waitForEndpoint(t, ctx, cli2, id, harness.IPAcquisitionBudget)
	t.Logf("after restart:  ip=%s mac=%s", ipAfter, macAfter)
	if ipAfter != ipBefore {
		t.Errorf("IP changed across daemon restart: before=%s after=%s", ipBefore, ipAfter)
	}
	if macAfter != macBefore {
		t.Errorf("MAC changed across daemon restart: before=%s after=%s", macBefore, macAfter)
	}
}

// waitLeaseObtained polls Plugin.Health until leases_obtained moves
// past the window's opening baseline, i.e. the endpoint's persistent
// DHCP client has fired its first dhcpcd "bound" event and lease
// release-on-shutdown is armed.
//
// Takes the window rather than a bare baseline int so the wait fails
// loudly if the plugin restarts underneath it. Watching a counter climb
// past a number the counter no longer remembers is the #405 bug in its
// purest form.
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

// waitForEndpoint mirrors RunContainer's polling loop but works on
// an already-started container.
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

// waitDaemonReady polls Ping on a fresh client until the daemon
// responds. The daemon takes ~5–15s to come up after systemctl
// restart docker; the budget here is generous to absorb a slow
// disk warmup or a plugin that takes time to enable.
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

// waitContainerRunning polls ContainerInspect until State.Running
// reports true. Used after daemon restart, where containerd-shim
// has the container alive but dockerd is briefly seeing it in the
// 'restarting' state as it reattaches.
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
