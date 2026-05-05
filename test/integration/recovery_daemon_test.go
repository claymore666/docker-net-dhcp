//go:build integration

package integration

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// TestRecovery_DaemonRestart_PreservesContainer is the integration
// counterpart to Phase D step 9 of the manual smoke test: bounce the
// whole docker daemon (systemctl restart docker) while a plugin-managed
// container is attached, and verify that
//
//   - the daemon comes back up (no hang on plugin re-enable — the
//     historical upstream failure mode this fork modernized away from)
//   - the container is still running (RestartPolicy=always so containerd
//     restarts it once dockerd is back)
//   - recoverEndpoints rebuilt our endpoint's dhcpManager
//     (Plugin.Health.recovered_ok ≥ 1)
//   - the IP and MAC are preserved across the restart
//
// **Do not parallelize.** systemctl restart docker drops every docker
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
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// We can't use harness.RunContainer because it doesn't take a
	// RestartPolicy. Inlining keeps the harness API stable.
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    harness.TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: ctrName,
		},
		&container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyAlways},
		},
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

	t.Log("systemctl restart docker — runner host's docker daemon is going down briefly")
	out, err := exec.CommandContext(ctx, "systemctl", "restart", "docker").CombinedOutput()
	if err != nil {
		t.Fatalf("systemctl restart docker: %v\n%s", err, out)
	}

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
	// respawned by docker. Poll until the new socket answers AND
	// recovery has run for at least one endpoint.
	deadline := time.Now().Add(30 * time.Second)
	var healthAfter *harness.HealthResponse
	for time.Now().Before(deadline) {
		h, err := harness.PluginHealth(ctx, cli2)
		if err == nil && h.RecoveredOK >= 1 {
			healthAfter = h
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if healthAfter == nil {
		t.Fatalf("Plugin.Health.recovered_ok did not reach 1 within 30s after daemon restart")
	}
	t.Logf("after restart: recovered_ok=%d recovery_failed=%d", healthAfter.RecoveredOK, healthAfter.RecoveryFailed)
	if healthAfter.RecoveryFailed != 0 {
		t.Errorf("recovery_failed=%d (recovery saw at least one endpoint it could not rebuild)", healthAfter.RecoveryFailed)
	}

	ins, err := cli2.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect after restart: %v", err)
	}
	var ipAfter, macAfter string
	for _, ep := range ins.NetworkSettings.Networks {
		if ep.IPAddress != "" {
			ipAfter = ep.IPAddress
			macAfter = ep.MacAddress
		}
	}
	t.Logf("after restart:  ip=%s mac=%s", ipAfter, macAfter)
	if ipAfter != ipBefore {
		t.Errorf("IP changed across daemon restart: before=%s after=%s", ipBefore, ipAfter)
	}
	if macAfter != macBefore {
		t.Errorf("MAC changed across daemon restart: before=%s after=%s", macBefore, macAfter)
	}
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
