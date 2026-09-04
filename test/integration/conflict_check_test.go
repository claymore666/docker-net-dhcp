// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// The executable check for #524 and D23: an endpoint leased an address
// that another device on the segment already holds must be DETECTED and
// DECLINED, not silently accepted.
//
// In production this cost an endpoint and was found only because the
// upgrade was verified against outside evidence — the plugin's own
// report said healthy:true with every counter at zero, because nothing
// looked. These tests are that "nothing looked" turned into something
// that goes red.
//
// WHAT CHANGED SINCE THE FIRST VERSION OF THIS FILE. The chassis's own
// datagram probe is gone; the DHCP library now runs RFC 5227 on a raw
// ARP socket inside the container's namespace, per network, under
// conflict_check. Two consequences shape every test below:
//
//  1. The DHCP SERVER's log is now evidence, where before it deliberately
//     was not. The old probe was passive and the server never learned
//     anything; RFC 5227 obliges a DHCPDECLINE (RFC 2131 section 3.1(5))
//     and the server writes it down. That line, and the fresh DHCPOFFER
//     after it, are the outside evidence for the whole mechanism.
//  2. The check is per-network and can be turned OFF, so a test can no
//     longer assume a probe was sent. conflict_check=off is asserted
//     from the segment's ARP capture — the absence of a frame, which no
//     counter can show. The capture listens on the DHCP-server end of
//     the fixture's veth pair, NOT on the macvlan parent: the parent
//     cannot see its own children's transmits, and the first version of
//     this file asserted the absence from there, where the absence was
//     guaranteed. See arpcapture.go.
//
// Nothing here asserts on the plugin's counters ALONE. Each case reads
// the server's log, the wire, or the container's own view of its
// address; the counters are cross-checks on top.

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

// conflictWait is how long the assertions wait for RFC 5227 to reach a
// conclusion.
//
// The worst-case section 2.1 window is PROBE_WAIT 1s + 2 x PROBE_MAX 2s
// + ANNOUNCE_WAIT 2s = 7s, and a DECLINE costs RFC 2131 section 3.1(5)'s
// ten-second restart minimum plus a fresh DORA on top. This has to
// outlast that plus the scheduling slack of a loaded runner; it is a
// ceiling on waiting, not a measurement of anything.
const conflictWait = 45 * time.Second

// The pool is TWO addresses wide on purpose. The squatter sits on the
// first, so there is exactly one other address for the server to fall
// back to after the DECLINE — which makes "the container came up on a
// different address" a fact about the mechanism rather than about the
// pool's size. Guessing which address a pool will yield is not sound
// (see harness.WithPool), so the first is pinned by squatting it.
const (
	squatAddr = "192.168.101.42"
	altAddr   = "192.168.101.43"
)

// recycleAfterDeliberateConflict retires the fault a test induces on
// purpose.
//
// address_conflicts is a FATAL floor counter and the floor is absolute
// over the whole run, so a conflict induced here would fail the shard
// after the test itself passed. Recycling the plugin is the only way to
// clear a counter — they are process-local by design — and it is the
// reason the counter can stay fatal: nothing else in the suite should
// ever move it, so the floor needs no notion of an expected conflict.
func recycleAfterDeliberateConflict(t *testing.T) {
	t.Cleanup(func() {
		bg, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := cliReset(bg, t); err != nil {
			t.Logf("WARN: could not recycle the plugin after the deliberate conflict: %v\n"+
				"  address_conflicts stays non-zero and the health floor will fail this shard.", err)
		}
	})
}

