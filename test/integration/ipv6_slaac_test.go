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

// Unit tests drive the address set, its lifetimes and a forming mode's endings; these read `ip -6 addr` inside the
// container and the DHCP server's log, and the plugin's counters only after the container's own state (#808, #818,
// #819).

// The address forms from the Prefix Information option only after an advertisement arrives (RFC 4862 section 5.5.3),
// so the acquisition budget and harness.RABudget are in series.

// slaacAddrBudget is how long an assertion waits for the formed address to appear on the container's link.
func slaacAddrBudget() time.Duration {
	return harness.IPAcquisitionBudget + harness.RABudget()
}

// v6InPrefix returns the first address inside prefix that `ip -6 -o addr show` printed, and whether there was one.
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

// A reported address on an empty link is the shape #818 fixed, so the address is read from the container. The engine
// installs the reported address itself when it builds the sandbox, so presence is not the plugin's install; this
// helper suits the restart arm, and awaitPluginAppliedV6 the arms about what the plugin installed.

// awaitContainerV6 polls the container until an address inside prefix is on its link and returns it with its flags and lifetimes.
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

// The counter moves on the netlink call that installed the address, so this is one exec round-trip on a loaded runner.

// formedAddrReadFloor is the shortest budget awaitPluginAppliedV6 gives the container read once the counter has moved.
const formedAddrReadFloor = 5 * time.Second

// libnetwork installs the reported address permanent with IFA_F_NODAD (0x02) while the plugin's client is still
// acquiring, measured on engine 29.8.0, run 35153680517; only the lifetimes and the counter separate the two installs
// (#818). ipv6_slaac_addresses moves once per address newly added to the plugin's set (pkg/plugin/metrics.go), so
// neither a renewal nor the engine's install satisfies it, and with no t.Parallel a move is this endpoint's. The cond
// argument is v6InstalledSinceBaseline, or v6InstalledByThisProcess after a restart.

// awaitPluginAppliedV6 waits for this plugin to install a formed address on the container's link, then reads the container's view of it.
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
	// The gap between the engine's install and the plugin's is the endpoint's own acquisition, logged and not asserted.
	t.Logf("the plugin's own install landed %s after the container started",
		time.Since(start).Round(100*time.Millisecond))

	read := budget - time.Since(start)
	if read < formedAddrReadFloor {
		read = formedAddrReadFloor
	}
	return awaitContainerV6(t, ctx, id, prefix, read)
}

// v6InstalledSinceBaseline reports whether this plugin installed a formed address after the window opened.
func v6InstalledSinceBaseline(now, before *harness.HealthResponse) bool {
	return now.IPv6SLAACAddresses > before.IPv6SLAACAddresses
}

// The counters start at zero with the process, and a resumed endpoint can install before the first health read
// succeeds, so a delta would wait for a second install that never comes (#818).

// v6InstalledByThisProcess reports whether a just-restarted plugin process has installed any formed address.
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

// The library ran RFC 4862 section 5.4 detection before reporting the address, and a modified EUI-64 identifier gets
// no second try after a failure (section 5.4.5), so a kernel re-probe could take it away for good.

// assertHealthyFormedAddress checks the NODAD flag and lifetimes a formed address must carry on the container's link.
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

// TestSLAAC_AnAdvertisedPrefixReachesTheContainer checks that with no DHCPv6 server the container forms its IPv6 address from the advertisement, with NODAD and the advertised lifetimes (#808, #818).
func TestSLAAC_AnAdvertisedPrefixReachesTheContainer(t *testing.T) {
	testSLAAC_AnAdvertisedPrefixReachesTheContainer(t, onV6Bridge)
}

func TestSLAAC_AnAdvertisedPrefixReachesTheContainer_Macvlan(t *testing.T) {
	testSLAAC_AnAdvertisedPrefixReachesTheContainer(t, onV6Macvlan)
}

