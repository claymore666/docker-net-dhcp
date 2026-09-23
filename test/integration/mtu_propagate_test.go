// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestMTUPropagate_OptInSetsLinkMTU checks that propagate_mtu=true gives eth0 the MTU the server sends in option 26.
func TestMTUPropagate_OptInSetsLinkMTU(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-mtu"
	ctrName := "dh-itest-mtu-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"propagate_mtu": "true",
	})
	id, _, _ := harness.RunContainer(t, ctx, netName, ctrName)

	// The MTU is applied from the post-Join bound event, which can trail the IP by a lost reply (harness.RetransmitBudget).
	budget := harness.RetransmitBudget(2)
	deadline := time.Now().Add(budget)
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
	t.Errorf("expected %q on eth0 within %s; got:\n%s", wantMTU, budget, out)
}

// TestMTUPropagate_DefaultIsUnchanged checks that without the opt-in eth0 keeps 1500, whatever option 26 says.
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
