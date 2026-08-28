// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// startOnV6Segment brings a container up on a fresh network over the
// given fixture's bridge and returns the ContainerStart error, or nil.
//
// It deliberately does NOT fail the test on a start error: three of the
// tests in this file are about whether the endpoint is created at all,
// and for them the error IS the measurement. It returns the container
// id so callers that expect a running container can look inside it.
func startOnV6Segment(t *testing.T, ctx context.Context, cli *docker.Client, f *harness.V6Fixture, netName string) (string, error) {
	t.Helper()
	harness.CreateNetwork(t, ctx, netName, "bridge", map[string]string{
		"bridge":        f.Bridge(),
		"ipv6":          "true",
		"propagate_dns": "true",
	})
	ctrName := netName + "-ctr"
	create, err := cli.ContainerCreate(ctx,
		&container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: ctrName},
		harness.HostConfig(),
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {}},
		},
		nil, ctrName)
	if err != nil {
		t.Fatalf("ContainerCreate(%s): %v", ctrName, err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.Background(), create.ID, container.RemoveOptions{Force: true})
	})
	return create.ID, cli.ContainerStart(ctx, create.ID, container.StartOptions{})
}

// dumpOnFailure wires the fixture's and the plugin's logs to a failing
// test. Every subtest here needs it and none of them needs anything
// else from its cleanup.
func dumpOnFailure(t *testing.T, f *harness.V6Fixture) {
	t.Cleanup(func() {
		if t.Failed() {
			f.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
}

// TestDHCPv6_NoAddressModes_StartTheEndpoint is the fix for #868.
//
// On an IPv6 network whose router advertisements hand out no DHCPv6
// address — stateless, SLAAC, or a segment with no router on it at all
// — a container must start. Before this, CreateEndpoint treated a
// DHCPv6 acquisition failure as fatal for the whole endpoint, so on
// exactly those networks nothing could ever come up. This file used to
// pin that defect by asserting the refusal; the assertions are now
// flipped, which is what the pinned version was written to make happen.
//
// WHAT SEPARATES THE THREE ARMS. Tolerating a missing DHCPv6 address is
// correct only when the segment said there was none, so the plugin
// classifies the absence rather than swallowing it, and the two health
// counters are the operator's view of that classification:
//
//	stateless / slaac  RA arrived, M flag clear  -> dhcpv6_not_offered
//	no router at all   no RA arrived             -> dhcpv6_no_router_advert
//
// Each arm asserts its OWN counter moved and the other did not. A
// single "some v6 counter moved" assertion would be satisfied by a
// plugin that cannot tell the two situations apart, which is the whole
// distinction #868's fix rests on.
//
// The fatal case has its own test: TestDHCPv6_Managed_ServerSilent_IsStillFatal.
func TestDHCPv6_NoAddressModes_StartTheEndpoint(t *testing.T) {
	// Each mode gets its own segment and its own container, and asserts
	// against those. There is no aggregate claim here: "these modes
	// start" is not the same statement as "each of them starts".
	cases := []struct {
		name string
		mode harness.V6Mode
		net  string
		// wantRA is whether an advertisement reaches the client on
		// this segment, which is exactly what decides which counter
		// the plugin should move.
		wantRA bool
	}{
		{"stateless", harness.V6Stateless, "dh-itest-v6sl", true},
		{"slaac", harness.V6SLAAC, "dh-itest-v6slaac", true},
		{"nora", harness.V6NoRA, "dh-itest-v6nora", false},
	}

	// NON-VACUITY, and this table is #868's acceptance criterion: the
	// three network shapes on which no container could start. Emptying
	// it leaves this test green, the lane green and
	// check-test-weakening.sh clean -- "the fix is verified end to end"
	// would then be a statement about nothing.
	//
	// It runs BEFORE the fixture and the engine are touched, so it is
	// reachable without root and cannot be reported as an environment
	// failure.
	//
	// Keyed on the modes and on both wantRA polarities rather than on a
	// row count, because the two counters exist precisely to tell an
	// advertised absence from an absent advertisement, and a table
	// holding only one polarity cannot see that distinction at all.
	want := map[harness.V6Mode]bool{
		harness.V6Stateless: false,
		harness.V6SLAAC:     false,
		harness.V6NoRA:      false,
	}
	polarities := map[bool]int{}
	for _, tc := range cases {
		want[tc.mode] = true
		polarities[tc.wantRA]++
	}
	for mode, present := range want {
		if !present {
			t.Fatalf("no arm for the %s segment. All three are shapes on which #868 "+
				"refused every container, and an arm that is not here is a shape "+
				"nothing checks", mode)
		}
	}
	if polarities[true] < 1 || polarities[false] < 1 {
		t.Fatalf("the table has %d arms expecting a router advertisement and %d "+
			"expecting none; both are needed, since the whole discrimination under "+
			"test is between an advertised absence and an absent advertisement",
			polarities[true], polarities[false])
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

			w := harness.BeginCounterWindow(t, ctx, cli,
				"dhcpv6_not_offered", "dhcpv6_no_router_advert")

			_, err := startOnV6Segment(t, ctx, cli, f, tc.net)
			if err != nil {
				t.Fatalf("the container did not start on a %s segment, which is the "+
					"defect #868 describes:\n%v", tc.mode, err)
			}

			before, after := w.End()
			notOffered := after.DHCPv6NotOffered - before.DHCPv6NotOffered
			noRouter := after.DHCPv6NoRouterAdvert - before.DHCPv6NoRouterAdvert

			if tc.wantRA {
				if notOffered < 1 {
					t.Errorf("dhcpv6_not_offered moved by %d on a %s segment, want at least 1 — "+
						"the endpoint came up, but the plugin did not record that the segment "+
						"advertised no DHCPv6 address, so an operator has no evidence of why "+
						"the container has no v6 lease", notOffered, tc.mode)
				}
				if noRouter != 0 {
					t.Errorf("dhcpv6_no_router_advert moved by %d on a %s segment, want 0 — "+
						"a router DID advertise here, and reporting otherwise sends an operator "+
						"looking for a missing router that is not missing", noRouter, tc.mode)
				}
			} else {
				if noRouter < 1 {
					t.Errorf("dhcpv6_no_router_advert moved by %d on a %s segment, want at least 1 — "+
						"nothing advertised on this segment and the plugin did not say so",
						noRouter, tc.mode)
				}
				if notOffered != 0 {
					t.Errorf("dhcpv6_not_offered moved by %d on a %s segment, want 0 — "+
						"no advertisement arrived, so there was no offer to read as absent; "+
						"the plugin is treating silence as an answer", notOffered, tc.mode)
				}
			}

			// Outside evidence that the segment did what the mode
			// says, from the DHCP server's own log rather than from
			// the plugin's opinion of it. Without this the subtest
			// passes just as well against a segment that is simply
			// broken, and then it is not proving tolerance — it is
			// observing nothing and calling it a result.
			gotRA := f.CountLogLines("RTR-ADVERT(") > 0
			if gotRA != tc.wantRA {
				t.Errorf("the %s segment sent router advertisements=%v, want %v; "+
					"the counter verdict above is attributed to a segment that was "+
					"not in the mode this subtest asked for", tc.mode, gotRA, tc.wantRA)
			}
		})
	}
}

// TestDHCPv6_Stateless_ConfigurationReachesTheContainer is the
// integration half of #815, and it could not be written until #868 was
// fixed.
//
// #815 is about a stateless DHCPv6 REPLY carrying configuration and no
// address being received rather than discarded. Its unit coverage has
// been in the tree since that issue; the end-to-end proof was
// impossible, because on a stateless segment no endpoint was ever
// created and so no persistent client ever ran to receive the reply.
//
// It asserts on the container's resolver, not on dhcpv6_config_only: a
// counter proves the plugin formed an intention, and only the file
// inside the container proves the configuration arrived. The counter is
// checked too, as the narrower claim that the reply was read at all.
func TestDHCPv6_Stateless_ConfigurationReachesTheContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6Stateless)
	dumpOnFailure(t, f)

	w := harness.BeginCounterWindow(t, ctx, cli, "dhcpv6_config_only", "ipv6_link_enable_failures")
	id, err := startOnV6Segment(t, ctx, cli, f, "dh-itest-v6slcfg")
	if err != nil {
		t.Fatalf("the container did not start on a stateless segment: %v", err)
	}

	// The information reply is answered after the endpoint is up, so
	// the counter is awaited rather than sampled. A sample here would
	// be a race that reports "the reply was discarded" whenever the
	// test wins it.
	if _, ok := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.DHCPv6ConfigOnly > before.DHCPv6ConfigOnly
	}); !ok {
		t.Errorf("dhcpv6_config_only did not move within 30s on a stateless segment — " +
			"the DHCPv6 information reply was not received (#815)")
	}
	_, after := w.End()

	// The engine disables IPv6 outright on a sandbox interface whose
	// endpoint has no IPv6 address, which is exactly this endpoint —
	// so the plugin has to clear that before its DHCPv6 client starts
	// (#868, v6_link.go). If it could not, nothing above could have
	// happened, and this says which of the two it was: a segment that
	// went quiet, or a link nothing could ever arrive on.
	if after.IPv6LinkEnableFailures > 0 {
		t.Errorf("ipv6_link_enable_failures moved to %d — IPv6 could not be enabled on the "+
			"container link, so no DHCPv6 exchange on it was possible",
			after.IPv6LinkEnableFailures)
	}

	// propagate_dns is on for this network, so the reply's DNS server
	// and search domain must reach the container's resolver.
	resolv := harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
	if !strings.Contains(resolv, harness.V6DNSServer) {
		t.Errorf("the container's resolver does not carry the DHCPv6 nameserver %s; "+
			"the reply was counted but nothing reached the container:\n%s",
			harness.V6DNSServer, resolv)
	}
	if !strings.Contains(resolv, harness.V6SearchDomain) {
		t.Errorf("the container's resolver does not carry the DHCPv6 search domain %s:\n%s",
			harness.V6SearchDomain, resolv)
	}
}

