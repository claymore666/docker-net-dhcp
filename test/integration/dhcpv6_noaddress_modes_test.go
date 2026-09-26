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

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// startOnV6Segment starts a container on a fresh network over the fixture's bridge and returns its id and the ContainerStart error, without failing the test.
func startOnV6Segment(t *testing.T, ctx context.Context, cli *docker.Client, f *harness.V6Fixture, netName string) (string, error) {
	t.Helper()
	return startOnV6SegmentWithOpts(t, ctx, cli, f, netName, nil)
}

// startOnV6SegmentWithOpts is startOnV6Segment with driver options that replace the defaults of the same name (#817).
func startOnV6SegmentWithOpts(t *testing.T, ctx context.Context, cli *docker.Client, f *harness.V6Fixture, netName string, extra map[string]string) (string, error) {
	t.Helper()
	return startOnV6SegmentAs(t, ctx, cli, f, onV6Bridge, netName, extra)
}

// startContainerOn creates the shape's network with opts and starts a container on it, returning the ContainerStart error.
func startContainerOn(t *testing.T, ctx context.Context, cli *docker.Client, netName string, at v6Attach, opts map[string]string) (string, error) {
	t.Helper()
	at.createNet(t, ctx, netName, opts)
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

// dumpOnFailure wires the fixture's and the plugin's logs to a failing test.
func dumpOnFailure(t *testing.T, f *harness.V6Fixture) {
	t.Cleanup(func() {
		if t.Failed() {
			f.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
}

// A missing DHCPv6 address is tolerated only when the segment said there was none, so the plugin classifies it: an RA
// with the M flag clear moves dhcpv6_not_offered, no RA moves dhcpv6_no_router_advert, and each arm asserts its own
// counter moved and the other did not (#868). The fatal case is TestDHCPv6_Managed_ServerSilent_IsStillFatal.

// TestDHCPv6_NoAddressModes_StartTheEndpoint checks that a container starts on stateless, SLAAC and router-less IPv6 segments (#868).
func TestDHCPv6_NoAddressModes_StartTheEndpoint(t *testing.T) {
	testDHCPv6_NoAddressModes_StartTheEndpoint(t, onV6Bridge)
}

func TestDHCPv6_NoAddressModes_StartTheEndpoint_Macvlan(t *testing.T) {
	testDHCPv6_NoAddressModes_StartTheEndpoint(t, onV6Macvlan)
}

func TestDHCPv6_NoAddressModes_StartTheEndpoint_IPAM(t *testing.T) {
	testDHCPv6_NoAddressModes_StartTheEndpoint(t, onV6IPAMBridge)
}

func testDHCPv6_NoAddressModes_StartTheEndpoint(t *testing.T, at v6Attach) {
	cases := []struct {
		name string
		mode harness.V6Mode
		net  string
		// wantRA decides which counter the plugin should move.
		wantRA bool
	}{
		{"stateless", harness.V6Stateless, "dh-itest-v6sl", true},
		{"slaac", harness.V6SLAAC, "dh-itest-v6slaac", true},
		{"nora", harness.V6NoRA, "dh-itest-v6nora", false},
	}

	// The table is #868's acceptance criterion; an empty one leaves every gate green, so it is checked before the fixture
	// is touched, keyed on the modes and both wantRA polarities.
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

			id, err := startOnV6SegmentAs(t, ctx, cli, f, at, at.net(tc.net), nil)
			if err != nil {
				t.Fatalf("the container did not start on a %s segment, which is the "+
					"defect #868 describes:\n%v", tc.mode, err)
			}
			assertAttachedAs(t, ctx, f, id, at)

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

			// The server's log shows the segment did what the mode says, so a broken segment cannot pass.
			gotRA := f.CountLogLines("RTR-ADVERT(") > 0
			if gotRA != tc.wantRA {
				t.Errorf("the %s segment sent router advertisements=%v, want %v; "+
					"the counter verdict above is attributed to a segment that was "+
					"not in the mode this subtest asked for", tc.mode, gotRA, tc.wantRA)
			}
			// No address was ever handed out on these segments, which flat counters alone cannot show.
			f.AssertExchange(30 * time.Second)
		})
	}
}

// Since 2.0 the library reports a stateless reply as its own event, so the acquisition ends when the reply arrives
// (#815). The resolver inside the container is the evidence; dhcpv6_config_only only says the reply was read.

// TestDHCPv6_Stateless_ConfigurationReachesTheContainer checks that a stateless DHCPv6 reply's configuration reaches the container's resolver (#815, #868).
func TestDHCPv6_Stateless_ConfigurationReachesTheContainer(t *testing.T) {
	testDHCPv6_Stateless_ConfigurationReachesTheContainer(t, onV6Bridge)
}

func TestDHCPv6_Stateless_ConfigurationReachesTheContainer_Macvlan(t *testing.T) {
	testDHCPv6_Stateless_ConfigurationReachesTheContainer(t, onV6Macvlan)
}

func testDHCPv6_Stateless_ConfigurationReachesTheContainer(t *testing.T, at v6Attach) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6Stateless)
	dumpOnFailure(t, f)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"dhcpv6_config_only", "ipv6_link_enable_failures", "router_advert_guard_failures")
	id, err := startOnV6SegmentAs(t, ctx, cli, f, at, at.net("dh-itest-v6slcfg"), nil)
	if err != nil {
		t.Fatalf("the container did not start on a stateless segment: %v", err)
	}
	assertAttachedAs(t, ctx, f, id, at)

	// The information reply arrives after the endpoint is up, so the counter is awaited.
	if _, ok := w.Await(30*time.Second, func(now, before *harness.HealthResponse) bool {
		return now.DHCPv6ConfigOnly > before.DHCPv6ConfigOnly
	}); !ok {
		t.Errorf("dhcpv6_config_only did not move within 30s on a stateless segment — " +
			"the DHCPv6 information reply was not received (#815)")
	}
	_, after := w.End()

	// The engine disables IPv6 on a sandbox interface whose endpoint has no v6 address, so the plugin clears that before
	// its client starts (#868, v6_link.go).
	if after.IPv6LinkEnableFailures > 0 {
		t.Errorf("ipv6_link_enable_failures moved to %d — IPv6 could not be enabled on the "+
			"container link, so no DHCPv6 exchange on it was possible",
			after.IPv6LinkEnableFailures)
	}

	// The RA guard runs in the same namespace entry as that clear (#911), and an endpoint with no DHCPv6 address is where
	// a broken guard is invisible; a stateless container reaches the network over SLAAC and the advertised router.
	if after.RouterAdvertGuardFailures > 0 {
		t.Errorf("router_advert_guard_failures moved to %d on a stateless segment — the "+
			"container's kernel may not be processing Router Advertisements, and on this "+
			"segment that is the ONLY way it can get a route at all (#911)",
			after.RouterAdvertGuardFailures)
	}

	// propagate_dns is on, so the reply's DNS server and search domain must reach the resolver.
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

	// A stateless segment must see an INFORMATION-REQUEST and no ADVERTISE, which a client that solicited trips (#915).
	f.AssertExchange(30 * time.Second)
}

