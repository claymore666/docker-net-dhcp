// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// dhcpcd keys its pidfile and control socket by interface name in a shared runtime dir, so a second eth0 client once
// forwarded its argv to the first and exited 0, leaving an IP nobody renewed; no other test waits for a renewal with
// two containers. The second container starts after the first client is live, so the collision is the expected case.
// Renewal is driven by T1 on an ephemeral fixture with a long lease, as in TestLeaseRenew_HonorsT1 (#330, #356).

// TestConcurrentRenew_SameInterfaceNameBothRenew checks that two eth0 containers on one network each renew their own lease (#330).
func TestConcurrentRenew_SameInterfaceNameBothRenew(t *testing.T) {
	const (
		renewT1 = 12 // seconds; dhcpcd renews here, above its floor
		renewT2 = 25 // seconds; rebind — kept past the wait window
		// Join spawns the persistent client from a goroutine, so RunContainer returning does not mean it runs yet (#330).
		settle  = 3 * time.Second
		waitFor = 20 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	netName := "dh-itest-corenew"

	ef := harness.NewEphemeralFixture(t, harness.WithRenewTimes(renewT1, renewT2))
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})

	type ctr struct {
		name      string
		id        string
		ip        string
		mac       string
		startACKs int
	}
	ctrs := make([]*ctr, 2)

	for i := range ctrs {
		name := fmt.Sprintf("dh-itest-corenew-ctr-%d", i)
		id, ip, mac := harness.RunContainer(t, ctx, netName, name)
		ctrs[i] = &ctr{name: name, id: id, ip: ip, mac: mac}
		t.Logf("container %d up: ip=%s mac=%s", i, ip, mac)

		if i == 0 {
			select {
			case <-ctx.Done():
				t.Fatalf("context cancelled while settling: %v", ctx.Err())
			case <-time.After(settle):
			}
		}
	}

	if ctrs[0].ip == ctrs[1].ip {
		t.Fatalf("both containers got the same IP %s — the fixture pool is not handing out distinct leases", ctrs[0].ip)
	}

	// CountLogLines AND-matches substrings, so an empty MAC matches every ACK and two equal MACs share one count; a
	// plugin that reported no MAC would return "" without failing anything (#330).
	for i, c := range ctrs {
		if c.mac == "" {
			t.Fatalf("container %d has no MAC, so its ACK count would match every ACK in the "+
				"fixture log and this test would measure the whole server rather than one client", i)
		}
	}
	if ctrs[0].mac == ctrs[1].mac {
		t.Fatalf("both containers report MAC %s, so the two ACK counts below are the same count "+
			"and one renewing client would satisfy both", ctrs[0].mac)
	}

	// After the binds, each further ACK for a MAC is that container's renewal.
	for i, c := range ctrs {
		c.startACKs = ef.CountLogLines("DHCPACK", c.mac)
		t.Logf("container %d: %d DHCPACK(s) at start of renewal window", i, c.startACKs)
	}

	t.Logf("waiting %s for both renewal cycles (T1=%ds, T2=%ds)...", waitFor, renewT1, renewT2)
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

	for i, c := range ctrs {
		endACKs := ef.CountLogLines("DHCPACK", c.mac)
		if endACKs <= c.startACKs {
			t.Errorf("container %d (mac=%s, ip=%s): no renewal ACK in %s — %d ACK(s) before, %d after. "+
				"Its persistent DHCP client is not renewing; the lease will lapse at expiry.",
				i, c.mac, c.ip, waitFor, c.startACKs, endACKs)
		} else {
			t.Logf("container %d: renewed (%d -> %d ACKs)", i, c.startACKs, endACKs)
		}

		ins, err := cli.ContainerInspect(ctx, c.id)
		if err != nil {
			t.Fatalf("ContainerInspect(%s): %v", c.name, err)
		}
		var ipAfter string
		for _, ep := range ins.NetworkSettings.Networks {
			ipAfter = ep.IPAddress
		}
		if ipAfter != c.ip {
			t.Errorf("container %d: IP changed across the renewal window: %s -> %s", i, c.ip, ipAfter)
		}
	}
}
