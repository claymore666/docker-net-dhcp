// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"sort"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// attachObservationBudget is the plugin's attach budget plus the daemon-busy grace, the longest an attach it still completes can take.
const attachObservationBudget = 30 * time.Second

// The population is the attaches of one shard, roughly a tenth of the run, so its p99 is close to the maximum. The
// test attaches a container of its own so the population is never empty by scheduling, and it asserts on the
// instrument, not the numbers: a p99 threshold would be today's value on a shared runner (#385), and a run with no
// attach line looks like a run where every attach was fast (#401, #403).

// TestJoinDuration_DistributionInThisShard logs the Join duration distribution of the attaches in this shard (#403).
func TestJoinDuration_DistributionInThisShard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli, "join_attach_completed")

	// Marked after the counter window's baseline read and read before it closes, so the log window sits inside the
	// counter window on both ends; the whole-log figures below span the plugin's life and cannot be cross-read (#417).
	mark := harness.MarkPluginLog(t, ctx)

	harness.CreateNetwork(t, ctx, "dhcptest-joindur", "macvlan", nil)
	_, ipv4, _ := harness.RunContainer(t, ctx, "dhcptest-joindur", "dhcptest-joindur-c1")
	harness.AssertIP(t, ipv4)

	// The attach is a goroutine the Join response does not wait for; the counter moved about 320 ms after RunContainer
	// returned on a hosted runner (#417).
	_, moved := w.Await(attachObservationBudget, func(now, before *harness.HealthResponse) bool {
		return now.JoinAttachCompleted > before.JoinAttachCompleted
	})
	if !moved {
		t.Fatalf("join_attach_completed did not move within %s of a container that HAS its "+
			"address (%s).\n"+
			"The attach happened, so this is the recording of it that stopped. Without the "+
			"counter a host running the shipped LOG_LEVEL=info carries no per-attach duration "+
			"at all except join_attach_slow, which is the tail (#403).", attachObservationBudget, ipv4)
	}
	// Awaited, not sampled: a `<=` over an empty window is satisfied by emptying it (#417).
	window := harness.AwaitPluginLogSince(t, ctx, mark, attachObservationBudget,
		func(w string) bool { return len(harness.AttachDurations(w)) > 0 })
	windowTook := harness.AttachDurations(window)

	// End also checks that the plugin did not restart under the reads above.
	before, after := w.End()
	counted := after.JoinAttachCompleted - before.JoinAttachCompleted

	if len(windowTook) == 0 {
		t.Errorf("no attach line reached the plugin log in this test's own window, though the "+
			"counter moved by %d for a container that HAS its address (%s).\n"+
			"The two records of #403 are written side by side from one elapsed value; a window "+
			"with the counter and without the line is the line being lost.", counted, ipv4)
	}
	// The counter is incremented before the line is written, so a line missing from the delta is a counter that missed
	// the attach or an increment that beat the baseline read by that gap; the red names both readings (#403, #417).
	if int32(len(windowTook)) > counted {
		t.Errorf("this test's window carries %d attach line(s) and the counter moved by %d "+
			"over a window that contains it, with no plugin restart inside either.\n"+
			"Either the counter missed an attach, or an attach incremented it in the moments "+
			"before the baseline read and wrote its line after the log mark. The first is a "+
			"defect in the only per-attach record a host running the shipped LOG_LEVEL has "+
			"(#403); the second is a gap of microseconds. Read them against the window_lines "+
			"and window_counted figures logged below.",
			len(windowTook), counted)
	}

	log := harness.ReadWholePluginLog(t, ctx)
	if log == "" {
		t.Fatal("the plugin log is empty, so the distribution below would be computed over nothing")
	}

	took := harness.AttachDurations(log)
	if len(took) == 0 {
		t.Fatalf("no successful attach in the plugin log carried a duration, though "+
			"join_attach_completed counted %d.\n"+
			"This test attached a container of its own, so zero lines means the per-attach "+
			"timing line stopped being written and not that no attach happened. Without it "+
			"the only per-attach duration a run carries is join_attach_slow, which counts the "+
			"attaches that outran the budget and says nothing about the rest (#403). "+
			"Log length: %d bytes.", after.JoinAttachCompleted, len(log))
	}

	// Counter and line are written side by side from one elapsed value, so fewer lines than counted is a lost line; fewer
	// counted than logged is normal after a restart mid-shard resets the counters (#403).
	if int32(len(took)) < after.JoinAttachCompleted {
		t.Errorf("the log carries %d attach durations but the plugin counted %d attaches. "+
			"The figures below are over a subset of the attaches that happened, which is a "+
			"population nobody chose (#403).", len(took), after.JoinAttachCompleted)
	}

	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	// instance and uptime say whether the whole-life lines and the per-process counters cover the same stretch (#417).
	t.Logf("JOIN-DURATION shard-local n=%d p50=%s p90=%s p99=%s max=%s counted=%d "+
		"instance=%s uptime=%.0fs window_lines=%d window_counted=%d "+
		"(budget: AWAIT_TIMEOUT as installed on this lane; the population is this shard, not the run; "+
		"n is over the plugin's whole life and counted is over the process named by instance)",
		len(took), harness.Percentile(took, 50), harness.Percentile(took, 90), harness.Percentile(took, 99), took[len(took)-1], after.JoinAttachCompleted,
		after.InstanceID, after.UptimeSeconds, len(windowTook), counted)
	t.Logf("JOIN-DURATION-BUCKETS shard-local under_1s=%d 1s_to_budget=%d slow=%d max_ms=%d "+
		"instance=%s uptime=%.0fs "+
		"(the same distribution as a host running the shipped LOG_LEVEL sees it, over the process "+
		"named by instance and not over the shard)",
		after.JoinAttachUnder1s, after.JoinAttach1sToBudget, after.JoinAttachSlow, after.JoinAttachMsMax,
		after.InstanceID, after.UptimeSeconds)

	// Start records its phases in a deferred exit that once ran only on failure; restoring that leaves the line and
	// `took` in place and empties only the phases (#406).
	if n := harness.AttachLinesWithoutPhases(log); n > 0 {
		t.Errorf("%d of %d attach lines carry no phase breakdown. Start is recording its "+
			"phases only for the attaches that FAILED again, so the run has durations with "+
			"nothing to attribute them to (#403, #406).", n, len(took)+n)
	}

	// A finished attach has a renewal client, so a long one is slow, not broken; an unparseable duration is the fault (#403).
	if bad := harness.MalformedAttachLines(log); bad > 0 {
		t.Errorf("%d attach lines carried a duration this test could not parse; the figures "+
			"above are over the %d it could, which is a population nobody chose", bad, len(took))
	}
}
