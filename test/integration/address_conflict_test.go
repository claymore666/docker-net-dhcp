// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// The executable check for #524: an endpoint leased an address that
// another device on the segment already holds must be REPORTED, not
// silently accepted.
//
// In production this cost an endpoint and was found only because the
// upgrade was verified against outside evidence — the plugin's own
// report said healthy:true with every counter at zero, because nothing
// looked. These tests are that "nothing looked" turned into something
// that goes red.
//
// The fixture server's log is deliberately NOT an assertion here, and
// that is not an oversight. From the server's point of view the lease
// was issued perfectly normally; it cannot see a statically-configured
// host that never asked it for anything. A server-side assertion would
// pass in both the broken and the fixed world.

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
)

// conflictProbeBudget is how long the assertions wait for the probe's
// verdict. The probe itself is capped at 2s and runs asynchronously
// after the lease, so this only has to outlast that plus the scheduling
// slack of a loaded runner.
const conflictProbeWait = 20 * time.Second

// squatAddr is the single address each fixture below is allowed to hand
// out, so the squatter can be parked on it before the container asks.
// Guessing which address a pool will yield is not sound — see
// harness.WithPool.
const squatAddr = "192.168.101.42"

// TestAddressConflict_SquattedAddressIsReported is the positive case.
// The server has exactly one address to give and somebody else already
// answers for it.
func TestAddressConflict_SquattedAddressIsReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const netName = "dh-itest-conflict"

	ef := harness.NewEphemeralFixture(t,
		harness.WithPool(squatAddr, squatAddr),
		// Without an address on the segment the probe falls back to a
		// link-local source, which this fixture's gateway-less namespace
		// never answers — see WithParentAddress.
		harness.WithParentAddress(harness.EphemeralParentAddr))
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// address_conflicts is a FATAL floor counter, and the floor is
	// absolute over the whole run — so the conflict this test induces on
	// purpose would fail the shard after the test itself passed. Recycle
	// the plugin afterwards so the deliberate fault does not outlive the
	// test that brackets it.
	//
	// This is the same disable/enable the recovery tests use, and it is
	// the reason the counter can stay fatal: nothing else in the suite
	// should ever move it, so the floor needs no notion of an expected
	// conflict.
	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer bgCancel()
		if err := cliReset(bg, t); err != nil {
			t.Logf("WARN: could not recycle the plugin after the deliberate conflict: %v\n"+
				"  address_conflicts stays non-zero and the health floor will fail this shard.", err)
		}
	})

	// Park the squatter BEFORE the container exists, so the address is
	// already taken at the moment the lease is granted — the ordering
	// of the production incident, where the other host had been sitting
	// on the address for as long as it had been racked.
	squatMAC := ef.Squat(squatAddr)
	t.Logf("squatter holds %s at %s", squatAddr, squatMAC)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli, "address_conflicts", "address_conflict_probes")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	_, ip, mac := harness.RunContainer(t, ctx, netName, "dh-itest-conflict-ctr")
	t.Logf("endpoint bound: ip=%s mac=%s", ip, mac)

	// The container must actually have been given the squatted address,
	// or the rest of this test is about nothing. WithPool leaves the
	// server no choice, and this asserts it rather than trusting it.
	// Docker reports NetworkSettings.IPAddress bare, so this compares
	// like with like.
	if ip != squatAddr {
		t.Fatalf("endpoint was leased %q, want the squatted %s — the pool was not pinned, so this run cannot test a conflict", ip, squatAddr)
	}
	if mac == squatMAC {
		t.Fatalf("endpoint MAC equals the squatter's (%s); the two devices are indistinguishable and a conflict could not be detected even in principle", mac)
	}

	after, ok := w.Await(conflictProbeWait, func(now, before *harness.HealthResponse) bool {
		return now.AddressConflicts > before.AddressConflicts
	})
	if !ok {
		t.Fatalf("address_conflicts never moved within %v.\n"+
			"A container is running on an address another device holds and the plugin "+
			"did not say so — this is exactly the #524 production fault, reproduced.\n"+
			"probes=%d failures=%d healthy=%v",
			conflictProbeWait, after.AddressConflictProbes, after.ConflictProbeFailures, after.Healthy)
	}

	// The probe must have reached a verdict, not merely failed loudly.
	if after.AddressConflictProbes == w.Before().AddressConflictProbes {
		t.Errorf("address_conflicts moved but address_conflict_probes did not; the counters disagree about whether a probe ran at all")
	}
	if after.Healthy {
		t.Error("healthy is still true with a conflict recorded; /Plugin.Health is the surface operators page on, and it is saying the endpoint is fine")
	}

	// A window that is opened and never closed measured nothing while
	// looking exactly like one that passed, so the harness fails the
	// test for it. Closed here rather than deferred so it runs before
	// the plugin recycle registered above.
	w.End()
}