// TestDHCPv6_Managed_StillRequiresALease is the preservation control
// for #868.
//
// Managed DHCPv6 differs from stateless in exactly one thing — whether
// the server hands out addresses — and a container must still get a
// real v6 lease there. Without this, every assertion in
// TestDHCPv6_NoAddressModes_StartTheEndpoint is satisfied by a plugin
// that stopped asking for IPv6 anywhere, and "#868 is fixed" would be
// indistinguishable from "the v6 path was removed".
//
// The address is the assertion, not the container's exit status: a
// container that starts with no IPv6 address on a segment that offers
// one is precisely the regression this guards.
func TestDHCPv6_Managed_StillRequiresALease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"dhcpv6_not_offered", "dhcpv6_no_router_advert")

	const netName = "dh-itest-v6managed"
	id, err := startOnV6Segment(t, ctx, cli, f, netName)
	if err != nil {
		t.Fatalf("a container failed to start on a MANAGED DHCPv6 segment, where a "+
			"DHCPv6 address is available — this is not #868, it is a regression in "+
			"the working case:\n%v", err)
	}

	inspect, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	settings, ok := inspect.NetworkSettings.Networks[netName]
	if !ok {
		t.Fatalf("the container is not attached to %s at all; attached to %v",
			netName, inspect.NetworkSettings.Networks)
	}
	if settings.GlobalIPv6Address == "" {
		t.Fatalf("the container started on a managed DHCPv6 segment with NO IPv6 address. " +
			"That is the shape #868's fix must not produce: tolerating an absent v6 lease " +
			"where the segment offers one turns a fatal misconfiguration into a silent one")
	}
	if !strings.HasPrefix(settings.GlobalIPv6Address, harness.V6Prefix) {
		t.Errorf("the container's IPv6 address %q is not from this segment's prefix %q — "+
			"it did not come from the fixture's DHCPv6 server",
			settings.GlobalIPv6Address, harness.V6Prefix)
	}

	// OUTSIDE EVIDENCE, and this is the assertion the test rests on.
	//
	// Everything above reads GlobalIPv6Address, which is the plugin's
	// OWN returned value relayed back by the engine. A plugin that
	// fabricated an in-prefix address — or kept returning a remembered
	// one after DHCPv6 stopped working — satisfies every one of those
	// checks. This is #868's preservation control, the thing that
	// separates "the fix tolerates an absent offer" from "the v6 path
	// was removed", so it is the worst possible place to take the
	// plugin's word for it.
	//
	// The server's own log is the only record that an address was
	// actually leased: dnsmasq writes one DHCPREPLY per blessed
	// request, naming the address it handed out. Counters prove intent;
	// only the server proves effect.
	//
	// A bare substring match is sound for the address: dnsmasq renders
	// it with inet_ntop and the engine with net.IP.String(), so both
	// sides are canonical RFC 5952 lowercase. The same pattern is
	// already read out of an identically-configured dnsmasq log by
	// countDHCPv6Replies in ipv6_test.go. If that ever stopped holding
	// the failure would be legible rather than mysterious, which is why
	// the message below also reports the unfiltered DHCPREPLY count.
	if n := f.CountLogLines("DHCPREPLY", settings.GlobalIPv6Address); n < 1 {
		t.Errorf("the DHCPv6 server logged no DHCPREPLY carrying %s (it logged %d "+
			"DHCPREPLY lines in total). docker inspect reports that address, but the "+
			"server never handed it out — so it came from the plugin rather than from "+
			"DHCPv6, and this preservation control would pass against a plugin that "+
			"had stopped asking for IPv6 entirely",
			settings.GlobalIPv6Address, f.CountLogLines("DHCPREPLY"))
	}

	before, after := w.End()
	if d := after.DHCPv6NotOffered - before.DHCPv6NotOffered; d != 0 {
		t.Errorf("dhcpv6_not_offered moved by %d on a MANAGED segment, want 0 — "+
			"the segment advertised the managed flag and handed out an address, "+
			"so classifying it as 'no DHCPv6 offered' is wrong even though the "+
			"endpoint came up", d)
	}
	if d := after.DHCPv6NoRouterAdvert - before.DHCPv6NoRouterAdvert; d != 0 {
		t.Errorf("dhcpv6_no_router_advert moved by %d on a MANAGED segment, want 0", d)
	}
}

