// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The fixture installs FORWARD ACCEPT rules for its bridge (#103); this case removes them, so the host drops the
// bridged DHCP frames as an operator's fresh bridge would (#1116). The evidence is the plugin log, the error `docker
// start` returns, iptables -C, and dnsmasq's DISCOVER count, never a plugin counter. The rules are restored in
// t.Cleanup, and a container after the restore must lease.

// TestBridgeFirewallVerdict_NamedAtCreateAndAtTheDeadline checks that an existing bridge behind a dropping firewall is
// warned about at create and named in the error when the first lease attempt times out (#1116).
func TestBridgeFirewallVerdict_NamedAtCreateAndAtTheDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	// The fixture rules exist because the host drops bridged frames without them (#103), so a host that does not fails.
	if drops, detail := harness.ForwardDropState(); !drops {
		t.Fatalf("the host would not drop bridged frames once the fixture's rules are gone (%s), so the fixture's premise of #103 no longer holds", detail)
	}

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

	const (
		netName = "dh-itest-bridge-verdict"
		ctrName = netName + "-ctr"
		okName  = netName + "-ok"
	)
	rule := "iptables -A FORWARD -i " + harness.BridgeName + " -j ACCEPT"

	harness.WithoutBridgeForward(t, harness.BridgeName)
	mark := harness.MarkPluginLog(t, ctx)
	discovers := fixture.CountBridgeLogLines("DHCPDISCOVER")

	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{"lease_timeout": "15s"})

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
		t.Fatalf("no warning line names the bridge %s and %q in the plugin log written by the create:\n%s", harness.BridgeName, rule, created)
	}
	for _, want := range []string{"bridge=" + harness.BridgeName, "is DROP", "docs/bridge-mode.md"} {
		if !strings.Contains(warning, want) {
			t.Errorf("the create-time warning lacks %q: %s", want, warning)
		}
	}

	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		harness.HostConfig(),
		&network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{netName: {}}},
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
		t.Fatalf("the container started although no frame can reach the DHCP server; the fixture's rules were not out of the way")
	}
	for _, want := range []string{"context deadline exceeded", "is DROP", rule} {
		if !strings.Contains(startErr.Error(), want) {
			t.Errorf("the error docker start returned lacks %q: %v", want, startErr)
		}
	}
	logged := harness.AwaitPluginLogSince(t, ctx, deadlineMark, 10*time.Second, func(w string) bool {
		return strings.Contains(w, "Error while processing request") && strings.Contains(w, rule)
	})
	if !strings.Contains(logged, "Error while processing request") || !strings.Contains(logged, rule) {
		t.Errorf("the plugin's log line for the failed request does not carry the verdict:\n%s", logged)
	}
	// The absence that makes the run meaningful: dnsmasq heard nothing while the rules were out.
	if got := fixture.CountBridgeLogLines("DHCPDISCOVER") - discovers; got != 0 {
		t.Fatalf("dnsmasq heard %d DISCOVER although the FORWARD rules were removed; the firewall was not what failed the lease", got)
	}

	// The restore: with the rules back the same network leases, and a healthy lease carries no verdict.
	restoreMark := harness.MarkPluginLog(t, ctx)
	harness.RestoreBridgeForward(t, harness.BridgeName)
	_, ipv4, _ := harness.RunContainer(t, ctx, netName, okName)
	harness.AssertBridgeIP(t, ipv4)
	if got := fixture.CountBridgeLogLines("DHCPDISCOVER") - discovers; got < 1 {
		t.Errorf("dnsmasq logged no DISCOVER after the restore (%d); its log would not have shown the drop either", got)
	}
	if w := harness.ReadPluginLogSince(t, ctx, restoreMark); strings.Contains(w, rule) {
		t.Errorf("a lease on a healthy firewall logged the verdict:\n%s", w)
	}
}
