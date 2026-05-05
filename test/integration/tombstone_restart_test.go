//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"
)

// TestTombstoneRestart_PreservesMACAndIP guards the v0.5.x stability
// guarantee that `docker restart <ctr>` keeps the same MAC and IP.
//
// Mechanism: on Leave the plugin writes a tombstone with the MAC and
// last-known IP, keyed by (network, endpoint). On the subsequent
// CreateEndpoint Docker hands the same endpoint ID back, so the
// plugin reuses the tombstoned MAC for the new macvlan child and
// asks udhcpc to renew the same IP. tombstoneTTL was bumped to 60s
// in v0.6.1 (see #55) so a slow `systemctl restart docker` doesn't
// drop the entry — but `docker restart <ctr>` itself completes in
// well under that window.
func TestTombstoneRestart_PreservesMACAndIP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-tombstone-net"
	ctrName := "dh-itest-tombstone-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, ipBefore, macBefore := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("before restart: ip=%s mac=%s", ipBefore, macBefore)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}

	// Re-poll inspect for the post-restart endpoint values; the
	// endpoint is torn down and re-created so the IP can briefly
	// be empty mid-restart.
	deadline := time.Now().Add(harness.IPAcquisitionBudget)
	var ipAfter, macAfter string
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				ipAfter = ep.IPAddress
				macAfter = ep.MacAddress
			}
		}
		if ipAfter != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ipAfter == "" {
		t.Fatalf("container did not re-acquire an IP within %v after restart", harness.IPAcquisitionBudget)
	}
	t.Logf("after restart:  ip=%s mac=%s", ipAfter, macAfter)

	if macAfter != macBefore {
		t.Errorf("MAC changed across restart: before=%s after=%s (tombstone not honored)", macBefore, macAfter)
	}
	if ipAfter != ipBefore {
		t.Errorf("IP changed across restart: before=%s after=%s (DHCP did not renew the cached lease)", ipBefore, ipAfter)
	}
}
