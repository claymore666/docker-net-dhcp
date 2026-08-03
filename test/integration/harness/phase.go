// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"testing"
	"time"
)

// Per-phase wall-clock instrumentation (#368).
//
// #276 gave us one number per test, which was enough to find the
// `docker stop` grace (#367) and the dnsmasq lease floor (#356). It is
// not enough for what is left: after #367 removed the 10s teardown tax,
// roughly 9 seconds per main-suite test remain unattributed — about
// seven minutes of the job that nobody can currently point at.
//
// The suspects are all *budgets* that should return early
// (preflightProbeBudget 8s, dhcpClientReapTimeout 5s,
// dhcpClientFinishTimeout 5s, dnsPropagateTimeout 2s). Whether they
// actually do under test conditions is exactly the unmeasured thing,
// and #279 is the standing reminder that intuition about where time
// goes is wrong often enough to be worth measuring instead.
//
// This is deliberately informational: it owns no pass/fail, adds no
// budget of its own, and an absent PHASE line means "not instrumented",
// never "failed". scripts/integration-timing.sh aggregates the lines
// into a phase table beside the existing per-test one.
//
// An explicitly acceptable outcome is that the phases turn out to be
// real DHCP round-trips with nothing to reclaim. That closes the
// question, which is worth as much as a win.

// PhaseLogPrefix marks a machine-readable timing line. Kept as a
// constant because scripts/integration-timing.sh greps for it and
// scripts/test-integration-timing.sh pins the format — changing the
// literal here without changing both is how the aggregation silently
// starts reporting an empty table.
const PhaseLogPrefix = "PHASE"

// Phase names. Constants rather than string literals at the call sites
// so a typo cannot silently create a second bucket that looks like a
// real phase in the summary but only ever holds one sample.
const (
	PhaseNetworkCreate   = "network_create"
	PhaseNetworkRemove   = "network_remove"
	PhaseContainerCreate = "container_create"
	PhaseContainerStart  = "container_start"
	PhaseIPAcquisition   = "ip_acquisition"
	PhaseContainerStop   = "container_stop"
	PhaseContainerRemove = "container_remove"
)

// EndPhase emits one timing line for the span that began at start.
//
// The intended shape at a call site is
//
//	defer harness.EndPhase(t, harness.PhaseNetworkCreate, time.Now())
//
// which reads oddly the first time: the arguments — including
// time.Now() — are evaluated when the `defer` statement runs, and only
// the call itself is deferred. So `start` is the moment control reached
// the defer, which is what we want, and no closure is needed.
//
// For spans that are not function-scoped, take a `t0 := time.Now()` and
// call EndPhase directly.
//
// The duration is emitted with millisecond resolution: the phases being
// measured are seconds-scale, and a fixed 3 decimals keeps the grep
// pattern in the aggregator simple.
func EndPhase(t *testing.T, name string, start time.Time) {
	t.Helper()
	t.Logf("%s %s %.3fs", PhaseLogPrefix, name, time.Since(start).Seconds())
}
