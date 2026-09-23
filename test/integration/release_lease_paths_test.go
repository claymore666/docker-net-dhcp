// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// endpointDeletedBudget covers the log write only: moby's handleContainerExit runs Cleanup, which deletes the
// endpoint, before SetStopped releases ContainerStop (moby daemon/monitor.go, v28.5.2), so the call has happened (#1016).
const endpointDeletedBudget = 10 * time.Second

func releasePathsClient(t *testing.T) *docker.Client {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func dumpReleasePathsOnFailure(t *testing.T) {
	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
}

func releaseLines(ip string) int {
	return fixture.CountLogLines("DHCPRELEASE", ip)
}

// awaitEndpointsDeleted is a precondition, not a verdict: an endpoint Docker has not deleted is JOINED, and neither
// the removal nor the restart below has anything held to hand back (#984).
func awaitEndpointsDeleted(t *testing.T, ctx context.Context, mark int64, endpoints ...string) {
	t.Helper()
	deleted := func(window, ep string) bool {
		for _, line := range strings.Split(window, "\n") {
			if strings.Contains(line, "Endpoint deleted") && strings.Contains(line, "endpoint="+ep) {
				return true
			}
		}
		return false
	}
	window := harness.AwaitPluginLogSince(t, ctx, mark, endpointDeletedBudget, func(w string) bool {
		for _, ep := range endpoints {
			if !deleted(w, ep) {
				return false
			}
		}
		return true
	})
	for _, ep := range endpoints {
		if !deleted(window, ep) {
			t.Fatalf("the plugin logged no DeleteEndpoint for endpoint %s after the stop returned, so "+
				"its record may still be JOINED and nothing below would be judging a held address", ep)
		}
	}
}

func stopContainer(t *testing.T, ctx context.Context, cli *docker.Client, id string) time.Time {
	t.Helper()
	if err := cli.ContainerStop(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop(%s): %v", id, err)
	}
	return time.Now()
}

// Recycle bounds, moby plugin/manager_linux.go v28.5.2 (#1016). Disable returns after shutdownPlugin: SIGTERM,
// 10s, SIGKILL, 10s. Enable returns once the socket dials, giving up after 500ms plus four 3s retries, and has set
// Enabled, so the readback needs no more. The health probe then gets three of its 5s requests. The enable
// Timeout is the per-call client timeout the engine gives the plugin, its own default (pkg/plugins/client.go).
const (
	pluginDisableBudget = 2 * 10 * time.Second
	pluginEnableBudget  = 500*time.Millisecond + 4*3*time.Second
	pluginHealthBudget  = 3 * 5 * time.Second
	pluginCallTimeout   = 30
)

// recyclePlugin disables the plugin, runs whileDown, and returns once the new process answers its socket.
func recyclePlugin(t *testing.T, ctx context.Context, cli *docker.Client, whileDown func()) {
	t.Helper()
	t.Cleanup(func() {
		bg, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: pluginCallTimeout}); err != nil &&
			!strings.Contains(err.Error(), "already enabled") {
			t.Logf("WARN: cleanup PluginEnable: %v", err)
		}
	})
	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, pluginDisableBudget); err != nil {
		t.Fatalf("plugin did not reach disabled state: %v", err)
	}
	if whileDown != nil {
		whileDown()
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: pluginCallTimeout}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, pluginEnableBudget); err != nil {
		t.Fatalf("plugin did not re-enable: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, pluginHealthBudget)
}

// awaitReleaseLine waits for dnsmasq to log a DHCPRELEASE naming ip beyond the before count, and returns when it saw it.
func awaitReleaseLine(ip string, before int, budget time.Duration) (time.Time, bool) {
	deadline := time.Now().Add(budget)
	for {
		if releaseLines(ip) > before {
			return time.Now(), true
		}
		if !time.Now().Before(deadline) {
			return time.Time{}, false
		}
		time.Sleep(releaseVisiblePoll)
	}
}

// TestReleaseLease_OnRemoveNetworkRemovalHandsTheHeldAddressBackAtOnce checks `docker network rm` against the server (#984, #1016).
func TestReleaseLease_OnRemoveNetworkRemovalHandsTheHeldAddressBackAtOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-onremove-netrm"
		ctrName = "dh-itest-onremove-netrm-ctr"
	)
	dumpReleasePathsOnFailure(t)
	netID := harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"release_lease": "on_remove"})
	cli := releasePathsClient(t)

	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)
	if !waitLeaseFile(t, fixture.LeaseFile(), ip, true) {
		t.Fatalf("dnsmasq's lease DB has no entry for %s, so nothing below can be read", ip)
	}
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	w := harness.BeginCounterWindow(t, ctx, cli, "releases_sent_v4", "release_failures_v4")
	releasesBefore := releaseLines(ip)
	mark := harness.MarkPluginLog(t, ctx)

	stopped := stopContainer(t, ctx, cli, id)
	awaitEndpointsDeleted(t, ctx, mark, ep)
	if err := cli.ContainerRemove(ctx, id, container.RemoveOptions{}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}

	if !leaseFileHolds(t, fixture.LeaseFile(), ip) {
		t.Fatalf("dnsmasq gave %s up after the stop and the container's removal, before the network "+
			"was removed. The removal below would have nothing left to hand back", ip)
	}
	if got := releaseLines(ip) - releasesBefore; got != 0 {
		t.Fatalf("dnsmasq logged %d DHCPRELEASE line(s) for %s before the network was removed; "+
			"release_lease=on_remove holds the address for the restart window", got, ip)
	}

	removed := time.Now()
	if err := cli.NetworkRemove(ctx, netID); err != nil {
		t.Fatalf("NetworkRemove: %v", err)
	}

	gone := waitLeaseFile(t, fixture.LeaseFile(), ip, false)
	sinceStop := time.Since(stopped)
	t.Logf("the lease DB lost %s %s after the network removal, %s after the stop",
		ip, time.Since(removed), sinceStop)
	if !gone {
		t.Errorf("dnsmasq still holds %s %s after `docker network rm`. The removal deletes the "+
			"options and tombstones the window needs, so it must hand the address back itself", ip, releaseVisibleBudget)
	}
	if gone && sinceStop >= onRemoveWindow {
		t.Errorf("the address went back %s after the stop, not inside the %s window: the "+
			"sweep can have sent it, so this run does not show the removal did", sinceStop, onRemoveWindow)
	}
	if got := releaseLines(ip) - releasesBefore; got != 1 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for %s across the network removal, want "+
			"exactly 1: one datagram per held address", got, ip)
	}

	before, after := w.End()
	if got := after.ReleasesSentV4 - before.ReleasesSentV4; got < 1 {
		t.Errorf("releases_sent_v4 moved by %d, want at least 1", got)
	}
	if got := after.ReleaseFailuresV4 - before.ReleaseFailuresV4; got != 0 {
		t.Errorf("release_failures_v4 moved by %d on a release that reached the server", got)
	}
}

