// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink"

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

// refusalLogMsg is noteV6Absence's refusal line; the code name is also inside its error= field and Docker's relayed
// error, so the test reads the status_code field and not the name anywhere on the line (#816).
const refusalLogMsg = `msg="The DHCPv6 server refused this client; it answered and has no address for it"`

var (
	statusCodeField = regexp.MustCompile(`(?:^| )status_code=(\S+)`)
	endpointField   = regexp.MustCompile(`(?:^| )endpoint=([0-9a-f]{12})(?: |$)`)
	pluginHostLink  = regexp.MustCompile(`^dh-([0-9a-f]{12})$`)
)

// watchEndpointsJoining records, from rtnetlink, the 12-hex endpoint prefix of every plugin host link the kernel enslaves
// to bridge, so the endpoint a log line names is checked against the kernel and not against the log (#1016). The
// returned read waits up to budget for the first one; Close does not wake a blocked Receive, so nothing waits on the
// channel closing.
func watchEndpointsJoining(t *testing.T, bridge string) func(budget time.Duration) []string {
	t.Helper()
	br, err := netlink.LinkByName(bridge)
	if err != nil {
		t.Fatalf("look up the fixture bridge %s: %v", bridge, err)
	}
	updates := make(chan netlink.LinkUpdate, 256)
	done := make(chan struct{})
	var mu sync.Mutex
	var subErr error
	seen := map[string]bool{}
	if err := netlink.LinkSubscribeWithOptions(updates, done, netlink.LinkSubscribeOptions{
		ErrorCallback: func(err error) { mu.Lock(); subErr = err; mu.Unlock() },
	}); err != nil {
		t.Fatalf("subscribe to link updates: %v", err)
	}
	t.Cleanup(func() { close(done) })
	go func() {
		for u := range updates {
			a := u.Link.Attrs()
			if m := pluginHostLink.FindStringSubmatch(a.Name); m != nil && a.MasterIndex == br.Attrs().Index {
				mu.Lock()
				seen[m[1]] = true
				mu.Unlock()
			}
		}
	}()
	return func(budget time.Duration) []string {
		t.Helper()
		for deadline := time.Now().Add(budget); ; time.Sleep(50 * time.Millisecond) {
			mu.Lock()
			n, err := len(seen), subErr
			mu.Unlock()
			if err != nil {
				t.Fatalf("the link subscription failed, so which endpoint joined %s is unknown: %v", bridge, err)
			}
			if n > 0 || time.Now().After(deadline) {
				break
			}
		}
		mu.Lock()
		defer mu.Unlock()
		ids := make([]string, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		return ids
	}
}

// TestDHCPv6_ARefusalLogsItsStatusCodeByNameOnce checks that a NoAddrsAvail answer gives one log line naming the code beside the endpoint and one dhcpv6_refused (#816, #1016).
func TestDHCPv6_ARefusalLogsItsStatusCodeByNameOnce(t *testing.T) {
	// The line is written before CreateEndpoint answers, and the daemon waits 30 s for that answer, so ContainerStart
	// returning bounds the write; the 30 s read budget covers only the file reaching the disk (#868).
	const daemonCallDeadline = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 4*daemonCallDeadline)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6ManagedExhausted)
	dumpOnFailure(t, f)

	before := readV6FailureCounters(t, ctx, cli)
	mark := harness.MarkPluginLog(t, ctx)
	joined := watchEndpointsJoining(t, f.Bridge())

	_, err = startOnV6SegmentWithOpts(t, ctx, cli, f, "dh-itest-v6refusedlog", map[string]string{"ipv6": "", "ipv6_mode": "dhcp"})
	if err == nil {
		t.Fatal("the container STARTED on a managed segment whose server answers NoAddrsAvail")
	}
	f.AssertExchange(daemonCallDeadline)

	window := harness.AwaitPluginLogSince(t, ctx, mark, daemonCallDeadline, func(w string) bool {
		return strings.Contains(w, refusalLogMsg)
	})
	var lines []string
	for _, line := range strings.Split(window, "\n") {
		if strings.Contains(line, refusalLogMsg) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d refusal line(s) in the plugin log for one refused endpoint, want exactly 1:\n%s",
			len(lines), strings.Join(lines, "\n"))
	}
	line := lines[0]
	// The name is the library's rendering of RFC 9915 section 21.13's code 2, the one dnsmasq's static-only range sends.
	if m := statusCodeField.FindStringSubmatch(line); m == nil || m[1] != "NoAddrsAvail" {
		t.Errorf("the refusal line does not carry status_code=NoAddrsAvail as a field (got %v); "+
			"docs/reference.md says the log line names the code:\n%s", m, line)
	}
	// The kernel queued the enslave event before CreateEndpoint answered, inside the daemon's call deadline; the same
	// bound covers reading it, and the read returns at the first sighting (#1016).
	ids := joined(daemonCallDeadline)
	if len(ids) != 1 {
		t.Fatalf("the kernel reported %d plugin host link(s) %v joining %s for one container, want exactly 1, "+
			"so this endpoint's id is unknown", len(ids), ids, f.Bridge())
	}
	if m := endpointField.FindStringSubmatch(line); m == nil || m[1] != ids[0] {
		t.Errorf("the refusal line names endpoint %v; the kernel enslaved dh-%s to %s for this container, "+
			"so the line is not about this endpoint:\n%s", m, ids[0], f.Bridge(), line)
	}

	after := readV6FailureCounters(t, ctx, cli)
	for _, name := range v6FailureCounters {
		want := int64(0)
		if name == "dhcpv6_refused" {
			want = 1
		}
		if d := after[name] - before[name]; d != want {
			t.Errorf("%s moved by %d for one refused endpoint, want %d", name, d, want)
		}
	}
}