// TestDHCPv6_Managed_StillRequiresALease checks that a managed DHCPv6 segment still gives the container a real v6 lease (#868).
func TestDHCPv6_Managed_StillRequiresALease(t *testing.T) {
	testDHCPv6_Managed_StillRequiresALease(t, onV6Bridge)
}

func TestDHCPv6_Managed_StillRequiresALease_Macvlan(t *testing.T) {
	testDHCPv6_Managed_StillRequiresALease(t, onV6Macvlan)
}

func testDHCPv6_Managed_StillRequiresALease(t *testing.T, at v6Attach) {
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

	netName := at.net("dh-itest-v6managed")
	id, err := startOnV6SegmentAs(t, ctx, cli, f, at, netName, nil)
	if err != nil {
		t.Fatalf("a container failed to start on a MANAGED DHCPv6 segment, where a "+
			"DHCPv6 address is available — this is not #868, it is a regression in "+
			"the working case:\n%v", err)
	}

	assertAttachedAs(t, ctx, f, id, at)

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

	// GlobalIPv6Address is the plugin's own value relayed by the engine; dnsmasq's DHCPREPLY naming the address is the
	// evidence. dnsmasq renders it with inet_ntop and the engine with net.IP.String(), both RFC 5952 lowercase, as
	// countDHCPv6Replies in ipv6_test.go already relies on (#868).
	if n := f.CountLogLines("DHCPREPLY", settings.GlobalIPv6Address); n < 1 {
		t.Errorf("the DHCPv6 server logged no DHCPREPLY carrying %s (it logged %d "+
			"DHCPREPLY lines in total). docker inspect reports that address, but the "+
			"server never handed it out — so it came from the plugin rather than from "+
			"DHCPv6, and this preservation control would pass against a plugin that "+
			"had stopped asking for IPv6 entirely",
			settings.GlobalIPv6Address, f.CountLogLines("DHCPREPLY"))
	}

	// The live positive of AssertExchange, the one segment where SOLICIT, ADVERTISE and REPLY are known to be on the wire (#915).
	f.AssertExchange(30 * time.Second)

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

// A container that starts here has silently lost its IPv6 address on a segment that advertised managed DHCPv6: the
// fix must key on the advertisement, not on the failure, and a pass here means the fix is wrong (#868).

// TestDHCPv6_Managed_ServerSilent_IsStillFatal checks that a container fails to start when the segment advertises managed DHCPv6 and the server stays silent (#868).
func TestDHCPv6_Managed_ServerSilent_IsStillFatal(t *testing.T) {
	testDHCPv6_Managed_ServerSilent_IsStillFatal(t, onV6Bridge)
}

func TestDHCPv6_Managed_ServerSilent_IsStillFatal_Macvlan(t *testing.T) {
	testDHCPv6_Managed_ServerSilent_IsStillFatal(t, onV6Macvlan)
}

func TestDHCPv6_Managed_ServerSilent_IsStillFatal_IPAM(t *testing.T) {
	testDHCPv6_Managed_ServerSilent_IsStillFatal(t, onV6IPAMBridge)
}

func testDHCPv6_Managed_ServerSilent_IsStillFatal(t *testing.T, at v6Attach) {
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

	_, err = startOnV6SegmentAs(t, ctx, cli, f, at, at.net("dh-itest-v6silent"), nil)
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

	// Only a refused solicit tells this segment from a plain managed one, so a broken ignore directive cannot pass.
	f.AwaitIgnoredSolicit(30 * time.Second)
	assertServedOnSegment(t, f)

	// V6ManagedSilent's mustLine needs DHCPSOLICIT and the refusal word on one line; read on the whole log it passes on any
	// fixture whose v4 half logged a refusal (#915).
	f.AssertExchange(30 * time.Second)

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
