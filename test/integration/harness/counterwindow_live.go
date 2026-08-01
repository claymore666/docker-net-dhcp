//go:build integration

package harness

import (
	"context"
	"testing"

	docker "github.com/docker/docker/client"
)

// CounterWindow brackets a pair of /Plugin.Health reads and refuses to
// hand back a delta it cannot vouch for (#405).
//
// Before this existed, 29 measurement sites across 9 files took a
// `before` and an `after` by hand and subtracted them. The plugin's
// counters are in-memory and reset with the plugin process; three tests
// in this suite end that process on purpose. A pair straddling one of
// those reads as "no change" — or goes negative and reads as no change
// again — and nothing anywhere noticed. Four separate incidents were
// worked around individually before the shared cause was named.
//
// The type exists so the delta is unobtainable without the check having
// run, rather than the check being one more thing to remember at a
// thirtieth call site.
//
// Usage:
//
//	w := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")
//	... exercise ...
//	before, after := w.End()
//	if after.LeasesObtained-before.LeasesObtained != 1 { ... }
//
// The counter names are optional and only shape the failure text; they
// let the message say which numbers are void instead of leaving the
// reader to work it out.
type CounterWindow struct {
	t             *testing.T
	ctx           context.Context
	cli           *docker.Client
	before        *HealthResponse
	counters      []string
	expectRecycle bool
	ended         bool
}

// BeginCounterWindow reads the plugin's health and opens a window.
//
// A failure here is fatal rather than skipped: a test that cannot
// establish a baseline cannot make a claim about a delta, and carrying
// on would produce exactly the confident-but-baseless number this type
// exists to prevent.
func BeginCounterWindow(t *testing.T, ctx context.Context, cli *docker.Client, counters ...string) *CounterWindow {
	t.Helper()
	before, err := PluginHealth(ctx, cli)
	if err != nil {
		t.Fatalf("opening a counter window: %v\n"+
			"Without a baseline read there is nothing to compare against; failing "+
			"rather than measuring from an assumed zero.", err)
	}
	w := &CounterWindow{t: t, ctx: ctx, cli: cli, before: before, counters: counters}
	t.Cleanup(func() {
		// A window opened and never closed measured nothing, but looks
		// from the outside exactly like one that passed. Only complain
		// when the test was otherwise fine, so this never buries the
		// real failure.
		if !w.ended && !t.Failed() {
			t.Errorf("counter window opened at BeginCounterWindow was never closed with End() — "+
				"no reset check ran and no delta was verified (counters: %v)", w.counters)
		}
	})
	return w
}

// ExpectRecycle declares that this window is *meant* to span a plugin
// restart, and that End should fail if the plugin did not restart.
//
// This is deliberately an assertion, not an escape hatch. It cannot be
// used to silence the check: a window carrying it fails when the
// recycle does not happen, and still fails when identity cannot be
// established at all. The opt-out shape — something like
// SkipRecycleCheck — is exactly what CLAUDE.md's "never weaken a
// failing test" rule forbids, and what #413's gate should reject.
//
// The three tests that legitimately need it are the ones that end the
// plugin process on purpose: TestRecovery_PluginDisableEnable_PreservesEndpoint,
// TestIPv6_MACSurvivesPluginRecycle and TestRecovery_DaemonRestart_PreservesContainer.
// Counter deltas across such a window are void by construction; what
// those tests assert is that the endpoint survived, not that a number
// moved.
func (w *CounterWindow) ExpectRecycle() *CounterWindow {
	w.expectRecycle = true
	return w
}

// Before returns the opening read. Provided for assertions that need a
// value rather than a delta; it does not close the window.
func (w *CounterWindow) Before() *HealthResponse {
	return w.before
}

// End takes the closing read, verifies the plugin did not silently
// restart in between, and returns both reads for the caller to
// subtract.
//
// Every failure path is fatal. An unreadable closing read, an
// unexpected recycle, and an identity that cannot be established all
// mean the same thing: the number the test is about to compute does not
// describe what the test did.
func (w *CounterWindow) End() (before, after *HealthResponse) {
	w.t.Helper()
	w.ended = true

	after, err := PluginHealth(w.ctx, w.cli)
	if err != nil {
		w.t.Fatalf("closing a counter window: %v\n"+
			"The delta for %v cannot be computed, and an unreadable plugin is not "+
			"a passing measurement.", err, w.counters)
	}
	if msg := CounterWindowError(CompareInstances(w.before, after), w.expectRecycle, w.before, after, w.counters...); msg != "" {
		w.t.Fatal(msg)
	}
	return w.before, after
}
