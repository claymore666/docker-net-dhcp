//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

// TestMTUPropagate_OptInSetsLinkMTU is the v0.9.0 / T1-2 guard:
// when `propagate_mtu=true` is set on the network, the container's
// eth0 must come up with the DHCP-supplied MTU (option 26,
// advertised by the fixture's dnsmasq as harness.TestMTU).
//
// Together with TestMTUPropagate_DefaultIsUnchanged below this pins
// both the opt-in and the historical default.
func TestMTUPropagate_OptInSetsLinkMTU(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-mtu"
	ctrName := "dh-itest-mtu-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"propagate_mtu": "true",
	})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	// MTU is applied from the persistent-client `bound` event
	// (post-Join). Poll briefly to absorb the gap between
	// inspect-shows-IP and bound-event-processed.
	deadline := time.Now().Add(5 * time.Second)
	wantMTU := "mtu " + harness.TestMTU
	var out string
	for time.Now().Before(deadline) {
		out = harness.ExecOutput(t, ctx, id, "ip", "link", "show", "eth0")
		if strings.Contains(out, wantMTU) {
			t.Logf("eth0 inside container shows %s", wantMTU)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("expected %q on eth0 within 5s; got:\n%s", wantMTU, out)
}

// TestMTUPropagate_DefaultIsUnchanged: without the opt-in, eth0
// retains the link-default MTU (1500 for ethernet, regardless of
// what DHCP option 26 advertises).
func TestMTUPropagate_DefaultIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-mtu-default"
	ctrName := "dh-itest-mtu-default-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	out := harness.ExecOutput(t, ctx, id, "ip", "link", "show", "eth0")
	if strings.Contains(out, "mtu "+harness.TestMTU) {
		t.Errorf("propagate_mtu is off but eth0 still came up at MTU %s — default flipped?\nlink:\n%s",
			harness.TestMTU, out)
	}
}
