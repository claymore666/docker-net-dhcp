//go:build integration

// Runtime failure-injection tests (#128): what happens AFTER a
// container is bound and the world breaks. Each test runs against a
// per-test EphemeralFixture DHCP server (never the suite-static one —
// every other test depends on that staying up) and documents the
// *intended* degraded-mode behaviour it asserts, so those semantics
// are decided here rather than discovered in production.
//
// These tests cross real DHCP timing boundaries. They used to pay two
// minutes per boundary because dnsmasq's minimum lease is a hard 2m;
// the fixture now runs Kea, which honours whatever lease it is told,
// so each test asks for the shortest lease that still makes its own
// scenario meaningful (#356).
//
// That makes the lease a PARAMETER OF THE SCENARIO rather than a fact
// of life, and every test below states the inequality it needs —
// "the outage must outlive the lease", "the outage must cross T1 but
// not the lease". Change a number without re-checking its inequality
// and the test keeps passing while no longer crossing the boundary it
// is named after; that is precisely how the pre-#278 versions of two
// of these tests went green in ~77s while claiming to wait out a 120s
// lease.
//
// They are split out of the main suite into
// `make integration-test-failure` (second CI step).
//
// dhcpcd timing facts the asserts below lean on (see pkg/dhcp):
//
//   - a dead server produces NO event at T1/T2, and — the part this
//     file originally got wrong, see #353 — no usable event at expiry
//     either. Under `--noconfigure` dhcpcd reports a lapsed lease as
//     RELEASE, which is indistinguishable from the one a graceful stop
//     emits and so can never be counted as a loss. From the kill
//     onward dhcpcd may say nothing at all.
//
//   - dhcp_timeouts therefore moves on the plugin's OWN reckoning: the
//     outage watchdog knows the granted lease lifetime
//     (dhcp.Info.LeaseSeconds) and calls the outage once
//     lastAffirmed + lease + grace has passed, re-checking on each
//     tick. CI installs the plugin with OUTAGE_TICK=2s /
//     OUTAGE_GRACE=10s (#278), so on the outage tests' 20s lease the
//     rise lands ~32s after the last bind/renew. Against a plugin left
//     on the shipped 30s/25s defaults it is 45–75s instead.
//
//     The budgets below are sized for the SLOWER of the two on purpose.
//     They are poll deadlines, not waits: each test returns as soon as
//     the counter moves, so a generous budget costs nothing and keeps
//     these tests correct when run against a default-cadence plugin.
//
//   - dhcpcd keeps re-DISCOVERing forever, so recovery after the server
//     returns lands within ~30s, and while it's gone dhcp_timeouts keeps
//     climbing once per watchdog tick.
//
//   - the plugin DELIBERATELY does NOT tear down the address when the
//     lease lapses (would wipe copied routes, see dhcp_manager.go) —
//     the container keeps its address through an outage.
//
// TWO RULES THIS FILE LEARNED THE HARD WAY (#278). Both cost almost
// nothing to keep, and dropping either one silently guts these tests:
//
//  1. Establish that the persistent client is BOUND before injecting
//     the failure. RunContainer returns as soon as docker reports an
//     address, and that address comes from CreateEndpoint's one-shot
//     lease — the long-lived client Join starts may not have confirmed
//     its own lease yet. Kill the server inside that window and the
//     client never leaves the "acquiring" state it starts in; the
//     watchdog then fires one grace later and the test goes green
//     having never crossed the expiry it claims to exercise. Both
//     outage tests used to finish in ~77s, which is less than the one
//     120s lease they were supposedly waiting out.
//  2. Assert endpoint-scoped, not plugin-wide. Every health counter is
//     a plugin-level total, so "dhcp_timeouts went up" is satisfied by
//     ANY manager in the plugin, including an orphan left by an earlier
//     test. Pair each counter assertion with the plugin's own
//     endpoint=<short id> log line for the same event
//     (harness.CountPluginLogLines), and assert it as a delta across
//     the window so start-up churn cannot stand in for the real event.
package integration

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// Plugin log messages that record a DHCP outage against ONE endpoint.
// These sit next to the counter bumps in pkg/plugin — handleEvent's
// "leasefail" and "renew"-with-changed-IP arms, and the outage
// watchdog — and carry the manager's endpoint field, which the
// counters themselves do not.
const (
	logLeaseFail = "dhcp failed to get a lease"
	// The watchdog has two wordings and a bound client hits the second
	// one FIRST: a lease that lapses unheard is reported as the
	// deadline line, and only the repeat ticks after it say "still
	// unreachable". Matching just one of the two would make these
	// tests race the wording (#353).
	logWatchdog  = "DHCP server still unreachable"
	logLapse     = "passed its renewal deadline"
	logIPChanged = "dhcp renew with changed IP"

	// outageRiseBudget bounds the wait for the first dhcp_timeouts rise
	// after a BOUND client's server dies. Sized for the shipped 30s/25s
	// cadence — lease + grace (20s + 25s) plus up to one 30s tick, ~75s
	// worst case — not for the tighter cadence CI installs, so the same
	// test is valid either way. It is a deadline, not a wait: under
	// CI's 2s/10s the rise arrives at ~32s and the poll returns there.
	//
	// The headroom over 75s is deliberate and cheap: the budget is only
	// ever spent in full when the test is about to fail anyway.
	outageRiseBudget = 120 * time.Second
)