// TestReleaseLease_OnRemoveNetworkRemovalHandsBackOnlyThatNetworksAddresses checks the removal spares a sibling on_remove network (#984, #1016).
func TestReleaseLease_OnRemoveNetworkRemovalHandsBackOnlyThatNetworksAddresses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	const (
		removedNet = "dh-itest-onremove-netrm-a"
		siblingNet = "dh-itest-onremove-netrm-b"
	)
	dumpReleasePathsOnFailure(t)
	opts := map[string]string{"release_lease": "on_remove"}
	removedID := harness.CreateNetwork(t, ctx, removedNet, "macvlan", opts)
	harness.CreateNetwork(t, ctx, siblingNet, "macvlan", opts)
	cli := releasePathsClient(t)

	type held struct {
		name, id, ip, ep string
		before           int
	}
	ctrs := []*held{
		{name: "dh-itest-onremove-netrm-a1"},
		{name: "dh-itest-onremove-netrm-a2"},
		{name: "dh-itest-onremove-netrm-b1"},
	}
	for i, c := range ctrs {
		netName := removedNet
		if i == 2 {
			netName = siblingNet
		}
		c.id, c.ip, _ = harness.RunContainer(t, ctx, netName, c.name)
		c.ep = harness.EndpointShortID(t, ctx, cli, c.id, netName)
		if !waitLeaseFile(t, fixture.LeaseFile(), c.ip, true) {
			t.Fatalf("dnsmasq's lease DB has no entry for %s (%s)", c.ip, c.name)
		}
		t.Logf("%s holds %s on %s", c.name, c.ip, netName)
	}
	a1, a2, b1 := ctrs[0], ctrs[1], ctrs[2]

	w := harness.BeginCounterWindow(t, ctx, cli,
		"releases_sent_v4", "release_failures_v4", "releases_reclaimed_v4")
	for _, c := range ctrs {
		c.before = releaseLines(c.ip)
	}
	mark := harness.MarkPluginLog(t, ctx)

	stopContainer(t, ctx, cli, a1.id)
	stopContainer(t, ctx, cli, a2.id)
	siblingStopped := stopContainer(t, ctx, cli, b1.id)
	awaitEndpointsDeleted(t, ctx, mark, a1.ep, a2.ep, b1.ep)

	for _, c := range ctrs {
		if !leaseFileHolds(t, fixture.LeaseFile(), c.ip) {
			t.Fatalf("dnsmasq gave %s (%s) up at the stop; release_lease=on_remove holds it for "+
				"the restart window, so the removal below has nothing to prove", c.ip, c.name)
		}
	}

	if err := cli.NetworkRemove(ctx, removedID); err != nil {
		t.Fatalf("NetworkRemove(%s): %v", removedNet, err)
	}
	for _, c := range []*held{a1, a2} {
		if !waitLeaseFile(t, fixture.LeaseFile(), c.ip, false) {
			t.Errorf("dnsmasq still holds %s (%s) %s after its network was removed", c.ip, c.name, releaseVisibleBudget)
		}
		if got := releaseLines(c.ip) - c.before; got != 1 {
			t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for %s (%s) across the removal, want exactly 1",
				got, c.ip, c.name)
		}
	}
	if since := time.Since(siblingStopped); since >= onRemoveWindow {
		t.Fatalf("the removed network's addresses were judged %s after the sibling's stop, past its %s "+
			"window, so the sibling's lease below cannot tell the removal from its own deadline", since, onRemoveWindow)
	}
	if !leaseFileHolds(t, fixture.LeaseFile(), b1.ip) {
		t.Errorf("dnsmasq gave up %s on the sibling network %s after the stop, when a different "+
			"network was removed. Its window is still open and its container may restart into it",
			b1.ip, time.Since(siblingStopped))
	}
	if got := releaseLines(b1.ip) - b1.before; got != 0 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for the sibling's %s when another network was removed", got, b1.ip)
	}

	seen, ok := awaitReleaseLine(b1.ip, b1.before, onRemoveVisibleBudget-time.Since(siblingStopped))
	if !ok {
		t.Errorf("dnsmasq logged no DHCPRELEASE for the sibling's %s %s after its stop. The removal "+
			"of another network closed its record without sending, and the address is left to expire",
			b1.ip, onRemoveVisibleBudget)
	} else if at := seen.Sub(siblingStopped); at < onRemoveWindow {
		t.Errorf("the sibling's %s went back %s after its stop, inside its %s window", b1.ip, at, onRemoveWindow)
	} else {
		t.Logf("the sibling's %s went back %s after its stop, at its own deadline", b1.ip, at)
	}
	for _, c := range []*held{a1, a2} {
		if got := releaseLines(c.ip) - c.before; got != 1 {
			t.Errorf("after the sibling's window dnsmasq has %d DHCPRELEASE line(s) for %s (%s), want "+
				"exactly the 1 the removal sent", got, c.ip, c.name)
		}
	}

	before, after := w.End()
	if got := after.ReleasesSentV4 - before.ReleasesSentV4; got < 3 {
		t.Errorf("releases_sent_v4 moved by %d, want at least 3", got)
	}
	if got := after.ReleaseFailuresV4 - before.ReleaseFailuresV4; got != 0 {
		t.Errorf("release_failures_v4 moved by %d", got)
	}
	if got := after.ReleasesReclaimedV4 - before.ReleasesReclaimedV4; got != 0 {
		t.Errorf("releases_reclaimed_v4 moved by %d with nothing restarting", got)
	}
}

