// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// A production host's DHCP server once stayed silent to renewals for 7h52m while health and every counter read
// unchanged; an unanswered renewal produces no lease event, so dhcp_timeouts moves only when the lease lapses (#940).
// The lease here is 600s and never lapses, so dhcp_timeouts must stay flat while the new counter moves. The server is
// not running, so the evidence is a capture of renewal requests on the fixture's veth end, read after health; N
// requests on the wire prove at most N-1 unanswered ones, since the last one may still be answered.

// TestFailure_UnansweredRenewalsCounted checks that renewals a silent server leaves unanswered are counted against the wire (#940).
func TestFailure_UnansweredRenewalsCounted(t *testing.T) {
	const (
		// T1 at 12s, as TestLeaseRenew_HonorsT1 uses, scheduled from the server's option 58.
		renewT1 = 12
		// RFC 2131 section 4.4.5 retransmits after half the time remaining until T2, down to 60 seconds
		// (proto.RenewRetransmitFloor); half of 120 is that floor, so the retransmission at T1+60 is still RENEWING, with no
		// state change and no lease event (#940).
		renewT2 = renewT1 + 120
		// A lapse would move dhcp_timeouts and measure TestFailure_ServerLossDuringRenewal's scenario (#940).
		leaseSeconds = 600

		// wireBudget is T1 plus the 60s retransmit floor, plus room for a loaded runner.
		wireBudget = 120 * time.Second
		// The chassis folds the library's counters on a ticker at a quarter of the retransmit floor, so the lag is at most 15s (#940).
		healthBudget = 45 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const (
		netName = "dh-itest-unans"
		ctrName = "dh-itest-unans-ctr"
	)

	ef := harness.NewEphemeralFixture(t,
		harness.WithLeaseSeconds(leaseSeconds),
		harness.WithRenewTimes(renewT1, renewT2))

	// Opened before the container starts, so a missing renewal is told apart from a capture that never saw the client.
	wire := ef.StartDHCPCapture(t)

	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			wire.Dump(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("bound: ip=%s mac=%s", ip, mac)

	// Only the persistent client renews and reports, never the CreateEndpoint one-shot.
	awaitBoundPersistentClient(t, bindW)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	// Since #961 a sandbox-key attach starts its client with no name and renews at once to carry the daemon's name (RFC
	// 2131 section 4.4.5), about two milliseconds after the bind and answered, so every baseline is taken at the kill.
	beforeKill := len(wire.RenewalRequestsFrom(mac))

	killed := time.Now()
	ef.Stop()

	w := harness.BeginCounterWindow(t, ctx, cli,
		"renewals_unanswered", "dhcp_timeouts", "leases_renewed",
		"recovery_failed", "join_start_failures", "tombstone_write_failures")
	base := w.Before()
	baseWarn := harness.CountPluginLogLines(t, ctx, renewalWarnMarker, ep)
	t.Logf("server killed with a %ds lease held; T1=%ds, so the first renewal request goes into "+
		"silence at t+%ds and the retransmission at t+%ds",
		leaseSeconds, renewT1, renewT1, renewT1+60)

	requests, ok := wire.AwaitRenewalRequestsFrom(mac, beforeKill+2, wireBudget)
	if !ok {
		t.Fatalf("only %d renewal request(s) from %s reached the wire within %s of the kill "+
			"(%d before it); the capture holds %d client message(s) in total. With none at all "+
			"the instrument never saw this client and every count below would be a statement "+
			"about nothing; with one, the client stopped asking after its first try.",
			len(requests)-beforeKill, mac, wireBudget, beforeKill, len(wire.Frames()))
	}
	t.Logf("%d renewal request(s) on the wire by t+%.0fs after the kill (%d before it): %s",
		len(requests)-beforeKill, time.Since(killed).Seconds(), beforeKill, requests[len(requests)-1])

	// Health is read before the wire is counted.
	if _, ok := w.Await(healthBudget, func(now, before *harness.HealthResponse) bool {
		return now.RenewalsUnanswered > before.RenewalsUnanswered
	}); !ok {
		t.Fatalf("renewals_unanswered never rose above %d within %s of a SECOND renewal request "+
			"reaching the wire. That is #940: the outage is on the wire and the operator's view "+
			"does not carry it.", base.RenewalsUnanswered, healthBudget)
	}
	assertNoNewHealthFaults(t, w, "a DHCP server that stopped answering renewals is a degraded "+
		"mode, not a plugin failure")
	before, after := w.End()

	// Counted after every health read, so a request arriving in between can only widen the bound.
	onWire := len(wire.RenewalRequestsFrom(mac)) - beforeKill
	gain := after.RenewalsUnanswered - before.RenewalsUnanswered
	t.Logf("renewals_unanswered %d -> %d (+%d) against %d renewal request(s) on the wire",
		before.RenewalsUnanswered, after.RenewalsUnanswered, gain, onWire)

	if gain < 1 {
		t.Errorf("renewals_unanswered gained %d while %d renewal requests went unanswered on the "+
			"wire", gain, onWire)
	}
	if int(gain) > onWire-1 {
		t.Errorf("renewals_unanswered gained %d from %d renewal request(s) on the wire. The "+
			"request in flight has not been refused yet, so at most %d of them are PROVEN "+
			"unanswered; a counter that moves on the send rather than on the silence reports an "+
			"outage on a network that is answering perfectly well.", gain, onWire, onWire-1)
	}

	// The fixture is IPv4 only, and the family halves are two protocols (#730).
	if d := after.RenewalsUnansweredV4 - before.RenewalsUnansweredV4; d != gain {
		t.Errorf("renewals_unanswered rose by %d but its v4 half by %d on an IPv4-only fixture; "+
			"the aggregate is the SUM of the halves and the two must agree", gain, d)
	}
	if d := after.RenewalsUnansweredV6 - before.RenewalsUnansweredV6; d != 0 {
		t.Errorf("the v6 half rose by %d on an IPv4-only fixture", d)
	}

	if d := after.DHCPTimeouts - before.DHCPTimeouts; d != 0 {
		t.Errorf("dhcp_timeouts rose by %d while a %ds lease was still held. The two counters are "+
			"a pair -- one says the server stopped answering, the other says the client ran out "+
			"of lease -- and an outage counted in both is an outage neither can be read for.",
			d, leaseSeconds)
	}
	if d := after.LeasesRenewed - before.LeasesRenewed; d != 0 {
		t.Errorf("leases_renewed rose by %d after the server was killed; nothing answered these "+
			"requests", d)
	}

	// The counter cannot name an endpoint; the log line ties the rise to this one (#278).
	if got := harness.CountPluginLogLines(t, ctx, renewalWarnMarker, ep) - baseWarn; got < 1 {
		t.Errorf("the plugin logged no unanswered-renewal warning naming endpoint %s (+%d lines); "+
			"the counter rose, so either the rise belongs to another client or the operator who "+
			"reads logs rather than dashboards gets nothing", ep, got)
	}

	if !containerHasIP(t, ctx, id, ip) {
		t.Errorf("container lost %s while its lease was still valid; an unanswered renewal is "+
			"deliberately a no-op until the lease runs out", ip)
	}
}

// renewalWarnMarker is a stable phrase of pkg/plugin's renewalReporter warning, specific enough that no other line collides.
const renewalWarnMarker = "did not answer this endpoint's renewal request"
