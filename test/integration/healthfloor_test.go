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

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// healthFloorBudget is how long the floor waits for /Plugin.Health at the end of a suite, after tests that may recycle the plugin or the daemon.
const healthFloorBudget = 30 * time.Second

// checkHealthFloor is the absolute floor beside the per-test deltas of assertNoNewHealthFaults, which #374 left the
// suite without. The counters reset when the plugin does, so it covers the stretch since the last restart and says so
// (FloorCleanLine, #385); it asserts h.Healthy and every counter in floorCounters (#421), with the benign recovery
// paths counted apart (#376, #383). Package-level because the floor runs after m.Run() returns; zero means no
// baseline and judges the plugin's whole life (#584).

// floorHealthBaseline and floorLogBaseline are the plugin's counters and log length when TestMain started.
var (
	floorHealthBaseline *harness.HealthResponse
	floorLogBaseline    int64
)

func checkHealthFloor(suite time.Duration) int {
	// TestMain's own ctx carries a 60s setup timeout that has expired by now.
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
		// Fatal, not a skip: an unreachable plugin is itself worth failing on, and tests that disable it re-enable it in cleanup.
		fmt.Fprintf(os.Stderr,
			"HEALTH FLOOR: /Plugin.Health did not answer within %v: %v\n", healthFloorBudget, lastErr)
		return 1
	}

	// The censuses read the whole log, so a mid-suite plugin restart does not blind them: run 30699310641 went green with
	// three failed Joins because join_start_failures reset with the plugin and the floor saw the last ~12% of the run
	// (#385, #406).
	censusFailures, faultCount, conflictsInLog := printCensuses(ctx)

	// Whether the #406 grace carried attaches that would otherwise have failed, which a clean census cannot say.
	fmt.Fprint(os.Stderr, harness.AttachGraceLine(h, censusFailures))

	// Whether the #524 check ran at all.
	fmt.Fprint(os.Stderr, harness.ACDCensusLine(h))

	findings := harness.CheckHealthFloor(h)

	// Every run between #527 and #550 printed "2 probe(s) could not run at all" and stayed green (#551).
	findings = append(findings,
		harness.ACDCensusFindings(h, harness.AllowedARPSendFailures(), harness.AllowedUnprobedLeases(),
			harness.AllowedStagedConflicts(),
			conflictsInLog, floorHealthBaseline)...)
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
		// An absent counter has no value to print, and a printed zero is the confusion this finding ends.
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

// floorEvidenceTailLines is how much trailing plugin log the floor prints after the fault lines.
const floorEvidenceTailLines = 80

// CI runners are destroyed with the job, so a floor failure without the log lines is unactionable (#385).

// printFloorEvidence writes the plugin's log to stderr beside the counters the floor objected to.
func printFloorEvidence(ctx context.Context) {
	logPath, data, err := harness.PluginLog(ctx)
	if err != nil {
		// The counters decide the verdict; missing evidence does not make it wrong.
		fmt.Fprintf(os.Stderr, "  (plugin log unavailable: %v)\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "  --- plugin log evidence (full log: %s) ---\n", logPath)
	fmt.Fprint(os.Stderr, harness.FloorEvidence(data, floorEvidenceTailLines))
}

// The main suite recycles the plugin three times, so a run can carry a dozen Join-start failures and report a
// single-digit counter; sizing the Join budget needs the real number (#385, #401). One read serves both censuses.

// printCensuses reports the whole run's Join-start failures and other healthy-affecting faults from the plugin log.
func printCensuses(ctx context.Context) (joinFailures, otherFaults, probeFailuresInLog int) {
	_, data, err := harness.PluginLog(ctx)
	if err != nil {
		// Now that the verdict depends on the census, an unreadable log must not read as a clean one (#385).
		fmt.Fprintf(os.Stderr,
			"HEALTH FLOOR: could not read the plugin log to count faults: %v\n"+
				"  Treating that as a fault: the log is the only instrument that spans the\n"+
				"  whole run, so without it this run has no verdict to give (#385).\n", err)
		return 1, 0, 0
	}
	fmt.Fprint(os.Stderr, harness.JoinFailureCensus(data))
	faults, report := harness.FaultCensus(data)
	fmt.Fprint(os.Stderr, report)
	// The join and fault censuses stay whole-log (#385); the ACD census is scoped because it is judged against
	// allowances a test process declares and cannot carry across an exec.
	return harness.JoinFailureCount(data), faults,
		harness.ConflictsInLog(harness.LogSince(data, floorLogBaseline))
}
