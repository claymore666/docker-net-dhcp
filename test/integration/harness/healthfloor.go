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

import "encoding/json"

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

	// published is the key set of the payload this value was decoded
	// from. It exists because an absent JSON field decodes to zero,
	// which is indistinguishable from a counter that is genuinely at
	// zero — so without it the floor reads "clean" for counters the
	// plugin never sent (#377).
	//
	// nil means "this value was built by hand, not decoded", and the
	// presence check is skipped. UnmarshalJSON always sets a non-nil
	// map, including for an empty or null payload, so nil cannot occur
	// on the path that talks to a real plugin.
	published map[string]json.RawMessage
}

// UnmarshalJSON decodes as usual and additionally records which keys
// the payload actually carried.
//
// The `plain` alias is what stops this from recursing: an alias type
// has the same fields but not the methods, so the inner Unmarshal uses
// the default struct decoder. published is unexported and therefore
// invisible to encoding/json, which is also why nothing outside this
// package can fabricate a misleading key set.
func (h *HealthResponse) UnmarshalJSON(b []byte) error {
	type plain HealthResponse
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	if keys == nil {
		// A literal `null` body unmarshals into a nil map without
		// erroring. Normalise it: a payload that carried no keys is
		// "published nothing", not "presence unknown".
		keys = map[string]json.RawMessage{}
	}
	*h = HealthResponse(p)
	h.published = keys
	return nil
}

// floorCounter is one counter the floor reads.
//
// The table below is the single source of truth for that set: both the
// value check and the presence check iterate it, so a counter cannot be
// added to one and forgotten in the other. name must equal the JSON tag
// on the field read — TestFloorCounterNamesMatchJSONTags pins that, and
// the presence check catches the other half of the same drift, where
// the plugin renames a key this side has not followed.
type floorCounter struct {
	name  string
	read  func(*HealthResponse) int32
	fatal bool
	why   string
}

var floorCounters = []floorCounter{
	{
		name:  "join_start_failures",
		read:  func(h *HealthResponse) int32 { return h.JoinStartFailures },
		fatal: true,
		why:   "a running container was left without a renewal client; since #373 the benign container-exited case is counted separately as join_aborted_container_gone, so this counter now means only a real fault",
	},
	{
		name:  "tombstone_write_failures",
		read:  func(h *HealthResponse) int32 { return h.TombstoneWriteFailures },
		fatal: true,
		why:   "the plugin could not persist its tombstone state to disk; an endpoint will not keep its address across a restart",
	},
	{
		name:  "recovery_failed",
		read:  func(h *HealthResponse) int32 { return h.RecoveryFailed },
		fatal: false,
		why:   "NOT failing the run: this counter still bumps when a container merely exited before post-restart recovery reached it (#376). Once that lands this becomes fatal and the floor becomes a plain healthy==true check. If you are here because something else is wrong, this counter is worth reading — it may be a real recovery failure",
	},
}

// absentWhy explains a finding raised because the plugin did not
// publish a counter at all. Fatal regardless of the counter's own
// verdict — recovery_failed being merely noisy is a statement about
// what its value means, not a licence to stop looking at it.
const absentWhy = "the plugin did not publish this counter, so this run proves nothing about it — an absent JSON field decodes as zero and would otherwise read as clean. Either the plugin under test is an older build than the suite (rebuild and reinstall it), or the counter was renamed in pkg/plugin/endpoints.go without updating floorCounters in this file"

// FloorFinding is one healthy-affecting counter the floor took issue
// with — either it moved off zero, or the plugin did not report it.
type FloorFinding struct {
	Counter string
	Value   int32
	// Absent marks a counter the plugin never published. Value is
	// meaningless for these — the point is that there was no value.
	Absent bool
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
// non-zero or unreported, and nothing at all for a clean run. Each
// counter yields at most one finding.
//
// The values are ABSOLUTE, not deltas from the start of the run, and
// that is the point: an absolute floor is what notices a fault that
// predates the first test. The cost is that running against a
// long-lived plugin (a local box where the plugin has been up across
// several sessions) can report a counter from an earlier run, so the
// findings say "since plugin start" out loud.
//
// A counter the plugin does not publish is itself a fatal finding.
// That case is not hypothetical: an old build left installed on a dev
// box answers /Plugin.Health without join_start_failures at all, and
// before #377 the floor read the resulting zero as clean — weaker
// locally than in CI while looking identical. The same silence would
// follow a renamed JSON tag in CI, where the plugin is always built
// from the branch under test, so this is not a dev-box-only guard.
//
// Note this is deliberately NOT `!h.Healthy`. Two of the three
// counters behind that flag mean exactly one thing; the third,
// recovery_failed, still double-counts a benign container-exit as a
// plugin fault (#376), so asserting the flag itself would be flaky by
// construction. Once #376 lands, the table above collapses into a
// single check of h.Healthy.
func CheckHealthFloor(h *HealthResponse) []FloorFinding {
	if h == nil {
		return nil
	}
	var out []FloorFinding
	for _, c := range floorCounters {
		if h.published != nil {
			if _, ok := h.published[c.name]; !ok {
				out = append(out, FloorFinding{
					Counter: c.name,
					Absent:  true,
					Fatal:   true,
					Why:     absentWhy,
				})
				continue
			}
		}
		if v := c.read(h); v > 0 {
			out = append(out, FloorFinding{
				Counter: c.name,
				Value:   v,
				Fatal:   c.fatal,
				Why:     c.why,
			})
		}
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
