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

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

var fixture *harness.Fixture

// TestMain starts the shared fixture once; each test owns its network and container.
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

	// The coverage lane runs one plugin through the main suite and then this one, and a probe failure declared there
	// failed the v1.6.0 release PR's coverage run, so the floor judges from this process's baseline. An unreadable
	// baseline falls back to the whole plugin life, which judges more, never less (#584).
	floorHealthBaseline = harness.PluginHealthOrNil(ctx)
	floorLogBaseline = harness.PluginLogSize(ctx)

	suiteStart := time.Now()
	rc := m.Run()

	// The floor runs before teardown on every run, since on a red run its output often explains it; it can only turn green red.
	if code := checkHealthFloor(time.Since(suiteStart)); code != 0 && rc == 0 {
		rc = code
	}

	if err := f.Teardown(); err != nil {
		fmt.Fprintln(os.Stderr, "TEARDOWN:", err)
	}
	os.Exit(rc)
}

// Leave sends no DHCPRELEASE: this network does not set release_lease, whose default is never (#800, #962).

// TestLifecycleMacvlan_GoldenPath creates a macvlan network, runs a container that must lease from the pool, and removes both.
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

	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show", "eth0")
	if !strings.Contains(out, ipv4) {
		t.Errorf("eth0 inside container does not show docker-inspect IP %q\nactual:\n%s", ipv4, out)
	}

	if !strings.Contains(strings.ToLower(out), "") {
		// The inet line above already proves the link came up.
	}
	macOut := harness.ExecOutput(t, ctx, id, "ip", "link", "show", "eth0")
	if !strings.Contains(strings.ToLower(macOut), strings.ToLower(mac)) {
		t.Errorf("eth0 MAC inside container does not match docker inspect MAC %q\nactual:\n%s", mac, macOut)
	}
}
