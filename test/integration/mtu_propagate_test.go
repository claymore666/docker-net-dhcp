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

// eth0MTUOutput returns the container's `ip link show eth0` once it shows want, or the last read when budget runs out (#1320).
func eth0MTUOutput(t *testing.T, ctx context.Context, id, want string) (string, bool) {
	t.Helper()
	// The MTU is applied from the post-Join bound event, which can trail the IP by a lost reply (harness.RetransmitBudget).
	deadline := time.Now().Add(harness.RetransmitBudget(2))
	var out string
	for time.Now().Before(deadline) {
		out = harness.ExecOutput(t, ctx, id, "ip", "link", "show", "eth0")
		if strings.Contains(out, want) {
			return out, true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return out, false
}

// TestMTUPropagate_BridgeAndIpvlan checks that propagate_mtu=true gives eth0 option 26's MTU on bridge and ipvlan, and that
// eth0 keeps its inherited MTU without the opt-in (#1320).
func TestMTUPropagate_BridgeAndIpvlan(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		propagate bool
	}{
		{"bridge", true},
		{"bridge", false},
		{"ipvlan", true},
		{"ipvlan", false},
	} {
		name := tc.mode + "-default"
		var opts map[string]string
		if tc.propagate {
			name = tc.mode + "-optin"
			opts = map[string]string{"propagate_mtu": "true"}
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			netName := "dh-itest-mtu-" + name
			t.Cleanup(func() {
				if t.Failed() {
					fixture.DumpLogs(func(s string) { t.Log(s) })
					fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
					harness.DumpPluginLog(t)
				}
			})

			harness.CreateNetwork(t, ctx, netName, tc.mode, opts)
			id, _, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")

			want := "mtu " + harness.TestMTU
			if tc.propagate {
				if out, ok := eth0MTUOutput(t, ctx, id, want); !ok {
					t.Errorf("propagate_mtu=true but eth0 never showed %q:\n%s", want, out)
				}
				return
			}
			// Watch for as long as the opt-in case allows the 1400 to arrive: a single read could land before it (#1320).
			for end := time.Now().Add(harness.RetransmitBudget(2)); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
				if out := harness.ExecOutput(t, ctx, id, "ip", "link", "show", "eth0"); strings.Contains(out, want) {
					t.Fatalf("propagate_mtu is off but eth0 came up at %q:\n%s", want, out)
				}
			}
		})
	}
}
