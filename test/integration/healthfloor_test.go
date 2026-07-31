//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
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
//   - It does not assert h.Healthy. See CheckHealthFloor — one of the
//     three counters behind that flag still counts a benign event as
//     a fault (#376).
func checkHealthFloor() int {
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

	findings := harness.CheckHealthFloor(h)
	if len(findings) == 0 {
		fmt.Fprintf(os.Stderr,
			"HEALTH FLOOR: clean — no healthy-affecting counter moved (uptime %.0fs, healthy=%v)\n",
			h.UptimeSeconds, h.Healthy)
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
		fmt.Fprintf(os.Stderr, "  %s %s=%d: %s\n", verdict, f.Counter, f.Value, f.Why)
	}
	if harness.FloorFailed(findings) {
		return 1
	}
	return 0
}
