//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// TestNonRootContainer_PersistentClientStarts is the regression test
// for #317: the persistent DHCP client must start for a container whose
// init process runs as a NON-ROOT user.
//
// Opening /proc/<pid>/ns/net is gated by the kernel's PTRACE_MODE_READ
// check: same uid as the target, or CAP_SYS_PTRACE. Every other test in
// this suite runs its container as root, so the uid-match arm always
// passes and the capability is never needed — which is exactly how the
// missing CAP_SYS_PTRACE in config.json survived every release of this
// fork. In production (any compose service with a `USER`) the netns
// open failed with EACCES on every retry, the persistent client never
// started, and the lease silently went unrenewed and unreleased.
//
// The assertion strategy mirrors TestLeaseRenew_HonorsT1: a dedicated
// ephemeral fixture advertises short T1/T2 (option 58/59, #253), and a
// renewal DHCPACK within the window proves the persistent client is
// alive inside the non-root container's netns. On a pre-#317 plugin
// the client never starts, no renewal ACK arrives, and this test fails
// — verified against the unfixed manifest. join_start_failures must
// also stay flat (it's the counter #317 adds for this failure mode).
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
	// 65534 = nobody. uid != 0 is the whole point: the plugin (root)
	// must need CAP_SYS_PTRACE to enter this container's netns.
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

	// The suite shares one plugin instance, so assert the DELTA of the
	// failure counter across this test, not its absolute value. Closing
	// the window also proves the instance was the same one throughout,
	// without which the delta below would be arithmetic on two
	// unrelated numbers (#405).
	healthBefore, healthAfter := w.End()
	if d := healthAfter.JoinStartFailures - healthBefore.JoinStartFailures; d != 0 {
		t.Errorf("join_start_failures grew by %d during this test — persistent client failed to start", d)
	}
}