// TestConflictCheck_SquattedOfferIsDeclined is case (a): the address the
// server is about to hand out is already held.
//
// Run in wait and in async, because the two differ in WHEN the container
// is told its address, not in whether the conflict is found — and a
// mechanism that only worked in the mode that blocks would look correct
// from every counter.
func TestConflictCheck_SquattedOfferIsDeclined(t *testing.T) {
	for _, mode := range []string{"wait", "async"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()

			netName := "dh-itest-cc-" + mode

			ef := harness.NewEphemeralFixture(t, harness.WithPool(squatAddr, altAddr))
			cap := ef.StartARPCapture(t)
			t.Cleanup(func() {
				if t.Failed() {
					ef.DumpLogs(func(s string) { t.Log(s) })
					cap.Dump(func(s string) { t.Log(s) })
					harness.DumpPluginLog(t)
				}
			})
			recycleAfterDeliberateConflict(t)

			// Park the squatter BEFORE the container exists, so the
			// address is already taken at the moment the lease is
			// granted — the ordering of the production incident, where
			// the other host had been sitting on the address for as
			// long as it had been racked.
			squatMAC := ef.Squat(squatAddr)
			t.Logf("squatter holds %s at %s", squatAddr, squatMAC)

			// Declared where the squatter is planted, because this is
			// the only place that knows a conflict is coming. It does
			// not excuse the conflict -- the assertions below are the
			// conflict -- it excuses the health floor's counter being
			// allowed to under-report it by one after a later test
			// recycles the plugin, which the log outlives. Anything
			// beyond the declaration is still the seam dropping an
			// event (#524).
			harness.AllowStagedConflicts(1)

			cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
			if err != nil {
				t.Fatalf("docker client: %v", err)
			}
			defer cli.Close()

			declinesBefore := ef.CountLogLines("DHCPDECLINE")
			offersBefore := ef.CountLogLines("DHCPOFFER")

			w := harness.BeginCounterWindow(t, ctx, cli, "address_conflicts", "acd_probes_sent")

			harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
				"parent":         harness.EphemeralHostVeth,
				"conflict_check": mode,
			})
			id, ip, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
			t.Logf("endpoint bound: ip=%s mac=%s", ip, mac)

			// THE outside evidence, and the one the old datagram probe
			// could never produce: the DHCP server was told.
			if !awaitLogLines(t, ef, "DHCPDECLINE", declinesBefore+1, conflictWait) {
				t.Fatalf("the DHCP server never logged a DHCPDECLINE within %v.\n"+
					"A container is on an address another device holds and RFC 2131 section "+
					"3.1(5)'s MUST was not honoured — this is the #524 production fault, reproduced.",
					conflictWait)
			}
			if !awaitLogLines(t, ef, "DHCPOFFER", offersBefore+2, conflictWait) {
				t.Errorf("no fresh DHCPOFFER after the DECLINE; the server declined and then "+
					"offered nothing, so the container is not recovering onto another address "+
					"(offers before=%d now=%d)", offersBefore, ef.CountLogLines("DHCPOFFER"))
			}

			// The container's OWN view, which is the only one that can
			// be right after an address change: docker inspect reports
			// what was configured at CreateEndpoint and in async that
			// is the address being declined.
			final := awaitContainerAddr(t, ctx, id, squatAddr, conflictWait)
			if final == squatAddr {
				t.Fatalf("the container is still on the squatted address %s; it was declined "+
					"and nothing moved it", squatAddr)
			}
			t.Logf("container settled on %s (squatter holds %s)", final, squatAddr)

			// The wire says the check actually ran, rather than the
			// address changing for some unrelated reason.
			if probes := cap.ProbesFrom(mac); len(probes) == 0 {
				t.Errorf("no ARP Probe from the endpoint's MAC %s on the segment; the address "+
					"changed but RFC 5227 section 2.1 is not what changed it", mac)
			}

			after, ok := w.Await(conflictWait, func(now, before *harness.HealthResponse) bool {
				return now.AddressConflicts > before.AddressConflicts
			})
			if !ok {
				t.Errorf("address_conflicts never moved within %v, although the server logged a "+
					"DECLINE. The conflict happened and the plugin's health surface does not say "+
					"so — probes=%d healthy=%v", conflictWait, after.ACDProbesSent, after.Healthy)
			}
			if after.Healthy {
				t.Error("healthy is still true with a conflict recorded; /Plugin.Health is the " +
					"surface operators page on, and it is saying the endpoint is fine")
			}
			// The library's own count and the chassis's event-derived
			// one are two derivations of one population. They must
			// agree; a divergence is a seam defect, not a segment
			// property.
			if after.ACDConflictsDetected < after.AddressConflicts {
				t.Errorf("acd_conflicts_detected=%d is below address_conflicts=%d; the plugin "+
					"counted conflicts the library did not",
					after.ACDConflictsDetected, after.AddressConflicts)
			}

			// A window that is opened and never closed measured nothing
			// while looking exactly like one that passed, so the harness
			// fails the test for it. Closed here rather than deferred so
			// it runs before the plugin recycle registered above.
			w.End()
		})
	}
}

