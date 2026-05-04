//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

// TestLifecycleIPvlan_GoldenPath mirrors the macvlan golden path with
// mode=ipvlan. ipvlan-L2 is the default in newChildLink and the only
// mode that can carry DHCP (which needs L2 broadcast) — this test
// guards that invariant; an accidental switch to L3 mode would fail
// here with udhcpc never seeing an OFFER.
//
// **Currently skipped**: when the harness's DHCP server runs over a
// veth-pair parent (our setup) the broadcast OFFER doesn't reach the
// ipvlan slave inside the container netns. Real LAN DHCP servers
// (Fritz.Box etc) over a hardware NIC parent work — that path is
// covered by manual smoke testing. Investigating the veth-parent
// delivery quirk is tracked as a follow-up; documented in
// test/integration/README.md "ipvlan + veth" section.
//
// One observable mode-specific difference: ipvlan children inherit
// the parent's MAC, so docker inspect shows the HostVeth MAC instead
// of a random one. The test asserts the container's eth0 has the
// same MAC the daemon reported.
func TestLifecycleIPvlan_GoldenPath(t *testing.T) {
	t.Skip("ipvlan-L2 broadcast OFFER delivery to a slave whose parent is a veth doesn't work in our harness; tracked as a follow-up. Real LAN DHCP via hardware parent is covered by manual smoke testing.")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-ipvlan-golden"
	ctrName := "dh-itest-ipvlan-golden-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "ipvlan", nil)
	id, ipv4, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s: id=%s ip=%s mac=%s", ctrName, id[:12], ipv4, mac)

	harness.AssertIP(t, ipv4)

	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, ipv4) {
		t.Errorf("eth0 inside container does not show docker-inspect IP %q\nactual:\n%s", ipv4, out)
	}
	macOut := harness.ExecOutput(t, ctx, id, "ip", "link", "show", "eth0")
	if !strings.Contains(strings.ToLower(macOut), strings.ToLower(mac)) {
		t.Errorf("eth0 MAC inside container does not match docker inspect MAC %q\nactual:\n%s", mac, macOut)
	}
}
