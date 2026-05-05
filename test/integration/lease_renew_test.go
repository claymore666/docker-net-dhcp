//go:build integration

package integration

import (
	"context"
	"os"
	"strings"
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
// The fixture's lease time is 2m (dnsmasq's hard minimum, see
// LeaseTime in harness/fixture.go), so T1 (renewal trigger inside
// udhcpc) fires at 1m. We wait ~70s — past T1, well before T2
// (1m45s) — and assert:
//   - the container's IP from docker inspect hasn't changed
//   - dnsmasq's log shows at least 2 DHCPACK lines for our MAC
//     (one for the initial bind, one for the renewal)
//
// Without this test, a regression in dhcpManager.renew or the
// long-lived udhcpc client would be silent: the container starts
// fine, then loses its IP somewhere between T2 and the next operator
// noticing the connection dropped.
func TestLeaseRenew_HonorsT1(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	netName := "dh-itest-renew"
	ctrName := "dh-itest-renew-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, ipBefore, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("initial: ip=%s mac=%s", ipBefore, mac)

	startACKs := countDHCPACKs(t, mac)

	// Sleep past T1 (60s for a 2m lease) but well before T2 (105s)
	// to keep the assertion clean: the only ACK we expect on top of
	// the bind is a renewal ACK, not a rebind.
	t.Log("waiting 70s for lease renewal cycle...")
	select {
	case <-ctx.Done():
		t.Fatalf("context cancelled before renewal window: %v", ctx.Err())
	case <-time.After(70 * time.Second):
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

	endACKs := countDHCPACKs(t, mac)
	t.Logf("DHCPACKs for %s: start=%d, after=%d", mac, startACKs, endACKs)

	// The initial bind is one ACK; a renewal is one more. We've
	// waited past T1, so we expect at least one renewal ACK on top
	// of the bind. Strictly: endACKs - startACKs >= 1, and total >= 2.
	if endACKs-startACKs < 1 {
		t.Errorf("no renewal DHCPACK observed for %s in the 22s wait window — renewal client appears stuck or dnsmasq is not handling the renewal request", mac)
	}
	if endACKs < 2 {
		t.Errorf("expected at least 2 DHCPACKs for %s (bind + renewal), got %d", mac, endACKs)
	}
}

// countDHCPACKs reads the macvlan-fixture dnsmasq log and counts
// "DHCPACK" lines that mention `mac`. dnsmasq emits one ACK line per
// successful bind/renewal/rebind, so the count is a clean monotonic
// proxy for "how many times has dnsmasq blessed a request from this
// MAC". MAC matching is case-insensitive and substring-based — the
// log format prints lowercase MAC late in the line.
func countDHCPACKs(t *testing.T, mac string) int {
	t.Helper()
	data, err := os.ReadFile(fixture.DnsmasqLog())
	if err != nil {
		t.Fatalf("read dnsmasq log: %v", err)
	}
	macLower := strings.ToLower(mac)
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "DHCPACK") && strings.Contains(strings.ToLower(line), macLower) {
			count++
		}
	}
	return count
}
