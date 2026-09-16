// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The end-to-end half of #818, #819 and #808: an `ipv6_mode=slaac` or
// `ipv6_mode=auto` network puts the address the router advertised on
// the container's link.
//
// EVERY ASSERTION HERE IS OUTSIDE EVIDENCE, and that is the point of
// the file. The unit tests in pkg/plugin and pkg/dhcp drive the address
// set, its lifetimes and the endings a forming mode has; what they
// cannot say is whether anything reached the container. So these read
// `ip -6 addr` INSIDE the container -- the address, the kernel's flags
// and the kernel's two lifetimes -- and the DHCP server's own log for
// what the segment did. The plugin's counters are read too, always
// after the container's own state and never instead of it: a counter is
// evidence that the plugin formed an intention.

// slaacAddrBudget is how long an assertion here waits for the formed
// address to appear on the container's link.
//
// It is the acquisition budget plus the fixture's own advertisement
// budget, because the two are in series and nothing overlaps them: the
// address cannot form before an advertisement arrives (RFC 4862 section
// 5.5.3 forms it from the Prefix Information option), and the
// advertisement's own bound is dnsmasq's schedule, derived in
// harness.RABudget. A literal here would be a number that happened to
// work on the runner it was written on.
func slaacAddrBudget() time.Duration {
	return harness.IPAcquisitionBudget + harness.RABudget()
}

// v6InPrefix returns the first address inside prefix that `ip -6 -o addr
// show` printed, and whether there was one.
func v6InPrefix(out string, prefix netip.Prefix) (string, bool) {
	for _, field := range strings.Fields(out) {
		bare, _, ok := strings.Cut(field, "/")
		if !ok {
			continue
		}
		a, err := netip.ParseAddr(bare)
		if err != nil || !prefix.Contains(a) {
			continue
		}
		return bare, true
	}
	return "", false
}