// TestAddressConflict_CleanSegmentIsNotReported is the control, and it
// is not optional. Without it a counter that increments unconditionally
// passes the test above, and the suite would be asserting that the
// plugin can count rather than that it can detect.
func TestAddressConflict_CleanSegmentIsNotReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const netName = "dh-itest-noconflict"

	// Same fixture, same pinned pool, same parent address, no squatter.
	ef := harness.NewEphemeralFixture(t,
		harness.WithPool(squatAddr, squatAddr),
		harness.WithParentAddress(harness.EphemeralParentAddr))
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli, "address_conflicts", "address_conflict_probes")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	_, ip, _ := harness.RunContainer(t, ctx, netName, "dh-itest-noconflict-ctr")
	t.Logf("endpoint bound: ip=%s", ip)

	// Wait for a VERDICT, not for time to pass. Sleeping and then
	// asserting zero would pass just as well against a detector that
	// never ran, which is the failure this whole issue is about.
	after, ok := w.Await(conflictProbeWait, func(now, before *harness.HealthResponse) bool {
		return now.AddressConflictProbes > before.AddressConflictProbes
	})
	if !ok {
		t.Fatalf("no probe reached a verdict within %v, so this run says nothing about false positives.\n"+
			"conflicts=%d failures=%d", conflictProbeWait,
			after.AddressConflicts, after.ConflictProbeFailures)
	}
	if after.AddressConflicts > w.Before().AddressConflicts {
		t.Errorf("address_conflicts moved on a segment with no squatter — a false positive. "+
			"Every endpoint on this network would be reported broken. probes=%d",
			after.AddressConflictProbes)
	}
	if after.ConflictProbeFailures > w.Before().ConflictProbeFailures {
		t.Errorf("the probe could not run (conflict_probe_failures moved); the clean result above covers nothing")
	}
	w.End()
}

// TestAddressConflict_BridgeModeDoesNotSelfReport is the case that
// fails a naive implementation.
//
// In macvlan and ipvlan the parent cannot reach its own child, so any
// ARP reply is already somebody else's. In bridge mode the host CAN
// reach the container — our own endpoint answers the probe. A check
// that asked "did anything reply?" would report every single
// bridge-mode endpoint as a conflict, and the whole suite would go red
// for the fix rather than for the bug.
//
// This is deliberately a separate test from the clean control above
// rather than a table row: the two differ in the one property being
// tested — whether our own endpoint answers — and folding them together
// would let the macvlan case cover for the bridge case.
func TestAddressConflict_BridgeModeDoesNotSelfReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const netName = "dh-itest-conflict-bridge"

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	w := harness.BeginCounterWindow(t, ctx, cli, "address_conflicts", "address_conflict_probes")

	harness.CreateNetwork(t, ctx, netName, "bridge", nil)
	_, ip, mac := harness.RunContainer(t, ctx, netName, "dh-itest-conflict-br-ctr")
	t.Logf("bridge endpoint bound: ip=%s mac=%s", ip, mac)

	after, ok := w.Await(conflictProbeWait, func(now, before *harness.HealthResponse) bool {
		return now.AddressConflictProbes > before.AddressConflictProbes
	})
	if !ok {
		t.Fatalf("no probe reached a verdict within %v; this run cannot show whether bridge mode self-reports", conflictProbeWait)
	}
	if after.AddressConflicts > w.Before().AddressConflicts {
		t.Fatalf("bridge-mode endpoint reported itself as an address conflict.\n" +
			"The host can reach the container over a bridge, so our own endpoint answers the " +
			"probe — the MAC comparison in checkAddressConflict is what is supposed to tell " +
			"it apart from a squatter, and it did not.")
	}
	w.End()
}

