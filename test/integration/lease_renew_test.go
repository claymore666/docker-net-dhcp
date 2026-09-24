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

// The lease must outlive the wait, since an ACK after expiry is a re-acquisition, so the fixture keeps the default
// 120 s lease and advertises T1 and T2 (options 58 and 59) on its own ephemeral fixture; dhcpcd honours the server's T1
// (#253, #356).

// TestLeaseRenew_HonorsT1 checks that the persistent client renews at the server's T1 without changing the IP.
func TestLeaseRenew_HonorsT1(t *testing.T) {
	const (
		renewT1 = 12 // seconds; dhcpcd renews here, above its floor
		renewT2 = 25 // seconds; rebind — kept past the wait window
		// Past T1 and before T2, so the extra ACK is a renewal and not a rebind.
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

	if endACKs-startACKs < 1 {
		t.Errorf("no renewal DHCPACK observed for %s in the %s wait window — renewal client appears stuck or dnsmasq is not handling the renewal request", mac, waitFor)
	}
	if endACKs < 2 {
		t.Errorf("expected at least 2 DHCPACKs for %s (bind + renewal), got %d", mac, endACKs)
	}
}
