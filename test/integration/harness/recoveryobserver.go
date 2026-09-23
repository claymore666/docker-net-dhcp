// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// This file deliberately carries NO `//go:build integration` tag, for
// the same reason counterwindow.go does not: what it holds is the
// decision of how long a recovered endpoint may take to be counted and
// what a test says when it never is, and a decision that has never been
// observed going either way is not known to work. The polling that
// needs a live socket is in recoveryobserver_live.go.

package harness

import (
	"fmt"
	"strings"
	"time"
)

// Recovery counts an endpoint as rebuilt after the work (#376). recoverEndpoints returns before Listen binds the
// socket (pkg/plugin/plugin.go:1416-1418, 1457) but only spawns each rebuild; the goroutine at
// pkg/plugin/plugin.go:1240 runs dhcpManager.Start, which moves the sandbox route and rename counters first,
// and then increments recovered_ok (pkg/plugin/plugin.go:1265), a happens-after barrier for all of them.
// The failure arm takes longer: a Start that exhausts its context is classified with containerGone on a fresh
// context capped at recoveryPerNetworkTimeout, and only then moves recovery_failed (pkg/plugin/plugin.go:1258)
// or recovery_aborted_container_gone (pkg/plugin/plugin.go:1252). A budget ending at AWAIT_TIMEOUT would read
// recovery_failed == 0 before the counter could move.
const (
	// awaitTimeoutDefault is AWAIT_TIMEOUT as config.json ships it, the cap on each recovered Start (#376).
	awaitTimeoutDefault = 10 * time.Second
	// recoveryPerNetworkTimeoutDefault mirrors recoveryPerNetworkTimeout, which also caps the classifier (#376).
	recoveryPerNetworkTimeoutDefault = 3 * time.Second
	// recoveryBudgetDefault mirrors recoveryBudget in pkg/plugin.
	recoveryBudgetDefault = 30 * time.Second
	// recoveryDeferredDaemonWaitDefault mirrors recoveryDeferredDaemonWait in pkg/plugin.
	recoveryDeferredDaemonWaitDefault = 60 * time.Second

	// RecoveryRebuildBudget bounds the normal route: the daemon answered,
	// the walk ran inside NewPlugin, and the only thing left when the
	// socket starts serving is each endpoint's Start, plus the classifier
	// that runs if it failed. One poll interval on top, because a poll
	// loop takes its last sample just under its deadline and the
	// increment may land in that gap.
	RecoveryRebuildBudget = awaitTimeoutDefault + recoveryPerNetworkTimeoutDefault + awaitPollInterval

	// RecoveryDeferredRebuildBudget bounds the deferred route (#383): recoverEndpointsDeferred waits up to
	// wait+recoveryBudget, then spawns the Starts on a fresh context, so their budget is added.
	RecoveryDeferredRebuildBudget = recoveryDeferredDaemonWaitDefault + recoveryBudgetDefault + RecoveryRebuildBudget
)

// RecoveryRoutes renders the counters that separate the ways a recovery
// property can fail to hold.
//
// WHY EVERY ONE OF THEM. recovered_ok reading 0 has several readings and
// the failure text used to carry none of them (#376's own test printed
// recovered_ok alone). recovery_deferred says the walk had not run yet
// when the read was taken; recovery_already_managed says a Join claimed
// the endpoint first; recovery_network_gone says the network went away
// under the walk; recovery_failed and recovery_aborted_container_gone
// are the two arms of the Start failure classifier. Absent all five, 0
// means the rebuild was still in flight — which is the reading that has
// no evidence at all unless the other five are printed beside it.
//
// tombstones_consumed is here because it is recovered_ok's counterpart
// on a daemon restart: the address can be carried by either path.
func RecoveryRoutes(h *HealthResponse) string {
	if h == nil {
		return "no health read succeeded, so no counter can be quoted"
	}
	parts := []string{
		fmt.Sprintf("recovered_ok=%d", h.RecoveredOK),
		fmt.Sprintf("recovery_failed=%d", h.RecoveryFailed),
		fmt.Sprintf("recovery_aborted_container_gone=%d", h.RecoveryAbortedContainerGone),
		fmt.Sprintf("recovery_deferred=%d", h.RecoveryDeferred),
		fmt.Sprintf("recovery_already_managed=%d", h.RecoveryAlreadyManaged),
		fmt.Sprintf("recovery_network_gone=%d", h.RecoveryNetworkGone),
		fmt.Sprintf("tombstones_consumed=%d", h.TombstonesConsumed),
		fmt.Sprintf("instance=%s", short(h.InstanceID)),
		fmt.Sprintf("uptime=%.1fs", h.UptimeSeconds),
	}
	return strings.Join(parts, " ")
}