// TestReleaseLease_OnRemoveReleasesAtTheWindowsEndAcrossAPluginRestart checks the deadline survives in the lease record (#984, #1016).
func TestReleaseLease_OnRemoveReleasesAtTheWindowsEndAcrossAPluginRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-onremove-recycle"
		ctrName = "dh-itest-onremove-recycle-ctr"
	)
	dumpReleasePathsOnFailure(t)
	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"release_lease": "on_remove"})
	cli := releasePathsClient(t)

	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)
	if !waitLeaseFile(t, fixture.LeaseFile(), ip, true) {
		t.Fatalf("dnsmasq's lease DB has no entry for %s, so nothing below can be read", ip)
	}
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)
	releasesBefore := releaseLines(ip)
	mark := harness.MarkPluginLog(t, ctx)

	stopped := stopContainer(t, ctx, cli, id)
	awaitEndpointsDeleted(t, ctx, mark, ep)

	recyclePlugin(t, ctx, cli, nil)
	back := time.Since(stopped)
	t.Logf("the plugin answered again %s after the stop", back)
	if back >= onRemoveWindow {
		t.Fatalf("the plugin came back %s after the stop, past the %s window, so a release now "+
			"cannot be told from one sent at start", back, onRemoveWindow)
	}
	if !leaseFileHolds(t, fixture.LeaseFile(), ip) {
		t.Fatalf("dnsmasq gave %s up before the restarted plugin's deadline; the window did not "+
			"survive to the new process to be tested", ip)
	}
	if got := releaseLines(ip) - releasesBefore; got != 0 {
		t.Fatalf("dnsmasq logged %d DHCPRELEASE line(s) for %s across the restart, inside the window", got, ip)
	}

	w := harness.BeginCounterWindow(t, ctx, cli, "releases_sent_v4", "release_failures_v4")

	_, gone := waitLeaseFileWithin(t, fixture.LeaseFile(), ip, false, onRemoveVisibleBudget-time.Since(stopped))
	elapsed := time.Since(stopped)
	if !gone {
		t.Errorf("dnsmasq still holds %s %s after the stop. The deadline is in the lease record, so "+
			"the process that restarted inside the window must send the release at its end", ip, elapsed)
	} else if elapsed < onRemoveWindow {
		t.Errorf("the address went back %s after the stop, inside the %s window", elapsed, onRemoveWindow)
	} else {
		t.Logf("the address went back %s after the stop", elapsed)
	}
	if got := releaseLines(ip) - releasesBefore; got != 1 {
		t.Errorf("dnsmasq logged %d DHCPRELEASE line(s) for %s, want exactly 1: the lease can leave "+
			"the DB by expiring", got, ip)
	}

	before, after := w.End()
	if got := after.ReleasesSentV4 - before.ReleasesSentV4; got < 1 {
		t.Errorf("the restarted plugin's releases_sent_v4 moved by %d, want at least 1", got)
	}
	if got := after.ReleaseFailuresV4 - before.ReleaseFailuresV4; got != 0 {
		t.Errorf("release_failures_v4 moved by %d on a release that reached the server", got)
	}
}