// awaitContainerV6 polls the container's own view until an address
// inside prefix is on the link, and returns it with the kernel's flags
// and lifetimes.
//
// The address is taken from the CONTAINER and matched against the
// advertised prefix, not taken from docker inspect. An endpoint whose
// reported address is right and whose link is empty is exactly the
// shape #818 fixes, and a helper that read the reported one would pass
// on it.
//
// PRESENCE IS NOT THIS PLUGIN'S INSTALL, and a caller that needs the
// second wants awaitPluginAppliedV6 below. The engine puts the address
// CreateEndpoint reported on the link itself while it builds the
// sandbox, so an address inside the prefix is there before the plugin's
// persistent client has bound anything. This helper is right for the
// restart arm, whose question is whether an address that was already
// there is still there, and wrong for an arm whose question is what the
// plugin installed.
func awaitContainerV6(t *testing.T, ctx context.Context, id string, prefix netip.Prefix, budget time.Duration) (string, harness.V6AddrFlags) {
	t.Helper()
	var out string
	deadline := time.Now().Add(budget)
	for {
		out = harness.ExecOutput(t, ctx, id, "ip", "-6", "-o", "addr", "show", "scope", "global")
		if bare, ok := v6InPrefix(out, prefix); ok {
			return bare, harness.V6AddrFlagsFromAddrShow(out, bare)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no address inside %s on the container's link after %s. The router "+
				"advertises that prefix with the autonomous flag set, so RFC 4862 section "+
				"5.5.3 forms an address from it and this network's ipv6_mode says that "+
				"address is where the endpoint's IPv6 comes from.\n"+
				"`ip -6 -o addr show scope global` said:\n%s", prefix, budget, out)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// formedAddrReadFloor is the shortest budget awaitPluginAppliedV6 gives
// the container read once the counter has moved.
//
// The counter moves ON the netlink call that installed the address, so
// by then the address is on the link and this is a floor for one exec
// round-trip on a loaded runner, not a wait for anything to happen.
const formedAddrReadFloor = 5 * time.Second

// awaitPluginAppliedV6 waits for THIS PLUGIN to install a formed
// address on the container's link, and only then reads the container's
// own view of it.
//
// THE DIFFERENCE FROM awaitContainerV6 IS THE POINT. libnetwork
// installs the address CreateEndpoint reported when it builds the
// sandbox, and it installs it permanent: MEASURED on engine 29.8.0
// (run 35153680517) the container's link carries
// `inet6 <formed>/64 scope global flags 02 valid_lft forever
// preferred_lft forever` while the plugin's persistent client is still
// acquiring. Neither presence NOR the flag separates the two installs:
// 0x02 is IFA_F_NODAD and the engine sets it as well. The kernel's two
// lifetimes do, and so does the counter.
//
// The wait is on the counter, and the assertions afterwards are still
// on the container's own kernel. ipv6_slaac_addresses moves on the
// netlink call this plugin makes, once per address that was not already
// in the set it installed (pkg/plugin/metrics.go), so a renewal of an
// address already on the link cannot satisfy it and neither can the
// engine's install of one. Nothing in this suite calls t.Parallel and
// each of these arms runs one container, so a move inside the window is
// this endpoint's.
//
// WHICH QUESTION TO ASK OF THE COUNTER is the cond argument, and there
// are two. v6InstalledSinceBaseline is the ordinary one. A plugin that
// has just been restarted needs the other, for the reason
// v6InstalledByThisProcess gives.
func awaitPluginAppliedV6(t *testing.T, ctx context.Context, w *harness.CounterWindow, id string,
	prefix netip.Prefix, budget time.Duration,
	cond func(now, before *harness.HealthResponse) bool) (string, harness.V6AddrFlags) {
	t.Helper()
	start := time.Now()
	if _, ok := w.Await(budget, cond); !ok {
		out := harness.ExecOutput(t, ctx, id, "ip", "-6", "-o", "addr", "show", "scope", "global")
		if addr, there := v6InPrefix(out, prefix); there {
			t.Fatalf("the container holds %s and ipv6_slaac_addresses did not move in %s. "+
				"That address is the one CreateEndpoint reported and the engine configured "+
				"on the link at container start -- permanent, and carrying IFA_F_NODAD, "+
				"which is why neither its presence nor its flags say anything about this "+
				"plugin. The plugin's own install, the one that carries the advertised "+
				"lifetimes, never ran for this endpoint.\n"+
				"`ip -6 -o addr show scope global` said:\n%s", addr, budget, out)
		}
		t.Fatalf("no address inside %s on the container's link after %s, and "+
			"ipv6_slaac_addresses did not move. The router advertises that prefix with "+
			"the autonomous flag set, so RFC 4862 section 5.5.3 forms an address from it "+
			"and this network's ipv6_mode says that address is where the endpoint's IPv6 "+
			"comes from.\n"+
			"`ip -6 -o addr show scope global` said:\n%s", prefix, budget, out)
	}
	// The window between the engine's install and this plugin's, which
	// is how long a container holds the formed address permanent and
	// preferred. It is logged and not asserted on: it is the acquisition
	// this endpoint's own client has to finish, and a bound on it would
	// be a bound on a DHCPv6 or router-discovery exchange. Reading it
	// off the runs is how it becomes a number at all.
	t.Logf("the plugin's own install landed %s after the container started",
		time.Since(start).Round(100*time.Millisecond))

	read := budget - time.Since(start)
	if read < formedAddrReadFloor {
		read = formedAddrReadFloor
	}
	return awaitContainerV6(t, ctx, id, prefix, read)
}

// v6InstalledSinceBaseline is the ordinary condition: this plugin
// installed a formed address after the window opened.
func v6InstalledSinceBaseline(now, before *harness.HealthResponse) bool {
	return now.IPv6SLAACAddresses > before.IPv6SLAACAddresses
}

// v6InstalledByThisProcess is the same question asked of a plugin that
// has just been restarted, where a delta cannot ask it.
//
// The counters are in-memory and start at zero with the process, so any
// value at all is an install this process made. A baseline read taken
// after the restart is a race the test would lose silently: the resumed
// endpoint can install its address before the first health read
// succeeds, and a delta against that baseline would then wait for a
// second install that is never coming.
func v6InstalledByThisProcess(now, _ *harness.HealthResponse) bool {
	return now.IPv6SLAACAddresses >= 1
}

// v6SegmentPrefix is the /64 every v6 fixture in this suite advertises.
func v6SegmentPrefix(t *testing.T) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(harness.V6Prefix + "/64")
	if err != nil {
		t.Fatalf("the harness's v6 prefix %q does not parse: %v", harness.V6Prefix, err)
	}
	return p
}

// assertHealthyFormedAddress is the shape a formed address has to be in
// on the container's link, whichever mode formed it.
//
// NODAD is asserted for the reason assertLeasedV6IsInstalledWithNODAD
// gives for a leased address, and it is a STRONGER claim here: the
// library ran RFC 4862 section 5.4 duplicate address detection on the
// formed address before it reported it, and a modified EUI-64 interface
// identifier gets no second try after a failure (section 5.4.5), so an
// address the kernel re-probes is one the kernel can take away with no
// way back.
func assertHealthyFormedAddress(t *testing.T, addr string, f harness.V6AddrFlags) {
	t.Helper()
	if !f.NoDAD {
		t.Errorf("the formed address %s is on the link WITHOUT IFA_F_NODAD. The library "+
			"already ran duplicate address detection on it and the chassis installs it "+
			"saying so; without the flag the kernel repeats RFC 4862 section 5.4 on an "+
			"address that has just passed it, and section 5.4.5 gives a fixed interface "+
			"identifier no retry if that second run fails. Line: %q", addr, f.Line)
	}
	if f.Tentative {
		t.Errorf("the formed address %s is still tentative: %q", addr, f.Line)
	}
	if f.DADFailed {
		t.Errorf("the formed address %s is DADFAILED, so the kernel has taken it out of "+
			"service and RFC 4862 section 5.4.5 offers no retry for the identifier that "+
			"formed it: %q", addr, f.Line)
	}
	if !f.Lifetimes {
		t.Fatalf("the kernel printed no lifetimes for %s, so nothing here can say whether "+
			"the advertised ones were installed at all: %q", addr, f.Line)
	}
}

// TestSLAAC_AnAdvertisedPrefixReachesTheContainer is #808's shape and
// #818's proof: a segment with a router and NO DHCPv6 server at all
// leases IPv4 from DHCP and forms IPv6 from the advertisement.
//
// It closes defeat row 2 -- the address arrives and carries neither the
// NODAD flag nor the advertised lifetimes -- by reading the flags and
// both lifetimes out of the container's kernel rather than the address
// alone.
func TestSLAAC_AnAdvertisedPrefixReachesTheContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6SLAAC)
	dumpOnFailure(t, f)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"ipv6_slaac_addresses", "dhcpv6_not_offered", "dhcpv6_no_router_advert",
		"dhcpv6_slaac_no_address", "dhcpv6_slaac_no_prefix")

	netName := "dh-itest-slaac1"
	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, netName,
		map[string]string{"ipv6": "", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("the container did not start on an ipv6_mode=slaac segment: %v", err)
	}

	prefix := v6SegmentPrefix(t)
	addr, flags := awaitPluginAppliedV6(t, ctx, w, id, prefix, slaacAddrBudget(), v6InstalledSinceBaseline)
	t.Logf("formed address inside the container: %q", flags.Line)
	assertHealthyFormedAddress(t, addr, flags)

	// THE LIFETIMES ARE THE ROUTER'S. The fixture's advertisement
	// carries 1800 seconds in both fields (MEASURED, and pinned
	// verbatim in harness/v6signature_test.go), and the kernel counts
	// them down from the moment it installed the address. So the
	// assertion is an interval and not an equality, and the upper
	// bound is what a defect would break: an address installed with
	// the kernel's infinity, or with a lifetime the plugin invented,
	// lands outside it.
	const advertised = 1800
	if flags.Valid.Forever || flags.Preferred.Forever {
		t.Errorf("the address carries an infinite lifetime (valid=%s preferred=%s) on a "+
			"segment advertising %ds for both. An address the router can stop advertising "+
			"and the kernel will never remove is the renumbering defect #819 is about: %q",
			flags.Valid, flags.Preferred, advertised, flags.Line)
	} else {
		if flags.Valid.Seconds <= 0 || flags.Valid.Seconds > advertised {
			t.Errorf("valid_lft = %ds, want between 1 and %d: the advertised valid lifetime "+
				"counting down. Line: %q", flags.Valid.Seconds, advertised, flags.Line)
		}
		if flags.Preferred.Seconds <= 0 || flags.Preferred.Seconds > advertised {
			t.Errorf("preferred_lft = %ds, want between 1 and %d. A zero here on a freshly "+
				"advertised prefix would mean the address was installed deprecated (RFC "+
				"4862 section 5.5.4) on a segment that deprecated nothing. Line: %q",
				flags.Preferred.Seconds, advertised, flags.Line)
		}
		if flags.Preferred.Seconds > flags.Valid.Seconds {
			t.Errorf("preferred_lft %ds is past valid_lft %ds, which RFC 4862 section 5.5.3 "+
				"says makes the option unusable: %q",
				flags.Preferred.Seconds, flags.Valid.Seconds, flags.Line)
		}
	}

	// Docker's own view. It reports ONE address per endpoint, and on
	// this single-prefix segment it must be the one on the link: a
	// reported address the container does not hold is what an operator
	// reads out of `docker inspect` and puts in a firewall rule.
	if got := inspectV6(t, ctx, cli, id, netName); got != addr {
		t.Errorf("docker inspect reports %q and the container holds %q", got, addr)
	}

	// EXACTLY ONE, and not "at least one", because the wait above
	// already required a move and an at-least-one here would be a check
	// with one possible verdict. This segment advertises one autonomous
	// prefix, RFC 4862 section 5.5.3 forms one address per prefix, and a
	// renewal of an address already installed does not count again
	// (TestApplyV6Addrs_InstallsEveryAddressAndRemovesWhatLeftTheLease),
	// so anything but 1 is a counter that describes something other
	// than the addresses this container has.
	before, after := w.End()
	if formed := after.IPv6SLAACAddresses - before.IPv6SLAACAddresses; formed != 1 {
		t.Errorf("ipv6_slaac_addresses moved by %d for one container on a segment "+
			"advertising one autonomous prefix, want 1", formed)
	}
	for _, c := range []struct {
		name string
		got  int32
	}{
		{"dhcpv6_not_offered", after.DHCPv6NotOffered - before.DHCPv6NotOffered},
		{"dhcpv6_no_router_advert", after.DHCPv6NoRouterAdvert - before.DHCPv6NoRouterAdvert},
		{"dhcpv6_slaac_no_address", after.DHCPv6SLAACNoAddress - before.DHCPv6SLAACNoAddress},
		{"dhcpv6_slaac_no_prefix", after.DHCPv6SLAACNoPrefix - before.DHCPv6SLAACNoPrefix},
	} {
		if c.got != 0 {
			t.Errorf("%s moved by %d on a segment where the address formed; every one of "+
				"these counters is an ending where it did not", c.name, c.got)
		}
	}

	// #808's other half, read from the server's own log: NO DHCPv6
	// EXCHANGE HAPPENED. The slaac contract is a must-NOT set over the
	// DHCP message names only dnsmasq's v6 path prints, so this fails
	// if anything leased this container an address over DHCPv6 -- which
	// is the reading under which every assertion above would pass for
	// the wrong reason.
	f.AssertExchange(30 * time.Second)
}

