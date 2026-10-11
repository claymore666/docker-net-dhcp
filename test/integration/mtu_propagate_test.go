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

// ctrLinkLine returns the container's `ip -o link show` line for the link that carries its leased address ip, or "" when no
// link does. Bridge mode names that link after the bridge (DstPrefix in pkg/plugin/network.go), so eth0 does not exist
// there, and a tunnel device a namespace may come with is not the link under test (#1320).
func ctrLinkLine(t *testing.T, ctx context.Context, id, ip string) string {
	t.Helper()
	for _, line := range strings.Split(harness.ExecOutput(t, ctx, id, "ip", "-o", "-4", "addr", "show"), "\n") {
		if f := strings.Fields(line); len(f) > 1 && strings.Contains(line, " "+ip+"/") {
			return harness.ExecOutput(t, ctx, id, "ip", "-o", "link", "show", "dev", f[1])
		}
	}
	return ""
}

// ctrLinkMTUOutput returns the container link's line once it shows want, or the last read when the budget runs out (#1320).
func ctrLinkMTUOutput(t *testing.T, ctx context.Context, id, ip, want string) (string, bool) {
	t.Helper()
	// The MTU is applied from the post-Join bound event, which can trail the IP by a lost reply (harness.RetransmitBudget).
	deadline := time.Now().Add(harness.RetransmitBudget(2))
	var line string
	for time.Now().Before(deadline) {
		line = ctrLinkLine(t, ctx, id, ip)
		if strings.Contains(line, want) {
			return line, true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return line, false
}

// TestMTUPropagate_BridgeAndIpvlan checks that propagate_mtu=true gives the container link option 26's MTU on bridge and ipvlan, and that
// the link keeps its inherited MTU without the opt-in (#1320).
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
			id, ip, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")

			want := "mtu " + harness.TestMTU
			if tc.propagate {
				if out, ok := ctrLinkMTUOutput(t, ctx, id, ip, want); !ok {
					t.Errorf("propagate_mtu=true but the container link never showed %q: %q", want, out)
				}
				return
			}
			// Watch for as long as the opt-in case allows the 1400 to arrive: a single read could land before it (#1320).
			seen := false
			for end := time.Now().Add(harness.RetransmitBudget(2)); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
				line := ctrLinkLine(t, ctx, id, ip)
				seen = seen || strings.Contains(line, " mtu ")
				if strings.Contains(line, want) {
					t.Fatalf("propagate_mtu is off but the container link came up at %q: %q", want, line)
				}
			}
			if !seen {
				t.Fatalf("the container had no link carrying %s, so the absence of the MTU above says nothing", ip)
			}
		})
	}
}
