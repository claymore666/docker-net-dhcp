// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// advertFormBudget is the acquisition budget plus one sender repeat, the longest a client waits for its first
// advertisement from harness.RASender (RFC 4862 section 5.5.3, #1016).
func advertFormBudget() time.Duration {
	return harness.IPAcquisitionBudget + harness.RASenderInterval
}

// advertChangeBudget is one sender repeat, covering a lost frame, plus one raWatchInterval: the chassis reads an
// advertisement change every minDelayBetweenRAs/4 = 750 ms (pkg/dhcp/chassis.go, #1016).
const advertChangeBudget = harness.RASenderInterval + 750*time.Millisecond

// kernelExpirySlack covers the plugin flooring valid_lft to whole seconds (pkg/dhcp/leaseinfo.go secondsUntil) and
// the kernel's address check: valid_lft 3 left the link after 3.03 to 3.32 s, measured 2026-09-24 (#1016).
const kernelExpirySlack = time.Second

// routeReadFloor is formedAddrReadFloor's exec round-trip on a loaded runner, applied to a route: the plugin installs
// it by one netlink call inside the advertisement event, so the rest is the read. Both skip_routes arms read to the
// same deadline, so a route the skip arm misses would also have been late for the positive arm (#1016).
const routeReadFloor = formedAddrReadFloor

func mustPrefix(t *testing.T, cidr string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		t.Fatalf("prefix %q does not parse: %v", cidr, err)
	}
	return p
}

func advertPrefix(t *testing.T, cidr string, autonomous bool, valid, preferred uint32) harness.RAPrefix {
	t.Helper()
	p := mustPrefix(t, cidr)
	return harness.RAPrefix{
		Prefix:            net.IP(p.Addr().AsSlice()),
		PrefixLen:         uint8(p.Bits()),
		OnLink:            true,
		Autonomous:        autonomous,
		ValidLifetime:     valid,
		PreferredLifetime: preferred,
	}
}

func advertRoute(t *testing.T, cidr string) harness.RARoute {
	t.Helper()
	p := mustPrefix(t, cidr)
	return harness.RARoute{Prefix: net.IP(p.Addr().AsSlice()), PrefixLen: uint8(p.Bits()), Lifetime: harness.AdvertRouteLifetime}
}

func slaacOn(p ...harness.RAPrefix) harness.RASpec {
	return harness.RASpec{RouterLifetime: harness.AdvertRouteLifetime, Prefixes: p}
}