// TestSLAAC_ADeprecatedPrefixArrivesDeprecated is #819's deprecation
// phase, and it closes defeat row 5: the assertion is the KERNEL's
// flag and the KERNEL's preferred lifetime, never the plugin's own
// number.
//
// The segment is V6SLAAC's with one directive changed, so it is started
// through NewV6FixtureWithArgs under the slaac name; see
// harness.V6DeprecatedPrefixArgs for why it is not a mode of its own.
// The wire half is asserted here rather than assumed, because the whole
// test is about a lifetime and the fixture's mode check does not read
// lifetimes.
func TestSLAAC_ADeprecatedPrefixArrivesDeprecated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6FixtureWithArgs(t, harness.V6SLAAC, harness.V6DeprecatedPrefixArgs())
	dumpOnFailure(t, f)

	// The wire, first: an advertisement whose prefix option is NOT
	// deprecated makes everything below a test of nothing.
	frames := f.AwaitRAAfter(f.EvidenceStartedAt(), harness.RABudget())
	if len(frames) == 0 {
		t.Fatalf("no router advertisement captured on the segment")
	}
	var sawDeprecated bool
	for _, fr := range frames {
		for _, p := range fr.Prefixes {
			if p.Autonomous && p.PreferredLifetime == 0 && p.ValidLifetime == harness.RAInfiniteLifetime {
				sawDeprecated = true
			}
		}
	}
	if !sawDeprecated {
		t.Fatalf("the segment advertises no autonomous prefix with preferred lifetime 0 and "+
			"an infinite valid lifetime, so there is nothing here for the container's "+
			"kernel to deprecate. Frames:\n%v", frames)
	}

	// Opened before the container exists, because the wait below is a
	// delta against it: the address the engine installs at container
	// start is valid forever and preferred forever, and the one this
	// test is about is valid forever and preferred 0sec, so on this arm
	// the kernel's own line is the only thing that separates them and
	// the counter is the only thing that says when to read it.
	w := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses")

	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, "dh-itest-slaacdep",
		map[string]string{"ipv6": "", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("the container did not start on an ipv6_mode=slaac segment whose prefix "+
			"is advertised deprecated. A deprecated address is one RFC 4862 section 5.5.4 "+
			"says to keep using for existing communications, not one to refuse an endpoint "+
			"over: %v", err)
	}

	addr, flags := awaitPluginAppliedV6(t, ctx, w, id, v6SegmentPrefix(t), slaacAddrBudget(), v6InstalledSinceBaseline)
	t.Logf("deprecated address inside the container: %q", flags.Line)
	assertHealthyFormedAddress(t, addr, flags)

	if !flags.Deprecated {
		t.Errorf("the kernel does not report %s as deprecated. The router advertised the "+
			"prefix with a preferred lifetime of zero, which RFC 4862 section 5.5.4 makes "+
			"a deprecated address: \"SHOULD continue to be used as a source address in "+
			"existing communications, but SHOULD NOT be used to initiate new "+
			"communications\". The plugin's own preferred number is not the oracle for "+
			"that state and is not read here. Line: %q", addr, flags.Line)
	}
	if flags.Preferred.Forever || flags.Preferred.Seconds != 0 {
		t.Errorf("preferred_lft = %s, want 0sec: that is how a deprecated address is "+
			"spelled to the kernel, and a non-zero value here means the advertised "+
			"lifetime never reached netlink. Line: %q", flags.Preferred, flags.Line)
	}
	if !flags.Valid.Forever {
		t.Errorf("valid_lft = %s, want forever: the advertisement carries RFC 4861 section "+
			"4.6.2's infinity in the valid field, and an address that expires on a segment "+
			"advertising that would leave the container with no IPv6 at all. Line: %q",
			flags.Valid, flags.Line)
	}

	before, after := w.End()
	if formed := after.IPv6SLAACAddresses - before.IPv6SLAACAddresses; formed != 1 {
		t.Errorf("ipv6_slaac_addresses moved by %d for one container on a segment "+
			"advertising one autonomous prefix, want 1", formed)
	}
}

