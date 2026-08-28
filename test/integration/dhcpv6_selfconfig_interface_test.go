// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// The self-configuring half of the IPv6 release bar (#875): a segment
// that hands out addresses by router advertisement rather than by
// DHCPv6 -- SLAAC, or stateless DHCPv6 where the advertisement supplies
// the address and DHCPv6 supplies only configuration.
//
// #868 made a container START on these segments. It did not make IPv6
// WORK on them, and this file is the difference between those two
// statements.
//
// WHAT IS ALREADY KNOWN, AND WHY THAT IS NOT ENOUGH. Measured in an
// isolated network namespace with the plugin's own dhcpcd argv: a
// global address forms from the first advertisement, and from the
// moment dhcpcd writes accept_ra=0 and autoconf=0 the kernel's
// lifetimes only count down -- 1797 -> 1787 -> 1647 against a no-dhcpcd
// control reading 1797 -> 1796 -> 1705 -- while the RA-derived default
// route disappears within about ten seconds. That is honest evidence
// about a LINK. It is not evidence about a CONTAINER on this plugin,
// and the release bar is about containers. Everything below is read
// from inside one.
//
// WHY IT HAS NEVER BEEN CAUGHT. The address is present the whole time,
// so anything that looks once passes. On-link ping6 keeps succeeding
// with no default route at all, so the obvious reachability check
// passes too. The two things that actually fail are invisible to a
// single sample: whether the lifetime is being refreshed, and whether
// anything OFF-link is reachable.

// The observation window, its cadence and the refresh floor live in
// the harness beside the verdict that reads them, so the table that
// drives that verdict drives the PRODUCTION values rather than numbers
// of its own. See harness.V6ObserveWindow / harness.V6RefreshFloor.
const (
	selfConfigWindow   = harness.V6ObserveWindow
	selfConfigInterval = harness.V6ObserveInterval
)

// selfConfigFormBy bounds how long self-configuration may take. DAD on
// the link-local is one RetransTimer (1s by default), and the
// solicit/advertise exchange that follows is one round trip on a veth.
// 60s is far past both and is a bound on something being WRONG, not a
// tuned wait.
const selfConfigFormBy = 60 * time.Second

// selfConfigSample is one observation of the container's v6 state.
type selfConfigSample struct {
	elapsed time.Duration
	addr    string // ip -6 addr, UNFILTERED: no device is named
	links   string // ip link, so the log always carries the real names
	route   string

	// The sysctls are RECORDED AND NOT ASSERTED ON, deliberately.
	// They are the mechanism this defect happens to run through; the
	// bar is about the address and the route. A fix that keeps the
	// address refreshing and the route present by some other means is
	// a fix, and a test keyed on the sysctl values would fail it.
	// They are logged because they are the first thing a reader of a
	// failure will want.
	acceptRA string
	autoconf string
}

// lftReadings projects the samples onto what the ledger reads: the
// elapsed clock and the UNFILTERED address output, nothing else. The
// sysctls and the route deliberately do not cross this boundary -- the
// verdict is about the address, and a verdict that could see the
// sysctls would be tempted to key on them.
func lftReadings(samples []selfConfigSample) []harness.V6LftReading {
	out := make([]harness.V6LftReading, 0, len(samples))
	for _, s := range samples {
		out = append(out, harness.V6LftReading{Elapsed: s.elapsed, Addr: s.addr})
	}
	return out
}

