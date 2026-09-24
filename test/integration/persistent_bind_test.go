// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestPersistentBind_ASecondNetworkOnTheSameFixtureBinds runs two networks in sequence on one server, the first
// endpoint deleted before the second starts, and holds each container's persistent client to a renewal (#1089).
// Only a bound client sends a DHCPREQUEST with ciaddr set (RFC 2131 section 4.4.5, table 5).
func TestPersistentBind_ASecondNetworkOnTheSameFixtureBinds(t *testing.T) {
	const (
		renewT1 = 12
		renewT2 = 25
		// Past T1 with room for the attach, before T2, so the request is a renewal and not a rebind.
		renewBudget = 22 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	ef := harness.NewEphemeralFixture(t, harness.WithDnsmasqBackend(), harness.WithRenewTimes(renewT1, renewT2))
	wire := ef.StartDHCPCapture(t)
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			wire.Dump(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	for i, netName := range []string{"dh-itest-bind1", "dh-itest-bind2"} {
		netID := harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
			"parent": harness.EphemeralHostVeth,
		})
		id, ip, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
		t.Logf("container %d: ip=%s mac=%s", i+1, ip, mac)

		renewals, ok := wire.AwaitRenewalRequestsFrom(mac, 1, renewBudget)
		if !ok {
			t.Fatalf("container %d sent no renewal request within %s of its start, with T1=%ds: its persistent "+
				"client never bound (#1089). The capture holds %d message(s) from %s.",
				i+1, renewBudget, renewT1, len(wire.FramesFrom(mac)), mac)
		}
		renewal := renewals[0]
		if renewal.CIAddr.String() != ip {
			t.Errorf("container %d renewed %s but holds %s", i+1, renewal.CIAddr, ip)
		}

		// The server's own record: the renewal's grant moves the expiry to at least the request plus the lease.
		want := renewal.At.Add(time.Duration(ef.LeaseSeconds())*time.Second - 2*time.Second)
		var expiry time.Time
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(200 * time.Millisecond) {
			var found bool
			if expiry, found = ef.LeaseExpiry(mac); found && !expiry.Before(want) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("container %d: the lease file holds expiry %v for %s after its renewal at %v; the "+
					"server never granted it", i+1, expiry, mac, renewal.At)
			}
		}
		t.Logf("container %d: renewal at %v, lease file expiry %v", i+1, renewal.At.Format(time.RFC3339), expiry)

		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		if got := ins.NetworkSettings.Networks[netName]; got == nil || got.IPAddress != ip {
			t.Errorf("container %d no longer holds %s after its renewal: %+v", i+1, ip, got)
		}

		if i == 0 {
			if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
				t.Fatalf("remove the first container: %v", err)
			}
			if err := cli.NetworkRemove(ctx, netID); err != nil {
				t.Fatalf("remove the first network: %v", err)
			}
		}
	}
}
