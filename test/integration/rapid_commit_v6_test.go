// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// dnsmasq answers every Solicit that carries option 14 with a Reply and has no DHCPv6 switch to turn that off, so the
// fixtures are unchanged and the "without" case on the same server is the only control. The observer is the server's log
// keyed on the endpoint's DUID; the Solicit repeats after a second, so presence is an ordered subsequence and absence
// is strict (#926).

var rapid6KindLine = regexp.MustCompile(`DHCP(SOLICIT|ADVERTISE|REQUEST|REPLY)\(\S+\)(?: (\S+))?`)

// rapid6Kinds is the DHCPv6 message kinds the server logged for this DUID, in order, and the first REPLY's address (#926).
func rapid6Kinds(logPath, duid string) (kinds []string, replyAddr string) {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return nil, ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(strings.ToLower(line), duid) {
			continue
		}
		m := rapid6KindLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		kinds = append(kinds, m[1])
		if m[1] == "REPLY" && replyAddr == "" && m[2] != "" && m[2] != duid {
			replyAddr = m[2]
		}
	}
	return kinds, replyAddr
}

func kindsInOrder(got, want []string) bool {
	i := 0
	for _, k := range got {
		if i < len(want) && k == want[i] {
			i++
		}
	}
	return i == len(want)
}

func waitRapid6Reply(t *testing.T, logPath, duid string) (kinds []string, replyAddr string) {
	t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if kinds, replyAddr = rapid6Kinds(logPath, duid); replyAddr != "" {
			return kinds, replyAddr
		}
	}
	t.Errorf("the server logged %v for %s and no DHCPREPLY with an address", kinds, duid)
	return kinds, ""
}

// assertTwoMessageV6 requires SOLICIT before the first REPLY and no ADVERTISE or REQUEST for the DUID at all (#926).
func assertTwoMessageV6(t *testing.T, kinds []string, duid string) {
	t.Helper()
	if !kindsInOrder(kinds, []string{"SOLICIT", "REPLY"}) {
		t.Errorf("the server logged %v for %s, want a SOLICIT and then a REPLY", kinds, duid)
	}
	for _, k := range kinds {
		if k == "ADVERTISE" || k == "REQUEST" {
			t.Errorf("the server logged %v for %s although the Solicit carried option 14", kinds, duid)
			return
		}
	}
}

func assertFourMessageV6(t *testing.T, kinds []string, duid string) {
	t.Helper()
	if !kindsInOrder(kinds, []string{"SOLICIT", "ADVERTISE", "REQUEST", "REPLY"}) {
		t.Errorf("the server logged %v for %s, want SOLICIT, ADVERTISE, REQUEST, REPLY in order", kinds, duid)
	}
}

// assertHeldV6 requires the container to hold the address the server's REPLY carried (#926).
func assertHeldV6(t *testing.T, ctx context.Context, ctrID, replyAddr string) {
	t.Helper()
	if replyAddr == "" {
		return
	}
	if linkGlobalV6(t, ctx, ctrID, harness.IPAcquisitionBudget) == "" {
		t.Errorf("the container holds no global IPv6 address, want %s", replyAddr)
		return
	}
	out := strings.ToLower(harness.ExecOutput(t, ctx, ctrID, "ip", "-6", "addr", "show", "scope", "global"))
	if !strings.Contains(out, strings.ToLower(replyAddr)+"/") {
		t.Errorf("the container's global addresses are %q, want the REPLY's %s", out, replyAddr)
	}
}

// TestRapidCommitV6_MacvlanTakesTheTwoMessageLease checks that rapid_commit reaches the macvlan endpoint's Solicit and its DISCOVER (#926, #1031).
func TestRapidCommitV6_MacvlanTakesTheTwoMessageLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rapidCommitDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-rc6", "macvlan", map[string]string{"rapid_commit": "true", "ipv6_mode": "dhcp"})
	id, ipv4, mac := harness.RunContainer(t, ctx, "dh-itest-rc6", "dh-itest-rc6-ctr")
	duid := "00:03:00:01:" + strings.ToLower(mac)

	assertPoolAddress(t, ipv4, harness.IsInPool)
	noOffer(t, wantFirstKinds(t, fixture.DnsmasqLog(), mac, "DISCOVER", "ACK"), mac)
	kinds, addr := waitRapid6Reply(t, fixture.DnsmasqLog(), duid)
	assertTwoMessageV6(t, kinds, duid)
	assertHeldV6(t, ctx, id, addr)
}

// TestRapidCommitV6_MacvlanWithoutTheOptionKeepsTheFourMessageExchange checks that a network without rapid_commit sends no option 14, on a server that would answer it (#926).
func TestRapidCommitV6_MacvlanWithoutTheOptionKeepsTheFourMessageExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rapidCommitDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-rc6-off", "macvlan", map[string]string{"ipv6_mode": "dhcp"})
	id, ipv4, mac := harness.RunContainer(t, ctx, "dh-itest-rc6-off", "dh-itest-rc6-off-ctr")
	duid := "00:03:00:01:" + strings.ToLower(mac)

	assertPoolAddress(t, ipv4, harness.IsInPool)
	wantFirstKinds(t, fixture.DnsmasqLog(), mac, "DISCOVER", "OFFER", "REQUEST", "ACK")
	kinds, addr := waitRapid6Reply(t, fixture.DnsmasqLog(), duid)
	assertFourMessageV6(t, kinds, duid)
	assertHeldV6(t, ctx, id, addr)
}

// TestRapidCommitV6_BridgeTakesTheTwoMessageLease checks that rapid_commit reaches the bridge-mode one-shot's Solicit (#926).
func TestRapidCommitV6_BridgeTakesTheTwoMessageLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rapidCommitDump(t)

	harness.CreateNetwork(t, ctx, "dh-itest-rc6-br", "bridge", map[string]string{"rapid_commit": "true", "ipv6_mode": "dhcp"})
	id, ipv4, mac := harness.RunContainer(t, ctx, "dh-itest-rc6-br", "dh-itest-rc6-br-ctr")
	duid := "00:03:00:01:" + strings.ToLower(mac)

	assertPoolAddress(t, ipv4, harness.IsInBridgePool)
	noOffer(t, wantFirstKinds(t, fixture.BridgeDnsmasqLogPath(), mac, "DISCOVER", "ACK"), mac)
	kinds, addr := waitRapid6Reply(t, fixture.BridgeDnsmasqLogPath(), duid)
	assertTwoMessageV6(t, kinds, duid)
	assertHeldV6(t, ctx, id, addr)
}

// TestRapidCommitV6_IPAMMacvlanTakesTheTwoMessageLease checks that rapid_commit reaches the IPAM-mode DHCPv6 lease, the fifth client-options site (#926).
func TestRapidCommitV6_IPAMMacvlanTakesTheTwoMessageLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	rapidCommitDump(t)

	ipam6Network(t, ctx, "dh-itest-rc6-ipam", map[string]string{"rapid_commit": "true", "ipv6_mode": "dhcp"})
	id, ipv4, mac := harness.RunContainer(t, ctx, "dh-itest-rc6-ipam", "dh-itest-rc6-ipam-ctr")
	duid := "00:03:00:01:" + strings.ToLower(mac)

	assertPoolAddress(t, ipv4, harness.IsInPool)
	noOffer(t, wantFirstKinds(t, fixture.DnsmasqLog(), mac, "DISCOVER", "ACK"), mac)
	kinds, addr := waitRapid6Reply(t, fixture.DnsmasqLog(), duid)
	assertTwoMessageV6(t, kinds, duid)
	assertHeldV6(t, ctx, id, addr)
}
