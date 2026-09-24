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
	docker "github.com/docker/docker/client"
)

// Do not parallelize: disabling the plugin takes RPC service from every other test.

// TestRecovery_PluginDisableEnable_PreservesEndpoint checks that a plugin recycle recovers an attached endpoint with its IP and MAC unchanged (#376).
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

	// Registered before the disable, so a failure between disable and enable still leaves the plugin enabled.
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer bgCancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
			if !strings.Contains(err.Error(), "already enabled") {
				t.Logf("WARN: cleanup PluginEnable: %v", err)
			}
		}
	})

	// PluginEnable starts a fresh process, so the counters below are absolute; ExpectRecycle fails if the plugin did not
	// restart (#405).
	w := harness.BeginCounterWindow(t, ctx, cli,
		"recovered_ok", "recovery_failed",
		"sandbox_key_entries", "sandbox_key_entry_failures", "sandbox_pid_fallbacks",
		"sandbox_key_absent", "sandbox_key_not_permitted", "sandbox_key_not_a_namespace",
		"sandbox_key_wrong_ns_type", "sandbox_key_unavailable").ExpectRecycle()

	// RFC 5227's probe runs in the CreateEndpoint one-shot (roleAcquire under ConflictWait), but a recovered endpoint
	// skips CreateEndpoint and the Join client probes asynchronously, racing teardown; one container, one v4 lease. Run
	// 34600486961 main-3 ran this test last and the floor read leases_obtained_v4=1 with acd_probes_sent=0 (#725).
	harness.AllowUnprobedLeases(1)

	// Marked here so the window is the recycle, dumped only on failure.
	logMark := harness.MarkPluginLog(t, ctx)
	harness.DumpPluginLogOnFailure(t, ctx, logMark, "the plugin was disabled")

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

	// Socket readiness is the start of the thing under test: the walk inside NewPlugin only spawns each rebuild,
	// and recovered_ok moves after Start returns (pkg/plugin/plugin.go:recoverOneEndpoint, #376). Plugin.Enabled flips
	// before the socket listens, so poll for the socket first, then wait for the rebuild.
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)

	// A timeout is the failure recovered_ok=0 used to be, with the route counters printed beside it (#376).
	const rebuilt = "recovery to rebuild this endpoint's renewal client (recovered_ok >= 1)"
	waited, ok := harness.AwaitRecoveryRebuildWindow(w, rebuilt,
		func(h *harness.HealthResponse) bool { return h.RecoveredOK >= 1 })
	if !ok {
		t.Errorf("%s", harness.RecoveryRebuildFailure(rebuilt, waited))
	}

	// End asserts the instance id changed, and is taken after the rebuild has written its counters.
	_, healthAfter := w.End()
	t.Logf("recovered_ok after: %d", healthAfter.RecoveredOK)
	if healthAfter.RecoveryFailed != 0 {
		t.Errorf("recovery_failed=%d (recovery saw at least one endpoint it could not rebuild)", healthAfter.RecoveryFailed)
	}
	// recovery_aborted_container_gone does not flip healthy, so a classifier calling a running container gone would hide
	// every real recovery failure; the container ran throughout (#376).
	if healthAfter.RecoveryAbortedContainerGone != 0 {
		t.Errorf("recovery_aborted_container_gone=%d: the container ran throughout the recycle, so recovery must not have classified it as gone (#376)",
			healthAfter.RecoveryAbortedContainerGone)
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

	// Join and recovery both reach dhcpManager.Start, which inspects the container, so NetworkSettings.SandboxKey is the
	// key source and nothing was added to the durable record (#725). The plugin's /var/run/docker is a bind-mount snapshot
	// taken at plugin start, so a sandbox that predates the plugin, as a recovered one does, is visible through its key on
	// every host; a younger one only where sandbox_netns_propagation is linked. Measured on the lane 2026-09-05, run
	// 33927195482: entries 1, failures 0, fallbacks 0 here, and 0/1/1 in every attach cell on that host (#725). On a
	// private host the PID route carries every attach, so the manifest keeps pidhost and CAP_SYS_PTRACE (#417).
	if healthAfter.SandboxKeyEntries == nil || healthAfter.SandboxPIDFallbacks == nil ||
		healthAfter.SandboxKeyEntryFailures == nil {
		t.Fatal("the recovered plugin publishes no sandbox route counters, so which route recovery " +
			"took cannot be judged — and reading their absence as zero is how a recovery that took " +
			"no route at all would pass this test")
	}
	keyEntries := *healthAfter.SandboxKeyEntries
	fallbacks := *healthAfter.SandboxPIDFallbacks
	keyFailures := *healthAfter.SandboxKeyEntryFailures
	t.Logf("CELL mode=recovery user=\"\": sandbox_key_entries %d, sandbox_key_entry_failures %d, sandbox_pid_fallbacks %d (absolute, fresh instance)",
		keyEntries, keyFailures, fallbacks)

	if keyEntries+fallbacks < 1 {
		t.Errorf("neither sandbox_key_entries nor sandbox_pid_fallbacks moved on the recovered "+
			"instance (%d and %d): recovery re-adopted the endpoint without entering its network "+
			"namespace at all, so the assertions below would be about an empty set", keyEntries, fallbacks)
	}
	if keyEntries < 1 {
		t.Errorf("sandbox_key_entries=%d on the recovered instance: the key route did NOT carry the "+
			"re-adoption. This cell is the positive half of the #725 measurement — the sandbox predates "+
			"this plugin process, so its netns mount is inside the bind snapshot and the key must "+
			"resolve. If this is zero the snapshot explanation is wrong and the whole finding in "+
			"SECURITY.md has to be re-derived, not patched", keyEntries)
	}
	if fallbacks != 0 {
		t.Errorf("sandbox_pid_fallbacks=%d on the recovered instance: recovery fell back to "+
			"/proc/<pid>/ns/net for a sandbox that predates this plugin process. Either the key route "+
			"regressed or the netns bind mount is no longer captured at plugin start; either way the "+
			"asymmetry this cell and sandbox_key_route_test.go measure together is gone", fallbacks)
	}
	if keyFailures != 0 {
		t.Errorf("sandbox_key_entry_failures=%d on the recovered instance: the key route was refused "+
			"for a sandbox it should be able to open, and a refusal here is the same regression as a "+
			"fallback", keyFailures)
	}

	// Recovery is the one path whose JoinRequest carries no key, so sandbox_key_absent shows here if the inspect fallback
	// stops finding one; and the same code that refuses every attach cell's key refuses nothing here (#725).
	if healthAfter.SandboxKeyAbsent == nil || healthAfter.SandboxKeyNotPermitted == nil ||
		healthAfter.SandboxKeyNotANamespace == nil ||
		healthAfter.SandboxKeyWrongNSType == nil || healthAfter.SandboxKeyUnavailable == nil {
		t.Fatal("the recovered plugin publishes no sandbox key refusal arms, so 'no refusal fired' " +
			"cannot be judged — and reading their absence as zero is how a plugin that refused every " +
			"key would pass this")
	}
	t.Logf("CELL-ARM mode=recovery user=\"\": sandbox_key_absent %d, sandbox_key_not_permitted %d, "+
		"sandbox_key_not_a_namespace %d, sandbox_key_wrong_ns_type %d, sandbox_key_unavailable %d "+
		"(absolute, fresh instance)",
		*healthAfter.SandboxKeyAbsent, *healthAfter.SandboxKeyNotPermitted,
		*healthAfter.SandboxKeyNotANamespace,
		*healthAfter.SandboxKeyWrongNSType, *healthAfter.SandboxKeyUnavailable)
	for _, arm := range []struct {
		name string
		got  int32
	}{
		{"sandbox_key_absent", *healthAfter.SandboxKeyAbsent},
		{"sandbox_key_not_permitted", *healthAfter.SandboxKeyNotPermitted},
		{"sandbox_key_not_a_namespace", *healthAfter.SandboxKeyNotANamespace},
		{"sandbox_key_wrong_ns_type", *healthAfter.SandboxKeyWrongNSType},
		{"sandbox_key_unavailable", *healthAfter.SandboxKeyUnavailable},
	} {
		if arm.got != 0 {
			t.Errorf("%s=%d on the recovered instance: a sandbox that predates this plugin process "+
				"was refused, so the asymmetry the attach cells and this one measure together — the "+
				"same key form accepted here and refused there — is gone, and the reason SECURITY.md "+
				"gives for keeping pidhost and CAP_SYS_PTRACE has to be re-derived", arm.name, arm.got)
		}
	}

	// The kernel's view from inside the namespace, after the recycle.
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show")
	if !strings.Contains(out, ipAfter+"/") {
		t.Errorf("`ip -4 addr show` inside the container does not carry %s after the recycle.\n%s",
			ipAfter, out)
	}
}