// TestDHCPv6_SelfConfiguring_AddressAndRouteSurvive is the SLAAC and
// stateless half of #875.
//
// Each arm gets its OWN segment and its OWN container and asserts
// against those. There is no aggregate claim here: "self-configuring
// segments work" is not the same statement as "each of them works",
// and the fixture's own header says why that distinction is the one
// that matters.
func TestDHCPv6_SelfConfiguring_AddressAndRouteSurvive(t *testing.T) {
	cases := []struct {
		name string
		mode harness.V6Mode
		net  string
	}{
		{"slaac", harness.V6SLAAC, "dh-itest-v6slif"},
		{"stateless", harness.V6Stateless, "dh-itest-v6slsif"},
	}

	// NON-VACUITY. This table IS the acceptance criterion for the
	// self-configuring half of #875 -- emptying it leaves this test
	// green, the lane green and check-test-weakening.sh clean, and
	// "IPv6 works on self-configuring networks" would then be a
	// statement about no network at all.
	//
	// Keyed on the two modes rather than on a row count, because a
	// duplicated row satisfies a count. Both are needed and they are
	// not interchangeable: SLAAC has no DHCPv6 server on the segment
	// at all, stateless has one that answers configuration but hands
	// out no address, and the plugin runs a DHCPv6 client on both. A
	// fix that restored advertisements only on the path where no
	// DHCPv6 client ever bound would pass a SLAAC-only table.
	//
	// It runs BEFORE the fixture and the engine are touched, so it is
	// reachable without root and cannot be reported as an environment
	// failure.
	want := map[harness.V6Mode]bool{
		harness.V6SLAAC:     false,
		harness.V6Stateless: false,
	}
	for _, tc := range cases {
		want[tc.mode] = true
	}
	for mode, present := range want {
		if !present {
			t.Fatalf("no arm for the %s segment. It is one of the two ways a network "+
				"hands out an address by advertisement, and an arm that is not here "+
				"is a shape nothing checks", mode)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := harness.NewV6Fixture(t, tc.mode)
			dumpOnFailure(t, f)

			id, err := startOnV6Segment(t, ctx, cli, f, tc.net)
			if err != nil {
				t.Fatalf("a container failed to start on a %s segment. #868 made this "+
					"work; nothing below can be measured:\n%v", tc.mode, err)
			}

			ifname := containerIfname(t, ctx, id)
			t.Logf("%s segment: container interface discovered as %q", tc.mode, ifname)

			start := time.Now()
			var samples []selfConfigSample
			for {
				elapsed := time.Since(start)
				s := selfConfigSample{
					elapsed:  elapsed,
					addr:     harness.ExecOutput(t, ctx, id, "ip", "-6", "addr"),
					links:    harness.ExecOutput(t, ctx, id, "ip", "link"),
					route:    harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show"),
					acceptRA: harness.ExecOutput(t, ctx, id, "cat", fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", ifname)),
					autoconf: harness.ExecOutput(t, ctx, id, "cat", fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/autoconf", ifname)),
				}
				samples = append(samples, s)
				if elapsed >= selfConfigWindow {
					break
				}
				time.Sleep(selfConfigInterval)
			}

			f0, fN := samples[0], samples[len(samples)-1]
			t.Logf("=== %s segment, t+0 ===\nip -6 addr:\n%s\nip -6 route:\n%s\n"+
				"accept_ra=%s autoconf=%s",
				tc.mode, f0.addr, f0.route,
				strings.TrimSpace(f0.acceptRA), strings.TrimSpace(f0.autoconf))
			t.Logf("=== %s segment, t+%s ===\nip -6 addr:\n%s\nip -6 route:\n%s\n"+
				"accept_ra=%s autoconf=%s",
				tc.mode, fN.elapsed.Round(time.Second), fN.addr, fN.route,
				strings.TrimSpace(fN.acceptRA), strings.TrimSpace(fN.autoconf))

			// ---- S1: does an address FORM, in bounded time, usable? ----
			//
			// This asserted on samples[0], at t+0, and failed there on a
			// segment that does configure itself correctly. That is not
			// a product defect, it is a premise no host can satisfy:
			// at t+0 the link-local is still `tentative`, and until DAD
			// completes there is no source address to send a Router
			// Solicitation from, so no advertisement has been answered
			// and no global address can exist yet. The run that caught
			// this shows exactly that -- link-local `tentative` and no
			// global at t+0, both address and default route present
			// later in the same container.
			//
			// The bar is not "instantly"; it is that the address APPEARS
			// and then STAYS usable. So the formation is bounded rather
			// than instantaneous, and the durability assertions below
			// carry the rest. Bounding it is STRONGER than asserting at
			// t+0, which could only ever fail: an address that never
			// arrives still fails here, and one that arrives and later
			// rots still fails S3/S4/S5.
			var a0 harness.V6Addr
			var formed *selfConfigSample
			for i := range samples {
				if a, ok := harness.GlobalInSubnet(harness.ParseV6Addrs(samples[i].addr), harness.V6SubnetV6CIDR); ok {
					a0, formed = a, &samples[i]
					break
				}
			}
			if formed == nil {
				// WHICH DEFECT IS THIS? Two of them produce exactly this
				// symptom and they have different owners.
				//
				// On a stateless or SLAAC segment the endpoint carries no
				// IPv6 address, and the engine disables IPv6 outright on a
				// sandbox interface whose endpoint has none. The plugin
				// clears that switch before starting its DHCPv6 client
				// (#868). If that clear failed, the container has no IPv6
				// on the interface at all, and "no global address" says
				// nothing whatever about advertisement processing.
				//
				// The kernel forms a link-local on any IPv6-enabled
				// interface without needing a router, so its presence
				// settles which of the two this is. Reported rather than
				// asserted separately, because the verdict is the same
				// either way -- what changes is who reads the failure.
				ll, haveLL := harness.LinkLocal(harness.ParseV6Addrs(fN.addr))
				attribution := fmt.Sprintf("The interface HAS a link-local (%s), so IPv6 is "+
					"enabled on it and the missing global address is about advertisements "+
					"not being acted on — #875, this test's subject.", ll.CIDR)
				if !haveLL {
					attribution = "The interface has NO IPv6 link-local either, so IPv6 is " +
						"administratively disabled on it and nothing downstream is a " +
						"statement about #875. That is #868's enable path failing — check " +
						"ipv6_link_enable_failures before reading anything else here."
				}
				t.Fatalf("S1 FAILED: no global IPv6 address in the fixture's subnet %s ever "+
					"appeared on ANY interface inside the container across %s, on a %s "+
					"segment whose router advertisements carry that prefix.\n\n%s\n\n"+
					"Last state:\nip -6 addr:\n%s\nip link:\n%s",
					harness.V6SubnetV6CIDR, selfConfigWindow, tc.mode, attribution,
					fN.addr, fN.links)
			}
			if formed.elapsed > selfConfigFormBy {
				t.Errorf("S1 FAILED: the address %s took %s to appear, past the %s bound. "+
					"Self-configuration is a link-local DAD plus one solicit/advertise "+
					"exchange; taking this long means something is retrying, not "+
					"configuring.", a0.CIDR, formed.elapsed.Round(time.Second), selfConfigFormBy)
			}
			t.Logf("S1: address %s formed by t+%s", a0.CIDR, formed.elapsed.Round(time.Second))
			for _, bad := range []string{"tentative", "dadfailed", "deprecated"} {
				if strings.Contains(a0.Flags, bad) {
					t.Errorf("S1 FAILED: the address %s is %s (%q) — it is on the interface "+
						"but not usable as a source address", a0.CIDR, bad, a0.Flags)
				}
			}

			// ---- S2: does the segment agree it advertised? ----
			//
			// Outside evidence, and the negative control for S3/S4: if
			// the server never advertised, then a missing route says
			// nothing about the plugin. The fixture already asserts an
			// advertisement at startup; this asserts it kept going,
			// which is the premise every claim below rests on.
			if n := f.CountLogLines("RTR-ADVERT("); n < 2 {
				t.Errorf("S2: the server logged %d router advertisements across %s, want at "+
					"least 2. Without a second advertisement there is nothing that COULD "+
					"have refreshed the address, so the refresh claim below is not a "+
					"statement about the plugin", n, selfConfigWindow)
			}

			// ---- S3: does the address stay valid? ----
			t.Logf("S3: lifetime ledger on the %s segment", tc.mode)
			harness.AssertV6LifetimeRefreshed(t, lftReadings(samples), tc.mode,
				harness.V6SubnetV6CIDR, selfConfigWindow)

			// ---- S4: is there a default route, at EVERY sample? ----
			//
			// Not just at the end: the measured shape is a route that
			// is present at +3s and gone by +13s, so a test that looked
			// only at the ends would still catch it but would not say
			// WHEN it went, and a fix that reinstalled the route late
			// would look identical to one that never lost it.
			for _, s := range samples {
				if !harness.HasDefaultV6Route(s.route) {
					t.Errorf("S4 FAILED at t+%s on the %s segment: the container has NO IPv6 "+
						"default route. On-link traffic still works, which is what hides "+
						"this; nothing off-link is reachable over IPv6:\n%s",
						s.elapsed.Round(time.Second), tc.mode, s.route)
				}
			}

			// ---- S5: off-link reachability ----
			//
			// On-link ping6 succeeds with no default route at all, so
			// it proves nothing. This targets a different /64 to force
			// the route to be consulted, and reads the ERROR CLASS
			// rather than reachability: nothing answers at the far end
			// either way. "Network unreachable" is the kernel refusing
			// for want of a route; a timeout means a route existed and
			// was used.
			const offLink = "fd00:6470:6865:ffff::1"
			ping := harness.ExecOutput(t, ctx, id, "ping", "-6", "-c", "1", "-W", "2", offLink)
			t.Logf("S5: ping -6 %s from the container:\n%s", offLink, ping)
			if strings.Contains(strings.ToLower(ping), "unreachable") {
				t.Errorf("S5 FAILED on the %s segment: sending to the off-link address %s "+
					"failed with a network-unreachable error, which is the kernel saying "+
					"it has no route at all — not the far end being silent. IPv6 is "+
					"on-link only:\n%s", tc.mode, offLink, ping)
			}
		})
	}
}
