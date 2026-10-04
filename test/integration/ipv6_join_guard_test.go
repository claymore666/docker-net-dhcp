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

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// forcedRAInterval lands an advertisement between the engine's link-up and its default-route add on every start (#1145).
const forcedRAInterval = 20 * time.Millisecond

// Without Join's write a fresh sandbox's accept_ra=1 lets an advertisement install a route that fails the engine's add
// with EEXIST; the read of default/accept_ra is what only this change makes print 0 (#1145).

func sandboxDefault(t *testing.T, ctx context.Context, id, knob string) string {
	t.Helper()
	return strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/proc/sys/net/ipv6/conf/default/"+knob))
}

// assertJoinGuardHolds checks the container's sandbox defaults and that the kernel added no second default route.
func assertJoinGuardHolds(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	for _, knob := range []string{"accept_ra", "autoconf"} {
		if got := sandboxDefault(t, ctx, id, knob); got != "0" {
			t.Errorf("the sandbox's default/%s reads %q, want 0: Join did not write it before the engine moved the link "+
				"in, so an advertisement in that window can install a route the engine then collides with (#1145)", knob, got)
		}
	}
	time.Sleep(time.Second)
	out := harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show", "default")
	if n := harness.CountDefaultRoutes(out); n != 1 {
		t.Errorf("the container has %d IPv6 default routes, want exactly one, the plugin's. The kernel adds its own "+
			"from an advertisement while accept_ra is 1 (#821, #1145):\n%s", n, out)
	}
}

// startForcedWindow brings a slaac segment whose only router is the 20 ms harness sender.
func startForcedWindow(t *testing.T) *harness.V6Fixture {
	t.Helper()
	f := harness.NewV6FixtureWithArgs(t, harness.V6NoRA, harness.RangeArgsFor(harness.V6NoRA))
	dumpOnFailure(t, f)
	harness.StartRASenderEvery(t, f.Bridge(), slaacOn(advertPrefix(t, harness.AdvertPrefixA, true, 1800, 1800)), forcedRAInterval)
	return f
}

// TestJoinGuard_AStartInsideAForcedAdvertisementWindowKeepsTheGateway checks that a slaac container starts and keeps one default route under an advertisement every 20 ms (#1145).
func TestJoinGuard_AStartInsideAForcedAdvertisementWindowKeepsTheGateway(t *testing.T) {
	testJoinGuard_AStartInsideAForcedAdvertisementWindow(t, onV6Bridge)
}

func TestJoinGuard_AStartInsideAForcedAdvertisementWindowKeepsTheGateway_Macvlan(t *testing.T) {
	testJoinGuard_AStartInsideAForcedAdvertisementWindow(t, onV6Macvlan)
}

func testJoinGuard_AStartInsideAForcedAdvertisementWindow(t *testing.T, at v6Attach) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	f := startForcedWindow(t)
	w := harness.BeginCounterWindow(t, ctx, cli, "router_advert_guard_failures")

	id, err := startOnV6SegmentAs(t, ctx, cli, f, at, at.net("dh-itest-jguard"),
		map[string]string{"ipv6": "", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("the container did not start on a slaac segment advertising every %s: %v. An advertisement "+
			"between the engine's link-up and its default-route add makes that add fail with EEXIST unless Join "+
			"wrote accept_ra=0 into the sandbox first (#1145)", forcedRAInterval, err)
	}
	assertAttachedAs(t, ctx, f, id, at)
	assertJoinGuardHolds(t, ctx, id)

	before, after := w.End()
	if n := after.RouterAdvertGuardFailures - before.RouterAdvertGuardFailures; n != 0 {
		t.Errorf("router_advert_guard_failures moved by %d: a guard step did not take", n)
	}
}

// TestJoinGuard_ARestartedContainerIsGuardedAgain checks that a fresh sandbox after docker restart gets the defaults written again (#1145).
func TestJoinGuard_ARestartedContainerIsGuardedAgain(t *testing.T) {
	testJoinGuard_ARestartedContainerIsGuardedAgain(t, onV6Bridge)
}

func TestJoinGuard_ARestartedContainerIsGuardedAgain_Macvlan(t *testing.T) {
	testJoinGuard_ARestartedContainerIsGuardedAgain(t, onV6Macvlan)
}

func testJoinGuard_ARestartedContainerIsGuardedAgain(t *testing.T, at v6Attach) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	f := startForcedWindow(t)
	w := harness.BeginCounterWindow(t, ctx, cli, "router_advert_guard_failures")

	id, err := startOnV6SegmentAs(t, ctx, cli, f, at, at.net("dh-itest-jguardr"),
		map[string]string{"ipv6": "", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("the container did not start on a slaac segment advertising every %s: %v", forcedRAInterval, err)
	}
	assertJoinGuardHolds(t, ctx, id)
	first := sandboxKeyOf(t, ctx, id)

	if err := cli.ContainerRestart(ctx, id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v. The second start builds a new sandbox and runs Join again, which must "+
			"write the defaults again or the same collision is open on every restart (#1145)", err)
	}
	if second := sandboxKeyOf(t, ctx, id); second == first {
		t.Fatalf("the sandbox key %q is the same after the restart, so this run did not reach a second Join", first)
	}
	assertAttachedAs(t, ctx, f, id, at)
	assertJoinGuardHolds(t, ctx, id)

	before, after := w.End()
	if n := after.RouterAdvertGuardFailures - before.RouterAdvertGuardFailures; n != 0 {
		t.Errorf("router_advert_guard_failures moved by %d across the restart: a guard step did not take", n)
	}
}

func sandboxKeyOf(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	cli := dockerClientFor(t)
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	if ins.NetworkSettings == nil || ins.NetworkSettings.SandboxKey == "" {
		t.Fatalf("docker inspect reports no sandbox key for %s", id)
	}
	return ins.NetworkSettings.SandboxKey
}
