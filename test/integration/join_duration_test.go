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

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// attachObservationBudget bounds the wait for the attach counter to
// move. The attach runs in a goroutine the Join response does not wait
// for, so it is observable only after the container is up; the budget
// is the plugin's own attach budget plus the daemon-busy grace, which
// is the longest an attach it will still complete can take.
const attachObservationBudget = 30 * time.Second

// TestJoinDuration_DistributionInThisShard is #403's first step: how
// long a Join actually takes, against the 10s AWAIT_TIMEOUT that caps
// it.
//
// WHY A PASS OVER THE LOG AND NOT A COUNTER. join_attach_slow counts
// the attaches that outran the budget. That is the tail, and #401 is
// the record of what arguing about a budget from the tail costs: the
// first attempt raised the timeout on the assumption of slowness and
// measured no improvement, because nine of the twelve failures were
// something else. A distribution needs the attaches that FINISHED in
// time as well, and since #403 the plugin logs one line per successful
// attach with the same phase names the failure line carries.
//
// WHAT THE POPULATION IS, and the name says it because the first
// version's did not. The suite is partitioned into shards, each its own
// job with its own daemon, plugin and log, so this reads the attaches
// of the tests that ran before it IN ONE SHARD — roughly a tenth of the
// run. A p99 over that n is the maximum under another name, and the
// handover reports it as a shard figure.
//
// IT ATTACHES A CONTAINER OF ITS OWN, which is not decoration. Nothing
// holds this test's position inside its shard: a rebalance, or a new
// test costed ahead of it, can leave it first, and then a distribution
// over zero samples fails for a scheduling reason with the instrument
// working perfectly. Its own attach makes the population non-empty by
// construction, so a zero here is the instrument and nothing else.
//
// IT ASSERTS, and the assertions are about the instrument rather than
// the numbers. A p99 threshold here would be a threshold set to today's
// value on a shared runner, which is the floor #385 already paid for.
// What must not silently become true is that the instrument stopped
// producing lines: a run with no attach line looks exactly like a run
// where every attach was fast.
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

	// Marked BEFORE the window opens, so the log window and the counter
	// window cover the same stretch of time. The whole-log figures
	// further down cannot be cross-read against a counter: the log
	// spans the plugin's whole life and the counter spans the current
	// process, and at this PR's first head the two printed 7 and 1 in
	// one shard with nothing published to say which of the two readings
	// applied (#417 review r1).
	mark := harness.MarkPluginLog(t, ctx)

	w := harness.BeginCounterWindow(t, ctx, cli, "join_attach_completed")

	harness.CreateNetwork(t, ctx, "dhcptest-joindur", "macvlan", nil)
	_, ipv4, _ := harness.RunContainer(t, ctx, "dhcptest-joindur", "dhcptest-joindur-c1")
	harness.AssertIP(t, ipv4)

	// The counter moves AFTER RunContainer returns: MEASURED at about
	// 320 ms on a hosted runner, because the attach is a goroutine the
	// Join response does not wait for. A single read here would be a
	// read taken too early on a fast host, so wait for the attach the
	// container proves happened, bounded by the budget it runs under.
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
	// End closes the window, which is also the check that the plugin
	// did not restart under the reading above.
	before, after := w.End()

	// THE TWO RECORDS OVER ONE WINDOW. Both sides are now bounded by
	// the same stretch, and End has already established that the plugin
	// did not restart inside it, so a reset cannot explain a
	// disagreement here.
	//
	// The direction is decided by the order the plugin writes them in:
	// noteAttachDuration increments the counter and the timing line is
	// written after it, so at any instant the counter leads the log by
	// at most the attach in flight. A window carrying MORE lines than
	// the counter counted is therefore a counter that missed attaches,
	// not a sampling race.
	//
	// Awaited rather than sampled for the same reason: the positive
	// assertion below would otherwise judge whatever had reached the
	// file, and a `<=` over an empty window is satisfied by emptying
	// it.
	window := harness.AwaitPluginLogSince(t, ctx, mark, attachObservationBudget,
		func(w string) bool { return len(harness.AttachDurations(w)) > 0 })
	windowTook := harness.AttachDurations(window)
	counted := after.JoinAttachCompleted - before.JoinAttachCompleted
	if len(windowTook) == 0 {
		t.Errorf("no attach line reached the plugin log in this test's own window, though the "+
			"counter moved by %d for a container that HAS its address (%s).\n"+
			"The two records of #403 are written side by side from one elapsed value; a window "+
			"with the counter and without the line is the line being lost.", counted, ipv4)
	}
	if int32(len(windowTook)) > counted {
		t.Errorf("this test's window carries %d attach line(s) and the counter moved by %d. "+
			"The plugin did not restart inside the window, and it increments the counter before "+
			"it writes the line, so this is the counter missing attaches. Every figure a host "+
			"running the shipped LOG_LEVEL reads for #403 comes from that counter.",
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

	// THE TWO RECORDS, cross-read. The counter and the line are written
	// side by side from the same elapsed value, so a log with fewer
	// lines than the plugin counted is the line being lost: a level
	// change, a rotated file, a filter. Fewer counted than logged is
	// normal and not asserted — a plugin restarted mid-shard resets its
	// counters while the log keeps the older lines.
	if int32(len(took)) < after.JoinAttachCompleted {
		t.Errorf("the log carries %d attach durations but the plugin counted %d attaches. "+
			"The figures below are over a subset of the attaches that happened, which is a "+
			"population nobody chose (#403).", len(took), after.JoinAttachCompleted)
	}

	sort.Slice(took, func(i, j int) bool { return took[i] < took[j] })
	// instance and uptime travel with the figures. The lines below are
	// over the plugin's whole life and the counters are over the
	// current process, so a reader comparing them needs to know whether
	// those are the same stretch. Without it a recycle and a counter
	// that missed attaches look alike, which is what a seven-to-one
	// disagreement looked like at this PR's first head.
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

	// THE PHASES, and this is the assertion that holds the success-side
	// recording rather than the log line. Start records its phase
	// summary in a deferred exit that used to run only when Start
	// failed. Restoring that condition leaves this line in place and
	// leaves `took` on it, because the elapsed time is measured by the
	// caller; only the phases go empty. A line without them dates the
	// attach and says nothing about where its time went, which is the
	// half #406 added and #403 needs.
	if n := harness.AttachLinesWithoutPhases(log); n > 0 {
		t.Errorf("%d of %d attach lines carry no phase breakdown. Start is recording its "+
			"phases only for the attaches that FAILED again, so the run has durations with "+
			"nothing to attribute them to (#403, #406).", n, len(took)+n)
	}

	// The one number that is a fault rather than a measurement: an
	// attach that finished is an attach whose container has a renewal
	// client, so a long one is slow and not broken. A DURATION THAT
	// CANNOT BE PARSED is the instrument lying, and is worth failing on
	// because every figure above would then be computed over a subset
	// nobody declared.
	if bad := harness.MalformedAttachLines(log); bad > 0 {
		t.Errorf("%d attach lines carried a duration this test could not parse; the figures "+
			"above are over the %d it could, which is a population nobody chose", bad, len(took))
	}
}