// The fixture's 2 minute lease floor (harness.LeaseTime) expires the address inside this wait, so the lease DB proves
// nothing here and the server's log is the observer. The wait is the plugin's longest release path, the on_remove
// window plus the settle and one sweep tick, measured from its return (docs/reference.md release_lease row, #1016).

// TestReleaseLease_OnStopLeavesTheAddressToExpireWhenThePluginMissedTheStop pins the reference's "does not cover" case (#962, #1016).
func TestReleaseLease_OnStopLeavesTheAddressToExpireWhenThePluginMissedTheStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-onstop-missed"
		ctrName = "dh-itest-onstop-missed-ctr"
	)
	dumpReleasePathsOnFailure(t)
	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"release_lease": "on_stop"})
	cli := releasePathsClient(t)

	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s holds ip=%s mac=%s", ctrName, ip, mac)
	if mac == "" || fixture.CountLogLines("DHCPACK", mac) < 1 {
		t.Fatalf("dnsmasq logged no DHCPACK for %q, so a zero DHCPRELEASE count below would say "+
			"nothing about the plugin", mac)
	}
	releasesBefore := releaseLines(ip)

	recyclePlugin(t, ctx, cli, func() {
		start := time.Now()
		stopContainer(t, ctx, cli, id)
		t.Logf("docker stop returned after %s with the plugin disabled", time.Since(start))
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect: %v", err)
		}
		if ins.State == nil || ins.State.Running {
			t.Fatalf("the container is still running after docker stop")
		}
		p, _, err := cli.PluginInspectWithRaw(ctx, harness.PluginRef)
		if err != nil {
			t.Fatalf("PluginInspect: %v", err)
		}
		if p.Enabled {
			t.Fatalf("the plugin was enabled when docker stop returned, so the stop may have reached it")
		}
	})
	back := time.Now()

	w := harness.BeginCounterWindow(t, ctx, cli, "releases_sent_v4", "release_failures_v4")

	if seen, ok := awaitReleaseLine(ip, releasesBefore, onRemoveVisibleBudget); ok {
		t.Errorf("dnsmasq logged a DHCPRELEASE for %s %s after the plugin came back. The reference "+
			"says a stop the plugin was not running for opens nothing and the address is left to "+
			"expire; a background pass that released it would hand back every address on the host "+
			"after a restart", ip, seen.Sub(back))
	}

	before, after := w.End()
	if got := after.ReleasesSentV4 - before.ReleasesSentV4; got != 0 {
		t.Errorf("releases_sent_v4 moved by %d after a stop the plugin never saw", got)
	}
	if got := after.ReleaseFailuresV4 - before.ReleaseFailuresV4; got != 0 {
		t.Errorf("release_failures_v4 moved by %d: the plugin attempted a release for a stop it never saw", got)
	}
}
