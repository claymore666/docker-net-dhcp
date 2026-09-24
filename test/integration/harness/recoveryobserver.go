// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"fmt"
	"strings"
	"time"
)

// Recovery counts an endpoint as rebuilt after the work (#376). recoverEndpoints returns before Listen binds the
// socket (pkg/plugin/plugin.go:NewPlugin, pkg/plugin/plugin.go:Listen) but only spawns each rebuild; the goroutine in
// pkg/plugin/plugin.go:recoverOneEndpoint runs dhcpManager.Start, which moves the sandbox route and rename counters
// first, and then increments recovered_ok (pkg/plugin/plugin.go:recoverOneEndpoint), a happens-after barrier for all
// of them. The failure arm takes longer: a Start that exhausts its context is classified with containerGone on a
// fresh context capped at recoveryPerNetworkTimeout, and only then moves recovery_failed
// (pkg/plugin/plugin.go:recoverOneEndpoint) or recovery_aborted_container_gone
// (pkg/plugin/plugin.go:recoverOneEndpoint). A budget ending at AWAIT_TIMEOUT would read recovery_failed == 0 before
// the counter could move.
const (
	// awaitTimeoutDefault is AWAIT_TIMEOUT as config.json ships it, the cap on each recovered Start (#376).
	awaitTimeoutDefault = 10 * time.Second
	// recoveryPerNetworkTimeoutDefault mirrors recoveryPerNetworkTimeout, which also caps the classifier (#376).
	recoveryPerNetworkTimeoutDefault = 3 * time.Second
	// recoveryBudgetDefault mirrors recoveryBudget in pkg/plugin.
	recoveryBudgetDefault = 30 * time.Second
	// recoveryDeferredDaemonWaitDefault mirrors recoveryDeferredDaemonWait in pkg/plugin.
	recoveryDeferredDaemonWaitDefault = 60 * time.Second

	// RecoveryRebuildBudget bounds the normal route: each Start and its classifier, plus one poll interval (#376).
	RecoveryRebuildBudget = awaitTimeoutDefault + recoveryPerNetworkTimeoutDefault + awaitPollInterval

	// RecoveryDeferredRebuildBudget bounds the deferred route (#383): recoverEndpointsDeferred waits up to
	// wait+recoveryBudget, then spawns the Starts on a fresh context, so their budget is added.
	RecoveryDeferredRebuildBudget = recoveryDeferredDaemonWaitDefault + recoveryBudgetDefault + RecoveryRebuildBudget
)

// RecoveryRoutes renders every counter that tells apart the readings of recovered_ok == 0 (#376).
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

// InstalledAwaitTimeoutDrift returns how the installed plugin's AWAIT_TIMEOUT disagrees with the budgets' value, or "".
// env is the plugin's Settings.Env, measured on an installed plugin 2026-09-17 as NAME=value entries (#376).
// AWAIT_TIMEOUT is settable, so `docker plugin set` can move the cap while config.json stays put; a missing or
// unparseable setting is drift. The lane builds the plugin from this checkout, so its manifest declares the setting.
func InstalledAwaitTimeoutDrift(env []string) string {
	const name = "AWAIT_TIMEOUT"
	for _, e := range env {
		// Match the whole name: EXTRA_AWAIT_TIMEOUT or AWAIT_TIMEOUT_MS is a different setting.
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
				"(pkg/plugin/plugin.go:recoverOneEndpoint), so the wait below is bounded by the wrong number: "+
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

// recoveryPoll is one bounded attempt at a condition: the last health read and whether the condition held.
type recoveryPoll func(budget time.Duration) (*HealthResponse, bool)

// awaitRecoveryRebuild polls within the normal route's budget, and extends to the deferred one only when the plugin
// reports it deferred (#383); it never fails the test itself.
func awaitRecoveryRebuild(logf func(string, ...any), what string, verify func(), poll recoveryPoll) (*HealthResponse, bool) {
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

	// Keep the first poll's read when the extension gets none, so the failure text still quotes the counters.
	extended, ok := poll(extra)
	if extended == nil {
		return h, ok
	}
	return extended, ok
}

// RecoveryRebuildFailure is the text a test prints when the rebuild it waited for never held.
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

// recoveryVerdict says whether the plugin classified a failure or the rebuild was still in flight (#376).
func recoveryVerdict(h *HealthResponse) string {
	switch {
	case h == nil:
		return "No counter could be read, so nothing above is evidence of anything."
	case h.RecoveryFailed > 0 || h.RecoveryAbortedContainerGone > 0:
		return fmt.Sprintf("Recovery recorded a failure against this endpoint: recovery_failed=%d, "+
			"recovery_aborted_container_gone=%d. The rebuild FAILED, it was not still running when "+
			"the budget ran out. The two counters want different next steps. "+
			"recovery_aborted_container_gone means a Start failed and the container was gone when "+
			"the plugin looked afterwards (pkg/plugin/plugin.go:recoverOneEndpoint); the container may well have "+
			"been there when recovery began. recovery_failed is every other recorded failure and "+
			"not only a failing Start: a Start that failed with the container still present "+
			"(pkg/plugin/plugin.go:recoverOneEndpoint), a walk-level failure before any Start reached this "+
			"endpoint (pkg/plugin/plugin.go:recoverEndpoints), and a deferred walk whose daemon never came "+
			"back (pkg/plugin/plugin.go:recoverEndpointsDeferred).",
			h.RecoveryFailed, h.RecoveryAbortedContainerGone)
	default:
		return "recovered_ok is incremented only after the recovered endpoint's client has " +
			"restarted (pkg/plugin/plugin.go:recoverOneEndpoint), and no failure arm moved either, so the " +
			"rebuild was still in flight when the budget ran out."
	}
}