func testSLAAC_AnAdvertisedPrefixReachesTheContainer(t *testing.T, at v6Attach) {
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

	netName := at.net("dh-itest-slaac1")
	id, err := startOnV6SegmentAs(t, ctx, cli, f, at, netName,
		map[string]string{"ipv6": "", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("the container did not start on an ipv6_mode=slaac segment: %v", err)
	}
	assertAttachedAs(t, ctx, f, id, at)

	prefix := v6SegmentPrefix(t)
	addr, flags := awaitPluginAppliedV6(t, ctx, w, id, prefix, slaacAddrBudget(), v6InstalledSinceBaseline)
	t.Logf("formed address inside the container: %q", flags.Line)
	assertHealthyFormedAddress(t, addr, flags)

	// The fixture advertises 1800 seconds in both fields (pinned in harness/v6signature_test.go) and the kernel counts down
	// from its install, so the check is an interval whose upper bound catches infinity or an invented lifetime (#818).
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

	// Docker reports one address per endpoint, and operators copy it from `docker inspect` into firewall rules.
	if got := inspectV6(t, ctx, cli, id, netName); got != addr {
		t.Errorf("docker inspect reports %q and the container holds %q", got, addr)
	}

	// One autonomous prefix forms one address (RFC 4862 section 5.5.3), and a renewal does not count again
	// (TestApplyV6Addrs_InstallsEveryAddressAndRemovesWhatLeftTheLease), so the count is exactly 1.
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

	// The slaac contract's must-NOT set fails if anything leased this container an address over DHCPv6 (#808).
	f.AssertExchange(30 * time.Second)
}

// The segment is V6SLAAC's with one directive changed (harness.V6DeprecatedPrefixArgs), and the wire half is asserted
// because the fixture's mode check does not read lifetimes (#819).

// TestSLAAC_ADeprecatedPrefixArrivesDeprecated checks that a deprecated prefix's address arrives with the kernel's deprecated flag and zero preferred lifetime (#819).
func TestSLAAC_ADeprecatedPrefixArrivesDeprecated(t *testing.T) {
	testSLAAC_ADeprecatedPrefixArrivesDeprecated(t, onV6Bridge)
}

func TestSLAAC_ADeprecatedPrefixArrivesDeprecated_Macvlan(t *testing.T) {
	testSLAAC_ADeprecatedPrefixArrivesDeprecated(t, onV6Macvlan)
}

func testSLAAC_ADeprecatedPrefixArrivesDeprecated(t *testing.T, at v6Attach) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6FixtureWithArgs(t, harness.V6SLAAC, harness.V6DeprecatedPrefixArgs())
	dumpOnFailure(t, f)

	// An advertisement whose prefix is not deprecated would make everything below a test of nothing.
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

	// The engine's install is preferred forever and this one preferred 0sec, so the counter says when to read the line.
	w := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses")

	id, err := startOnV6SegmentAs(t, ctx, cli, f, at, at.net("dh-itest-slaacdep"),
		map[string]string{"ipv6": "", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("the container did not start on an ipv6_mode=slaac segment whose prefix "+
			"is advertised deprecated. A deprecated address is one RFC 4862 section 5.5.4 "+
			"says to keep using for existing communications, not one to refuse an endpoint "+
			"over: %v", err)
	}
	assertAttachedAs(t, ctx, f, id, at)

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

// No earlier fixture advertised the managed flag beside an autonomous prefix; harness.V6AutoFallback does (#817).

// TestSLAAC_AutoFallsBackOntoTheAdvertisedPrefix checks that ipv6_mode=auto falls back to the advertised prefix when DHCPv6 is silent (#817).
func TestSLAAC_AutoFallsBackOntoTheAdvertisedPrefix(t *testing.T) {
	testSLAAC_AutoFallsBackOntoTheAdvertisedPrefix(t, onV6Bridge)
}

func TestSLAAC_AutoFallsBackOntoTheAdvertisedPrefix_Macvlan(t *testing.T) {
	testSLAAC_AutoFallsBackOntoTheAdvertisedPrefix(t, onV6Macvlan)
}

func testSLAAC_AutoFallsBackOntoTheAdvertisedPrefix(t *testing.T, at v6Attach) {
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

	id, err := startOnV6SegmentAs(t, ctx, cli, f, at, at.net("dh-itest-autofb"),
		map[string]string{"ipv6": "", "ipv6_mode": "auto"})
	if err != nil {
		t.Fatalf("the container did not start on an ipv6_mode=auto segment that advertises "+
			"DHCPv6, answers no Solicit and advertises an autonomous prefix. That segment "+
			"is exactly what the fallback exists for: %v", err)
	}
	assertAttachedAs(t, ctx, f, id, at)

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

	// The server's log must carry an ignored Solicit and no Advertise or Reply, so a plugin that never solicited cannot pass.
	f.AssertExchange(60 * time.Second)
}

// The ipv6_mode=dhcp row is the preservation control: a present DHCPv6 server answers whether or not a router
// advertises (#868), so a change that made the ending fatal for everyone fails that row.

// TestSLAAC_ASegmentWithNoRouterEndsAFormingEndpoint checks that a forming IPv6 mode fails on a segment with no router advertisement (#989).
func TestSLAAC_ASegmentWithNoRouterEndsAFormingEndpoint(t *testing.T) {
	testSLAAC_ASegmentWithNoRouterEndsAFormingEndpoint(t, onV6Bridge)
}

func TestSLAAC_ASegmentWithNoRouterEndsAFormingEndpoint_Macvlan(t *testing.T) {
	testSLAAC_ASegmentWithNoRouterEndsAFormingEndpoint(t, onV6Macvlan)
}

func testSLAAC_ASegmentWithNoRouterEndsAFormingEndpoint(t *testing.T, at v6Attach) {
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

			id, startErr := startOnV6SegmentAs(t, ctx, cli, f, at, at.net(c.net),
				map[string]string{"ipv6": "", "ipv6_mode": c.mode})
			if startErr == nil {
				assertAttachedAs(t, ctx, f, id, at)
			} else {
				assertServedOnSegment(t, f)
			}

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

			// The counter covers every endpoint that saw no advertisement, whatever the outcome.
			before, after := w.End()
			if n := after.DHCPv6NoRouterAdvert - before.DHCPv6NoRouterAdvert; n < 1 {
				t.Errorf("dhcpv6_no_router_advert moved by %d in ipv6_mode=%s, want at "+
					"least 1: nothing advertised on this segment and the plugin did not "+
					"say so", n, c.mode)
			}
		})
	}
}

