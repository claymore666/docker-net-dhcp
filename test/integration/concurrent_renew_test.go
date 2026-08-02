//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// TestConcurrentRenew_SameInterfaceNameBothRenew verifies that two
// containers on the same network — both with the default container-side
// interface name `eth0` — each keep a persistent DHCP client that
// actually renews its own lease.
//
// This closes a structural blind spot rather than a hypothetical one.
// dhcpcd keys its pidfile and control sockets by interface name alone,
// in a runtime dir shared across the plugin's mount view. When only the
// *state* dir was shadowed per client, the second container's dhcpcd
// found the first container's live control socket, forwarded its argv
// to it, and exited 0 — so the second container held an IP that no
// client was renewing or releasing, while the first container's dhcpcd
// was reconfigured with the second's settings.
//
// Nothing in the suite could see that. TestConcurrency_DistinctLeases
// is the only concurrent multi-container test and asserts only the
// *initial* IP/MAC, which comes from the one-shot client at
// CreateEndpoint — it never waits for a renewal. Every renewal test
// (TestLeaseRenew_HonorsT1, TestLeaseRenewIPv6_HonorsT1, the failure
// suite) runs a single container. So the suite passed identically with
// the bug present and absent, and the fix for it — a tmpfs over the
// runtime dir — was covered only by a unit test asserting that string
// appears in the generated mount script.
//
// The ordering is deliberate: the second container starts only after
// the first is up and settled, so the first container's persistent
// client is guaranteed live and holding the shared socket when the
// second one starts. Concurrent starts would make the collision racy;
// this makes it the expected case.
//
// Mechanism as in TestLeaseRenew_HonorsT1: an ephemeral fixture
// advertising option 58/59 so renewal fires ~12s in rather than at half
// the lease. The lease itself stays long on purpose — an ACK observed
// after expiry would be a re-acquisition rather than a renewal, and
// this test would pass while proving the opposite of its name (#356).
// Self-validating — if renewal never fires for either container, both
// assertions fail rather than silently passing.
func TestConcurrentRenew_SameInterfaceNameBothRenew(t *testing.T) {
	const (
		renewT1 = 12 // seconds; dhcpcd renews here, above its floor
		renewT2 = 25 // seconds; rebind — kept past the wait window
		// Let the first container's persistent client come up before
		// the second starts: the client is spawned from a goroutine in
		// Join, so returning from RunContainer does not imply it is
		// running yet.
		settle = 3 * time.Second
		// Past T1 for the container that started last, comfortably
		// before its T2.
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

	// Count from here: the binds have happened, so any further ACK for
	// a MAC is a renewal for that container specifically.
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
