// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// metricCounter reads one counter out of the plugin's own /metrics
// exposition, over the plugin's socket.
//
// READ FROM /metrics AND NOT FROM /Plugin.Health, deliberately. The two
// are served from the same fields, so this is not a second opinion
// about the number -- what it adds is that the row an operator's
// scraper will actually see EXISTS, is named what the reference says,
// and carries the value. #644's lesson in the other direction: a
// counter that only the health document carries is a counter no
// dashboard can plot.
func metricCounter(t *testing.T, ctx context.Context, cli *docker.Client, name string) int64 {
	t.Helper()
	body, _, err := harness.PluginMetrics(ctx, cli)
	if err != nil {
		t.Fatalf("reading /metrics: %v", err)
	}
	re := regexp.MustCompile(`(?m)^net_dhcp_` + regexp.QuoteMeta(name) + `_total (\d+)$`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no net_dhcp_%s_total row in the plugin's /metrics exposition. "+
			"docs/reference.md documents the counter, so either the metric was never "+
			"exposed or it was renamed on one side only.", name)
	}
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatalf("net_dhcp_%s_total is not a number: %q", name, m[1])
	}
	return v
}

// v6FailureCounters is the whole set #816 is about, read together.
//
// Together and not one at a time: the complaint is that two endings
// produced ONE number, so an arm that reads only its own counter would
// pass for a plugin that moved all of them at once. Every arm below
// reads the set and asserts on every member.
var v6FailureCounters = []string{"dhcpv6_refused", "dhcpv6_no_server", "dhcpv6_slaac_no_prefix"}

func readV6FailureCounters(t *testing.T, ctx context.Context, cli *docker.Client) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, n := range v6FailureCounters {
		out[n] = metricCounter(t, ctx, cli, n)
	}
	return out
}