// assertNoNewHealthFaults is what a bare `!h.Healthy` check should have
// been all along.
//
// `healthy` is derived from plugin-wide, lifetime-cumulative counters,
// and one plugin instance serves the entire job — including the main
// suite, which runs first in a separate `go test` invocation. So a
// single unrelated fault anywhere before these tests start would fail
// every health assertion here and say nothing about the scenario under
// test. That is exactly what happened in #373.
//
// #278 already drew this conclusion for dhcp_timeouts ("the counter is
// plugin-wide, so this rise belongs to some other client"). The health
// flag next to it kept the absolute form. This applies the same rule:
// assert that THIS test introduced no new fault, not that the plugin
// has been faultless for its whole life.
func assertNoNewHealthFaults(t *testing.T, w *harness.CounterWindow, what string) {
	t.Helper()
	// Closing the window here rather than taking the caller's
	// mid-flight reading is deliberate on two counts. It proves the
	// plugin never restarted across the stretch these deltas describe
	// (#405), and it reads the counters at the latest possible moment —
	// they only ever climb, so a check at the end is strictly stronger
	// than one taken when some earlier condition first held.
	base, now := w.End()
	if base == nil || now == nil {
		return
	}
	if d := now.RecoveryFailed - base.RecoveryFailed; d > 0 {
		t.Errorf("%s: recovery_failed rose by %d during this test", what, d)
	}
	if d := now.JoinStartFailures - base.JoinStartFailures; d > 0 {
		t.Errorf("%s: join_start_failures rose by %d during this test — a running container was left without a renewal client", what, d)
	}
	if d := now.TombstoneWriteFailures - base.TombstoneWriteFailures; d > 0 {
		t.Errorf("%s: tombstone_write_failures rose by %d during this test", what, d)
	}
}

// failureHealth is gone. harness.CounterWindow.Await runs the same
// poll and additionally rejects a reading taken after the plugin
// restarted underneath it. "This counter rose above its baseline" is
// exactly the condition that breaks silently across a reset — the
// counter being watched went back to zero, so the wait either times out
// blaming the wrong thing or is satisfied later for the wrong reason
// (#405). The poll interval and its #254 rationale moved with it.

// awaitBoundPersistentClient blocks until the plugin records a bind
// beyond the window's opening baseline — i.e. the long-lived client
// started in Join holds its OWN lease and the lease clock these tests
// wait out is actually running. Rule 1 in this file's header.
func awaitBoundPersistentClient(t *testing.T, w *harness.CounterWindow) {
	t.Helper()
	if _, ok := w.Await(45*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.LeasesObtained > before.LeasesObtained
	}); !ok {
		t.Fatal("persistent client never confirmed its own bind; the failure below would land on an acquiring client, not a bound one (#278)")
	}
	// This window's only job was the bind; close it here so it is not
	// left open for the rest of the test. An unclosed window is flagged
	// by the harness, and rightly — it looks identical to one that
	// verified something.
	w.End()
}