// TestSLAAC_AutoFallsBackOntoTheAdvertisedPrefix drives #817's fallback
// end to end, which is the bound that release stated and could not
// close: no fixture advertised the managed flag beside an autonomous
// prefix, so nothing made the library actually fall back.
//
// harness.V6AutoFallback is that segment. It closes defeat row 15.
func TestSLAAC_AutoFallsBackOntoTheAdvertisedPrefix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6AutoFallback)
	dumpOnFailure(t, f)

	w := harness.BeginCounterWindow(t, ctx, cli,
		"dhcpv6_auto_fallbacks", "ipv6_slaac_addresses", "dhcpv6_no_server")

	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, "dh-itest-autofb",
		map[string]string{"ipv6": "", "ipv6_mode": "auto"})
	if err != nil {
		t.Fatalf("the container did not start on an ipv6_mode=auto segment that advertises "+
			"DHCPv6, answers no Solicit and advertises an autonomous prefix. That segment "+
			"is exactly what the fallback exists for: %v", err)
	}

	addr, flags := awaitPluginAppliedV6(t, ctx, w, id, v6SegmentPrefix(t), slaacAddrBudget(), v6InstalledSinceBaseline)
	t.Logf("address formed by the auto fallback: %q", flags.Line)
	assertHealthyFormedAddress(t, addr, flags)

	before, after := w.End()
	if n := after.DHCPv6AutoFallbacks - before.DHCPv6AutoFallbacks; n < 1 {
		t.Errorf("dhcpv6_auto_fallbacks moved by %d, want at least 1. The container has an "+
			"address formed from the advertised prefix on a segment whose router said "+
			"DHCPv6, so the fallback is what produced it and the counter that reports "+
			"fallbacks did not move", n)
	}
	if n := after.IPv6SLAACAddresses - before.IPv6SLAACAddresses; n != 1 {
		t.Errorf("ipv6_slaac_addresses moved by %d for one container on a segment "+
			"advertising one autonomous prefix, want 1", n)
	}
	if n := after.DHCPv6NoServer - before.DHCPv6NoServer; n != 0 {
		t.Errorf("dhcpv6_no_server moved by %d on a segment where the fallback produced an "+
			"address; that counter is the ending where the endpoint FAILED because nothing "+
			"answered, and this endpoint did not fail", n)
	}

	// Outside evidence that the DHCPv6 half really was tried and
	// really was silent: the server's own log has to carry a Solicit
	// it ignored and no Advertise or Reply. Without it, a plugin that
	// never solicited at all would satisfy every assertion above --
	// and "auto fell back" would be a claim about an exchange that
	// never happened.
	f.AssertExchange(60 * time.Second)
}

