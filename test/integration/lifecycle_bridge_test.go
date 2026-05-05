//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

// TestLifecycleBridge_GoldenPath is the bridge-mode counterpart to
// TestLifecycleMacvlan_GoldenPath. The plugin's bridge mode predates
// the macvlan/ipvlan additions and goes through a structurally
// different code path: per-endpoint veth pair, host side attached to
// a user-provided Linux bridge, container side moved into the
// container netns. udhcpc still runs in the container netns, but the
// DHCP server it talks to here is the second dnsmasq the bridge
// fixture starts on the bridge interface (192.168.100/24, distinct
// from the macvlan path's 192.168.99/24 to avoid lease cross-talk).
//
// Exercises: CreateNetwork (mode=bridge branch with bridge-link
// validation), createBridgeEndpoint, dhcpManager.Start over the
// host-side veth, Join (move container-side veth into netns), Leave,
// DeleteEndpoint (bridge cleanup branch), DeleteNetwork.
func TestLifecycleBridge_GoldenPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-bridge-golden"
	ctrName := "dh-itest-bridge-golden-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "bridge", nil)
	id, ipv4, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s: id=%s ip=%s mac=%s", ctrName, id[:12], ipv4, mac)

	ip := harness.AssertBridgeIP(t, ipv4)
	t.Logf("✓ container IP %s falls in bridge DHCP pool", ip)

	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, ipv4) {
		t.Errorf("eth0 inside container does not show docker-inspect IP %q\nactual:\n%s", ipv4, out)
	}
	macOut := harness.ExecOutput(t, ctx, id, "ip", "link", "show", "eth0")
	if !strings.Contains(strings.ToLower(macOut), strings.ToLower(mac)) {
		t.Errorf("eth0 MAC inside container does not match docker inspect MAC %q\nactual:\n%s", mac, macOut)
	}
}