// TestConflictCheck_SquatterAfterTheFact is case (b) for the modes that
// look: RFC 5227 section 2.4, a conflict that arrives long after the
// address was checked and taken into use.
//
// Section 2.1 cannot cover this and never claimed to. The old datagram
// probe could not either, and said so in the documentation — "a
// collision that starts after the endpoint is up will not appear here".
// This is that gap closed, and the test that shows it closed.
func TestConflictCheck_SquatterAfterTheFact(t *testing.T) {
	for _, mode := range []string{"wait", "async"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()

			netName := "dh-itest-cc24-" + mode

			ef := harness.NewEphemeralFixture(t,
				harness.WithPool(squatAddr, altAddr),
				harness.WithParentAddress(harness.EphemeralParentAddr))
			cap := ef.StartARPCapture(t)
			t.Cleanup(func() {
				if t.Failed() {
					ef.DumpLogs(func(s string) { t.Log(s) })
					cap.Dump(func(s string) { t.Log(s) })
					harness.DumpPluginLog(t)
				}
			})
			recycleAfterDeliberateConflict(t)

			cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
			if err != nil {
				t.Fatalf("docker client: %v", err)
			}
			defer cli.Close()

			w := harness.BeginCounterWindow(t, ctx, cli, "address_conflicts", "acd_probes_sent")

			harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
				"parent":         harness.EphemeralHostVeth,
				"conflict_check": mode,
			})
			id, ip, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
			t.Logf("endpoint bound clean: ip=%s mac=%s", ip, mac)

			declinesBefore := ef.CountLogLines("DHCPDECLINE")

			// Now the squatter arrives, on the address the container is
			// already using, and puts a frame on the wire claiming it.
			squatMAC := ef.Squat(ip)
			t.Logf("squatter took the LIVE address %s at %s", ip, squatMAC)
			// One staged section 2.4 conflict; see the note in
			// TestConflictCheck_SquattedOfferIsDeclined.
			harness.AllowStagedConflicts(1)
			changeStart := time.Now()
			ef.AnnounceSquatter(ip, harness.EphemeralParentAddr[:strings.Index(harness.EphemeralParentAddr, "/")])

			if !awaitLogLines(t, ef, "DHCPDECLINE", declinesBefore+1, conflictWait) {
				t.Fatalf("no DHCPDECLINE within %v after a squatter took the live address.\n"+
					"RFC 5227 section 2.4's ongoing detection did not fire, so the container is "+
					"sharing %s with %s and nothing knows.", conflictWait, ip, squatMAC)
			}

			final := awaitContainerAddr(t, ctx, id, ip, conflictWait)
			gap := time.Since(changeStart)
			if final == ip {
				t.Fatalf("the container is still on the contested address %s after %v", ip, gap)
			}
			// The MEASUREMENT the handover records: how long the
			// container spent between the two addresses. Reported
			// whatever the outcome, because a number nobody prints is a
			// number nobody checks.
			t.Logf("MEASURED: address changed %s -> %s, %.1fs from the squatter's announcement "+
				"to the container carrying the new address (mode=%s)",
				ip, final, gap.Seconds(), mode)

			after, _ := w.Await(conflictWait, func(now, before *harness.HealthResponse) bool {
				return now.AddressConflicts > before.AddressConflicts
			})
			if after.AddressConflicts == w.Before().AddressConflicts {
				t.Errorf("address_conflicts did not move although the server logged a DECLINE")
			}
			w.End()
		})
	}
}

