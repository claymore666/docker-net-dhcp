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

	// PluginDisable kills the plugin process and PluginEnable starts a
	// fresh one, so every counter below is from a brand-new instance
	// starting at zero. A before/after delta across this point is void,
	// which is why the assertions further down are absolute (`after >=
	// 1`) rather than deltas.
	//
	// That used to be recorded here as a comment and enforced by
	// nothing. The window makes it an assertion: ExpectRecycle fails if
	// the plugin does *not* restart, so if PluginDisable ever stops
	// ending the process this test says so instead of quietly measuring
	// a delta against a stale baseline (#405). It is also the only
	// place instance_id is exercised against a real recycle rather than
	// a fabricated payload.
	w := harness.BeginCounterWindow(t, ctx, cli,
		"recovered_ok", "recovery_failed",
		"sandbox_key_entries", "sandbox_key_entry_failures", "sandbox_pid_fallbacks").ExpectRecycle()

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
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)

	// Closing the window does both jobs at once: it takes the
	// post-recycle read the assertions below use, and it asserts the
	// instance id actually changed — i.e. that this test really did
	// exercise a fresh plugin process.
	_, healthAfter := w.End()
	t.Logf("recovered_ok after: %d", healthAfter.RecoveredOK)

	if healthAfter.RecoveredOK < 1 {
		t.Errorf("recovered_ok=%d (expected >=1; recovery did not pick up our endpoint after the recycle)",
			healthAfter.RecoveredOK)
	}
	if healthAfter.RecoveryFailed != 0 {
		t.Errorf("recovery_failed=%d (recovery saw at least one endpoint it could not rebuild)", healthAfter.RecoveryFailed)
	}
	// The other direction of the #376 classifier, and the reason this
	// assertion is worth having: recovery_aborted_container_gone is
	// the arm that does NOT flip healthy, so a classifier that called
	// a running container "gone" would turn every real recovery
	// failure into a silent one and recovered_ok/recovery_failed above
	// would look fine. The container ran throughout this recycle, so
	// nothing may land in the benign bucket.
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

	// THE RECOVERY CELL OF THE #725 MEASUREMENT.
	//
	// The brief asked whether the sandbox key survives a plugin
	// restart, since recovery re-adopts an endpoint with no Join to
	// carry one — "the key must come from the durable record — does
	// it?". MEASURED ANSWER: it need not. Join and recovery both reach
	// dhcpManager.Start, which already inspects the container, so the
	// key is read from NetworkSettings.SandboxKey on a live inspect in
	// both cases. Nothing was added to the durable record, and this is
	// the assertion that says the live source is in fact enough.
	//
	// The reads are ABSOLUTE, not deltas, for the reason stated at the
	// window above: this is a fresh plugin process and its counters
	// started at zero. That is what makes them exactly right here —
	// every entry counted on this instance was made by recovery,
	// because nothing else has run on it yet.
	if healthAfter.SandboxKeyEntries == nil || healthAfter.SandboxPIDFallbacks == nil ||
		healthAfter.SandboxKeyEntryFailures == nil {
		t.Fatal("the recovered plugin publishes no sandbox route counters, so which route recovery " +
			"took cannot be judged — and reading their absence as zero is how a recovery that fell " +
			"back to the container PID would pass this test")
	}
	t.Logf("CELL mode=recovery user=\"\": sandbox_key_entries %d, sandbox_key_entry_failures %d, sandbox_pid_fallbacks %d (absolute, fresh instance)",
		*healthAfter.SandboxKeyEntries, *healthAfter.SandboxKeyEntryFailures, *healthAfter.SandboxPIDFallbacks)
	if *healthAfter.SandboxKeyEntries < 1 {
		t.Errorf("sandbox_key_entries=%d on the recovered instance: recovery re-adopted an endpoint "+
			"without entering its namespace through the key, so \"no fallbacks\" below would be a "+
			"statement about an empty set", *healthAfter.SandboxKeyEntries)
	}
	if *healthAfter.SandboxPIDFallbacks != 0 {
		t.Errorf("sandbox_pid_fallbacks=%d on the recovered instance: the sandbox key did NOT survive "+
			"the recycle and the /proc/<pid>/ns/net route carried the re-adoption. That is the cell "+
			"that keeps pidhost in config.json — record it in SECURITY.md, do not silence it",
			*healthAfter.SandboxPIDFallbacks)
	}
	if *healthAfter.SandboxKeyEntryFailures != 0 {
		t.Errorf("sandbox_key_entry_failures=%d on the recovered instance: the key route was refused "+
			"or timed out during recovery even though something later succeeded",
			*healthAfter.SandboxKeyEntryFailures)
	}

	// Outside evidence, the same as every other cell: the kernel's view
	// from inside the namespace, not Docker's record of it. The inspect
	// above proves libnetwork still believes the endpoint; this proves
	// the address is actually configured on the interface after the
	// recycle.
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show")
	if !strings.Contains(out, ipAfter+"/") {
		t.Errorf("`ip -4 addr show` inside the container does not carry %s after the recycle.\n%s",
			ipAfter, out)
	}
}