// TestDHCPv6_RefusedAndSilentAreTwoRows is #816.
//
// TWO SEGMENTS THAT END THE SAME WAY FOR THE CONTAINER. On both of
// them the router advertises the managed-address flag, the endpoint
// gets no DHCPv6 address, and `docker run` fails. What differs is the
// fault, and it is the difference an operator has to act on:
//
//	managed-exhausted  the server ANSWERED and refused this client
//	                   (RFC 9915 section 21.13's Status Code,
//	                   NoAddrsAvail) -> go and look at the pool
//	managed-silent     no server answered at all -> go and look at
//	                   whether there is a server
//
// Before this, both moved nothing and read identically: one message,
// no counter, no code. The assertion is therefore not "a counter
// moved" but that each segment moves ITS OWN row and leaves the other
// two where they were -- a plugin that cannot tell the two apart
// passes any weaker form of this test.
//
// The plugin's counters are its own account of itself, so each arm
// also reads the SERVER's log through the fixture's exchange contract:
// an Advertise the server sent on the refusing segment, and a Solicit
// it ignored on the silent one.
func TestDHCPv6_RefusedAndSilentAreTwoRows(t *testing.T) {
	cases := []struct {
		name string
		mode harness.V6Mode
		net  string
		// want is the counter this segment must move by one.
		want string
		// opts is what the network is created with. The refusing arm
		// states `ipv6_mode=dhcp` and no `ipv6` at all, which is also
		// #817's end-to-end proof that the new option switches IPv6 on
		// by itself and reaches the client.
		opts map[string]string
		// evidence reads the server's own log for the exchange that
		// makes this segment the one it claims to be.
		evidence func(*harness.V6Fixture)
	}{
		{
			name: "the server answered and refused",
			mode: harness.V6ManagedExhausted,
			net:  "dh-itest-v6refused",
			want: "dhcpv6_refused",
			opts: map[string]string{"ipv6": "", "ipv6_mode": "dhcp"},
			evidence: func(f *harness.V6Fixture) {
				// The contract for this mode requires a DHCPSOLICIT and
				// a DHCPADVERTISE and forbids a DHCPREPLY: the server
				// answered, and the client never got as far as asking,
				// because it was refused at the Advertise.
				f.AssertExchange(30 * time.Second)
			},
		},
		{
			name: "no server answered",
			mode: harness.V6ManagedSilent,
			net:  "dh-itest-v6silent2",
			want: "dhcpv6_no_server",
			opts: nil,
			evidence: func(f *harness.V6Fixture) {
				f.AwaitIgnoredSolicit(30 * time.Second)
				f.AssertExchange(30 * time.Second)
			},
		},
	}

	// NON-VACUITY. #816 is a statement about a PAIR, so one arm proves
	// nothing: a counter that moves on one segment and is never read
	// on the other is exactly the shape the issue describes. The check
	// runs before the engine is touched, so it cannot be reported as
	// an environment failure.
	seen := map[string]bool{}
	for _, c := range cases {
		seen[c.want] = true
	}
	if len(seen) < 2 {
		t.Fatalf("the table moves %d distinct counter(s); #816 is the claim that a "+
			"refusal and a silence are different rows, and one row cannot carry it", len(seen))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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

			before := readV6FailureCounters(t, ctx, cli)

			_, err := startOnV6SegmentWithOpts(t, ctx, cli, f, tc.net, tc.opts)
			if err == nil {
				t.Fatalf("the container STARTED on a %s segment. The advertisement carried "+
					"the managed flag, so an address was offered over DHCPv6 and none was "+
					"obtained; starting anyway drops the IPv6 address an operator configured, "+
					"with nothing to say so. Do not relax this assertion.", tc.mode)
			}
			if !strings.Contains(err.Error(), "via DHCPv6") {
				t.Fatalf("the container failed on a %s segment, but not for the DHCPv6 "+
					"reason this test is about:\n%v", tc.mode, err)
			}

			// The server's own account, before the plugin's.
			tc.evidence(f)

			after := readV6FailureCounters(t, ctx, cli)
			for _, name := range v6FailureCounters {
				d := after[name] - before[name]
				switch {
				case name == tc.want && d < 1:
					t.Errorf("%s moved by %d on a %s segment, want at least 1. That row is "+
						"the only machine-readable difference between this fault and the "+
						"other one, and without it both read as \"no DHCPv6 address\".",
						name, d, tc.mode)
				case name != tc.want && d != 0:
					t.Errorf("%s moved by %d on a %s segment, want 0. The two faults are "+
						"different things to go and fix, and a plugin that moves both rows "+
						"tells an operator neither.", name, d, tc.mode)
				}
			}
		})
	}
}

// TestIPv6Mode_SurvivesAPluginRestart is the replay half of #817.
//
// Docker does not store a plugin's options for it: at plugin start it
// REPLAYS `CreateNetwork` for every network the driver owns, with the
// options the operator typed. So an option the plugin accepts at create
// and refuses on replay -- or normalises into something the second
// create cannot read -- takes every endpoint on that network down at
// the next plugin upgrade, and nothing before the upgrade says so.
//
// The observable is the endpoint, not the option: the recovered client
// re-binds the same address under the same DUID after the plugin comes
// back, which cannot happen if the replayed CreateNetwork failed.
func TestIPv6Mode_SurvivesAPluginRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	netName := "dh-itest-v6moderestart"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	// `ipv6_mode=dhcp` ALONE, with no `ipv6` beside it. That is the
	// spelling the reference documents, and it is the one that has to
	// survive: the plugin derives "this network has IPv6" from the mode
	// on every path, including the one a replay takes.
	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{"ipv6_mode": "dhcp"})
	id, _, _ := harness.RunContainer(t, ctx, netName, "dh-itest-v6moderestart-ctr")

	v6 := linkGlobalV6(t, ctx, id, harness.IPAcquisitionBudget)
	if v6 == "" {
		t.Fatal("no global IPv6 appeared on a network created with ipv6_mode=dhcp alone, " +
			"so the option did not switch IPv6 on or did not reach the client")
	}
	t.Logf("ipv6_mode=dhcp leased %s; restarting the plugin", v6)

	assertDUIDStableAcrossAPluginRestart(t, ctx, cli, v6)
}
