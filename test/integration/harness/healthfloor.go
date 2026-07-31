// This file deliberately carries NO `//go:build integration` tag,
// unlike the rest of the package. The floor decides whether a whole
// integration run passes, so its logic has to be testable without a
// live plugin — a floor that has never been observed rejecting
// anything is not known to work. Keeping the decision pure and
// untagged puts healthfloor_test.go in the ordinary `go test ./...`
// unit job. Everything that needs a socket stays in health.go.
//
// HealthResponse lives here rather than in health.go for the same
// reason: the floor takes one, so it has to compile untagged.

package harness

// HealthResponse mirrors pkg/plugin.HealthResponse. Duplicated here
// so the integration package doesn't pull on pkg/plugin internals.
type HealthResponse struct {
	Healthy           bool    `json:"healthy"`
	UptimeSeconds     float64 `json:"uptime_seconds"`
	ActiveEndpoints   int     `json:"active_endpoints"`
	PendingHints      int     `json:"pending_hints"`
	RecoveredOK       int32   `json:"recovered_ok"`
	RecoveryFailed    int32   `json:"recovery_failed"`
	JoinStartFailures int32   `json:"join_start_failures"`
	// JoinAbortedContainerGone is the benign twin of JoinStartFailures:
	// the container exited before the persistent client was up. Not
	// healthy-affecting (#373).
	JoinAbortedContainerGone int32 `json:"join_aborted_container_gone"`
	TombstoneWriteFailures   int32 `json:"tombstone_write_failures"`
	LeaseChanged             int32 `json:"lease_changed"`
	LeasesObtained           int32 `json:"leases_obtained"`
	LeasesRenewed            int32 `json:"leases_renewed"`
	DHCPTimeouts             int32 `json:"dhcp_timeouts"`
	LeaseReleaseFailures     int32 `json:"lease_release_failures"`
	NAKsReceived             int32 `json:"naks_received"`
	LedgerWriteFailures      int32 `json:"ledger_write_failures"`
}

// FloorFinding is one healthy-affecting counter that moved off zero
// during a run.
type FloorFinding struct {
	Counter string
	Value   int32
	// Fatal distinguishes "this counter only ever means a real plugin
	// fault" from "this counter is known to also count benign events".
	// A non-fatal finding is still printed — loudly — because it is a
	// signal, just not one we can hang a build on yet.
	Fatal bool
	// Why explains the verdict in the failure output. The reader is
	// someone staring at a red CI job, not someone with this file open.
	Why string
}

// CheckHealthFloor answers "is the plugin OK?" for a whole run, which
// is a different question from the per-test deltas in
// assertNoNewHealthFaults. Deltas catch "did this test break
// something"; the floor catches a fault that no individual test
// happened to bracket — including one left behind by the main suite
// before the failure suite even started.
//
// It returns findings for every healthy-affecting counter that is
// non-zero, and nothing at all for a clean run.
//
// The values are ABSOLUTE, not deltas from the start of the run, and
// that is the point: an absolute floor is what notices a fault that
// predates the first test. The cost is that running against a
// long-lived plugin (a local box where the plugin has been up across
// several sessions) can report a counter from an earlier run, so the
// findings say "since plugin start" out loud.
//
// One caveat that only shows up locally: a counter the running plugin
// does not publish decodes as zero and so reads as clean. An old build
// left installed on a dev box answers /Plugin.Health without
// join_start_failures or join_aborted_container_gone at all, which
// makes the floor quietly weaker there than in CI, where the plugin is
// always built from the branch under test. Nothing to fix in the floor
// — the fix is to reinstall the plugin — but worth knowing before
// trusting a local green.
//
// Note this is deliberately NOT `!h.Healthy`. Two of the three
// counters behind that flag mean exactly one thing; the third,
// recovery_failed, still double-counts a benign container-exit as a
// plugin fault (#376), so asserting the flag itself would be flaky by
// construction. Once #376 lands, the three cases below collapse into
// a single check of h.Healthy.
func CheckHealthFloor(h *HealthResponse) []FloorFinding {
	if h == nil {
		return nil
	}
	var out []FloorFinding

	if h.JoinStartFailures > 0 {
		out = append(out, FloorFinding{
			Counter: "join_start_failures",
			Value:   h.JoinStartFailures,
			Fatal:   true,
			Why:     "a running container was left without a renewal client; since #373 the benign container-exited case is counted separately as join_aborted_container_gone, so this counter now means only a real fault",
		})
	}
	if h.TombstoneWriteFailures > 0 {
		out = append(out, FloorFinding{
			Counter: "tombstone_write_failures",
			Value:   h.TombstoneWriteFailures,
			Fatal:   true,
			Why:     "the plugin could not persist its tombstone state to disk; an endpoint will not keep its address across a restart",
		})
	}
	if h.RecoveryFailed > 0 {
		out = append(out, FloorFinding{
			Counter: "recovery_failed",
			Value:   h.RecoveryFailed,
			Fatal:   false,
			Why:     "NOT failing the run: this counter still bumps when a container merely exited before post-restart recovery reached it (#376). Once that lands this becomes fatal and the floor becomes a plain healthy==true check. If you are here because something else is wrong, this counter is worth reading — it may be a real recovery failure",
		})
	}
	return out
}

// FloorFailed reports whether any finding is fatal. Split from
// CheckHealthFloor so callers print every finding and fail on a
// subset, rather than choosing between reporting and enforcing.
func FloorFailed(findings []FloorFinding) bool {
	for _, f := range findings {
		if f.Fatal {
			return true
		}
	}
	return false
}