func dockerClientFor(t *testing.T) *docker.Client {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// startSenderSegment brings up a segment whose dnsmasq sends no advertisement, then the harness sender over it.
func startSenderSegment(t *testing.T, rangeArgs []string, spec harness.RASpec) (*harness.V6Fixture, *harness.RASender) {
	t.Helper()
	f := harness.NewV6FixtureWithArgs(t, harness.V6NoRA, rangeArgs)
	dumpOnFailure(t, f)
	return f, harness.StartRASender(t, f.Bridge(), spec)
}

func formedAtLeast(n int32) func(now, before *harness.HealthResponse) bool {
	return func(now, before *harness.HealthResponse) bool {
		return now.IPv6SLAACAddresses-before.IPv6SLAACAddresses >= n
	}
}

// globalV6Set is every global-scope IPv6 address on the container's links, sorted.
func globalV6Set(t *testing.T, ctx context.Context, id string) ([]string, string) {
	t.Helper()
	out := harness.ExecOutput(t, ctx, id, "ip", "-6", "-o", "addr", "show", "scope", "global")
	var set []string
	for _, field := range strings.Fields(out) {
		bare, _, ok := strings.Cut(field, "/")
		if !ok {
			continue
		}
		if a, err := netip.ParseAddr(bare); err == nil && a.Is6() {
			set = append(set, bare)
		}
	}
	slices.Sort(set)
	return set, out
}

func frameAdvertises(fr harness.RAFrame, p netip.Prefix) bool {
	for _, pi := range fr.Prefixes {
		a, ok := netip.AddrFromSlice(pi.Prefix)
		if ok && a.Unmap() == p.Addr() && int(pi.PrefixLen) == p.Bits() && pi.ValidLifetime > 0 {
			return true
		}
	}
	return false
}

// awaitSenderFrame returns every captured frame once the one sent at `at` is on the wire, failing if it never is.
func awaitSenderFrame(t *testing.T, f *harness.V6Fixture, at time.Time) []harness.RAFrame {
	t.Helper()
	if _, ok := f.RACapture().AwaitRAAfter(at, harness.RASenderInterval); !ok {
		t.Fatalf("the sender's advertisement of %s never reached the capture on %s within one repeat (%s)",
			at.Format("15:04:05.000"), f.Bridge(), harness.RASenderInterval)
	}
	return f.RACapture().Frames()
}

// lastFrameAdvertising is when a nonzero valid lifetime for p last went out, the instant its countdown restarted.
func lastFrameAdvertising(t *testing.T, frames []harness.RAFrame, p netip.Prefix) time.Time {
	t.Helper()
	var last time.Time
	for _, fr := range frames {
		if frameAdvertises(fr, p) && fr.At.After(last) {
			last = fr.At
		}
	}
	if last.IsZero() {
		t.Fatalf("no captured advertisement carries %s with a valid lifetime", p)
	}
	return last
}

// TestMainPrefix_TheSecondOfTwoAdvertisedPrefixesIsWhatDockerReports checks ipv6_main_prefix picks a later prefix's address (#1016).
func TestMainPrefix_TheSecondOfTwoAdvertisedPrefixesIsWhatDockerReports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	a, b := mustPrefix(t, harness.AdvertPrefixA), mustPrefix(t, harness.AdvertPrefixB)
	f, _ := startSenderSegment(t, harness.RangeArgsFor(harness.V6NoRA), slaacOn(
		advertPrefix(t, harness.AdvertPrefixA, true, 1800, 1800),
		advertPrefix(t, harness.AdvertPrefixB, true, 1800, 1800)))

	w := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses", "ipv6_main_prefix_unmatched")
	netName := "dh-itest-mainb"
	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, netName, map[string]string{
		"ipv6": "", "ipv6_mode": "slaac", "ipv6_main_prefix": harness.AdvertPrefixB})
	if err != nil {
		t.Fatalf("the container did not start on a segment advertising %s and %s: %v", a, b, err)
	}

	addrB, _ := awaitPluginAppliedV6(t, ctx, w, id, b, advertFormBudget(), formedAtLeast(2))
	addrA, _ := awaitContainerV6(t, ctx, id, a, formedAddrReadFloor)

	if got := inspectV6(t, ctx, cli, id, netName); got != addrB {
		t.Errorf("docker inspect reports %q; ipv6_main_prefix=%s names the second advertised prefix, "+
			"whose address on the link is %q (the first prefix formed %q)", got, b, addrB, addrA)
	}
	before, after := w.End()
	if n := after.IPv6MainPrefixUnmatched - before.IPv6MainPrefixUnmatched; n != 0 {
		t.Errorf("ipv6_main_prefix_unmatched moved by %d although %s holds an address in %s", n, addrB, b)
	}
}