// outageLines counts, for one endpoint, the plugin-log records of the
// two events that bump dhcp_timeouts: a leasefail (a dhcpcd TIMEOUT
// while acquiring) and an outage-watchdog tick. Returned separately
// because which of the two fires is the diagnostic — a leasefail means
// dhcpcd spoke, a watchdog line means the plugin synthesised the
// signal from the lease deadline. For a client that was BOUND before
// the server died, expect the watchdog: dhcpcd's lapse report is a
// RELEASE and is deliberately dropped (#353).
func outageLines(t *testing.T, ctx context.Context, endpoint string) (leasefail, watchdog int) {
	t.Helper()
	return harness.CountPluginLogLines(t, ctx, endpoint, logLeaseFail),
		harness.CountPluginLogLines(t, ctx, endpoint, logWatchdog) +
			harness.CountPluginLogLines(t, ctx, endpoint, logLapse)
}

// containerIPv4 returns the container's first non-loopback IPv4
// address as seen inside its own netns, or "" if it has none.
func containerIPv4(t *testing.T, ctx context.Context, ctrID string) string {
	t.Helper()
	for _, f := range strings.Fields(harness.ExecOutput(t, ctx, ctrID, "ip", "-4", "addr")) {
		if !strings.Contains(f, "/") {
			continue
		}
		bare := strings.SplitN(f, "/", 2)[0]
		if ip := net.ParseIP(bare); ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			return bare
		}
	}
	return ""
}

// containerHasIP reports whether `ip -4 addr` inside the container
// still shows the given address.
func containerHasIP(t *testing.T, ctx context.Context, ctrID, ip string) bool {
	t.Helper()
	out := harness.ExecOutput(t, ctx, ctrID, "ip", "-4", "addr")
	return strings.Contains(out, ip+"/")
}

// inRange reports whether bare IPv4 ip falls inside [start, end].
func inRange(ip, start, end string) bool {
	v4 := net.ParseIP(ip).To4()
	s := net.ParseIP(start).To4()
	e := net.ParseIP(end).To4()
	if v4 == nil || s == nil || e == nil {
		return false
	}
	return bytes.Compare(v4, s) >= 0 && bytes.Compare(v4, e) <= 0
}