// A client reading its own Neighbor Advertisement as a conflict would drop the address on every restart, with no
// second try under RFC 4862 section 5.4.5.

// TestSLAAC_TheAddressComesBackAfterAPluginRestart checks that a formed address survives a plugin restart and is not dadfailed (#818).
func TestSLAAC_TheAddressComesBackAfterAPluginRestart(t *testing.T) {
	testSLAAC_TheAddressComesBackAfterAPluginRestart(t, onV6Bridge)
}

func TestSLAAC_TheAddressComesBackAfterAPluginRestart_Macvlan(t *testing.T) {
	testSLAAC_TheAddressComesBackAfterAPluginRestart(t, onV6Macvlan)
}

func testSLAAC_TheAddressComesBackAfterAPluginRestart(t *testing.T, at v6Attach) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6SLAAC)
	dumpOnFailure(t, f)

	// The engine's permanent NODAD copy survives a restart untouched, so both reads are of this plugin's install.
	w := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses")

	id, err := startOnV6SegmentAs(t, ctx, cli, f, at, at.net("dh-itest-slaacrs"),
		map[string]string{"ipv6": "", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("the container did not start on an ipv6_mode=slaac segment: %v", err)
	}
	assertAttachedAs(t, ctx, f, id, at)
	prefix := v6SegmentPrefix(t)
	addrBefore, _ := awaitPluginAppliedV6(t, ctx, w, id, prefix, slaacAddrBudget(), v6InstalledSinceBaseline)

	// CounterWindow refuses a window that spans a restart.
	if before, after := w.End(); after.IPv6SLAACAddresses-before.IPv6SLAACAddresses != 1 {
		t.Errorf("ipv6_slaac_addresses moved by %d before the restart, want 1",
			after.IPv6SLAACAddresses-before.IPv6SLAACAddresses)
	}

	// The resumed endpoint's v4 probe is the asynchronous one and races teardown; one container, one lease, as in
	// TestRecovery_PluginDisableEnable_PreservesEndpoint (#551).
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

	// The restarted process's counters start at zero, so v6InstalledByThisProcess asks whether it installed the address.
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
