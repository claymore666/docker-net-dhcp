// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// Opening /proc/<pid>/ns/net passes the kernel's PTRACE_MODE_READ check by matching uid or by CAP_SYS_PTRACE; every
// other test runs as root, which is how the missing capability shipped. A renewal ACK under short T1 (option 58, #253)
// proves the client runs; join_start_failures must stay flat (#317).

// TestNonRootContainer_PersistentClientStarts checks that the persistent client starts for a non-root container (#317).
func TestNonRootContainer_PersistentClientStarts(t *testing.T) {
	const (
		renewT1 = 12 // seconds; dhcpcd renews here, above its floor
		renewT2 = 25 // seconds; rebind — kept past the wait window
		waitFor = 18 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-nonroot"
	ctrName := "dh-itest-nonroot-ctr"

	ef := harness.NewEphemeralFixture(t, harness.WithRenewTimes(renewT1, renewT2))
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
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli, "join_start_failures")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	// 65534 is nobody: a uid other than 0 makes the root plugin need CAP_SYS_PTRACE (#317).
	id, ipBefore, mac := harness.RunContainerUser(t, ctx, netName, ctrName, "65534:65534")
	t.Logf("initial: ip=%s mac=%s user=nobody", ipBefore, mac)

	startACKs := ef.CountLogLines("DHCPACK", mac)

	t.Logf("waiting %s for a renewal from the non-root container (T1=%ds, T2=%ds)...", waitFor, renewT1, renewT2)
	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled before renewal window: %v", ctx.Err())
	case <-time.After(waitFor):
	}

	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	var ipAfter string
	for _, ep := range ins.NetworkSettings.Networks {
		ipAfter = ep.IPAddress
	}
	if ipAfter != ipBefore {
		t.Errorf("IP changed during renewal window: before=%s after=%s", ipBefore, ipAfter)
	}

	endACKs := ef.CountLogLines("DHCPACK", mac)
	t.Logf("DHCPACKs for %s: start=%d, after=%d", mac, startACKs, endACKs)
	if endACKs-startACKs < 1 {
		t.Errorf("no renewal DHCPACK for the non-root container within %s — persistent client did not start in its netns (#317: check CAP_SYS_PTRACE in the plugin manifest)", waitFor)
	}

	// Closing the window proves one plugin instance throughout, so the delta compares one process (#405).
	healthBefore, healthAfter := w.End()
	if d := healthAfter.JoinStartFailures - healthBefore.JoinStartFailures; d != 0 {
		t.Errorf("join_start_failures grew by %d during this test — persistent client failed to start", d)
	}
}