// TestConflictCheck_OffSendsNoProbe is the other half of case (b), and
// the one that can only be answered from the wire.
//
// conflict_check=off must run no probe and no listener. No counter can
// show that: zero probes sent and a plugin that forgot to send them are
// the same reading. The ARP capture is the instrument, and the
// assertion is the ABSENCE of a frame from the endpoint's own MAC.
//
// An absence assertion is worth exactly what its instrument is worth,
// and this one's first instrument was worthless: bound to the macvlan
// parent it could not see a macvlan child's transmits at all, so "no
// Probe from the container" was true on every run, probing or not. The
// capture now sits on the far end of the veth pair (see arpcapture.go),
// and the run is not allowed to conclude until the capture has proved
// itself alive on that link by recording the squatter's own frames.
//
// The squatter is real and takes the live address exactly as in the
// test above, so this is not "nothing happened on a quiet segment" —
// the same stimulus that changes the address in wait and async must
// change nothing here.
func TestConflictCheck_OffSendsNoProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	const netName = "dh-itest-cc-off"

	ef := harness.NewEphemeralFixture(t,
		harness.WithPool(squatAddr, altAddr),
		harness.WithParentAddress(harness.EphemeralParentAddr))
	cap := ef.StartARPCapture(t)
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			cap.Dump(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	// This shard leases an address on a network that will never probe.
	// Declared here, next to the conflict_check=off that causes it, so
	// the census gate judges only what is left — without this the
	// zero-probes finding would fire on a correct run and the fix
	// reached for would be to delete the gate (#551).
	harness.AllowUnprobedLeases(1)

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli, "address_conflicts", "acd_probes_sent")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent":         harness.EphemeralHostVeth,
		"conflict_check": "off",
	})
	id, ip, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	t.Logf("endpoint bound: ip=%s mac=%s (conflict_check=off)", ip, mac)

	declinesBefore := ef.CountLogLines("DHCPDECLINE")

	squatMAC := ef.Squat(ip)
	t.Logf("squatter took the LIVE address %s at %s", ip, squatMAC)
	ef.AnnounceSquatter(ip, harness.EphemeralParentAddr[:strings.Index(harness.EphemeralParentAddr, "/")])

	// Long enough for the whole mechanism to have run if it were going
	// to. The two tests above complete well inside this; waiting less
	// would let "off works" mean "we did not wait".
	time.Sleep(30 * time.Second)

	// The evidence-positive check first, because a frame that IS there
	// says something whatever the instrument's state is.
	sent := 0
	for _, f := range cap.FramesFrom(mac) {
		if f.IsProbe() || f.IsAnnouncement() {
			sent++
			t.Errorf("conflict_check=off sent %s — RFC 5227 ran on a network the "+
				"operator turned it off for", f)
		}
	}

	// The positive control gates the PASS, not the failure above. An
	// absence read off a dead instrument is a pass with no content, and
	// this assertion is the one carrying conflict_check=off. The
	// squatter shares the captured link and has just ARPed on it, so
	// its frames must be here; if they are not, the capture is not
	// watching the segment the container is on and "no probes" is a
	// verdict the instrument could not have reached otherwise.
	if sent == 0 {
		if live := cap.FramesFrom(squatMAC); len(live) == 0 {
			cap.Dump(func(s string) { t.Log(s) })
			t.Fatalf("the ARP capture recorded nothing from the squatter %s, which is on the "+
				"captured link and has just announced %s. The instrument is not seeing this "+
				"segment, so the no-probe result above is vacuous rather than favourable.",
				squatMAC, ip)
		}
		t.Logf("no RFC 5227 frame from %s, on a capture that recorded %d frame(s) from the "+
			"squatter %s on the same link", mac, len(cap.FramesFrom(squatMAC)), squatMAC)
	}
	if sent > 0 {
		cap.Dump(func(s string) { t.Log(s) })
	}
	if got := ef.CountLogLines("DHCPDECLINE"); got > declinesBefore {
		t.Errorf("conflict_check=off produced a DHCPDECLINE (%d -> %d); nothing was looking, "+
			"so nothing had grounds to decline", declinesBefore, got)
	}

	now := containerAddr(t, ctx, id)
	if now != ip {
		t.Errorf("the container's address changed from %s to %s with conflict_check=off; "+
			"the operator asked for the address to be left alone", ip, now)
	}

	before, after := w.End()
	if after.AddressConflicts > before.AddressConflicts {
		t.Errorf("address_conflicts moved with conflict_check=off")
	}
	if after.ACDProbesSent > before.ACDProbesSent {
		t.Errorf("acd_probes_sent moved with conflict_check=off: %d -> %d",
			before.ACDProbesSent, after.ACDProbesSent)
	}
}

