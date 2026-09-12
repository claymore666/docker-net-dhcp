// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// TestFailure_UnansweredRenewalsCounted is #940 on a real wire.
//
// THE DEFECT. A production host's DHCP server stopped answering
// renewals for 7h52m. The container kept its address and the client
// kept asking, which is the protocol working; what was missing was any
// way to know. /Plugin.Health read healthy, every counter on it read
// unchanged, and the first one that would have moved was dhcp_timeouts
// at the END of the lease -- a day later, by which time the address is
// gone. A renewal request that goes unanswered produces no lease event
// at all: the state machine stays in RENEWING and retransmits, so there
// was nothing for the plugin to fold on.
//
// WHAT THIS TEST ADDS OVER ITS NEIGHBOURS. TestFailure_ServerLossDuring
// Renewal already kills a server under a bound client -- with a 20s
// lease, and it waits for that lease to LAPSE. This one is the window
// before the lapse, which is where a production outage is actually
// lived: the lease is 600s and is never allowed to run out, so
// dhcp_timeouts must stay exactly where it was while the new counter
// moves. If both moved, one outage would be counted twice and the pair
// would be unreadable.
//
// THE ASSERTION IS AGAINST THE WIRE. The counter's own value cannot
// support a claim about the counter, and the server's log cannot help
// here: Kea and dnsmasq alike log a request they decline to serve
// AFTER deciding to serve it, and this server is not running at all.
// So a capture on the fixture's end of the veth counts the renewal
// requests that actually left the host, and the counter is read against
// that number. The reading is taken in a fixed order -- health first,
// wire second -- so that a request landing between the two can only
// widen the bound, never satisfy it.
//
// THE BOUND IS TWO-SIDED, and the upper half is the interesting one.
// The request currently in flight has not been refused yet: it may
// still be answered. So a client that has put N renewal requests on the
// wire has proof of exactly N-1 unanswered ones, and a counter reading
// N would be counting the send rather than the silence -- the wrong
// counter, the one that reports an outage on a network that is working.
func TestFailure_UnansweredRenewalsCounted(t *testing.T) {
	const (
		// T1 at 12s, as TestLeaseRenew_HonorsT1 uses: short enough to
		// be cheap, long enough to be a renewal the client schedules
		// from the server's option 58 rather than from anything local.
		renewT1 = 12
		// T2 at T1 + 120s, and the 120 is arithmetic, not taste. RFC
		// 2131 section 4.4.5 retransmits after "one-half of the
		// remaining time until T2 ... down to a minimum of 60 seconds",
		// which the library spells proto.RenewRetransmitFloor. Half of
		// 120 IS that floor, so the retransmission falls at T1+60 and
		// REBINDING is still a minute away when the reading is taken:
		// the second request on the wire is a genuine RENEWING
		// retransmission with no state change behind it, which is the
		// case that produces no lease event and the case #940 is about.
		renewT2 = renewT1 + 120
		// The lease must OUTLIVE the whole test by a wide margin. The
		// moment it lapses the client loses the address, dhcp_timeouts
		// moves, and this test would be measuring its neighbour's
		// scenario instead of its own.
		leaseSeconds = 600

		// wireBudget bounds the wait for the retransmission itself:
		// T1 + the 60s floor, plus room for a loaded runner. A deadline,
		// not a wait -- the poll returns as soon as the second request
		// is captured.
		wireBudget = 120 * time.Second
		// healthBudget bounds the lag between a request leaving the
		// host and the plugin's counter reflecting it. The chassis
		// folds the library's counters on a ticker at a quarter of the
		// retransmission floor, so the worst case is 15s.
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

	// The capture opens BEFORE the container starts. A capture opened
	// afterwards has no bind exchange to show, and could not then tell
	// "this client sent no renewal" apart from "this instrument never
	// saw this client at all".
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

	// Rule 1 of this file: the lease under test must belong to the
	// PERSISTENT client, not to the CreateEndpoint one-shot. Only the
	// persistent client renews, and only it reports here.
	awaitBoundPersistentClient(t, bindW)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"renewals_unanswered", "dhcp_timeouts", "leases_renewed",
		"recovery_failed", "join_start_failures", "tombstone_write_failures")
	base := w.Before()
	baseWarn := harness.CountPluginLogLines(t, ctx, renewalWarnMarker, ep)

	killed := time.Now()
	ef.Stop()
	t.Logf("server killed with a %ds lease held; T1=%ds, so the first renewal request goes into "+
		"silence at t+%ds and the retransmission at t+%ds",
		leaseSeconds, renewT1, renewT1, renewT1+60)

	// --- outside evidence, part one: the requests are really on the wire.
	requests, ok := wire.AwaitRenewalRequestsFrom(mac, 2, wireBudget)
	if !ok {
		t.Fatalf("only %d renewal request(s) from %s reached the wire within %s of the kill; "+
			"the capture holds %d client message(s) in total. With none at all the instrument "+
			"never saw this client and every count below would be a statement about nothing; "+
			"with one, the client stopped asking after its first try.",
			len(requests), mac, wireBudget, len(wire.Frames()))
	}
	t.Logf("%d renewal request(s) on the wire by t+%.0fs after the kill: %s",
		len(requests), time.Since(killed).Seconds(), requests[len(requests)-1])

	// --- the plugin's reading, taken BEFORE the wire is counted.
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

	// --- outside evidence, part two: counted after every health read,
	// so a request arriving in between can only widen the bound.
	onWire := len(wire.RenewalRequestsFrom(mac))
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

	// The family halves are two protocols, not two views of one (#730).
	// This fixture is IPv4 only, so the v6 half moving would mean the
	// gain was attributed by something other than the client's family.
	if d := after.RenewalsUnansweredV4 - before.RenewalsUnansweredV4; d != gain {
		t.Errorf("renewals_unanswered rose by %d but its v4 half by %d on an IPv4-only fixture; "+
			"the aggregate is the SUM of the halves and the two must agree", gain, d)
	}
	if d := after.RenewalsUnansweredV6 - before.RenewalsUnansweredV6; d != 0 {
		t.Errorf("the v6 half rose by %d on an IPv4-only fixture", d)
	}

	// The preservation control, and the reason this counter exists at
	// all: the lease is 600s and nothing has expired.
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

	// The counter is plugin-wide and cannot name an endpoint; the log
	// line is what an operator reading logs at 3am has to find, and it
	// is what ties this rise to THIS endpoint (#278's rule).
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

// renewalWarnMarker is the stable half of the warning pkg/plugin's
// renewalReporter emits. Matched on a phrase rather than the whole
// sentence so a wording change does not silently stop matching, and on
// enough of it that no other line in the log can collide.
const renewalWarnMarker = "did not answer this endpoint's renewal request"
