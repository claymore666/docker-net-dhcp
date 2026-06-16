//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// TestLeaseRenew_HonorsT1 verifies the persistent renewal client the
// plugin starts in dhcpManager.Start actually renews the lease before
// it expires, and that the renewal goes through (DHCPACK from
// dnsmasq) without disturbing the container's IP.
//
// dnsmasq's minimum *lease* is a hard 2m, so we can't shorten the
// lease to make renewal fire sooner. Instead this test runs on a
// dedicated ephemeral fixture that advertises DHCP option 58 (T1,
// renewal) / 59 (T2, rebind) explicitly — independent of the lease
// length — via WithRenewTimes. dhcpcd honours the server-supplied
// T1, so renewal fires at renewT1 (~12s) instead of half of a 2m
// lease (~60s). We wait past T1, well before T2, and assert:
//   - the container's IP from docker inspect hasn't changed
//   - the fixture's dnsmasq log shows at least 2 DHCPACK lines for
//     our MAC (one for the initial bind, one for the renewal)
//
// The shared suite fixture's 2m lease is left untouched — every other
// test depends on its stability (#253).
//
// The mechanism is self-validating: if dnsmasq doesn't honour the
// advertised T1 (or dhcpcd floors it), no renewal ACK lands in the
// shortened window and the assertions below fail — it never silently
// passes.
//
// Without this test, a regression in dhcpManager.renew or the
// long-lived dhcpcd client would be silent: the container starts
// fine, then loses its IP somewhere between T2 and the next operator
// noticing the connection dropped.
func TestLeaseRenew_HonorsT1(t *testing.T) {
	const (
		renewT1 = 12 // seconds; dhcpcd renews here, above its floor
		renewT2 = 25 // seconds; rebind — kept past the wait window
		// Wait past T1 but comfortably before T2, so the only ACK we
		// expect on top of the bind is a renewal ACK, not a rebind.
		waitFor = 18 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-renew"
	ctrName := "dh-itest-renew-ctr"

	ef := harness.NewEphemeralFixture(t, harness.WithRenewTimes(renewT1, renewT2))
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, ipBefore, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("initial: ip=%s mac=%s", ipBefore, mac)

	startACKs := ef.CountLogLines("DHCPACK", mac)

	t.Logf("waiting %s for lease renewal cycle (T1=%ds, T2=%ds)...", waitFor, renewT1, renewT2)
	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled before renewal window: %v", ctx.Err())
	case <-time.After(waitFor):
	}

	// Re-poll inspect: the IP must not have changed during the
	// renewal window. Any change here means the renewal client lost
	// the lease and DISCOVERed a new one.
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	var ipAfter string
	for _, ep := range ins.NetworkSettings.Networks {
		ipAfter = ep.IPAddress
	}
	if ipAfter != ipBefore {
		t.Errorf("IP changed during renewal window: before=%s after=%s (renewal client did not preserve lease)", ipBefore, ipAfter)
	}

	endACKs := ef.CountLogLines("DHCPACK", mac)
	t.Logf("DHCPACKs for %s: start=%d, after=%d", mac, startACKs, endACKs)

	// The initial bind is one ACK; a renewal is one more. We've
	// waited past T1, so we expect at least one renewal ACK on top
	// of the bind. Strictly: endACKs - startACKs >= 1, and total >= 2.
	if endACKs-startACKs < 1 {
		t.Errorf("no renewal DHCPACK observed for %s in the %s wait window — renewal client appears stuck or dnsmasq is not handling the renewal request", mac, waitFor)
	}
	if endACKs < 2 {
		t.Errorf("expected at least 2 DHCPACKs for %s (bind + renewal), got %d", mac, endACKs)
	}
}