// TestConflictCheck_WaitAcquisitionIsTimed is case (c): the price of
// the default, measured rather than asserted from the arithmetic.
//
// RFC 5227 section 2.1.1 costs PROBE_WAIT 1s + (PROBE_NUM-1) x up to
// PROBE_MAX 2s + ANNOUNCE_WAIT 2s: 4.0s best, 7.0s worst, 5.5s mean.
// RFC 2131 section 4.1 puts one DISCOVER retransmission of 4s +/-1s in
// front of it, so the chassis's own bound is 12.0s from the first
// DISCOVER, which is what defaultLeaseTimeout is derived from.
//
// The wire is the instrument: the ARP capture gives the interval from
// the first Probe to the last Announcement directly, with none of
// docker's container-start overhead in it. The end-to-end number is
// logged beside it because that is what an operator actually waits.
func TestConflictCheck_WaitAcquisitionIsTimed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	const netName = "dh-itest-cc-timed"

	ef := harness.NewEphemeralFixture(t, harness.WithPool(squatAddr, altAddr))
	cap := ef.StartARPCapture(t)
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			cap.Dump(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	w := harness.BeginCounterWindow(t, ctx, cli, "address_conflicts", "acd_probes_sent")

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent":         harness.EphemeralHostVeth,
		"conflict_check": "wait",
	})

	start := time.Now()
	_, ip, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	endToEnd := time.Since(start)

	probes, ok := cap.AwaitProbeFrom(mac, 1, 10*time.Second)
	if !ok {
		t.Fatalf("conflict_check=wait sent no ARP Probe from %s; the address was handed to the "+
			"container without being checked, which is D22 violated", mac)
	}
	anns := cap.AnnouncementsFrom(mac)
	if len(anns) == 0 {
		t.Errorf("no ARP Announcement from %s: section 2.1 completed but section 2.3 did not run, "+
			"so the segment was never told the address is taken", mac)
	}

	// MEASURED, printed whether or not it passes: a number nobody
	// prints is a number nobody checks.
	msg := "MEASURED: conflict_check=wait acquisition of " + ip
	if len(anns) > 0 {
		acd := anns[len(anns)-1].At.Sub(probes[0].At)
		t.Logf("%s — %.2fs on the wire from the first ARP Probe to the last Announcement "+
			"(%d probe(s), %d announcement(s)); RFC 5227 section 2.1.1 gives 4.0s best, "+
			"5.5s mean, 7.0s worst. End-to-end `docker run` to a configured address: %.2fs "+
			"(the chassis's own bound is 12.0s from the first DISCOVER, plus container start).",
			msg, acd.Seconds(), len(probes), len(anns), endToEnd.Seconds())

		// The RFC's own worst case, with a wide allowance for a loaded
		// runner. This is a bound on the MECHANISM, not a performance
		// assertion: what it catches is a probe schedule that has
		// silently become an order of magnitude longer, which would
		// blow through lease_timeout in production and show up as
		// intermittent `docker run` failures.
		if acd > 30*time.Second {
			t.Errorf("the section 2.1 window took %.2fs, far beyond the 7.0s worst case; "+
				"lease_timeout is derived from that arithmetic and would be wrong", acd.Seconds())
		}
	} else {
		t.Logf("%s — end-to-end %.2fs, no announcement captured", msg, endToEnd.Seconds())
	}

	before, after := w.End()
	if after.AddressConflicts > before.AddressConflicts {
		t.Error("address_conflicts moved on a segment with no squatter — a false positive. " +
			"Every endpoint on this network would be reported broken.")
	}
	if after.ACDProbesSent == before.ACDProbesSent {
		t.Error("acd_probes_sent did not move although the capture saw a Probe; the plugin's " +
			"counter and the wire disagree")
	}
}

// TestConflictCheck_RestartInsideTheAsyncWindow is case (d) and D23's
// durable half.
//
// In async the container has its address while section 2.1 is still
// running. If the plugin restarts inside that window the next process
// has to know the check never finished — which is why the ACD phase is
// written into the durable record and handed back on Resume. The
// evidence is on the wire: probes from the endpoint's MAC AFTER the
// plugin came back.
func TestConflictCheck_RestartInsideTheAsyncWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	const netName = "dh-itest-cc-restart"

	ef := harness.NewEphemeralFixture(t, harness.WithPool(squatAddr, altAddr))
	cap := ef.StartARPCapture(t)
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			cap.Dump(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent":         harness.EphemeralHostVeth,
		"conflict_check": "async",
	})
	id, ip, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	t.Logf("async endpoint bound at once: ip=%s mac=%s", ip, mac)

	// Recycle immediately. In async CreateEndpoint returns without
	// waiting for section 2.1, so the restart lands inside the window
	// by construction rather than by racing a sleep.
	restartAt := time.Now()
	if err := cliReset(ctx, t); err != nil {
		t.Fatalf("plugin restart: %v", err)
	}
	t.Logf("plugin restarted %.2fs after the endpoint was created", time.Since(restartAt).Seconds())

	// The container must still hold its address across the restart.
	// That is the recovery path this suite already covers; asserted
	// here so a restart that lost the lease cannot be mistaken for the
	// conflict machinery working.
	if now := containerAddr(t, ctx, id); now != ip {
		t.Fatalf("the container's address changed from %s to %s across the plugin restart, "+
			"which is a recovery failure and makes the rest of this test unreadable", ip, now)
	}

	// The wire, after the restart: the resumed client re-checks the
	// address rather than assuming a check that never finished.
	var after []harness.ARPFrame
	deadline := time.Now().Add(conflictWait)
	for time.Now().Before(deadline) {
		after = nil
		for _, f := range cap.FramesFrom(mac) {
			if f.At.After(restartAt) && (f.IsProbe() || f.IsAnnouncement()) {
				after = append(after, f)
			}
		}
		if len(after) > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(after) == 0 {
		cap.Dump(func(s string) { t.Log(s) })
		t.Errorf("no ARP Probe or Announcement from %s after the plugin restart.\n"+
			"The endpoint was handed an address while section 2.1 was still running and the "+
			"restart lost the fact — the container keeps an address nothing ever finished "+
			"checking (D23).", mac)
	} else {
		t.Logf("MEASURED: %d RFC 5227 frame(s) from %s after the restart, first at +%.2fs: %s",
			len(after), mac, after[0].At.Sub(restartAt).Seconds(), after[0])
	}
}

