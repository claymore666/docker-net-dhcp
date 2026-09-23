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

// metricCounter reads one counter from the plugin's /metrics over its socket, so the series an operator scrapes must exist (#644).
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

// Every arm reads all three counters: two endings once moved one number, so an arm reading only its own would pass that (#816).
var v6FailureCounters = []string{"dhcpv6_refused", "dhcpv6_no_server", "dhcpv6_slaac_no_prefix"}

func readV6FailureCounters(t *testing.T, ctx context.Context, cli *docker.Client) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, n := range v6FailureCounters {
		out[n] = metricCounter(t, ctx, cli, n)
	}
	return out
}

// On both segments the RA sets the managed flag and `docker run` fails. On managed-exhausted the server answers with
// NoAddrsAvail (RFC 9915 section 21.13), on managed-silent nobody answers; each must move its own counter and no other,
// and each arm also reads the server's log for the exchange (#816).

// TestDHCPv6_RefusedAndSilentAreTwoRows checks that a refusing and a silent DHCPv6 server move different counters (#816).
func TestDHCPv6_RefusedAndSilentAreTwoRows(t *testing.T) {
	cases := []struct {
		name string
		mode harness.V6Mode
		net  string
		want string
		// The refusing arm sets ipv6_mode=dhcp and no ipv6, which is also the proof that the mode alone enables IPv6 (#817).
		opts     map[string]string
		evidence func(*harness.V6Fixture)
	}{
		{
			name: "the server answered and refused",
			mode: harness.V6ManagedExhausted,
			net:  "dh-itest-v6refused",
			want: "dhcpv6_refused",
			opts: map[string]string{"ipv6": "", "ipv6_mode": "dhcp"},
			evidence: func(f *harness.V6Fixture) {
				// The contract requires Solicit and Advertise and forbids Reply: the client was refused at the Advertise (#816).
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

	// #816 is about a pair, so both arms must have run; checked before the engine is touched.
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

// At plugin start Docker replays CreateNetwork with the operator's options, so an option refused or mis-normalised on
// replay takes every endpoint down at the next upgrade; the recovered client re-binds the same address and DUID (#817).

// TestIPv6Mode_SurvivesAPluginRestart checks that an ipv6_mode=dhcp network keeps its DHCPv6 address across a plugin restart (#817).
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

	// ipv6_mode=dhcp alone is the documented spelling, and the mode must imply IPv6 on the replay path too (#817).
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