// TestMainPrefix_AnUnadvertisedPrefixReportsTheFirstAdvertisedOne checks the fallback's address, counter and log line (#1016).
func TestMainPrefix_AnUnadvertisedPrefixReportsTheFirstAdvertisedOne(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	a, b := mustPrefix(t, harness.AdvertPrefixA), mustPrefix(t, harness.AdvertPrefixB)
	f, _ := startSenderSegment(t, harness.RangeArgsFor(harness.V6NoRA), slaacOn(
		advertPrefix(t, harness.AdvertPrefixA, true, 1800, 1800),
		advertPrefix(t, harness.AdvertPrefixB, true, 1800, 1800)))

	w := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses", "ipv6_main_prefix_unmatched")
	mark := harness.MarkPluginLog(t, ctx)
	netName := "dh-itest-mainx"
	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, netName, map[string]string{
		"ipv6": "", "ipv6_mode": "slaac", "ipv6_main_prefix": harness.UnadvertisedPrefix})
	if err != nil {
		t.Fatalf("the container did not start with an ipv6_main_prefix nothing advertises; the option "+
			"picks the reported address and never fails the endpoint: %v", err)
	}

	addrA, _ := awaitPluginAppliedV6(t, ctx, w, id, a, advertFormBudget(), formedAtLeast(2))
	awaitContainerV6(t, ctx, id, b, formedAddrReadFloor)

	if got := inspectV6(t, ctx, cli, id, netName); got != addrA {
		t.Errorf("docker inspect reports %q; with %s unadvertised the first advertised prefix %s "+
			"is reported, whose address on the link is %q", got, harness.UnadvertisedPrefix, a, addrA)
	}

	const warning = "No address of this endpoint falls inside the network's ipv6_main_prefix"
	named := func(window string) bool {
		for _, line := range strings.Split(window, "\n") {
			if strings.Contains(line, warning) && strings.Contains(line, harness.UnadvertisedPrefix) &&
				strings.Contains(line, addrA) {
				return true
			}
		}
		return false
	}
	if window := harness.AwaitPluginLogSince(t, ctx, mark, formedAddrReadFloor, named); !named(window) {
		t.Errorf("no plugin log line says %q naming both %s and the reported %s; window:\n%s",
			warning, harness.UnadvertisedPrefix, addrA, window)
	}

	before, after := w.End()
	if n := after.IPv6MainPrefixUnmatched - before.IPv6MainPrefixUnmatched; n != 1 {
		t.Errorf("ipv6_main_prefix_unmatched moved by %d for one endpoint whose prefix matched nothing, want 1", n)
	}
}

// TestAutoStrict_ASilentManagedServerFailsTheEndpointAndTheDefaultFallsBack checks ipv6_auto_strict on an M-flag segment (#1016).
func TestAutoStrict_ASilentManagedServerFailsTheEndpointAndTheDefaultFallsBack(t *testing.T) {
	cases := []struct {
		name, net, strict string
		wantStart         bool
	}{
		{"strict", "dh-itest-austr", "true", false},
		{"default", "dh-itest-audef", "", true},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := harness.NewV6Fixture(t, harness.V6AutoFallback)
			dumpOnFailure(t, f)

			w := harness.BeginCounterWindow(t, ctx, cli,
				"dhcpv6_auto_fallbacks", "dhcpv6_no_server", "ipv6_slaac_addresses")
			id, startErr := startOnV6SegmentWithOpts(t, ctx, cli, f, c.net, map[string]string{
				"ipv6": "", "ipv6_mode": "auto", "ipv6_auto_strict": c.strict})

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
				before, after := w.End()
				if n := after.DHCPv6AutoFallbacks - before.DHCPv6AutoFallbacks; n != 0 {
					t.Errorf("dhcpv6_auto_fallbacks moved by %d under ipv6_auto_strict=true", n)
				}
				if n := after.IPv6SLAACAddresses - before.IPv6SLAACAddresses; n != 0 {
					t.Errorf("ipv6_slaac_addresses moved by %d: strict formed an address anyway", n)
				}
				if n := after.DHCPv6NoServer - before.DHCPv6NoServer; n != 1 {
					t.Errorf("dhcpv6_no_server moved by %d for one endpoint failed by a silent server, want 1", n)
				}
				return
			}

			if startErr != nil {
				t.Fatalf("the container did not start with ipv6_auto_strict unset; the default falls "+
					"back to the advertised prefix: %v", startErr)
			}
			addr, _ := awaitPluginAppliedV6(t, ctx, w, id, v6SegmentPrefix(t), slaacAddrBudget(), v6InstalledSinceBaseline)
			if got := inspectV6(t, ctx, cli, id, c.net); got != addr {
				t.Errorf("docker inspect reports %q and the fallback formed %q", got, addr)
			}
			f.AwaitIgnoredSolicit(harness.IPAcquisitionBudget)
			before, after := w.End()
			if n := after.DHCPv6AutoFallbacks - before.DHCPv6AutoFallbacks; n != 1 {
				t.Errorf("dhcpv6_auto_fallbacks moved by %d for one endpoint that fell back, want 1", n)
			}
			if n := after.DHCPv6NoServer - before.DHCPv6NoServer; n != 0 {
				t.Errorf("dhcpv6_no_server moved by %d on an endpoint that fell back and started", n)
			}
		})
	}
}

