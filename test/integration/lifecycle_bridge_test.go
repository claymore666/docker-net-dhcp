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

// TestLifecycleBridge_GoldenPath creates a bridge-mode network, runs a container on it and removes both.
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

	// Bridge mode names the container interface after the bridge (DstPrefix in pkg/plugin/network.go), not eth0.
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr")
	if !strings.Contains(out, ipv4) {
		t.Errorf("no interface inside container reports docker-inspect IP %q\nactual:\n%s", ipv4, out)
	}
	macOut := harness.ExecOutput(t, ctx, id, "ip", "link")
	if !strings.Contains(strings.ToLower(macOut), strings.ToLower(mac)) {
		t.Errorf("no interface inside container reports docker-inspect MAC %q\nactual:\n%s", mac, macOut)
	}
}