// TestConflictCheck_BridgeModeDoesNotSelfReport is the case that fails a
// naive implementation, and it survives the change of mechanism intact.
//
// In macvlan and ipvlan the parent cannot reach its own child, so any
// ARP reply is already somebody else's. In bridge mode the host CAN
// reach the container, and the container's own kernel answers for the
// address it is probing. A check that asked "did anything reply?" would
// report every single bridge-mode endpoint as a conflict, and the whole
// suite would go red for the fix rather than for the bug.
//
// Under RFC 5227 the exemption is the library's own-traffic filter,
// keyed on Params.CHAddr (M6 review r2, finding 1): a reply whose sender
// hardware address is the client's own is not a conflict. If the chassis
// ever passes something other than the link's address there, this is the
// test that goes red — which is why it is kept as its own test rather
// than folded into a table with the macvlan cases.
func TestConflictCheck_BridgeModeDoesNotSelfReport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	const netName = "dh-itest-cc-bridge"

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

	w := harness.BeginCounterWindow(t, ctx, cli, "address_conflicts", "acd_probes_sent")

	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{"conflict_check": "wait"})
	_, ip, mac := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	t.Logf("bridge endpoint bound: ip=%s mac=%s", ip, mac)

	after, ok := w.Await(conflictWait, func(now, before *harness.HealthResponse) bool {
		return now.ACDProbesSent > before.ACDProbesSent
	})
	if !ok {
		t.Fatalf("no ARP Probe was sent within %v; this run cannot show whether bridge mode "+
			"self-reports, because nothing looked", conflictWait)
	}
	if after.AddressConflicts > w.Before().AddressConflicts {
		t.Fatalf("bridge-mode endpoint reported itself as an address conflict.\n" +
			"The host can reach the container over a bridge, so our own endpoint answers the " +
			"probe — the library's own-traffic exemption is keyed on Params.CHAddr, and the " +
			"chassis is not filling it with the link's own hardware address.")
	}
	w.End()
}

// awaitLogLines waits for the fixture's DHCP server log to reach want
// occurrences of substr.
func awaitLogLines(t *testing.T, ef *harness.EphemeralFixture, substr string, want int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if ef.CountLogLines(substr) >= want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// containerAddr reads the address the CONTAINER has, not the one Docker
// recorded when the endpoint was created.
//
// The two differ exactly when this file's subject fires: an address
// change after CreateEndpoint leaves docker inspect stale, so asserting
// on it alone would report the conflict as unhandled when it was
// handled correctly.
func containerAddr(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "-o", "addr", "show", "dev", "eth0")
	for _, f := range strings.Fields(out) {
		if strings.Contains(f, ".") && strings.Contains(f, "/") {
			return strings.SplitN(f, "/", 2)[0]
		}
	}
	return ""
}

// awaitContainerAddr waits until the container's address is something
// other than was, and returns whatever it ends on.
func awaitContainerAddr(t *testing.T, ctx context.Context, id, was string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	last := was
	for {
		last = containerAddr(t, ctx, id)
		if last != was && last != "" {
			return last
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// cliReset recycles the plugin process, which is the only way to clear
// a counter — they are process-local by design. Used by the conflict
// tests to retire a fault induced on purpose, and by the restart case
// as the restart itself.
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