// InstalledAwaitTimeoutDrift compares the AWAIT_TIMEOUT the installed
// plugin is actually running with against the value both recovery
// budgets are derived from, and returns the text of the disagreement,
// or "" when there is none. env is the plugin's Settings.Env, whose
// entries are NAME=value (MEASURED against an installed plugin
// 2026-09-17: ["LOG_LEVEL=trace" "AWAIT_TIMEOUT=10s" "STATE_DIR=..."]).
//
// WHY THE INSTALLED VALUE AND NOT ONLY THE MANIFEST.
// TestRecoveryBudgetsTrackTheProduct reads config.json, which is what
// the plugin is BUILT from. AWAIT_TIMEOUT is declared settable there,
// so `docker plugin set <ref> AWAIT_TIMEOUT=30s` is a legal line, and
// the three integration workflows already run one `docker plugin set`
// each. A lane that adds that one word moves the product's cap while
// the manifest, and every budget derived from it, stays where it is:
// the flake this branch is about, back, with a wait in front of it and
// a coupling test still green. The manifest half is a copy nobody
// checks; this is the half the copy cannot see.
//
// A setting that is missing or unparseable is drift too: the value the
// plugin runs with is then unknown, and an unknown cap is not evidence
// that the bound is right.
//
// WHY THAT ARM DOES NOT RED A CORRECT LANE, stated properly because the
// first version of this comment gave a false reason. Settings.Env is the
// INSTALLED plugin's own manifest, not this tree's: installed plugins on
// a development box report different numbers of entries from each other
// and from the manifest here, because each was built from the manifest
// of its own version. So "config.json declares it" says nothing about
// what a given installed plugin publishes. What does say something is
// that the integration lane creates the plugin from this checkout
// (`docker plugin create "$REF" plugin`, .github/workflows/integration.yml:772),
// so the plugin under test carries this tree's manifest and this
// tree's manifest declares the setting. Reached anyway, the arm is
// still right: a plugin whose manifest lacks the setting is not the one
// this tree builds, and its cap is unknown here.
func InstalledAwaitTimeoutDrift(env []string) string {
	const name = "AWAIT_TIMEOUT"
	for _, e := range env {
		// Whole name, both ends. A setting whose name merely contains
		// this one is a different setting, and reading either
		// EXTRA_AWAIT_TIMEOUT or AWAIT_TIMEOUT_MS as this one would let
		// a plugin that never published AWAIT_TIMEOUT pass the check.
		k, v, ok := strings.Cut(e, "=")
		if !ok || k != name {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Sprintf("the installed plugin runs with %s=%q, which is not a duration. "+
				"The recovery budgets are bounded by %s and there is no way to tell whether that "+
				"still covers one Start", name, v, awaitTimeoutDefault)
		}
		if d != awaitTimeoutDefault {
			return fmt.Sprintf("the installed plugin runs with %s=%s and the recovery budgets are "+
				"derived from %s. Each recovery Start is capped at the installed value "+
				"(pkg/plugin/plugin.go:1240), so the wait below is bounded by the wrong number: "+
				"either it gives up on a rebuild the plugin was still allowed to finish, or it "+
				"waits past the point where one could still be running. config.json is the "+
				"manifest the plugin is built from and `docker plugin set` overrides it, which is "+
				"why the mirrored constant alone does not answer this",
				name, d, awaitTimeoutDefault)
		}
		return ""
	}
	return fmt.Sprintf("the installed plugin publishes no %s setting, so the %s the recovery "+
		"budgets are derived from cannot be confirmed against the plugin under test. Settings.Env "+
		"carries the installed plugin's own manifest, and the lane creates the plugin from this "+
		"checkout, whose manifest declares the setting, so the plugin answering here is not the "+
		"one this tree builds", name, awaitTimeoutDefault)
}

// recoveryPoll is one bounded attempt at a condition: the last health
// read it managed and whether the condition ever held.
type recoveryPoll func(budget time.Duration) (*HealthResponse, bool)

