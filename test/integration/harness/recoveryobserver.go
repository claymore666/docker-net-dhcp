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

// Recovery counts an endpoint as rebuilt AFTER the work, not before it.
//
// recoverEndpoints runs inside NewPlugin and returns before Listen binds
// the socket (pkg/plugin/plugin.go:3186-3188, 3249), so a test that can
// read /Plugin.Health knows the walk finished. The walk only SPAWNS each
// endpoint's rebuild, though: the goroutine at pkg/plugin/plugin.go:2907
// calls dhcpManager.Start and increments recovered_ok at
// pkg/plugin/plugin.go:2944 once it returns. Everything Start does moves
// its counters before that — the sandbox route (pkg/plugin/dhcp_manager.go:2599)
// and the host-side rename (pkg/plugin/host_ifname.go:217) — which makes
// recovered_ok the last thing to move and therefore a happens-after
// barrier for all of them.
//
// A test that reads the counters when the socket first answers is
// reading the middle of that work. These budgets say how long the work
// may take, on each of the two routes into it.
const (
	// awaitTimeoutDefault is AWAIT_TIMEOUT as config.json ships it. The
	// per-endpoint Start runs on a context capped at it
	// (pkg/plugin/plugin.go:2908), so a rebuild that has not been
	// counted by then never will be: its context is dead.
	awaitTimeoutDefault = 10 * time.Second
	// recoveryBudgetDefault is recoveryBudget (pkg/plugin/plugin.go:117).
	recoveryBudgetDefault = 30 * time.Second
	// recoveryDeferredDaemonWaitDefault is recoveryDeferredDaemonWait
	// (pkg/plugin/plugin.go:139).
	recoveryDeferredDaemonWaitDefault = 60 * time.Second

	// RecoveryRebuildBudget bounds the normal route: the daemon answered,
	// the walk ran inside NewPlugin, and the only thing left when the
	// socket starts serving is each endpoint's Start. One poll interval
	// on top, because a poll loop takes its last sample just under its
	// deadline and the increment may land in that gap.
	RecoveryRebuildBudget = awaitTimeoutDefault + awaitPollInterval

	// RecoveryDeferredRebuildBudget bounds the other route. When the
	// daemon was not serving yet the walk is handed to
	// recoverEndpointsDeferred (pkg/plugin/plugin.go:3246), which runs
	// on wait+recoveryBudget (pkg/plugin/plugin.go:2639) and only then
	// spawns the Starts — whose own context is a fresh Background, so it
	// is added, not absorbed.
	RecoveryDeferredRebuildBudget = recoveryDeferredDaemonWaitDefault + recoveryBudgetDefault + awaitTimeoutDefault + awaitPollInterval
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
func awaitRecoveryRebuild(logf func(string, ...any), what string, poll recoveryPoll) (*HealthResponse, bool) {
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
	return poll(extra)
}

// RecoveryRebuildFailure is the text a test prints when the property it
// waited for never held. Shared so the three recycle sites cannot drift
// into describing the same event differently, and so none of them can
// report "recovery did not pick up our endpoint" about a plugin whose
// counters say it was deferred or displaced.
//
// The budget is read back off the health document rather than passed
// in: the caller would otherwise have to know which of the two routes
// its own wait took, and a number typed at the call site is the one
// place this could claim a wait it did not perform.
func RecoveryRebuildFailure(what string, h *HealthResponse) string {
	waited := RecoveryRebuildBudget
	if h != nil && h.RecoveryDeferred >= 1 {
		waited = RecoveryDeferredRebuildBudget
	}
	return fmt.Sprintf("waited %s for %s and it never held.\n"+
		"  %s\n"+
		"  recovered_ok is incremented only after the recovered endpoint's client has restarted "+
		"(pkg/plugin/plugin.go:2944), so 0 here with every other counter at 0 means the rebuild was "+
		"still in flight when the budget ran out — a different fault from one the classifier counted.",
		waited, what, RecoveryRoutes(h))
}