// resolvOrder is resolv.conf's nameservers and search domains in file order.
func resolvOrder(resolv string) (nameservers, search []string) {
	for _, line := range strings.Split(resolv, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "nameserver":
			nameservers = append(nameservers, fields[1])
		case "search":
			search = append(search, fields[1:]...)
		}
	}
	return nameservers, search
}

// TestAdvertDNS_ReachesTheResolverAndDHCPv6ComesFirst checks RFC 8106 RDNSS and DNSSL in resolv.conf, after DHCPv6's own (#1016).
func TestAdvertDNS_ReachesTheResolverAndDHCPv6ComesFirst(t *testing.T) {
	cases := []struct {
		name, net  string
		mode       harness.V6Mode
		args       []string
		opts       map[string]string
		wantNS     []string
		wantSearch []string
	}{
		{
			name: "advert only", net: "dh-itest-radns", mode: harness.V6SLAAC,
			args:       harness.RangeArgsFor(harness.V6SLAAC),
			opts:       map[string]string{"ipv6": "", "ipv6_mode": "slaac"},
			wantNS:     []string{harness.V6DNSServer},
			wantSearch: []string{harness.V6SearchDomain},
		},
		{
			name: "dhcpv6 and advert", net: "dh-itest-bodns", mode: harness.V6Managed,
			args:       append(harness.RangeArgsFor(harness.V6Managed), harness.V6DHCPOnlyDNSArgs()...),
			wantNS:     []string{harness.V6DHCPOnlyDNSServer, harness.V6DNSServer},
			wantSearch: []string{harness.V6DHCPOnlySearchDomain, harness.V6SearchDomain},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := harness.NewV6FixtureWithArgs(t, c.mode, c.args)
			dumpOnFailure(t, f)

			id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, c.net, c.opts)
			if err != nil {
				t.Fatalf("the container did not start: %v", err)
			}

			frames := f.RACapture().FramesAfter(f.StartedAt())
			var rdnss []string
			var dnssl []string
			for _, fr := range frames {
				for _, ip := range fr.DNSServers {
					rdnss = append(rdnss, ip.String())
				}
				dnssl = append(dnssl, fr.SearchDomains...)
			}
			if !slices.Contains(rdnss, harness.V6DNSServer) || !slices.Contains(dnssl, harness.V6SearchDomain) {
				t.Fatalf("no captured advertisement carries RDNSS %s and DNSSL %s, so the advertisement is "+
					"not a source here (RDNSS %v, DNSSL %v)", harness.V6DNSServer, harness.V6SearchDomain, rdnss, dnssl)
			}
			if slices.Contains(rdnss, harness.V6DHCPOnlyDNSServer) || slices.Contains(dnssl, harness.V6DHCPOnlySearchDomain) {
				t.Fatalf("the advertisement carries the DHCPv6-only values (RDNSS %v, DNSSL %v), so the order "+
					"below cannot tell the two sources apart", rdnss, dnssl)
			}
			if c.mode == harness.V6Managed && f.CountLogLines("dns-server", harness.V6DHCPOnlyDNSServer) == 0 {
				t.Fatalf("the server's log shows no DHCPv6 reply carrying dns-server %s", harness.V6DHCPOnlyDNSServer)
			}

			var resolv string
			var ns, search []string
			deadline := time.Now().Add(persistentV6BindBudget)
			for {
				resolv = harness.ExecOutput(t, ctx, id, "cat", "/etc/resolv.conf")
				ns, search = resolvOrder(resolv)
				if slices.Equal(v6Only(ns), c.wantNS) && isSubsequence(c.wantSearch, search) {
					return
				}
				if !time.Now().Before(deadline) {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			t.Errorf("resolv.conf after %s has IPv6 nameservers %v and search %v, want nameservers %v and "+
				"search %v in that order: DHCPv6's entries first, the advertisement's after (RFC 8106 "+
				"section 5.3.1):\n%s", persistentV6BindBudget, v6Only(ns), search, c.wantNS, c.wantSearch, resolv)
		})
	}
}

