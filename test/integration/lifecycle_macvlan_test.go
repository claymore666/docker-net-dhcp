// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
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

	// Baseline the plugin before any test runs, so the floor can judge
	// what THIS process caused rather than everything the plugin has
	// done since it started.
	//
	// The sharded lanes create a plugin per job, so the two are the same
	// thing there. The coverage lane drives ONE instrumented plugin
	// through the main suite and then this one, and a probe failure the
	// main suite declared as deliberate was still on the counter when
	// this process started with an allowance of zero. That failed the
	// v1.6.0 release PR's coverage run on a run in which nothing was
	// wrong.
	//
	// Best-effort by design: a baseline that cannot be read leaves the
	// zero value, which restores the old whole-plugin-life behaviour.
	// That direction judges more than this process caused, never less.
	floorHealthBaseline = harness.PluginHealthOrNil(ctx)
	floorLogBaseline = harness.PluginLogSize(ctx)

	suiteStart := time.Now()
	rc := m.Run()

	// The health floor runs before teardown, while the plugin is
	// still serving, and unconditionally: on an already-red run its
	// output is often what explains the red. It can turn a green run
	// red, never the reverse. See checkHealthFloor.
	if code := checkHealthFloor(time.Since(suiteStart)); code != 0 && rc == 0 {
		rc = code
	}

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
// dhcpManager.Start (initial lease via one-shot dhcpcd), Join (move link
// into netns), Leave (Stop the manager; no DHCPRELEASE since #800 — the
// address is left to expire), DeleteEndpoint
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
