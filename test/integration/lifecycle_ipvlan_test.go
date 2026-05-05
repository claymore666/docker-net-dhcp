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
// One observable mode-specific difference: ipvlan children inherit
// the parent's MAC, so docker inspect shows the HostVeth MAC instead
// of a random one. The test asserts the container's eth0 has the
// same MAC the daemon reported.
//
// Earlier this test was `t.Skip`'d because the OFFER never reached
// the slave through a veth parent. The fix landed in pkg/udhcpc:
// setting the BROADCAST flag in DISCOVER (`udhcpc -B`) for ipvlan
// mode forces the OFFER to be L2-broadcast at the wire level, which
// the kernel then floods correctly to all slaves of the parent.
// dnsmasq's `--dhcp-broadcast` in our fixture is now belt-and-braces
// rather than the only thing keeping ipvlan honest.
func TestLifecycleIPvlan_GoldenPath(t *testing.T) {
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
