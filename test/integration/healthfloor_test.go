// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// healthFloorBudget is how long the floor waits for /Plugin.Health to
// answer at the end of a suite. Generous because the last test to run
// is not fixed: `go test` ordering puts whatever it likes last, and
// some tests recycle the plugin or the daemon, after which the socket
// takes a moment to come back.
const healthFloorBudget = 30 * time.Second

// checkHealthFloor asks the plugin whether anything went wrong during
// the run, and returns a non-zero exit code if something did.
//
// This is the complement to assertNoNewHealthFaults in
// failure_test.go, not a duplicate of it. Those are per-test DELTAS —
// "did this test break something". This is an absolute FLOOR — "is
// the plugin OK". A delta only catches a fault that happens to fall
// inside a test's own bracket; the floor catches one that no test
// bracketed, including a fault raised during fixture setup or between
// tests. #374 replaced four absolute `!h.Healthy` assertions with
// deltas, which was right, but left the suite with no floor at all.
//
// Two honest limits, stated here rather than discovered later:
//
//   - The counters reset when the plugin process does, and
//     TestRecovery_PluginDisableEnable_PreservesEndpoint,
//     TestIPv6_..._MACSurvivesPluginRecycle and the daemon-restart
//     test all recycle it. So this is "no fault since the last plugin
//     restart in this run", not "no fault in the whole run". The
//     failure suite has no such test, so there it does cover
//     everything. The per-test deltas cover what the reset erases.
//     Since #385 the verdict says which of the two it is instead of
//     printing an unqualified "clean" — see FloorCleanLine. Making the
//     floor actually span the whole run is the remaining half of #385.
//   - It asserts h.Healthy since #421, alongside the three counters
//     behind that flag. All three are now fatal — the benign paths that
//     used to be folded into recovery_failed are counted separately as
//     recovery_deferred (#383) and recovery_aborted_container_gone
//     (#376) — and the flag is checked as well as the table, so a
//     fourth healthy-affecting counter added to the plugin cannot slip
//     past this suite's mirror of it.
func checkHealthFloor(suite time.Duration) int {
	// TestMain's own ctx carries a 60s setup timeout and expired long
	// before m.Run() returned; the floor needs a fresh one.
	ctx, cancel := context.WithTimeout(context.Background(), healthFloorBudget+15*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Fprintln(os.Stderr, "HEALTH FLOOR: docker client:", err)
		return 1
	}
	defer cli.Close()

	var (
		h       *harness.HealthResponse
		lastErr error
	)
	deadline := time.Now().Add(healthFloorBudget)
	for {
		h, lastErr = harness.PluginHealth(ctx, cli)
		if lastErr == nil {
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		// Deliberately fatal rather than a skip. A floor that goes
		// quiet exactly when the plugin is unreachable is a floor
		// that reports green on the worst runs — the plugin having
		// died is itself the thing worth failing on. If a test leaves
		// the plugin disabled on purpose, it has to re-enable it,
		// which every such test already does in a cleanup.
		fmt.Fprintf(os.Stderr,
			"HEALTH FLOOR: /Plugin.Health did not answer within %v: %v\n", healthFloorBudget, lastErr)
		return 1
	}

	// Printed before the verdict, and on every run. The census reads the
	// whole log, so unlike the counters below it is not blinded by a
	// mid-suite plugin restart — see JoinFailureCensus.
	//
	// And now judged, not merely printed. Run 30699310641 is why: three
	// Joins failed, three containers were left without a renewal client,
	// and the run went green — because join_start_failures resets with
	// the plugin process and the floor below only sees the last ~12% of
	// a run. The census reads the whole log and had the number the whole
	// time. A measurement nobody fails on is a measurement that prevents
	// nothing (#385, #406).
	// Both censuses read the whole log, so unlike the counters below
	// they are not blinded by a mid-suite plugin restart. The Join half
	// already worked this way; extending it to the other two
	// healthy-affecting counters is the remaining half of #385. Their
	// increments each sit next to a distinct log line, and the log
	// spans the run while the counters span only the last restart —
	// 10% of one recent run.
	censusFailures, faultCount, probeFailuresInLog := printCensuses(ctx)

	// Printed before the verdict either way. The census answers "did
	// anything break"; this answers "did the #406 grace carry attaches
	// that would otherwise have broken", which a clean census cannot —
	// these failures are intermittent, so a zero can mean the fix
	// worked or that the condition never arose, and only this
	// distinguishes them.
	fmt.Fprint(os.Stderr, harness.AttachGraceLine(h, censusFailures))

	// Same question for the #524 detector: did it run at all? A green
	// run with address_conflicts=0 says nothing until this does.
	fmt.Fprint(os.Stderr, harness.ConflictProbeLine(h))

	findings := harness.CheckHealthFloor(h)

	// The census above printed whether the detector ran; this is what
	// acts on it (#551). Printing alone is what let every run between
	// #527 and #550 report "2 probe(s) could not run at all" and stay
	// green. Appended to the same findings list so it prints, counts and
	// fails through the existing path rather than a parallel one.
	findings = append(findings,
		harness.ConflictCensusFindings(h, harness.AllowedConflictProbeFailures(), probeFailuresInLog)...)
	if len(findings) == 0 && faultCount > 0 {
		fmt.Fprintf(os.Stderr,
			"HEALTH FLOOR: the counters came back clean, but the log records %d "+
				"healthy-affecting fault(s) across the run — see PLUGIN FAULTS above.\n"+
				"  The counters missed them because they reset when the plugin does.\n"+
				"  Failing on the log, which does not (#385).\n", faultCount)
		printFloorEvidence(ctx)
		return 1
	}
	if len(findings) == 0 {
		fmt.Fprint(os.Stderr, harness.FloorCleanLine(h, suite.Seconds()))
		if censusFailures > 0 {
			fmt.Fprintf(os.Stderr,
				"HEALTH FLOOR: the counters came back clean, but the log records %d Join "+
					"failure(s) across the run — see the census above.\n"+
					"  Each one is a running container left without a renewal client; its lease\n"+
					"  expires unrenewed. The counters missed them because they reset when the\n"+
					"  plugin does. Failing on the log, which does not (#385).\n", censusFailures)
			return 1
		}
		return 0
	}

	fmt.Fprintln(os.Stderr, "HEALTH FLOOR: the plugin's health surface did not come back clean.")
	fmt.Fprintf(os.Stderr, "  values are cumulative since the plugin started %.0fs ago, not deltas for this run;\n", h.UptimeSeconds)
	fmt.Fprintln(os.Stderr, "  against a long-lived local plugin a count may predate this run — re-check on a fresh enable.")
	for _, f := range findings {
		verdict := "WARN "
		if f.Fatal {
			verdict = "FATAL"
		}
		// An absent counter has no value to print, and printing its
		// zero is exactly the confusion this finding exists to end.
		if f.Absent {
			fmt.Fprintf(os.Stderr, "  %s %s=<not reported>: %s\n", verdict, f.Counter, f.Why)
			continue
		}
		if f.Flag {
			fmt.Fprintf(os.Stderr, "  %s %s=false: %s\n", verdict, f.Counter, f.Why)
			continue
		}
		fmt.Fprintf(os.Stderr, "  %s %s=%d: %s\n", verdict, f.Counter, f.Value, f.Why)
	}
	printFloorEvidence(ctx)
	if harness.FloorFailed(findings) {
		return 1
	}
	return 0
}

// floorEvidenceTailLines is how much trailing context the floor prints
// after the fault lines. Enough to show the run winding down around the
// last fault; short enough that the fault lines stay the thing you see.
const floorEvidenceTailLines = 80

// printFloorEvidence writes the plugin's own account of the run to
// stderr, next to the counters the floor just objected to.
//
// A counter is the symptom. The log lines behind it are the evidence,
// and on CI they live on an ephemeral runner that is destroyed with the
// job — so a floor failure that prints only a number is unactionable by
// the time anyone reads it (#385). Printed for warnings as well as
// fatal findings: a non-fatal finding is exactly the case where someone
// has to judge whether it matters, which needs the log.
func printFloorEvidence(ctx context.Context) {
	logPath, data, err := harness.PluginLog(ctx)
	if err != nil {
		// Not fatal on its own. The floor's verdict is decided by the
		// counters; missing evidence makes that verdict harder to act
		// on, it does not make it wrong.
		fmt.Fprintf(os.Stderr, "  (plugin log unavailable: %v)\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "  --- plugin log evidence (full log: %s) ---\n", logPath)
	fmt.Fprint(os.Stderr, harness.FloorEvidence(data, floorEvidenceTailLines))
}

// printJoinCensus reports how many Joins failed to start a persistent
// client across the whole run, grouped by cause.
//
// Silent when there were none, so it costs a healthy run nothing. When
// there were some, it is the only place that says so: the health floor's
// counters reset with the plugin process and the main suite recycles it
// three times, so a run can carry a dozen of these and still report a
// single-digit counter (#385). Sizing the Join budget for the host
// (#401) needs the real number, on every run, not the tail of it after
// something else has already gone red.
// Returns how many Join-start failures the log recorded, and how many
// other healthy-affecting faults it recorded. The floor fails on either.
//
// One read serves both censuses: the log is the single instrument that
// spans the whole run, and reading it twice would invite the two
// verdicts to disagree about which run they are describing.
func printCensuses(ctx context.Context) (joinFailures, otherFaults, probeFailuresInLog int) {
	_, data, err := harness.PluginLog(ctx)
	if err != nil {
		// A log we cannot read is reported as a fault rather than
		// passed over. It used to be quiet here because the census was
		// only a diagnostic; now that the run's verdict depends on it,
		// silence would mean an unreadable log reads as a clean one —
		// the failure mode this whole issue is about.
		fmt.Fprintf(os.Stderr,
			"HEALTH FLOOR: could not read the plugin log to count faults: %v\n"+
				"  Treating that as a fault: the log is the only instrument that spans the\n"+
				"  whole run, so without it this run has no verdict to give (#385).\n", err)
		// The 1 above already fails the run, so the 0 here cannot be
		// mistaken for "no probe failures" — nothing downstream gets to
		// treat this as a clean census.
		return 1, 0, 0
	}
	fmt.Fprint(os.Stderr, harness.JoinFailureCensus(data))
	faults, report := harness.FaultCensus(data)
	fmt.Fprint(os.Stderr, report)
	return harness.JoinFailureCount(data), faults, harness.ConflictProbeFailuresInLog(data)
}
