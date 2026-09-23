// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// An endpoint leased an address another device already held must detect and decline it (#524). The library runs
// RFC 5227 on a raw ARP socket in the container's namespace, per network, and the DHCPDECLINE it sends (RFC 2131
// section 3.1(5)) and the fresh DHCPOFFER after it are read from the server's log. conflict_check=off is asserted from
// the ARP capture on the server end of the veth pair, because a macvlan parent cannot see its children's transmits.

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"
)

// RFC 5227 section 2.1's worst case is PROBE_WAIT 1s + 2 x PROBE_MAX 2s + ANNOUNCE_WAIT 2s = 7s, and a decline adds
// RFC 2131 section 3.1(5)'s ten-second restart and a fresh DORA; this is a ceiling on waiting.

// conflictWait is how long the assertions wait for RFC 5227 to reach a conclusion.
const conflictWait = 45 * time.Second

// The DHCP range holds two addresses and the squatter pins the first, so after the decline the server has exactly
// one other address to offer (#524).
const (
	squatAddr = "192.168.101.42"
	altAddr   = "192.168.101.43"
)

// address_conflicts is a fatal floor counter, absolute over the run, and counters are process-local, so only a
// plugin recycle clears a conflict a test induced (#524).

// recycleAfterDeliberateConflict recycles the plugin so a deliberately induced conflict does not fail the shard's floor.
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

// Wait and async differ in when the container is told its address, not in whether the conflict is found.

// TestConflictCheck_SquattedOfferIsDeclined checks that an offered address another host already holds is declined, in wait and in async (#524).
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

			// The squatter is parked before the container exists, as in the production incident (#524).
			squatMAC := ef.Squat(squatAddr)
			t.Logf("squatter holds %s at %s", squatAddr, squatMAC)

			// This excuses only the floor counter under-reporting the conflict by one after a later recycle (#524).
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

			// docker inspect reports what CreateEndpoint configured, which in async is the declined address.
			final := awaitContainerAddr(t, ctx, id, squatAddr, conflictWait)
			if final == squatAddr {
				t.Fatalf("the container is still on the squatted address %s; it was declined "+
					"and nothing moved it", squatAddr)
			}
			t.Logf("container settled on %s (squatter holds %s)", final, squatAddr)

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
			// The library's count and the chassis's event-derived one must agree on the v4 half: a DHCPv6 conflict is found by
			// Duplicate Address Detection and never reaches the RFC 5227 counter (#524).
			if after.ACDConflictsDetected < after.AddressConflictsV4 {
				t.Errorf("acd_conflicts_detected=%d is below address_conflicts_v4=%d; the plugin "+
					"counted conflicts the library did not",
					after.ACDConflictsDetected, after.AddressConflictsV4)
			}

			// Closed here so it runs before the plugin recycle registered above.
			w.End()
		})
	}
}

// TestConflictCheck_SquatterAfterTheFact checks that a conflict arriving after the address was taken into use is defended under RFC 5227 section 2.4 (#524).
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

			squatMAC := ef.Squat(ip)
			t.Logf("squatter took the LIVE address %s at %s", ip, squatMAC)
			// One staged section 2.4 conflict, as in TestConflictCheck_SquattedOfferIsDeclined (#524).
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

// A capture bound to the macvlan parent cannot see a child's transmits, so the capture sits on the far end of the veth
// pair and must record the squatter's own frames before an absence counts (#524).

// TestConflictCheck_OffSendsNoProbe checks from the wire that conflict_check=off sends no probe while a real squatter takes the live address.
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

	// Declared beside the conflict_check=off that causes it, so the census gate judges only the rest (#551).
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

	// Longer than the whole mechanism takes in the tests above, so "off" is not "did not wait".
	time.Sleep(30 * time.Second)

	sent := 0
	for _, f := range cap.FramesFrom(mac) {
		if f.IsProbe() || f.IsAnnouncement() {
			sent++
			t.Errorf("conflict_check=off sent %s — RFC 5227 ran on a network the "+
				"operator turned it off for", f)
		}
	}

	// The squatter shares the captured link and has just sent ARP on it, so no frame from it means the capture is not on
	// the container's segment (#524).
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

// RFC 5227 section 2.1.1 costs 4.0s best, 7.0s worst and 5.5s mean, and RFC 2131 section 4.1 puts one 4s +/-1s
// DISCOVER retransmission in front of it, so defaultLeaseTimeout derives from 12.0s after the first DISCOVER.

