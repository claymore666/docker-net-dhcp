//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
)

// TestRecovery_PluginDisableEnable_PreservesEndpoint exercises the
// recoverEndpoints code path: forcibly recycle the plugin while a
// container is attached, then verify (a) Plugin.Health.recovered_ok
// advanced past zero, and (b) the container's IP and MAC are
// identical to what they were before the recycle.
//
// **Do not parallelize.** This test mutates daemon-global state by
// disabling/enabling the plugin. Other tests running concurrently
// would lose plugin RPC service mid-flight.
//
// Cleanup is defensive: the t.Cleanup re-enables the plugin even if
// any assertion failed mid-cycle, so a panic between disable and
// enable can't leave the runner host with the plugin stuck off
// (which would block every subsequent test and any smoke testing on
// the same host).
func TestRecovery_PluginDisableEnable_PreservesEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	netName := "dh-itest-recovery-net"
	ctrName := "dh-itest-recovery-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, ipBefore, macBefore := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("before recycle: ip=%s mac=%s", ipBefore, macBefore)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	// Belt-and-braces re-enable: registered immediately so any panic
	// or t.Fatal between here and the explicit enable still leaves
	// the plugin enabled. Idempotent — already-enabled is fine.
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
			if !strings.Contains(err.Error(), "already enabled") {
				t.Logf("WARN: cleanup PluginEnable: %v", err)
			}
		}
	})

	// We don't read recovered_ok before the recycle: PluginDisable
	// kills the plugin process and PluginEnable starts a fresh one,
	// so the counter we see after is from a brand-new instance with
	// initial value zero. The before-snapshot would be from a stale
	// process and isn't comparable. Asserting `after >= 1` is the
	// right invariant.

	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 15*time.Second); err != nil {
		t.Fatalf("plugin did not reach disabled state: %v", err)
	}
	t.Log("plugin disabled")

	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("plugin did not re-enable: %v", err)
	}
	t.Log("plugin re-enabled")

	// Plugin process is up; recoverEndpoints runs synchronously
	// inside NewPlugin so by the time the socket accepts requests
	// recovery is already complete. Poll briefly for socket
	// readiness — Plugin.Enabled flips slightly before the socket is
	// listening.
	var healthAfter *harness.HealthResponse
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		h, err := harness.PluginHealth(ctx, cli)
		if err == nil {
			healthAfter = h
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if healthAfter == nil {
		t.Fatalf("Plugin.Health (after) never became reachable")
	}
	t.Logf("recovered_ok after: %d", healthAfter.RecoveredOK)

	if healthAfter.RecoveredOK < 1 {
		t.Errorf("recovered_ok=%d (expected >=1; recovery did not pick up our endpoint after the recycle)",
			healthAfter.RecoveredOK)
	}
	if healthAfter.RecoveryFailed != 0 {
		t.Errorf("recovery_failed=%d (recovery saw at least one endpoint it could not rebuild)", healthAfter.RecoveryFailed)
	}

	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	var ipAfter, macAfter string
	for _, ep := range ins.NetworkSettings.Networks {
		if ep.IPAddress != "" {
			ipAfter = ep.IPAddress
			macAfter = ep.MacAddress
		}
	}
	if ipAfter != ipBefore {
		t.Errorf("IP changed across plugin recycle: before=%s after=%s", ipBefore, ipAfter)
	}
	if macAfter != macBefore {
		t.Errorf("MAC changed across plugin recycle: before=%s after=%s", macBefore, macAfter)
	}
}