// TestAddressConflict_BareParentIsUndetermined pins the honest half of
// the fix, and it is the test that would have caught #524's second life.
//
// A parent with no address on the leased subnet forces the probe onto a
// link-local source, and a host only answers an ARP request whose sender
// it can route back to. So against a gateway-less squatter the probe
// hears nothing — and hearing nothing is NOT the same as the address
// being free.
//
// The original implementation reported that silence as a clean segment.
// It passed its own positive test for months of development and failed
// the moment a real squatter was put on the wire, because the counter it
// moved (`address_conflict_probes`) was reporting that a check had
// happened while no check could have happened.
//
// So: same squatter, same everything, only the parent address removed.
// The probe must come back as UNDETERMINED — a probe failure — and must
// not claim a verdict either way.
func TestAddressConflict_BareParentIsUndetermined(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const netName = "dh-itest-conflict-bare"

	ef := harness.NewEphemeralFixture(t,
		harness.WithPool(squatAddr, squatAddr),
		harness.WithBareParent())

	// This test exists to make a probe fail, so it declares that one
	// conflict_probe_failures increment is expected and the shard's
	// census gate judges only the excess (#551). Declared HERE, next to
	// the WithBareParent that causes it, because this is the only place
	// that knows why the probe cannot run — a number kept anywhere else
	// would outlive its reason.
	harness.AllowConflictProbeFailures(1)

	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	squatMAC := ef.Squat(squatAddr)
	t.Logf("squatter holds %s at %s (parent deliberately bare)", squatAddr, squatMAC)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli,
		"address_conflicts", "address_conflict_probes", "conflict_probe_failures")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	_, ip, _ := harness.RunContainer(t, ctx, netName, "dh-itest-conflict-bare-ctr")
	t.Logf("endpoint bound: ip=%s", ip)

	after, ok := w.Await(conflictProbeWait, func(now, before *harness.HealthResponse) bool {
		return now.ConflictProbeFailures > before.ConflictProbeFailures
	})
	if !ok {
		t.Fatalf("conflict_probe_failures never moved within %v.\n"+
			"The probe could not have detected this squatter — it had no address on the segment "+
			"to send from — so it owes an explicit 'undetermined'. Silence here is the #524 fault "+
			"in its subtler form.\n"+
			"conflicts=%d probes=%d healthy=%v",
			conflictProbeWait, after.AddressConflicts, after.AddressConflictProbes, after.Healthy)
	}

	// The decisive assertion: no verdict was reached. A probe counted
	// here would mean the plugin believes it checked something.
	if after.AddressConflictProbes > w.Before().AddressConflictProbes {
		t.Errorf("address_conflict_probes moved on a probe that could not reach the squatter; " +
			"the plugin is reporting a check it did not perform")
	}
	if after.AddressConflicts > w.Before().AddressConflicts {
		t.Errorf("address_conflicts moved without a reply to base it on")
	}
	// An unanswered question must not latch the plugin unhealthy — that
	// is what separates it from a known-bad address.
	if !after.Healthy {
		t.Error("healthy = false on an undetermined probe alone; an unasked question is not a known-broken address")
	}
	w.End()
}

// cliReset recycles the plugin process, which is the only way to clear
// a counter — they are process-local by design. Used by the conflict
// test to retire the fault it induced on purpose.
func cliReset(ctx context.Context, t *testing.T) error {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()

	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		return err
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 15*time.Second); err != nil {
		return err
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		if !strings.Contains(err.Error(), "already enabled") {
			return err
		}
	}
	return harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second)
}