// TestConflictCheck_WaitAcquisitionIsTimed checks from the ARP capture that conflict_check=wait acquisition stays within the RFC 5227 bound.
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

	msg := "MEASURED: conflict_check=wait acquisition of " + ip
	if len(anns) > 0 {
		acd := anns[len(anns)-1].At.Sub(probes[0].At)
		t.Logf("%s — %.2fs on the wire from the first ARP Probe to the last Announcement "+
			"(%d probe(s), %d announcement(s)); RFC 5227 section 2.1.1 gives 4.0s best, "+
			"5.5s mean, 7.0s worst. End-to-end `docker run` to a configured address: %.2fs "+
			"(the chassis's own bound is 12.0s from the first DISCOVER, plus container start).",
			msg, acd.Seconds(), len(probes), len(anns), endToEnd.Seconds())

		// A bound on the mechanism: a probe schedule an order of magnitude longer would exceed lease_timeout in production.
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

// In async the container has its address while section 2.1 runs, so the ACD phase is written into the durable record
// and handed back on Resume. The pre-restart client's frames match the resumed client's by MAC, target and shape, so
// a frame counts only when captured after the plugin was confirmed back (run 33911095990, #524).

// TestConflictCheck_RestartInsideTheAsyncWindow checks that a plugin restart inside the async RFC 5227 window re-runs the probe.
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
	createdAt := time.Now()
	t.Logf("async endpoint bound at once: ip=%s mac=%s", ip, mac)

	// cliReset returns only once the plugin is back and the old process is gone, so frames after it are the resumed
	// client's. The DHCPACK count is a precondition that the RFC 2131 section 4.4.2 INIT-REBOOT exchange happened, not the
	// boundary: dnsmasq's log does not carry the request kind, so a late ACK to the pre-restart client counts the same.
	acksBefore := ef.CountLogLines("DHCPACK", mac)
	if acksBefore < 1 {
		t.Fatalf("the server logged no DHCPACK for %s before the restart: either the endpoint "+
			"never leased or this test cannot read the server's log, and the anchor below "+
			"would be an absence mistaken for an event", mac)
	}

	// In async CreateEndpoint returns before section 2.1 completes, so the restart lands inside the window.
	restartAt := time.Now()
	if err := cliReset(ctx, t); err != nil {
		t.Fatalf("plugin restart: %v", err)
	}
	backAt := time.Now()
	t.Logf("plugin recycled %.2fs after the endpoint was created; the disable/enable itself "+
		"took %.2fs", backAt.Sub(createdAt).Seconds(), backAt.Sub(restartAt).Seconds())

	if now := containerAddr(t, ctx, id); now != ip {
		t.Fatalf("the container's address changed from %s to %s across the plugin restart, "+
			"which is a recovery failure and makes the rest of this test unreadable", ip, now)
	}

	// A resumed probe runs after recovery, the INIT-REBOOT DHCPREQUEST (RFC 2131 section 4.4.2) and its ACK, so waiting
	// for the ACK separates a broken resume from one not yet done. ackAt is when the 100ms poll saw the count rise.
	var ackAt time.Time
	probesAtAnchor := -1
	deadline := time.Now().Add(conflictWait)
	for {
		if ef.CountLogLines("DHCPACK", mac) > acksBefore {
			ackAt = time.Now()
			probesAtAnchor = len(cap.ProbesFrom(mac))
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ackAt.IsZero() {
		cap.Dump(func(s string) { t.Log(s) })
		t.Fatalf("no new DHCPACK for %s within %s of the plugin coming back. The resumed "+
			"client never completed an INIT-REBOOT exchange, so an absence of probes below "+
			"would say nothing about D23 — this is a recovery failure, not a conflict-"+
			"detection one.", mac, conflictWait)
	}
	t.Logf("a DHCPACK the server had not logged before the restart was VISIBLE to this test "+
		"%.2fs after the plugin came back (detection, not the server's own stamp); %d Probe(s) "+
		"from %s had been captured up to that point",
		ackAt.Sub(backAt).Seconds(), probesAtAnchor, mac)

	// backAt, not ackAt, so the resumed client's first probe is not discarded. Only a section 2.1.1 Probe for this
	// address counts, because the kernel also emits an Announcement when an address is added to a link (#524).
	var after []harness.ARPFrame
	deadline = time.Now().Add(conflictWait)
	for {
		after = nil
		for _, f := range cap.ProbesFrom(mac) {
			if f.At.After(backAt) && f.TargetIP != nil && f.TargetIP.String() == ip {
				after = append(after, f)
			}
		}
		if len(after) > 0 || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(after) == 0 {
		cap.Dump(func(s string) { t.Log(s) })
		t.Errorf("no RFC 5227 section 2.1.1 ARP Probe for %s from %s after the plugin came "+
			"back (%d Probe(s) from this MAC in the whole capture, %d of them by the time the "+
			"resumed client's DHCPACK was visible).\n"+
			"The endpoint was handed an address while section 2.1 was still running and the "+
			"restart lost the fact — the container keeps an address nothing ever finished "+
			"checking (D23).", ip, mac, len(cap.ProbesFrom(mac)), probesAtAnchor)
	} else {
		t.Logf("MEASURED: %d section 2.1.1 Probe(s) for %s from %s after the plugin came back, "+
			"first at +%.2fs: %s",
			len(after), ip, mac, after[0].At.Sub(backAt).Seconds(), after[0])
	}
}

// In bridge mode the container's own kernel answers for the address it probes; the library's own-traffic filter keys
// on Params.CHAddr, so this fails if the chassis passes anything other than the link's address (#524).

// TestConflictCheck_BridgeModeDoesNotSelfReport checks that a bridge-mode endpoint does not report its own ARP reply as a conflict.
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

// awaitLogLines waits for the fixture's DHCP server log to reach want occurrences of substr.
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

// containerAddr reads the address the container has, which differs from docker inspect after an address change.
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

// awaitContainerAddr waits until the container's address is something other than was, and returns whatever it ends on.
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

// cliReset recycles the plugin process, which is the only way to clear its process-local counters.
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