// TestDHCPv6_Managed_ServerSilent_IsStillFatal is the test that keeps
// #868's fix from becoming "ignore DHCPv6 failures".
//
// READ THE POLARITY BEFORE CHANGING ANYTHING HERE. This test asserts
// the container FAILS to start. If a change makes it pass — if the
// container comes up on this segment — that is not this test going
// stale, it is the fix being WRONG. The segment advertises the managed
// flag, which means "there is a DHCPv6 address for you here", and then
// answers nothing. A container that starts anyway has silently lost its
// IPv6 address on a network where an operator configured one, and
// nothing downstream will tell them.
//
// The distinction the fix must hold is not "did DHCPv6 fail" but "did
// the segment ever offer DHCPv6". A fix keyed on the failure rather
// than on the advertisement passes every other test in this file and
// fails only this one.
func TestDHCPv6_Managed_ServerSilent_IsStillFatal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6ManagedSilent)
	dumpOnFailure(t, f)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"dhcpv6_not_offered", "dhcpv6_no_router_advert")

	_, err = startOnV6Segment(t, ctx, cli, f, "dh-itest-v6silent")
	if err == nil {
		t.Fatal("the container STARTED on a segment that advertised managed DHCPv6 and " +
			"then answered nothing. The fix for #868 is keyed on the acquisition failing " +
			"rather than on the advertisement, so a real DHCPv6 outage on a managed " +
			"network now produces a running container with no IPv6 address and no error. " +
			"Do not relax this assertion.")
	}
	if !strings.Contains(err.Error(), "via DHCPv6") {
		t.Fatalf("the container failed to start on a managed-but-silent segment, but not "+
			"for the DHCPv6 reason this test is about — this may be a different "+
			"defect:\n%v", err)
	}

	// Outside evidence, from the server rather than from the plugin:
	// this segment has the same startup signature as a plain managed
	// one, and only a refused solicit tells them apart. Without it a
	// mistyped ignore directive would leave a normal managed segment
	// here, the container would start, and the failure above would be
	// read as the fix being wrong when it is the fixture that broke.
	f.AwaitIgnoredSolicit(30 * time.Second)

	before, after := w.End()
	if d := after.DHCPv6NotOffered - before.DHCPv6NotOffered; d != 0 {
		t.Errorf("dhcpv6_not_offered moved by %d on a managed-but-silent segment, want 0 — "+
			"the RA carried the managed flag, so this is a DHCPv6 failure and not an "+
			"absent offer; counting it as the latter is how the fatal case becomes "+
			"tolerated next", d)
	}
	if d := after.DHCPv6NoRouterAdvert - before.DHCPv6NoRouterAdvert; d != 0 {
		t.Errorf("dhcpv6_no_router_advert moved by %d, want 0 — advertisements were sent "+
			"on this segment", d)
	}
}
