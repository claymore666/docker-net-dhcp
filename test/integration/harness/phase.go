// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"testing"
	"time"
)

// PHASE lines are informational: no pass/fail and no budget; scripts/integration-timing.sh aggregates them (#368).

// PhaseLogPrefix is grepped by scripts/integration-timing.sh and pinned by scripts/test-integration-timing.sh (#368).
const PhaseLogPrefix = "PHASE"

const (
	PhaseNetworkCreate   = "network_create"
	PhaseNetworkRemove   = "network_remove"
	PhaseContainerCreate = "container_create"
	PhaseContainerStart  = "container_start"
	PhaseIPAcquisition   = "ip_acquisition"
	PhaseContainerStop   = "container_stop"
	PhaseContainerRemove = "container_remove"
)

// EndPhase logs one timing line, in milliseconds, for the span that began at start; `defer EndPhase(t, name, time.Now())` evaluates start at the defer.
func EndPhase(t *testing.T, name string, start time.Time) {
	t.Helper()
	t.Logf("%s %s %.3fs", PhaseLogPrefix, name, time.Since(start).Seconds())
}