func v6Only(addrs []string) []string {
	var out []string
	for _, s := range addrs {
		if a, err := netip.ParseAddr(s); err == nil && a.Is6() {
			out = append(out, s)
		}
	}
	return out
}

func isSubsequence(want, got []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// pluginV6Routes drops `proto ra` lines, which only the container's own kernel installs from an advertisement it heard
// before the guard's accept_ra=0; the plugin's routes never carry that protocol (#911, #1016).
func pluginV6Routes(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.Contains(line, "proto ra") {
			lines = append(lines, line)
		}
	}
	return lines
}

func routeTo(lines []string, cidr string) (string, bool) {
	for _, line := range lines {
		if strings.HasPrefix(line, cidr+" ") {
			return line, true
		}
	}
	return "", false
}

// TestAdvertRoutes_BecomeContainerRoutesUnlessSkipRoutes checks RFC 4191 and on-link routes on both paths, and skip_routes on both (#1016).
func TestAdvertRoutes_BecomeContainerRoutesUnlessSkipRoutes(t *testing.T) {
	cases := []struct {
		name, net, skip string
		wantRoutes      bool
	}{
		{"routes", "dh-itest-rioon", "", true},
		{"skip_routes", "dh-itest-riosk", "true", false},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := harness.RASpec{
				Managed:        true,
				RouterLifetime: harness.AdvertRouteLifetime,
				Prefixes:       []harness.RAPrefix{advertPrefix(t, harness.AdvertOnLinkPrefix, false, 1800, 1800)},
				Routes:         []harness.RARoute{advertRoute(t, harness.AdvertRoutePrefix)},
			}
			f, sender := startSenderSegment(t, harness.V6PoolWithoutRAArgs(), spec)

			id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, c.net, map[string]string{"skip_routes": c.skip})
			if err != nil {
				t.Fatalf("the container did not start on a managed segment with a DHCPv6 server: %v", err)
			}
			addr := inspectV6(t, ctx, cli, id, c.net)
			if addr == "" {
				t.Fatal("docker inspect reports no IPv6 address on a segment whose DHCPv6 server answers")
			}

			bindDeadline := time.Now().Add(persistentV6BindBudget)
			for f.CountLogLines("DHCPREPLY", addr) < 2 {
				if !time.Now().Before(bindDeadline) {
					t.Fatalf("the server logged fewer than two DHCPREPLY lines for %s after %s: the persistent "+
						"client never bound, so the lease path was never exercised", addr, persistentV6BindBudget)
				}
				time.Sleep(250 * time.Millisecond)
			}

			spec.Routes = append(spec.Routes, advertRoute(t, harness.AdvertRoutePrefix2))
			at := sender.Set(spec)
			awaitSenderFrame(t, f, at)

			iface := containerV6Iface(t, ctx, id, addr)
			if got := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat",
				"/proc/sys/net/ipv6/conf/"+iface+"/accept_ra")); got != "0" {
				t.Errorf("accept_ra on %s is %q, want 0: the container's kernel would install advertised "+
					"routes itself and this test could not tell them from the plugin's", iface, got)
			}

			want := []string{harness.AdvertOnLinkPrefix, harness.AdvertRoutePrefix, harness.AdvertRoutePrefix2}
			var out string
			var lines []string
			deadline := at.Add(advertChangeBudget + routeReadFloor)
			for {
				out = harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show")
				lines = pluginV6Routes(out)
				present := 0
				for _, cidr := range want {
					if _, ok := routeTo(lines, cidr); ok {
						present++
					}
				}
				if c.wantRoutes && present == len(want) {
					break
				}
				if !time.Now().Before(deadline) {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}

			for _, cidr := range want {
				line, ok := routeTo(lines, cidr)
				switch {
				case c.wantRoutes && !ok:
					t.Errorf("no route to %s in the container %s after the advertisement carrying it "+
						"went out:\n%s", cidr, advertChangeBudget+routeReadFloor, out)
				case c.wantRoutes && cidr != harness.AdvertOnLinkPrefix && !strings.Contains(line, "via fe80::"):
					t.Errorf("the RFC 4191 route to %s is not via the advertising router's link-local "+
						"address: %q", cidr, line)
				case c.wantRoutes && cidr == harness.AdvertOnLinkPrefix && strings.Contains(line, " via "):
					t.Errorf("the on-link prefix %s is routed via a gateway: %q", cidr, line)
				case !c.wantRoutes && ok:
					t.Errorf("skip_routes=true and the container has a route to %s (%q) after the lease "+
						"path's bind and the advertisement-change path's %s:\n%s", cidr, line,
						advertChangeBudget+routeReadFloor, out)
				}
			}
			if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "default via fe80::") }) {
				t.Errorf("no IPv6 default route via the advertising router; skip_routes leaves the default "+
					"route alone:\n%s", out)
			}
		})
	}

	t.Run("advert after join", func(t *testing.T) { onLinkAfterJoin(t, ctx, cli) })
}