// TestFailure_ServerLossDuringRenewal: the "router rebooted at 3am"
// scenario, for an outage LONGER than the lease. Intended behaviour
// asserted:
//   - while the server is gone, the container KEEPS its address (the
//     lapse no-op), the plugin stays Healthy, and dhcp_timeouts
//     records the failure — for THIS endpoint, proven from the
//     plugin's own log rather than from the plugin-wide counter alone;
//   - when the server returns, the client re-binds without operator
//     intervention.
//
// It does NOT assert that the address survives, and cannot: the
// outage is only detectable once the lease has lapsed (the rise is at
// lease+grace, expiry at 120s — true at any grace, since the grace is
// added ON TOP of the lease), so by the time this test has proven an
// outage the server's lease DB no longer holds the client's address. Worse, the plugin's own retain-through-outage behaviour
// makes the old address look occupied: the container is still
// answering on it, dnsmasq pings before offering a freshly allocated
// address, and hands out a different one. Observed exactly that —
// DISCOVER requesting .21, OFFER .22.
//
// The same-address contract is real, but it belongs to an outage the
// lease OUTLIVES; TestFailure_ServerReturnsBeforeExpiry owns it.
// Asserting it here is what made the pre-#278 version of this test
// green for the wrong reason: it finished in ~77s, inside the 120s
// lease, so the server still held the entry and re-ACKed it.
func TestFailure_ServerLossDuringRenewal(t *testing.T) {
	// The outage is only detectable after a bound lease lapses (20s)
	// plus up to one watchdog period, and the re-bind poll after the
	// server returns adds up to 90s on top of that.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const netName = "dh-itest-floss"

	// INEQUALITY: the outage must OUTLIVE the lease — that is the whole
	// scenario, and it is what separates this test from
	// TestFailure_ServerReturnsBeforeExpiry. The outage here is bounded
	// by outageRiseBudget, which is lease + grace + a tick, so any
	// lease shorter than that budget satisfies it.
	ef := harness.NewEphemeralFixture(t, harness.WithLeaseSeconds(harness.EphemeralOutageLeaseSeconds))
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

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, ip, mac := harness.RunContainer(t, ctx, netName, "dh-itest-floss-ctr")
	t.Logf("bound: ip=%s mac=%s", ip, mac)

	awaitBoundPersistentClient(t, bindW)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	faultW := harness.BeginCounterWindow(t, ctx, cli,
		"recovery_failed", "join_start_failures", "tombstone_write_failures")
	base := faultW.Before()
	baseFail, baseWatch := outageLines(t, ctx, ep)
	if baseFail+baseWatch > 0 {
		// Not fatal: one dhcpcd TIMEOUT during initial acquisition on a
		// loaded runner is plausible and harmless. Asserting the delta
		// below keeps the proof intact either way.
		t.Logf("endpoint %s carried %d leasefail / %d watchdog line(s) from start-up; asserting on the delta", ep, baseFail, baseWatch)
	}

	// Kill the server uncleanly. The persistent client — now provably
	// holding its own lease — faces silent T1/T2 retries, expiry, and
	// a failing re-DISCOVER.
	killed := time.Now()
	ef.Stop()
	t.Logf("server killed; a BOUND lease (fixture lease %ds) has to lapse before the plugin can report a timeout", ef.LeaseSeconds())

	h, ok := faultW.Await(outageRiseBudget, func(h, _ *harness.HealthResponse) bool {
		return h.DHCPTimeouts > base.DHCPTimeouts
	})
	if !ok {
		t.Fatalf("dhcp_timeouts never rose above %d within %s of the server dying (last: %+v)", base.DHCPTimeouts, outageRiseBudget, h)
	}
	nowFail, nowWatch := outageLines(t, ctx, ep)
	t.Logf("dhcp_timeouts %d -> %d at t+%.0fs after the kill; endpoint %s logged +%d leasefail / +%d watchdog line(s)",
		base.DHCPTimeouts, h.DHCPTimeouts, time.Since(killed).Seconds(), ep, nowFail-baseFail, nowWatch-baseWatch)
	if (nowFail-baseFail)+(nowWatch-baseWatch) == 0 {
		t.Errorf("dhcp_timeouts rose but the plugin logged no outage line for endpoint %s: the counter is plugin-wide, so this rise belongs to some other client and says nothing about the endpoint under test (#278)", ep)
	}
	assertNoNewHealthFaults(t, faultW, "a dead DHCP server is a degraded mode, not a plugin failure")
	if !containerHasIP(t, ctx, id, ip) {
		t.Errorf("container lost %s during the outage; a lapsed lease is deliberately a no-op and should retain the address", ip)
	}

	// Server returns, lease DB intact: the dhcpcd retry loop must
	// re-bind to the same address within ~30s (poll 90s for margin).
	acksBefore := ef.CountLogLines("DHCPACK", mac)
	restarted := time.Now()
	ef.StartAgain()
	t.Log("server restarted with preserved lease DB; awaiting re-bind...")

	deadline := time.Now().Add(90 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		if ef.CountLogLines("DHCPACK", mac) > acksBefore {
			recovered = true
			break
		}
		// 90s deadline unchanged; tighter poll only shrinks the
		// overshoot past the re-bind ACK (#254).
		time.Sleep(250 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("no DHCPACK for the container's MAC within 90s of the server returning")
	}
	t.Logf("re-bound at t+%.0fs after the server came back", time.Since(restarted).Seconds())

	// The address may or may not be the original one (see this test's
	// header) — but whichever it is, the container and the server must
	// agree on it. A re-bind that leaves the container holding an
	// address the server has since given away is the failure mode worth
	// catching here, and it is what the dropped same-address assertion
	// was accidentally standing in for.
	live := containerIPv4(t, ctx, id)
	if live == "" {
		t.Fatal("container has no IPv4 address after the server returned; the client did not recover")
	}
	if live != ip {
		t.Logf("address changed across the outage: %s -> %s (expected when the outage outlives the lease)", ip, live)
	}
	if acked := ef.LastACKAddress(mac); acked != "" && acked != live {
		t.Errorf("container holds %s but the server's last DHCPACK for %s was %s; the two have diverged", live, mac, acked)
	}

	assertNoNewHealthFaults(t, faultW, "the server returned and the client re-bound")
}

// TestFailure_ServerReturnsBeforeExpiry: the same outage, but SHORT —
// the server is back while the lease is still live. This is where the
// address-stability contract belongs, and it is the common real case
// (a router reboot takes well under a lease). Intended behaviour:
//   - the client re-binds to the SAME address, because the server's
//     lease DB still holds it;
//   - lease_changed stays flat — no consumer sees a renumbering;
//   - dhcp_timeouts stays flat for this endpoint: the outage never
//     reached lease+grace, so there was nothing to report.
//
// Together with TestFailure_ServerLossDuringRenewal this pins both
// sides of the boundary: inside the lease the address is guaranteed,
// past it only recovery is.
func TestFailure_ServerReturnsBeforeExpiry(t *testing.T) {
	// INEQUALITY, and this test is nothing without it:
	//
	//	T1 (15s)  <  outage (25s)  <  lease (60s)
	//
	// The outage must cross T1 so the client actually attempts — and
	// fails — a renewal while the server is down; and it must end well
	// inside the lease so the server's DB still holds the entry when it
	// returns, which is the address-stability contract being asserted.
	// Break the left inequality and the test proves nothing (no renewal
	// was ever attempted); break the right one and it becomes
	// TestFailure_ServerLossDuringRenewal with a wrong assertion.
	//
	// T1/T2 are set explicitly rather than derived from the lease
	// (which would put T1 at 30s) so the outage can be short while the
	// lease stays comfortably long. The 35s of margin between the
	// outage ending and expiry is what absorbs the client's renewal
	// backoff on a loaded runner.
	const (
		leaseSeconds = 60
		renewT1      = 15 // above dhcpcd's internal renewal flooring
		renewT2      = 45 // rebind — deliberately past the outage window
		outage       = 25 * time.Second
	)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	const netName = "dh-itest-fshort"

	ef := harness.NewEphemeralFixture(t,
		harness.WithLeaseSeconds(leaseSeconds),
		harness.WithRenewTimes(renewT1, renewT2),
	)
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

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, ip, mac := harness.RunContainer(t, ctx, netName, "dh-itest-fshort-ctr")
	t.Logf("bound: ip=%s mac=%s", ip, mac)

	awaitBoundPersistentClient(t, bindW)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	faultW := harness.BeginCounterWindow(t, ctx, cli,
		"recovery_failed", "join_start_failures", "tombstone_write_failures")
	base := faultW.Before()
	baseFail, baseWatch := outageLines(t, ctx, ep)

	// Down and back up well inside the lease — see the inequality at
	// the top of this function.
	acksBefore := ef.CountLogLines("DHCPACK", mac)
	killed := time.Now()
	ef.Stop()
	t.Logf("server stopped inside the lease; restarting in %s (T1=%ds, lease=%ds, so the client fails one renewal and the lease stays live throughout)",
		outage, renewT1, leaseSeconds)

	select {
	case <-time.After(outage):
	case <-ctx.Done():
		t.Fatal("context expired during the short outage")
	}
	ef.StartAgain()
	t.Logf("server back at t+%.0fs, still inside the lease; awaiting the renewal ACK...", time.Since(killed).Seconds())

	deadline := time.Now().Add(90 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		if ef.CountLogLines("DHCPACK", mac) > acksBefore {
			recovered = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("no DHCPACK for the container's MAC within 90s of the server returning inside the lease")
	}
	t.Logf("re-ACKed at t+%.0fs after the kill", time.Since(killed).Seconds())

	// Both halves matter: the server must have handed the SAME address
	// back (its own ACK is the authority), and the container must still
	// be holding it. Checking only the container would pass on the
	// plugin's retain-through-outage behaviour alone, without the
	// server ever having agreed.
	if acked := ef.LastACKAddress(mac); acked != ip {
		t.Errorf("server's last DHCPACK for %s was %s, want %s; the lease was still live and must have been returned", mac, acked, ip)
	}
	if !containerHasIP(t, ctx, id, ip) {
		t.Errorf("container's address changed across an outage the lease outlived; the server still held %s and must have returned it", ip)
	}

	_, after := faultW.End()
	if after.LeaseChanged != base.LeaseChanged {
		t.Errorf("lease_changed moved %d -> %d across an outage shorter than the lease; want flat", base.LeaseChanged, after.LeaseChanged)
	}
	nowFail, nowWatch := outageLines(t, ctx, ep)
	if d := (nowFail - baseFail) + (nowWatch - baseWatch); d != 0 {
		t.Errorf("endpoint %s logged %d outage line(s) for an outage that never reached lease+grace; the watchdog fired early", ep, d)
	}
	assertNoNewHealthFaults(t, faultW, "an outage the plugin should have ridden out silently")
}

// TestFailure_LeaseRefusedOnRenewal: the site gets renumbered under a
// live lease — the server comes back on a different subnet and the
// container's held address is foreign to it. Two CI iterations showed
// dnsmasq REFUSES such renewals *silently* in several shapes (out-of-
// range REQUEST: ignored; address-taken REQUEST: ignored) rather than
// emitting DHCPNAK, so this test asserts the *intended degraded-mode
// semantics* rather than a specific wire message:
//   - the client re-acquires from the new subnet's pool without
//     operator intervention; lease_changed records the move;
//   - `docker inspect` keeps reporting the ORIGINAL address: libnetwork
//     has no in-place endpoint-IP swap RPC, so the inspect divergence
//     is the DEFINED degraded mode (#104) — lease_changed is the
//     operator's signal, and this assertion is the documentation;
//   - the plugin stays Healthy throughout.
//
// The naks_received counter's contract is pinned in unit tests
// (TestHandleEvent_Counters) — when a server does NAK, that's the
// path that counts it; any NAK observed here is logged for interest.
func TestFailure_LeaseRefusedOnRenewal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const netName = "dh-itest-fref"

	// INEQUALITY: re-acquisition cannot happen before the held lease
	// stops being defensible, so the shorter the lease the sooner this
	// test can conclude. Nothing here needs the lease to be long — the
	// scenario is "the address is foreign to the new server", which is
	// true from the first renewal attempt onwards.
	ef := harness.NewEphemeralFixture(t, harness.WithLeaseSeconds(harness.EphemeralOutageLeaseSeconds))
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

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, inspectIP, mac := harness.RunContainer(t, ctx, netName, "dh-itest-fref-ctr")
	t.Logf("bound: inspect ip=%s mac=%s", inspectIP, mac)

	// Settle: wait for the persistent client's own bound (it can
	// differ from CreateEndpoint's one-shot lease) so the baseline
	// isn't polluted by start-up churn.
	awaitBoundPersistentClient(t, bindW)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	faultW := harness.BeginCounterWindow(t, ctx, cli,
		"recovery_failed", "join_start_failures", "tombstone_write_failures")
	base := faultW.Before()
	baseChanged := harness.CountPluginLogLines(t, ctx, ep, logIPChanged)

	// Renumber the site: new server address, new pool, wiped DB. The
	// unicast T1 renewal dies (the old server address is gone); the
	// T2 broadcast rebind carries a foreign address; re-acquisition
	// follows somewhere between T2 and expiry + re-DISCOVER. On the
	// 20s lease, with T1/T2 derived from it, that is T2 ~17.5s and
	// expiry 20s, so re-acquisition lands within a few tens of seconds
	// rather than the ~135s a 2m lease imposed.
	renumbered := time.Now()
	ef.RestartOnSubnet(harness.EphemeralAltServerAddr, harness.EphemeralAltPoolStart, harness.EphemeralAltPoolEnd)
	t.Logf("server renumbered; awaiting re-acquisition (lease %ds, so T2 ~%.1fs, expiry %ds)...",
		ef.LeaseSeconds(), float64(ef.LeaseSeconds())*0.875, ef.LeaseSeconds())

	var liveIP string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr")
		for _, f := range strings.Fields(out) {
			if !strings.Contains(f, "/") {
				continue
			}
			bare := strings.SplitN(f, "/", 2)[0]
			if inRange(bare, harness.EphemeralAltPoolStart, harness.EphemeralAltPoolEnd) {
				liveIP = bare
			}
		}
		if liveIP != "" {
			break
		}
		// Each iteration is a docker exec, so hold a 500ms floor
		// rather than the 250ms used for cheap log/health polls —
		// still a quarter of the old 2s overshoot (#254).
		time.Sleep(500 * time.Millisecond)
	}
	if liveIP == "" {
		t.Fatalf("container never re-acquired from the new subnet's pool %s-%s; ip -4 addr:\n%s",
			harness.EphemeralAltPoolStart, harness.EphemeralAltPoolEnd,
			harness.ExecOutput(t, ctx, id, "ip", "-4", "addr"))
	}
	t.Logf("re-acquired: live ip=%s at t+%.0fs after the renumbering", liveIP, time.Since(renumbered).Seconds())

	h, ok := faultW.Await(30*time.Second, func(h, _ *harness.HealthResponse) bool {
		return h.LeaseChanged > base.LeaseChanged
	})
	if !ok {
		t.Errorf("lease_changed never recorded the re-acquisition (last: %+v)", h)
	}
	// lease_changed is plugin-wide like every other counter. The
	// address observed inside THIS container above is already
	// endpoint-scoped evidence; the plugin's own log line for this
	// endpoint is what ties the counter to it (#278).
	if nowChanged := harness.CountPluginLogLines(t, ctx, ep, logIPChanged); nowChanged == baseChanged {
		t.Errorf("endpoint %s re-addressed to %s but the plugin logged no lease-change line for it (%d before, %d after)", ep, liveIP, baseChanged, nowChanged)
	}
	assertNoNewHealthFaults(t, faultW, "a lease re-acquisition is a defined, healthy flow")
	if h != nil && h.NAKsReceived > base.NAKsReceived {
		t.Logf("server NAKed on the wire (naks_received %d -> %d)", base.NAKsReceived, h.NAKsReceived)
	}

	// docker inspect still shows the original address: the DEFINED
	// divergence (#104). If this ever fails because inspect tracks
	// the new IP, a re-Join mechanism landed — update the reference
	// manual's troubleshooting row along with this test.
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	var nowInspect string
	for _, epView := range ins.NetworkSettings.Networks {
		nowInspect = epView.IPAddress
	}
	if nowInspect != inspectIP {
		t.Errorf("docker inspect reports %s; expected the stale original %s (documented degraded mode, #104)", nowInspect, inspectIP)
	}
}