// TestSLAAC_ASegmentWithNoRouterEndsAFormingEndpoint is the endpoint
// half of the ending #989 documented as temporary: a forming mode with
// no advertisement has no source of an address, so the endpoint fails.
//
// BOTH DIRECTIONS RUN, on the same segment, in one table. The
// `ipv6_mode=dhcp` row is the preservation control and it is #868's
// tolerance: a DHCPv6 server that is there answers a Solicit whether or
// not a router advertises, so refusing that container buys nothing. A
// change that made the ending fatal for everyone passes the slaac row
// and fails this one.
func TestSLAAC_ASegmentWithNoRouterEndsAFormingEndpoint(t *testing.T) {
	cases := []struct {
		mode      string
		net       string
		wantStart bool
	}{
		{"slaac", "dh-itest-norasl", false},
		{"auto", "dh-itest-noraau", false},
		{"dhcp", "dh-itest-noradh", true},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			f := harness.NewV6Fixture(t, harness.V6NoRA)
			dumpOnFailure(t, f)

			w := harness.BeginCounterWindow(t, ctx, cli, "dhcpv6_no_router_advert")

			_, startErr := startOnV6SegmentWithOpts(t, ctx, cli, f, c.net,
				map[string]string{"ipv6": "", "ipv6_mode": c.mode})

			switch {
			case c.wantStart && startErr != nil:
				t.Errorf("the container did not start on an ipv6_mode=%s segment with no "+
					"router advertisement. That mode takes its address from a DHCPv6 "+
					"server, and a server that is there answers a Solicit whether or not "+
					"a router advertises, so refusing the container buys nothing (#868): "+
					"%v", c.mode, startErr)
			case !c.wantStart && startErr == nil:
				t.Errorf("the container STARTED on an ipv6_mode=%s segment with no router "+
					"advertisement. RFC 4862 section 5.5.3 forms the address from the "+
					"Prefix Information option and nothing advertised one, so the "+
					"container has no IPv6 from this plugin and nothing said so (#818)",
					c.mode)
			}

			// The counter is the same for both outcomes, because the
			// population it counts is every endpoint that saw no
			// advertisement. A change that made the forming modes
			// fatal by routing them to some other verdict would pass
			// the outcome check above and quietly empty this counter
			// for the two modes most likely to produce it.
			before, after := w.End()
			if n := after.DHCPv6NoRouterAdvert - before.DHCPv6NoRouterAdvert; n < 1 {
				t.Errorf("dhcpv6_no_router_advert moved by %d in ipv6_mode=%s, want at "+
					"least 1: nothing advertised on this segment and the plugin did not "+
					"say so", n, c.mode)
			}
		})
	}
}