// awaitPluginRoutes reads the container's plugin-installed IPv6 routes until done holds or the deadline passes.
func awaitPluginRoutes(t *testing.T, ctx context.Context, id string, deadline time.Time, done func([]string) bool) (string, []string, bool) {
	t.Helper()
	for {
		out := harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show")
		lines := pluginV6Routes(out)
		if done(lines) {
			return out, lines, true
		}
		if !time.Now().Before(deadline) {
			return out, lines, false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// onLinkAfterJoin binds the lease on a segment with no advertisement, so Join carries no on-link prefix, then
// advertises one: it must become a route, stay through a frame omitting it, and leave on Valid Lifetime 0 (RFC 4861
// section 6.3.4, #1088).
func onLinkAfterJoin(t *testing.T, ctx context.Context, cli *docker.Client) {
	const netName = "dh-itest-riolate"
	f := harness.NewV6FixtureWithArgs(t, harness.V6NoRA, harness.V6PoolWithoutRAArgs())
	dumpOnFailure(t, f)

	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, netName, nil)
	if err != nil {
		t.Fatalf("the container did not start on a segment whose DHCPv6 server answers: %v", err)
	}
	addr := inspectV6(t, ctx, cli, id, netName)
	if addr == "" {
		t.Fatal("docker inspect reports no IPv6 address on a segment whose DHCPv6 server answers")
	}
	bindDeadline := time.Now().Add(persistentV6BindBudget)
	for f.CountLogLines("DHCPREPLY", addr) < 2 {
		if !time.Now().Before(bindDeadline) {
			t.Fatalf("the server logged fewer than two DHCPREPLY lines for %s after %s: the persistent client "+
				"never bound, so no advertisement can arrive after the bind", addr, persistentV6BindBudget)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if frames := f.RACapture().Frames(); len(frames) != 0 {
		t.Fatalf("%d advertisements were on the segment before the sender started, so Join could have carried "+
			"the prefix and this arm would not test #1088", len(frames))
	}
	onLink := func(lines []string) bool { _, ok := routeTo(lines, harness.AdvertOnLinkPrefix); return ok }
	if out := harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show"); onLink(pluginV6Routes(out)) {
		t.Fatalf("the container has a route to %s before anything advertised it:\n%s", harness.AdvertOnLinkPrefix, out)
	}

	spec := harness.RASpec{
		Managed:        true,
		RouterLifetime: harness.AdvertRouteLifetime,
		Prefixes:       []harness.RAPrefix{advertPrefix(t, harness.AdvertOnLinkPrefix, false, 1800, 1800)},
	}
	at := time.Now()
	sender := harness.StartRASender(t, f.Bridge(), spec)
	awaitSenderFrame(t, f, at)
	out, lines, ok := awaitPluginRoutes(t, ctx, id, at.Add(advertChangeBudget+routeReadFloor), onLink)
	if !ok {
		t.Fatalf("no route to %s in the container %s after the first advertisement carrying it went out, "+
			"after the lease bound:\n%s", harness.AdvertOnLinkPrefix, advertChangeBudget+routeReadFloor, out)
	}
	if line, _ := routeTo(lines, harness.AdvertOnLinkPrefix); strings.Contains(line, " via ") {
		t.Errorf("the on-link prefix %s is routed via a gateway: %q", harness.AdvertOnLinkPrefix, line)
	}

	// The Route Information option (RFC 4191 section 2.3) is the frame's receipt: its route proves the plugin read the
	// frame that omits the prefix.
	spec.Prefixes = nil
	spec.Routes = []harness.RARoute{advertRoute(t, harness.AdvertRoutePrefix)}
	at = sender.Set(spec)
	awaitSenderFrame(t, f, at)
	read := func(lines []string) bool { _, ok := routeTo(lines, harness.AdvertRoutePrefix); return ok }
	out, lines, ok = awaitPluginRoutes(t, ctx, id, at.Add(advertChangeBudget+routeReadFloor), read)
	if !ok {
		t.Fatalf("no route to %s %s after the advertisement omitting %s went out, so nothing shows the plugin "+
			"read it:\n%s", harness.AdvertRoutePrefix, advertChangeBudget+routeReadFloor, harness.AdvertOnLinkPrefix, out)
	}
	if !onLink(lines) {
		t.Fatalf("an advertisement that only omitted %s removed its route; only Valid Lifetime 0 withdraws a "+
			"prefix (RFC 4861 section 6.3.4):\n%s", harness.AdvertOnLinkPrefix, out)
	}

	spec.Prefixes = []harness.RAPrefix{advertPrefix(t, harness.AdvertOnLinkPrefix, false, 0, 0)}
	at = sender.Set(spec)
	awaitSenderFrame(t, f, at)
	gone := func(lines []string) bool { return !onLink(lines) }
	if out, _, ok = awaitPluginRoutes(t, ctx, id, at.Add(advertChangeBudget+routeReadFloor), gone); !ok {
		t.Errorf("the route to %s is still in the container %s after an advertisement gave it Valid Lifetime 0:\n%s",
			harness.AdvertOnLinkPrefix, advertChangeBudget+routeReadFloor, out)
	}
}

// TestSLAAC_AnAddressWhoseValidLifetimeRunsOutLeavesTheLink checks RFC 4862 section 5.5.4 expiry and its counter (#1016).
func TestSLAAC_AnAddressWhoseValidLifetimeRunsOutLeavesTheLink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	const shortValid = 12
	a, b := mustPrefix(t, harness.AdvertPrefixA), mustPrefix(t, harness.AdvertPrefixB)
	f, sender := startSenderSegment(t, harness.RangeArgsFor(harness.V6NoRA), slaacOn(
		advertPrefix(t, harness.AdvertPrefixA, true, shortValid, shortValid),
		advertPrefix(t, harness.AdvertPrefixB, true, 1800, 1800)))

	w := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses", "ipv6_addresses_withdrawn")
	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, "dh-itest-v6exp", map[string]string{
		"ipv6": "", "ipv6_mode": "slaac", "ipv6_main_prefix": harness.AdvertPrefixB})
	if err != nil {
		t.Fatalf("the container did not start: %v", err)
	}
	addrB, _ := awaitPluginAppliedV6(t, ctx, w, id, b, advertFormBudget(), formedAtLeast(2))
	addrA, _ := awaitContainerV6(t, ctx, id, a, formedAddrReadFloor)

	at := sender.Set(slaacOn(advertPrefix(t, harness.AdvertPrefixB, true, 1800, 1800)))
	last := lastFrameAdvertising(t, awaitSenderFrame(t, f, at), a)
	gone := last.Add(shortValid*time.Second + kernelExpirySlack)

	var set []string
	var out string
	for {
		set, out = globalV6Set(t, ctx, id)
		if !slices.Contains(set, addrA) || !time.Now().Before(gone) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if slices.Contains(set, addrA) {
		t.Fatalf("%s is still on the link %s after the last advertisement of %s with valid lifetime "+
			"%ds went out (RFC 4862 section 5.5.4):\n%s", addrA, time.Since(last).Round(time.Millisecond), a, shortValid, out)
	}
	if !slices.Contains(set, addrB) {
		t.Errorf("%s, still advertised, left the link with %s:\n%s", addrB, addrA, out)
	}

	withdrawn := func(now, before *harness.HealthResponse) bool {
		return now.IPv6AddressesWithdrawn > before.IPv6AddressesWithdrawn
	}
	w.Await(time.Until(gone)+formedAddrReadFloor, withdrawn)
	before, after := w.End()
	if n := after.IPv6AddressesWithdrawn - before.IPv6AddressesWithdrawn; n != 1 {
		t.Errorf("ipv6_addresses_withdrawn moved by %d after one address's valid lifetime ran out, want 1", n)
	}
}

// TestSLAAC_RenumberingInOneAdvertisementEndsWithExactlyTheNewSet checks one prefix in and one out in the same RA (#1016).
func TestSLAAC_RenumberingInOneAdvertisementEndsWithExactlyTheNewSet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	const oldValid = 30
	a, b := mustPrefix(t, harness.AdvertPrefixA), mustPrefix(t, harness.AdvertPrefixB)
	f, sender := startSenderSegment(t, harness.RangeArgsFor(harness.V6NoRA), slaacOn(
		advertPrefix(t, harness.AdvertPrefixA, true, oldValid, oldValid)))

	w := harness.BeginCounterWindow(t, ctx, cli, "ipv6_slaac_addresses", "ipv6_addresses_withdrawn")
	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, "dh-itest-renum", map[string]string{
		"ipv6": "", "ipv6_mode": "slaac"})
	if err != nil {
		t.Fatalf("the container did not start: %v", err)
	}
	addrA, _ := awaitPluginAppliedV6(t, ctx, w, id, a, advertFormBudget(), v6InstalledSinceBaseline)

	at := sender.Set(slaacOn(
		advertPrefix(t, harness.AdvertPrefixB, true, 1800, 1800),
		advertPrefix(t, harness.AdvertPrefixA, true, 0, 0)))
	last := lastFrameAdvertising(t, awaitSenderFrame(t, f, at), a)
	addrB, _ := awaitContainerV6(t, ctx, id, b, harness.IPAcquisitionBudget)
	held := at.Add(oldValid*time.Second - harness.RASenderInterval - kernelExpirySlack)
	if set, out := globalV6Set(t, ctx, id); !time.Now().Before(held) {
		t.Fatalf("%s formed only %s after the renumbering frame, past the %s the old address is still "+
			"owed:\n%s", addrB, time.Since(at).Round(time.Millisecond), held.Sub(at), out)
	} else if !slices.Contains(set, addrA) {
		t.Errorf("%s left the link as soon as %s was advertised with valid 0, with under two hours "+
			"left; RFC 4862 section 5.5.3(e) keeps it to its own expiry:\n%s", addrA, a, out)
	}
	gone := last.Add(oldValid*time.Second + kernelExpirySlack)

	var set []string
	var out string
	for {
		set, out = globalV6Set(t, ctx, id)
		if slices.Equal(set, []string{addrB}) || !time.Now().Before(gone) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !slices.Equal(set, []string{addrB}) {
		t.Fatalf("the link holds %v %s after the last advertisement of %s (valid %ds) went out, want "+
			"exactly [%s]: the renumbered prefix %s is in, the old one's address %s is out:\n%s",
			set, time.Since(last).Round(time.Millisecond), a, oldValid, addrB, b, addrA, out)
	}

	withdrawn := func(now, before *harness.HealthResponse) bool {
		return now.IPv6AddressesWithdrawn > before.IPv6AddressesWithdrawn
	}
	w.Await(time.Until(gone)+formedAddrReadFloor, withdrawn)
	before, after := w.End()
	if n := after.IPv6AddressesWithdrawn - before.IPv6AddressesWithdrawn; n != 1 {
		t.Errorf("ipv6_addresses_withdrawn moved by %d for the one address renumbering took out, want 1", n)
	}
	if n := after.IPv6SLAACAddresses - before.IPv6SLAACAddresses; n != 2 {
		t.Errorf("ipv6_slaac_addresses moved by %d for the old and the new prefix, want 2", n)
	}
}
