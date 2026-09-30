// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestBridgeFirewallVerdict_NamedAtCreateAndAtTheDeadline checks that an existing bridge behind a dropping firewall is
// warned about at create and named when the first lease attempt times out, under the default lease_timeout and a
// short one. It sets FORWARD to DROP itself (engine 29.8 leaves ACCEPT), drops the fixture's ACCEPT rules (#103), and
// leases only from the challenger behind a port, since the fixture's server on the bridge's address never meets
// FORWARD. Everything is put back in t.Cleanup (#1116).
func TestBridgeFirewallVerdict_NamedAtCreateAndAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	fixture.StartBridgeChallenger(t)
	harness.WithForwardDrop(t)

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	const netName = "dh-itest-bridge-verdict"
	rule := "iptables -A FORWARD -i " + harness.BridgeName + " -j ACCEPT"

	harness.WithoutBridgeForward(t, harness.BridgeName)
	challengerDiscovers := func() int { return strings.Count(fixture.BridgeChallengerLog(), "DHCPDISCOVER") }
	discovers := challengerDiscovers()

	// The default 34 s lease_timeout outlives moby's 30 s for the call, so the daemon ends it and the plugin sees a
	// cancel; its log line carries the verdict, the error `docker start` returns is the daemon's own and cannot. A
	// 15 s lease_timeout ends first, so the plugin's own error carries it to the daemon (#1116).
	for _, tc := range []struct {
		name  string
		opts  map[string]string
		inErr bool
	}{
		{name: netName, opts: map[string]string{"dhcp_servers": harness.BridgeChallengerIP}},
		{name: netName + "-short", opts: map[string]string{"dhcp_servers": harness.BridgeChallengerIP, "lease_timeout": "15s"}, inErr: true},
	} {
		mark := harness.MarkPluginLog(t, ctx)
		harness.CreateNetwork(t, ctx, tc.name, "bridge", tc.opts)
		created := harness.AwaitPluginLogSince(t, ctx, mark, 10*time.Second, func(w string) bool {
			return strings.Contains(w, "level=warning") && strings.Contains(w, rule)
		})
		var warning string
		for _, line := range strings.Split(created, "\n") {
			if strings.Contains(line, "level=warning") && strings.Contains(line, rule) {
				warning = line
			}
		}
		if warning == "" {
			_, detail := harness.ForwardDropState()
			t.Fatalf("%s: no warning line names the bridge %s and %q in the plugin log written by the create (host: %s):\n%s", tc.name, harness.BridgeName, rule, detail, created)
		}
		for _, want := range []string{"bridge=" + harness.BridgeName, "is DROP", "docs/bridge-mode.md"} {
			if !strings.Contains(warning, want) {
				t.Errorf("%s: the create-time warning lacks %q: %s", tc.name, want, warning)
			}
		}

		ctrName := tc.name + "-ctr"
		create, err := cli.ContainerCreate(ctx,
			&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
			harness.HostConfig(),
			&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{tc.name: {}}},
			nil, ctrName)
		if err != nil {
			t.Fatalf("ContainerCreate(%s): %v", ctrName, err)
		}
		t.Cleanup(func() {
			_ = cli.ContainerRemove(context.Background(), create.ID, container.RemoveOptions{Force: true})
		})
		deadlineMark := harness.MarkPluginLog(t, ctx)
		startErr := cli.ContainerStart(ctx, create.ID, container.StartOptions{})
		if startErr == nil {
			t.Fatalf("%s: the container started although no frame can cross from its port to the challenger's; the drop did not take effect", tc.name)
		}
		if tc.inErr {
			for _, want := range []string{"context deadline exceeded", "is DROP", rule} {
				if !strings.Contains(startErr.Error(), want) {
					t.Errorf("%s: the error docker start returned lacks %q: %v", tc.name, want, startErr)
				}
			}
		}
		logged := harness.AwaitPluginLogSince(t, ctx, deadlineMark, 15*time.Second, func(w string) bool {
			return strings.Contains(w, "Error while processing request") && strings.Contains(w, rule)
		})
		if !strings.Contains(logged, "Error while processing request") || !strings.Contains(logged, rule) {
			t.Errorf("%s: the plugin's log line for the failed request does not carry the verdict:\n%s", tc.name, logged)
		}
	}
	// #1116: the challenger, behind a port in its own namespace, heard nothing while the rules were out.
	if got := challengerDiscovers() - discovers; got != 0 {
		t.Fatalf("the challenger heard %d DISCOVER although FORWARD drops bridged frames and its rules were removed; the firewall was not what failed the lease", got)
	}

	// #1116: with the rules back the same network leases from the challenger, and a healthy lease carries no verdict.
	restoreMark := harness.MarkPluginLog(t, ctx)
	harness.RestoreBridgeForward(t, harness.BridgeName)
	_, ipv4, mac := harness.RunContainer(t, ctx, netName, netName+"-ok")
	if ip := net.ParseIP(ipv4); ip == nil || !harness.IsInBridgeChallengerPool(ip) {
		t.Errorf("leased %q after the restore, want an address of the challenger's pool", ipv4)
	}
	if got := challengerDiscovers() - discovers; got < 1 {
		t.Errorf("the challenger logged no DISCOVER after the restore (%d); its log would not have shown the drop either", got)
	}
	if !ackedIn(fixture.BridgeChallengerLog(), ipv4, mac) {
		t.Errorf("no DHCPACK for %s/%s in the challenger's log after the restore", ipv4, mac)
	}
	if w := harness.ReadPluginLogSince(t, ctx, restoreMark); strings.Contains(w, rule) {
		t.Errorf("a lease on a healthy firewall logged the verdict:\n%s", w)
	}
}
