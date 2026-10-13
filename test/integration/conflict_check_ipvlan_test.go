// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestConflictCheck_IpvlanNeverHearsTheSquatter pins the documented limit that an ipvlan child does not receive the
// ARP replies to its own probes, so conflict_check cannot decline a squatted offer there (#1322, #1347).
//
// Each link of the chain is read before the absence counts: the probe on the wire, the squatter's reply on the wire,
// the server's offer of the squatted address, and then no DHCPDECLINE and the container on the squatted address.
func TestConflictCheck_IpvlanNeverHearsTheSquatter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	const netName = "dh-itest-cc-ipvlan"

	ef := harness.NewEphemeralFixture(t, harness.WithPool(squatAddr, altAddr))
	cap := ef.StartARPCapture(t)
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			cap.Dump(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	squatMAC := ef.Squat(squatAddr)
	declinesBefore := ef.CountLogLines("DHCPDECLINE")

	harness.CreateNetwork(t, ctx, netName, "ipvlan", map[string]string{
		"parent":         harness.EphemeralHostVeth,
		"conflict_check": "async",
	})
	id, ip, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
	mac := containerLinkMAC(t, ctx, id)
	if mac == "" {
		t.Fatal("could not read the container link's MAC, so its probes cannot be told from other frames")
	}

	// The probes run behind the bind under async; this waits out the whole window before the wire is counted below.
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		if got := ef.CountLogLines("DHCPDECLINE"); got > declinesBefore {
			t.Fatalf("a DHCPDECLINE reached the server (%d -> %d): conflict_check now works on ipvlan, so the "+
				"documented limit and this pin are out of date", declinesBefore, got)
		}
	}
	probed := 0
	for _, f := range cap.ProbesFrom(mac) {
		if f.TargetIP.String() == squatAddr {
			probed++
		}
	}
	if probed == 0 {
		t.Fatalf("no RFC 5227 Probe for %s from %s on the segment: the instrument saw no probe, so the missing "+
			"decline below says nothing about the limit", squatAddr, mac)
	}
	answered := 0
	for _, f := range cap.FramesFrom(squatMAC) {
		if f.Op == 2 && f.SenderIP.String() == squatAddr {
			answered++
		}
	}
	if answered == 0 {
		t.Fatalf("the squatter %s never answered for %s on the wire: the conflict was not on offer to be heard", squatMAC, squatAddr)
	}
	if ef.CountOffersOf(squatAddr) == 0 {
		t.Fatalf("the server never offered %s, so the squatted address was not the one in question", squatAddr)
	}
	t.Logf("probes for %s=%d, squatter replies=%d, offers of it=%d", squatAddr, probed, answered, ef.CountOffersOf(squatAddr))

	if ip != squatAddr {
		t.Errorf("docker inspect reports %s, want the squatted %s", ip, squatAddr)
	}
	if got := containerAddr(t, ctx, id); got != squatAddr {
		t.Errorf("the container carries %s, want the squatted %s", got, squatAddr)
	}
}