// TestFailure_LeaseExpiry: the server disappears permanently and the
// lease fully lapses. Intended behaviour asserted: address retention
// is DELIBERATE (deconfig no-op), the endpoint stays L2-reachable on
// the stale address, dhcp_timeouts keeps climbing as the retry loop
// spins, and the plugin reports Healthy — "server gone" is a defined
// degraded mode, not undefined behaviour.
//
// This is the test that leans hardest on rule 1 in the file header: a
// lease that was never held cannot expire, so the bind wait below is
// not hygiene, it is the entire premise.
func TestFailure_LeaseExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const netName = "dh-itest-fexp"

	// INEQUALITY: the lease must FULLY lapse and then keep lapsing —
	// this test asserts a recurring signal, so it needs the lease short
	// enough that expiry plus two watchdog ticks fits inside the
	// budgets below.
	ef := harness.NewEphemeralFixture(t, harness.WithLeaseSeconds(harness.EphemeralOutageLeaseSeconds))
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

	bindW := harness.BeginCounterWindow(t, ctx, cli, "leases_obtained")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent": harness.EphemeralHostVeth,
	})
	id, ip, mac := harness.RunContainer(t, ctx, netName, "dh-itest-fexp-ctr")
	t.Logf("bound: ip=%s mac=%s", ip, mac)

	awaitBoundPersistentClient(t, bindW)
	ep := harness.EndpointShortID(t, ctx, cli, id, netName)

	faultW := harness.BeginCounterWindow(t, ctx, cli,
		"recovery_failed", "join_start_failures", "tombstone_write_failures")
	base := faultW.Before()
	baseFail, baseWatch := outageLines(t, ctx, ep)
	if baseFail+baseWatch > 0 {
		t.Logf("endpoint %s carried %d leasefail / %d watchdog line(s) from start-up; asserting on the delta", ep, baseFail, baseWatch)
	}

	killed := time.Now()
	ef.Stop()
	t.Logf("server killed permanently; a BOUND lease (fixture lease %ds) now has to cross T2 and full expiry", ef.LeaseSeconds())

	first, ok := faultW.Await(outageRiseBudget, func(h, _ *harness.HealthResponse) bool {
		return h.DHCPTimeouts > base.DHCPTimeouts
	})
	if !ok {
		t.Fatalf("dhcp_timeouts never rose above %d within %s of the kill (last: %+v)", base.DHCPTimeouts, outageRiseBudget, first)
	}
	firstFail, firstWatch := outageLines(t, ctx, ep)
	t.Logf("first dhcp_timeouts rise %d -> %d at t+%.0fs after the kill; endpoint %s logged +%d leasefail / +%d watchdog line(s)",
		base.DHCPTimeouts, first.DHCPTimeouts, time.Since(killed).Seconds(), ep, firstFail-baseFail, firstWatch-baseWatch)
	if (firstFail-baseFail)+(firstWatch-baseWatch) == 0 {
		t.Errorf("dhcp_timeouts rose but the plugin logged no outage line for endpoint %s: the counter is plugin-wide, so this rise belongs to some other client (#278)", ep)
	}

	// The retry loop must keep recording failures — once per watchdog
	// tick — and keep recording them AGAINST THIS ENDPOINT: a watchdog
	// that stalled on our client is invisible in the plugin-wide total.
	// 80s covers one full 30s tick with headroom for a default-cadence
	// plugin; under CI's 2s tick the second rise lands almost at once.
	second, ok := faultW.Await(80*time.Second, func(h, _ *harness.HealthResponse) bool {
		return h.DHCPTimeouts > first.DHCPTimeouts
	})
	if !ok {
		t.Errorf("dhcp_timeouts stalled at %d; the re-DISCOVER loop should keep recording failures (last: %+v)", first.DHCPTimeouts, second)
	}
	secondFail, secondWatch := outageLines(t, ctx, ep)
	t.Logf("second dhcp_timeouts rise at t+%.0fs after the kill; endpoint %s now +%d leasefail / +%d watchdog line(s) since baseline",
		time.Since(killed).Seconds(), ep, secondFail-baseFail, secondWatch-baseWatch)
	if (secondFail-firstFail)+(secondWatch-firstWatch) == 0 {
		t.Errorf("endpoint %s logged no further outage line while the server stayed down; the recurring signal is not recurring for this client (#278)", ep)
	}
	assertNoNewHealthFaults(t, faultW, "a permanent server loss is a defined degraded mode")

	// Address retention past expiry is deliberate...
	if !containerHasIP(t, ctx, id, ip) {
		t.Errorf("container lost %s after lease expiry; retention (deconfig no-op) is the defined behaviour", ip)
	}

	// ...and the endpoint stays L2-reachable: ping the container from
	// the server side of the veth pair (the address survives on the
	// link even though the DHCP server is dead).
	//
	// Via the fixture, not exec.Command directly: the server address
	// lives in the fixture's own network namespace, so a ping issued
	// from the test process would fail because the source address is
	// not local here — a false negative indistinguishable from a real
	// unreachable container.
	if out, err := ef.PingFromServer(ip); err != nil {
		t.Errorf("container %s not L2-reachable on its expired-lease address: %v\n%s", ip, err, out)
	}
}
