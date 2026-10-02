// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// Both fixtures run dnsmasq --dhcp-rapid-commit: a DISCOVER with option 80 gets the ACK and no OFFER, one without is
// answered as before; the observer is the server's log (#1031).

var rapidKindLine = regexp.MustCompile(`DHCP(DISCOVER|OFFER|REQUEST|ACK)\(`)

// serverKinds is the order of the DHCP message kinds the server logged for this MAC.
func serverKinds(logPath, mac string) []string {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	var kinds []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(strings.ToLower(line), strings.ToLower(mac)) {
			continue
		}
		if m := rapidKindLine.FindStringSubmatch(line); m != nil {
			kinds = append(kinds, m[1])
		}
	}
	return kinds
}

// wantFirstKinds waits for len(want) logged messages for the MAC, then requires the first of them to be want (#1031).
func wantFirstKinds(t *testing.T, logPath, mac string, want ...string) []string {
	t.Helper()
	var got []string
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if got = serverKinds(logPath, mac); len(got) >= len(want) {
			break
		}
	}
	if len(got) < len(want) || strings.Join(got[:len(want)], ",") != strings.Join(want, ",") {
		t.Errorf("the server logged %v for %s, want it to start with %v", got, mac, want)
	}
	return got
}

// wantFourMessageExchange waits for an ACK logged for the MAC, then requires the exchange to be (DISCOVER OFFER)+ REQUEST ACK (#1154).
func wantFourMessageExchange(t *testing.T, logPath, mac string) {
	t.Helper()
	var got []string
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if got = serverKinds(logPath, mac); slices.Contains(got, "ACK") {
			break
		}
	}
	if err := harness.FourMessageExchange(got); err != nil {
		t.Errorf("%s: %v", mac, err)
	}
}

func rapidCommitDump(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
}

func noOffer(t *testing.T, kinds []string, mac string) {
	t.Helper()
	for _, k := range kinds {
		if k == "OFFER" {
			t.Errorf("the server logged an OFFER for %s (%v) although the DISCOVER carried option 80", mac, kinds)
		}
	}
}

func assertPoolAddress(t *testing.T, ipv4 string, inPool func(net.IP) bool) {
	t.Helper()
	if ip := net.ParseIP(ipv4); ip == nil || !inPool(ip) {
		t.Errorf("the container holds %q, want an address of the fixture's pool", ipv4)
	}
}

// TestRapidCommit_MacvlanTakesTheTwoMessageLease checks that rapid_commit reaches the macvlan endpoint's DISCOVER (#1031).
func TestRapidCommit_MacvlanTakesTheTwoMessageLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rapidCommitDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-rc", "macvlan", map[string]string{"rapid_commit": "true"})
	_, ipv4, mac := harness.RunContainer(t, ctx, "dh-itest-rc", "dh-itest-rc-ctr")

	assertPoolAddress(t, ipv4, harness.IsInPool)
	noOffer(t, wantFirstKinds(t, fixture.DnsmasqLog(), mac, "DISCOVER", "ACK"), mac)
}

// TestRapidCommit_MacvlanWithoutTheOptionKeepsTheFourMessageExchange checks that a network without rapid_commit sends no option 80 (#1031).
func TestRapidCommit_MacvlanWithoutTheOptionKeepsTheFourMessageExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rapidCommitDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-rc-off", "macvlan", nil)
	_, ipv4, mac := harness.RunContainer(t, ctx, "dh-itest-rc-off", "dh-itest-rc-off-ctr")

	assertPoolAddress(t, ipv4, harness.IsInPool)
	wantFourMessageExchange(t, fixture.DnsmasqLog(), mac)
}

// TestRapidCommit_BridgeTakesTheTwoMessageLease checks that rapid_commit reaches the bridge-mode one-shot in CreateEndpoint (#1031).
func TestRapidCommit_BridgeTakesTheTwoMessageLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rapidCommitDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-rc-br", "bridge", map[string]string{"rapid_commit": "true"})
	_, ipv4, mac := harness.RunContainer(t, ctx, "dh-itest-rc-br", "dh-itest-rc-br-ctr")

	assertPoolAddress(t, ipv4, harness.IsInBridgePool)
	noOffer(t, wantFirstKinds(t, fixture.BridgeDnsmasqLogPath(), mac, "DISCOVER", "ACK"), mac)
}

// TestRapidCommit_BridgeWithoutTheOptionKeepsTheFourMessageExchange checks that a bridge network without rapid_commit sends no option 80 (#1031).
func TestRapidCommit_BridgeWithoutTheOptionKeepsTheFourMessageExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rapidCommitDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-rc-br-off", "bridge", nil)
	_, ipv4, mac := harness.RunContainer(t, ctx, "dh-itest-rc-br-off", "dh-itest-rc-br-off-ctr")

	assertPoolAddress(t, ipv4, harness.IsInBridgePool)
	wantFourMessageExchange(t, fixture.BridgeDnsmasqLogPath(), mac)
}

// TestRapidCommit_IPAMMacvlanTakesTheTwoMessageLease checks that rapid_commit reaches the IPAM-mode lease at RequestAddress (#1031).
func TestRapidCommit_IPAMMacvlanTakesTheTwoMessageLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	rapidCommitDump(t)

	harness.CreateNetworkIPAM(t, ctx, "dh-itest-rc-ipam", "macvlan", harness.SubnetCIDR, nil,
		map[string]string{"rapid_commit": "true"})
	_, ipv4, mac := harness.RunContainer(t, ctx, "dh-itest-rc-ipam", "dh-itest-rc-ipam-ctr")

	assertPoolAddress(t, ipv4, harness.IsInPool)
	noOffer(t, wantFirstKinds(t, fixture.DnsmasqLog(), mac, "DISCOVER", "ACK"), mac)
}