// TestSLAAC_TheAddressComesBackAfterAPluginRestart is defeat row 12 and
// the design note's defeat 10: the resumed client re-forms the same
// address and runs duplicate address detection again, and the only
// node holding that address is the container itself.
//
// A client that read its own Neighbor Advertisement as a conflict would
// take the address off the link on every plugin restart, and RFC 4862
// section 5.4.5 gives a modified EUI-64 identifier no second try. The
// assertion is therefore that the address is STILL THERE and NOT
// dadfailed, read from the container after a real disable and enable.
func TestSLAAC_TheAddressComesBackAfterAPluginRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6SLAAC)
	dumpOnFailure(t, f)

	// BOTH READS ARE OF THIS PLUGIN'S INSTALL, and that is what makes
	// the arm about this plugin. The engine put the reported address on
	// the link when it built the sandbox, permanent and with
	// IFA_F_NODAD, and a plugin restart does nothing to an address the
	// engine installed: presence, the address being the same one, the
	// flags and the lifetimes printed are all satisfied by a copy this
	// plugin never touched, which would answer the arm's own question
	// with the thing it exists to exclude.
	w := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses")

	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, "dh-itest-slaacrs",
		map[string]string{"ipv6": "", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("the container did not start on an ipv6_mode=slaac segment: %v", err)
	}
	prefix := v6SegmentPrefix(t)
	addrBefore, _ := awaitPluginAppliedV6(t, ctx, w, id, prefix, slaacAddrBudget(), v6InstalledSinceBaseline)

	// Closed before the restart, because a window that spans one is
	// measuring across a counter reset and CounterWindow refuses it.
	if before, after := w.End(); after.IPv6SLAACAddresses-before.IPv6SLAACAddresses != 1 {
		t.Errorf("ipv6_slaac_addresses moved by %d before the restart, want 1",
			after.IPv6SLAACAddresses-before.IPv6SLAACAddresses)
	}

	// The resumed endpoint re-binds its v4 lease without going through
	// CreateEndpoint, so the RFC 5227 probe that bind would have run is
	// the asynchronous one beside the address, and it races this test's
	// own teardown. One lease, because one container is attached;
	// declaring more than the shard takes weakens the zero-probes gate
	// silently. Same cause and same declaration as
	// TestRecovery_PluginDisableEnable_PreservesEndpoint.
	harness.AllowUnprobedLeases(1)

	t.Cleanup(func() {
		bg, bgCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer bgCancel()
		if err := cli.PluginEnable(bg, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
			if !strings.Contains(err.Error(), "already enabled") {
				t.Logf("WARN: cleanup PluginEnable: %v", err)
			}
		}
	})

	if err := cli.PluginDisable(ctx, harness.PluginRef, types.PluginDisableOptions{Force: true}); err != nil {
		t.Fatalf("PluginDisable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, false, 15*time.Second); err != nil {
		t.Fatalf("plugin did not reach disabled state: %v", err)
	}
	if err := cli.PluginEnable(ctx, harness.PluginRef, types.PluginEnableOptions{Timeout: 30}); err != nil {
		t.Fatalf("PluginEnable: %v", err)
	}
	if err := harness.WaitPluginEnabled(ctx, cli, true, 30*time.Second); err != nil {
		t.Fatalf("plugin did not re-enable: %v", err)
	}
	harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)

	// A SECOND WINDOW, on the process that came back. Its counters
	// started at zero with it, so v6InstalledByThisProcess asks the
	// question a delta cannot ask here: did THIS plugin process install
	// a formed address for this endpoint, or is the address on the link
	// only the copy the engine made at container start, which no
	// restart could have removed.
	w2 := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses")

	addrAfter, flags := awaitPluginAppliedV6(t, ctx, w2, id, prefix, slaacAddrBudget(), v6InstalledByThisProcess)
	t.Logf("address after the restart: %q", flags.Line)
	if addrAfter != addrBefore {
		t.Errorf("the container held %s before the plugin restart and %s after it. The "+
			"address is formed from the advertised prefix and the link's MAC (RFC 4291 "+
			"appendix A), neither of which changed, so a different address means the "+
			"first one was taken away", addrBefore, addrAfter)
	}
	if flags.DADFailed {
		t.Errorf("the formed address is DADFAILED after a plugin restart. The resumed "+
			"client re-runs duplicate address detection, the only node holding that "+
			"address is this container, and reading one's own Neighbor Advertisement as "+
			"a conflict takes the address away on every restart with no retry (RFC 4862 "+
			"section 5.4.5). Line: %q", flags.Line)
	}
	assertHealthyFormedAddress(t, addrAfter, flags)

	if _, after := w2.End(); after.IPv6SLAACAddresses < 1 {
		t.Errorf("ipv6_slaac_addresses = %d on the plugin process that came back, want at "+
			"least 1: the resumed endpoint's address is on the link and this process "+
			"installed none of it", after.IPv6SLAACAddresses)
	}
}