// awaitRecoveryRebuild runs poll against the normal route's budget and,
// only if the plugin says it took the deferred route, against the rest
// of the deferred one.
//
// THE BOUND IS PER-ROUTE ON PURPOSE. A flat budget covering both is
// either the short one — which reds on a benign deferral, the case
// recovery_deferred exists to call benign — or the long one, which lets
// a rebuild that is genuinely wedged on the normal route hold the suite
// for a minute and a half and reports nothing extra for it. Asking the
// plugin which route it took costs one field of a read that has already
// been taken.
//
// It never fails the test itself. The caller's own assertion is the
// failure, because a helper that fataled here would turn the property
// into "the helper returned", and the caller is the only place that
// knows which property it was waiting for.
func awaitRecoveryRebuild(logf func(string, ...any), what string, verify func(), poll recoveryPoll) (*HealthResponse, bool) {
	// Before the first poll, never beside it: a bound checked after the
	// wait has already given its answer is not a check. Taking it as a
	// parameter is what makes the order observable here, where it can be
	// driven; a call the two live waits made for themselves could be
	// deferred, or moved below the poll, with nothing to see it.
	verify()
	h, ok := poll(RecoveryRebuildBudget)
	if ok {
		return h, true
	}
	if h == nil || h.RecoveryDeferred < 1 {
		return h, false
	}
	extra := RecoveryDeferredRebuildBudget - RecoveryRebuildBudget
	logf("%s did not hold within %s, and the plugin reports recovery_deferred=%d: the walk was "+
		"handed to the post-Listen retry, so the budget for this property is the deferred route's "+
		"%s and not the %s the normal route gets. Waiting a further %s. Counters so far: %s",
		what, RecoveryRebuildBudget, h.RecoveryDeferred,
		RecoveryDeferredRebuildBudget, RecoveryRebuildBudget, extra, RecoveryRoutes(h))

	// The first poll's read is kept when the extension never gets one of
	// its own. A plugin that stops answering during the extension would
	// otherwise return no read at all, and the failure text derives the
	// budget it quotes from the read — so it would report a 10s wait for
	// one that took a hundred, and quote no counters for a plugin that
	// had published recovery_deferred.
	extended, ok := poll(extra)
	if extended == nil {
		return h, ok
	}
	return extended, ok
}

// RecoveryRebuildFailure is the text a test prints when the property it
// waited for never held. Shared so the three recycle sites cannot drift
// into describing the same event differently, and so none of them can
// report "recovery did not pick up our endpoint" about a plugin whose
// counters say it was deferred or displaced.
//
// The headline number says where it comes from. A wait can only be
// bounded by the value this tree declares, and the plugin that answered
// may have been set to another one; that disagreement is a recorded
// failure by the time this prints, and a reader who takes the first
// line at face value would go looking for a rebuild that overran a cap
// nothing was running under. Naming the derivation costs one line and
// keeps the loudest sentence in the job from asserting something the
// same job has contradicted.
//
// The budget is read back off the health document instead of being
// passed in: the caller would otherwise have to know which of the two routes
// its own wait took, and a number typed at the call site is the one
// place this could claim a wait it did not perform.
func RecoveryRebuildFailure(what string, h *HealthResponse) string {
	waited := RecoveryRebuildBudget
	if h != nil && h.RecoveryDeferred >= 1 {
		waited = RecoveryDeferredRebuildBudget
	}
	return fmt.Sprintf("waited %s for %s and it never held.\n"+
		"  That budget is derived from AWAIT_TIMEOUT as this tree's manifest declares it (%s), "+
		"not from the plugin that answered here. If the two disagree, the drift check this wait "+
		"runs before spending anything has already said so above, and this line is the shorter "+
		"of the two.\n"+
		"  %s\n"+
		"  %s",
		waited, what, awaitTimeoutDefault, RecoveryRoutes(h), recoveryVerdict(h))
}

// recoveryVerdict reads the counters back and says which of the two
// things happened, because they want opposite next steps and the numbers
// alone do not say which is which to a reader.
//
// A classified failure is the product reporting a fault. "Still in
// flight" is this budget being too short, or a rebuild wedged somewhere
// that records nothing. Printing the second sentence over the first is
// the mistake this whole change was made to stop, one level up.
func recoveryVerdict(h *HealthResponse) string {
	switch {
	case h == nil:
		return "No counter could be read, so nothing above is evidence of anything."
	case h.RecoveryFailed > 0 || h.RecoveryAbortedContainerGone > 0:
		return fmt.Sprintf("Recovery recorded a failure against this endpoint: recovery_failed=%d, "+
			"recovery_aborted_container_gone=%d. The rebuild FAILED, it was not still running when "+
			"the budget ran out. The two counters want different next steps. "+
			"recovery_aborted_container_gone means a Start failed and the container was gone when "+
			"the plugin looked afterwards (pkg/plugin/plugin.go:1252); the container may well have "+
			"been there when recovery began. recovery_failed is every other recorded failure and "+
			"not only a failing Start: a Start that failed with the container still present "+
			"(pkg/plugin/plugin.go:1258), a walk-level failure before any Start reached this "+
			"endpoint (pkg/plugin/plugin.go:1000), and a deferred walk whose daemon never came "+
			"back (pkg/plugin/plugin.go:1106).",
			h.RecoveryFailed, h.RecoveryAbortedContainerGone)
	default:
		return "recovered_ok is incremented only after the recovered endpoint's client has " +
			"restarted (pkg/plugin/plugin.go:1265), and no failure arm moved either, so the " +
			"rebuild was still in flight when the budget ran out."
	}
}
