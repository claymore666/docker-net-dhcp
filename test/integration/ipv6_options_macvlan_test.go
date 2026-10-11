// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	docker "github.com/moby/moby/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The macvlan twins of the bridge cases for ipv6_main_prefix, ipv6_auto_strict and ipv6_iid (#1016, #1032). ipvlan has
// no twin: those options need slaac or auto, which ipvlan refuses at create (refusals_test.go, #1319).

// TestMainPrefix_WhatDockerReportsFollowsTheNamedPrefix_Macvlan checks which formed address docker inspect reports (#1319).
func TestMainPrefix_WhatDockerReportsFollowsTheNamedPrefix_Macvlan(t *testing.T) {
	cases := []struct {
		name, net, main string
		reportsSecond   bool
	}{
		{"the second advertised prefix", "dh-itest-mpm1", harness.AdvertPrefixB, true},
		{"an unadvertised prefix reports the first", "dh-itest-mpm2", harness.UnadvertisedPrefix, false},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	a, b := mustPrefix(t, harness.AdvertPrefixA), mustPrefix(t, harness.AdvertPrefixB)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _ := startSenderSegment(t, harness.RangeArgsFor(harness.V6NoRA), slaacOn(
				advertPrefix(t, harness.AdvertPrefixA, true, 1800, 1800),
				advertPrefix(t, harness.AdvertPrefixB, true, 1800, 1800)))

			id, err := startOnV6SegmentAs(t, ctx, cli, f, onV6Macvlan, c.net,
				map[string]string{"ipv6": "", "ipv6_mode": "slaac", "ipv6_main_prefix": c.main})
			if err != nil {
				t.Fatalf("the container did not start with ipv6_main_prefix=%s: %v", c.main, err)
			}
			assertAttachedAs(t, ctx, f, id, onV6Macvlan)

			addrA, _ := awaitContainerV6(t, ctx, id, a, advertFormBudget())
			addrB, _ := awaitContainerV6(t, ctx, id, b, advertFormBudget())
			want := addrA
			if c.reportsSecond {
				want = addrB
			}
			if got := inspectV6(t, ctx, cli, id, c.net); got != want {
				t.Errorf("docker inspect reports %q with ipv6_main_prefix=%s; want %q (first prefix %q, second %q)",
					got, c.main, want, addrA, addrB)
			}
		})
	}
}

// TestAutoStrict_ASilentManagedServerFailsTheEndpointAndTheDefaultFallsBack_Macvlan is the bridge case on macvlan (#1319).
func TestAutoStrict_ASilentManagedServerFailsTheEndpointAndTheDefaultFallsBack_Macvlan(t *testing.T) {
	cases := []struct {
		name, net, strict string
		wantStart         bool
	}{
		{"strict", "dh-itest-aum1", "true", false},
		{"default", "dh-itest-aum2", "", true},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := harness.NewV6Fixture(t, harness.V6AutoFallback)
			dumpOnFailure(t, f)

			id, startErr := startOnV6SegmentAs(t, ctx, cli, f, onV6Macvlan, c.net,
				map[string]string{"ipv6": "", "ipv6_mode": "auto", "ipv6_auto_strict": c.strict})

			if !c.wantStart {
				if startErr == nil {
					t.Fatal("the container STARTED with ipv6_auto_strict=true on a segment that advertised " +
						"DHCPv6 and answered no Solicit; strict exists to fail exactly this endpoint")
				}
				if !strings.Contains(startErr.Error(), "via DHCPv6") {
					t.Fatalf("the start failed, but not for the silent DHCPv6 server: %v", startErr)
				}
				if got := inspectV6(t, ctx, cli, id, c.net); got != "" {
					t.Errorf("docker inspect reports %q for an endpoint that failed", got)
				}
				f.AwaitIgnoredSolicit(harness.IPAcquisitionBudget)
				assertServedOnSegment(t, f)
				return
			}

			if startErr != nil {
				t.Fatalf("the container did not start with ipv6_auto_strict unset; the default falls "+
					"back to the advertised prefix: %v", startErr)
			}
			assertAttachedAs(t, ctx, f, id, onV6Macvlan)
			addr, _ := awaitContainerV6(t, ctx, id, v6SegmentPrefix(t), slaacAddrBudget())
			if got := inspectV6(t, ctx, cli, id, c.net); got != addr {
				t.Errorf("docker inspect reports %q and the fallback formed %q", got, addr)
			}
			f.AwaitIgnoredSolicit(harness.IPAcquisitionBudget)
		})
	}
}

// TestSLAAC_StablePrivacyFormsAnAddressThatIsNotTheMACsAndSurvivesARestart_Macvlan is the bridge case on macvlan (#1319).
func TestSLAAC_StablePrivacyFormsAnAddressThatIsNotTheMACsAndSurvivesARestart_Macvlan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	f := harness.NewV6Fixture(t, harness.V6SLAAC)
	dumpOnFailure(t, f)
	prefix := v6SegmentPrefix(t)

	const netName = "dh-itest-iidm"
	id, err := startOnV6SegmentAs(t, ctx, cli, f, onV6Macvlan, netName,
		map[string]string{"ipv6": "", "ipv6_mode": "slaac", "ipv6_iid": "stable-privacy"})
	if err != nil {
		t.Fatalf("the container did not start on an ipv6_iid=stable-privacy segment: %v", err)
	}
	assertAttachedAs(t, ctx, f, id, onV6Macvlan)
	_, mac1 := ipamNetworkAddress(t, ctx, cli, id, netName)

	before, _ := awaitContainerV6(t, ctx, id, prefix, slaacAddrBudget())
	if got, eui := lowHalf(t, before), lowHalf(t, eui64(t, prefix, mac1)); string(got) == string(eui) {
		t.Errorf("the container holds %s, whose identifier is the modified EUI-64 of its MAC %s: "+
			"ipv6_iid=stable-privacy did not reach the client (#1319)", before, mac1)
	}
	if shown := inspectV6(t, ctx, cli, id, netName); shown != before {
		t.Errorf("docker inspect reports %q and the container holds %q", shown, before)
	}

	if _, err := cli.ContainerRestart(ctx, id, docker.ContainerRestartOptions{}); err != nil {
		t.Fatalf("ContainerRestart: %v", err)
	}
	_, mac2 := ipamNetworkAddress(t, ctx, cli, id, netName)
	if !strings.EqualFold(mac1, mac2) {
		t.Fatalf("the endpoint came back on MAC %s, it held %s: the restart does not exercise a formation "+
			"from the same inputs (#1319)", mac2, mac1)
	}
	after, _ := awaitContainerV6(t, ctx, id, prefix, slaacAddrBudget())
	if after != before {
		t.Errorf("the container held %s before docker restart and %s after it, on the same MAC and prefix: "+
			"a stable-privacy identifier has to come back unchanged (RFC 7217 section 5, #1319)", before, after)
	}
	f.AssertExchange(30 * time.Second)
}
