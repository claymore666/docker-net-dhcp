//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

var fixture *harness.Fixture

// TestMain stands up the fixture (veth pair + dnsmasq) once per
// `go test` invocation. Per the v0.7.0 design choice 5c (hybrid
// isolation), tests share the fixture but own their own plugin
// network and container.
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := harness.VerifyPluginEnabled(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "PRE-CHECK:", err)
		os.Exit(1)
	}
	if err := harness.EnsureImage(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "PRE-CHECK image pull:", err)
		os.Exit(1)
	}

	f, err := harness.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "FIXTURE:", err)
		os.Exit(1)
	}
	fixture = f

	rc := m.Run()
	if err := f.Teardown(); err != nil {
		fmt.Fprintln(os.Stderr, "TEARDOWN:", err)
	}
	os.Exit(rc)
}

// TestLifecycleMacvlan_GoldenPath is the smoke test: create a
// macvlan-mode network on HostVeth, run a container, assert it gets
// an IP from the DHCP pool, exec a sanity command, then leave.
//
// This single test exercises CreateNetwork (mode=macvlan branch),
// validateParentForChild, createParentAttachedEndpoint,
// dhcpManager.Start (initial lease via udhcpc -q), Join (move link
// into netns), Leave (Stop the manager → DHCPRELEASE), DeleteEndpoint
// (parent-attached cleanup branch), and DeleteNetwork — covering
// the macvlan path end-to-end.
func TestLifecycleMacvlan_GoldenPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-macvlan-golden"
	ctrName := "dh-itest-macvlan-golden-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, ipv4, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s: id=%s ip=%s mac=%s", ctrName, id[:12], ipv4, mac)

	ip := harness.AssertIP(t, ipv4)
	t.Logf("✓ container IP %s falls in DHCP pool", ip)

	// Sanity: the container's own view of its IP must match docker
	// inspect (truthfulness invariant — see RELEASE_NOTES v0.6.0).
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, ipv4) {
		t.Errorf("eth0 inside container does not show docker-inspect IP %q\nactual:\n%s", ipv4, out)
	}

	// MAC parity: the container's eth0 MAC must equal the docker
	// inspect MAC. Not strictly required by the design, but a
	// sudden divergence would mean somebody is lying.
	if !strings.Contains(strings.ToLower(out), "") {
		// (presence of `inet` line implies link came up; relying on
		// the IP check above is enough)
	}
	macOut := harness.ExecOutput(t, ctx, id, "ip", "link", "show", "eth0")
	if !strings.Contains(strings.ToLower(macOut), strings.ToLower(mac)) {
		t.Errorf("eth0 MAC inside container does not match docker inspect MAC %q\nactual:\n%s", mac, macOut)
	}
}
