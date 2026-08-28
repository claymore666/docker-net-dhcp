// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// The half of IPv6 the maintainer's release bar names as "a plain
// address handed out by a DHCPv6 server", measured ON THE CONTAINER'S
// INTERFACE rather than in the plugin's report of it.
//
// WHY THIS IS NOT ALREADY COVERED BY
// TestDHCPv6_Managed_StillRequiresALease. That test reads
// ContainerInspect -- the ENGINE's record of what the plugin returned
// in res.Interface.AddressIPv6. It is a perfectly good preservation
// control for #868 and it is not evidence about the interface: an
// address the plugin reported and libnetwork failed to apply, an
// address applied and then withdrawn, and an address that is present
// but unusable all inspect identically. Two records with one observer
// is the failure to avoid, and inspect plus the plugin's counters are
// one observer wearing two hats.
//
// So everything below is read from INSIDE the container, and every
// address claim is cross-checked against the DHCP server's own log AND
// its lease database -- three records, three observers.
//
// WHY THE SIBLING CASE MAKES THIS URGENT. The stateless and SLAAC
// segments were measured in an isolated netns on 2026-08-27: a global
// address forms from the router advertisement, dhcpcd then writes
// accept_ra=0 and autoconf=0 on the link, and from that moment the
// kernel's lifetimes only count down -- 1797 -> 1787 -> 1647 against a
// control reading 1797 -> 1796 -> 1705 -- while the RA-derived default
// route is gone within about ten seconds. On-link ping6 keeps working
// throughout, which is exactly what hides it. `--noconfigure` does not
// prevent those two sysctl writes; a one-variable control with no
// dhcpcd at all, hand-writing the same two sysctls, reproduces both
// effects.
//
// The managed case runs the SAME persistent dhcpcd (`-6`,
// `--noconfigure`, pkg/dhcp/dhcpcd.go renderArgs) on the same link, so
// it inherits the same two sysctl writes. What differs is where the
// address comes from: here libnetwork applies it from
// res.Interface.AddressIPv6, so it is not the kernel's to expire. That
// makes the two questions COME APART, and this test asks them
// separately -- the address may well survive while the route does not.
//
// WHAT THE THREE SAMPLE POINTS ARE FOR. A single reading cannot tell a
// working configuration from one that is merely still in its first few
// seconds; that is precisely how the SLAAC defect stayed invisible.
type v6Sample struct {
	label   string
	elapsed time.Duration

	addr      string // ip -6 addr show dev <discovered ifname>
	route     string // ip -6 route show
	acceptRA  string // /proc/sys/net/ipv6/conf/<ifname>/accept_ra
	autoconf  string // .../autoconf
	disableV6 string // .../disable_ipv6

	// replies is the number of DHCPv6 REPLY lines the SERVER had
	// logged by this point. Rising between samples means the lease
	// was renewed ON THE WIRE, which is a different claim from the
	// interface's lifetimes being refreshed -- see M3.
	replies int
}

// v6Addr is one `inet6` entry with the flag text and lifetimes that
// belong to it.
type v6Addr struct {
	cidr     string
	flags    string // scope + any of tentative/dadfailed/deprecated/dynamic
	validLft string
	prefLft  string
}

// parseV6Addrs pairs each `inet6` line with the `valid_lft` line the
// kernel prints under it.
//
// Measured 2026-08-28 that this is parseable from the STOCK test
// image: alpine:3.20's `ip` is busybox, and busybox does print the
// lifetime fields and does render finite values --
//
//	inet 10.99.0.1/24 scope global dynamic eth0
//	   valid_lft 300sec preferred_lft 200sec
//
// -- so no iproute2 install is needed and the container under test
// stays the one every other test uses. The `dynamic` flag in that
// output is load-bearing here and not decoration: the kernel sets it
// on an address that carries a lifetime, so its ABSENCE beside
// `valid_lft forever` is what distinguishes an address libnetwork
// applied statically from one the kernel is ageing.
func parseV6Addrs(out string) []v6Addr {
	var addrs []v6Addr
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "inet6" {
			addrs = append(addrs, v6Addr{cidr: f[1], flags: strings.Join(f[2:], " ")})
			continue
		}
		if len(f) >= 2 && f[0] == "valid_lft" && len(addrs) > 0 {
			a := &addrs[len(addrs)-1]
			a.validLft = f[1]
			if len(f) >= 4 && f[2] == "preferred_lft" {
				a.prefLft = f[3]
			}
		}
	}
	return addrs
}

// globalFromPrefix picks the address under test: a global-scope
// address on the fixture's own prefix. Keyed on the prefix rather than
// on "the first global address" so a leaked address from another
// segment cannot be mistaken for this server's.
func globalFromPrefix(addrs []v6Addr, prefix string) (v6Addr, bool) {
	for _, a := range addrs {
		if strings.HasPrefix(a.cidr, prefix) && strings.Contains(a.flags, "global") {
			return a, true
		}
	}
	return v6Addr{}, false
}

func hasDefaultV6Route(routeOut string) bool {
	for _, line := range strings.Split(routeOut, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "default") {
			return true
		}
	}
	return false
}

// TestDHCPv6_Managed_AddressAndRouteOnTheInterface answers, on a
// segment where a DHCPv6 server really does assign an address: does
// the container end up with that address on its interface, does it
// keep it, and can it route off-link.
// containerIfname returns the name of the container's single
// non-loopback interface, DISCOVERED rather than assumed.
//
// The plugin does not always produce "eth0". pkg/plugin/network.go
// builds the Join response's DstPrefix from the endpoint mode: a
// parent-attached endpoint (macvlan/ipvlan) gets "eth", so libnetwork
// names the link eth0 -- but a BRIDGE endpoint gets the bridge name as
// the prefix, and the link inside the container is named "<bridge>0".
// The v6 fixtures are bridges. lifecycle_bridge_test.go already knew
// this and worked around it by never naming the link; these samples
// need the name, because per-interface sysctls live under it.
//
// This mattered: every sample here used to read "dev eth0", `ip`
// answered "can't find device", and the assertions reported that as
// "no global IPv6 address on the interface" -- a red that named a
// product defect and measured only a wrong device name.
//
// It fails rather than falling back to a default, because a sample
// keyed on the wrong device measures nothing at all.
func containerIfname(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	out := harness.ExecOutput(t, ctx, id, "ip", "-o", "link", "show")
	if name, ok := firstNonLoopback(out); ok {
		return name
	}
	t.Fatalf("no non-loopback interface inside the container, so there is nothing to "+
		"sample; `ip -o link show` said:\n%s", out)
	return ""
}

// firstNonLoopback picks the interface name out of `ip -o link show`
// output. Shared so the in-container and the nsenter callers cannot
// drift apart.
func firstNonLoopback(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		// "2: dh-itest-br60@if7: <BROADCAST,MULTICAST,UP,LOWER_UP> ..."
		_, rest, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		name := strings.TrimSpace(rest)
		if i := strings.IndexAny(name, ":@"); i >= 0 {
			name = name[:i]
		}
		if name = strings.TrimSpace(name); name == "" || name == "lo" {
			continue
		}
		return name, true
	}
	return "", false
}

func TestDHCPv6_Managed_AddressAndRouteOnTheInterface(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	f := harness.NewV6Fixture(t, harness.V6Managed)
	dumpOnFailure(t, f)

	const netName = "dh-itest-v6mgdif"
	id, err := startOnV6Segment(t, ctx, cli, f, netName)
	if err != nil {
		t.Fatalf("a container failed to start on a MANAGED DHCPv6 segment, where an "+
			"address is available. Nothing below can be measured:\n%v", err)
	}

	// The sample points.
	//
	// The lease this fixture hands out is harness.LeaseTime, which is
	// "2m" -- so the last sample at 150s is PAST the DHCPv6 lease's
	// own lifetime by 25%, and any address still present there either
	// was renewed or never carried that lifetime in the first place.
	//
	// It is NOT past the router advertisement's prefix lifetime, which
	// dnsmasq sets around 1800s and which is what the SLAAC arm was
	// measured against. That is a deliberate limit of this run, not an
	// oversight: covering it means a sample past ~30 minutes, and the
	// DHCPv6 lease is the thing this test is about. If the address
	// here turns out to be kernel-aged rather than static, the
	// follow-up measurement that settles the RA half is one more
	// sample at ~1900s.
	start := time.Now()
	samples := []struct {
		label string
		at    time.Duration
	}{
		{"a) as soon as it is up", 0},
		{"b) +15s", 15 * time.Second},
		{"c) +150s, past the 2m DHCPv6 lease", 150 * time.Second},
	}

	ifname := containerIfname(t, ctx, id)
	t.Logf("container interface discovered as %q (bridge mode does not name it eth0)", ifname)

	var got []v6Sample
	for _, s := range samples {
		if d := s.at - time.Since(start); d > 0 {
			time.Sleep(d)
		}
		smp := v6Sample{
			label:     s.label,
			elapsed:   time.Since(start).Round(time.Second),
			addr:      harness.ExecOutput(t, ctx, id, "ip", "-6", "addr", "show", "dev", ifname),
			route:     harness.ExecOutput(t, ctx, id, "ip", "-6", "route", "show"),
			acceptRA:  harness.ExecOutput(t, ctx, id, "cat", fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", ifname)),
			autoconf:  harness.ExecOutput(t, ctx, id, "cat", fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/autoconf", ifname)),
			disableV6: harness.ExecOutput(t, ctx, id, "cat", fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", ifname)),
			replies:   f.CountLogLines("DHCPREPLY"),
		}
		got = append(got, smp)
		t.Logf("=== %s (t+%s) ===\n"+
			"ip -6 addr show dev %s:\n%s\n"+
			"ip -6 route show:\n%s\n"+
			"accept_ra=%s autoconf=%s disable_ipv6=%s\n"+
			"server DHCPREPLY lines so far: %d",
			smp.label, smp.elapsed, ifname,
			smp.addr, smp.route,
			strings.TrimSpace(smp.acceptRA), strings.TrimSpace(smp.autoconf),
			strings.TrimSpace(smp.disableV6), smp.replies)
	}

	first, last := got[0], got[len(got)-1]

	// ---- M1: is the address on the interface, and is it usable? ----
	a0, ok := globalFromPrefix(parseV6Addrs(first.addr), harness.V6Prefix)
	if !ok {
		t.Fatalf("M1 FAILED: no global IPv6 address on the fixture's prefix %s is on %s "+
			"inside the container, on a segment whose DHCPv6 server assigns addresses. "+
			"The plugin may still report one to the engine; this is the interface:\n%s",
			harness.V6Prefix, ifname, first.addr)
	}
	for _, bad := range []string{"tentative", "dadfailed", "deprecated"} {
		if strings.Contains(a0.flags, bad) {
			t.Errorf("M1 FAILED: the address %s is %s (%q) — it is on the interface but "+
				"not usable as a source address", a0.cidr, bad, a0.flags)
		}
	}

	// ---- M2: did it come from THIS server? ----
	//
	// Two independent server-side records, neither of them the
	// plugin's. The bare address without its prefix length is what
	// both the log and the lease database carry.
	bare := strings.SplitN(a0.cidr, "/", 2)[0]
	if n := f.CountLogLines("DHCPREPLY", bare); n < 1 {
		t.Errorf("M2 FAILED: the server never logged a DHCPv6 REPLY carrying %s, so the "+
			"address on the container's interface did not come from this segment's "+
			"DHCPv6 server (the fixture log is dumped below by dumpOnFailure)", bare)
	}
	leases, err := os.ReadFile(f.LeaseFile())
	if err != nil {
		t.Errorf("M2: could not read the server's lease database %s: %v", f.LeaseFile(), err)
	} else if !strings.Contains(string(leases), bare) {
		t.Errorf("M2 FAILED: the server's own lease database does not record %s as handed "+
			"out, though the address is on the container's interface:\n%s", bare, leases)
	}

	// ---- M3: does it survive, and WHICH thing refreshed? ----
	aN, ok := globalFromPrefix(parseV6Addrs(last.addr), harness.V6Prefix)
	if !ok {
		t.Errorf("M3 FAILED: the address %s was on the interface at start and is GONE at t+%s, "+
			"past the %s DHCPv6 lease. The container has silently lost the IPv6 address "+
			"its network assigned it:\n%s", a0.cidr, last.elapsed, harness.LeaseTime, last.addr)
	} else {
		// The distinction the SLAAC case turned on. A lease renewing
		// on the wire is NOT the interface's lifetimes being
		// refreshed; report which of the two was observed rather than
		// letting either stand in for the other.
		renewedOnWire := last.replies > first.replies
		switch {
		case a0.validLft == "forever" && aN.validLft == "forever":
			t.Logf("M3: the address carries NO kernel lifetime at either point "+
				"(valid_lft forever, flags %q) — libnetwork applied it statically from "+
				"res.Interface.AddressIPv6, so it cannot expire on the interface and "+
				"the question 'do the lifetimes refresh' does not arise. The lease was "+
				"renewed on the wire: %v (server REPLY lines %d -> %d). NOTE the "+
				"divergence this creates: the interface keeps the address whether or "+
				"not the lease is still held.",
				aN.flags, renewedOnWire, first.replies, last.replies)
		default:
			t.Logf("M3: kernel lifetimes at t+%s: valid_lft=%s preferred_lft=%s; "+
				"at t+%s: valid_lft=%s preferred_lft=%s. Lease renewed on the wire: %v "+
				"(server REPLY lines %d -> %d).",
				first.elapsed, a0.validLft, a0.prefLft,
				last.elapsed, aN.validLft, aN.prefLft,
				renewedOnWire, first.replies, last.replies)
			if renewedOnWire && aN.validLft != "forever" && aN.validLft == a0.validLft {
				t.Logf("M3: NOTE — identical lifetime strings at both points; if this " +
					"is a countdown rather than a refresh the sample spacing hid it.")
			}
		}
	}

	// ---- M4: is there an IPv6 default route, and does it persist? ----
	//
	// This is the concern the isolated measurement could only INFER.
	// The plugin's ONLY source of GatewayIPv6 is a default route
	// already present on the host parent/bridge, read at Join
	// (pkg/plugin/network.go, the AF_INET6 arm of the route scan);
	// DHCPv6 itself carries no gateway, which CreateEndpoint says out
	// loud ("No gateways in DHCPv6!"). This fixture's bridge has no
	// default route on it, so the plugin has nothing to return, and
	// the only other candidate is the RA-derived route the kernel
	// installs -- which is the one dhcpcd's accept_ra=0 removes.
	for _, s := range got {
		if !hasDefaultV6Route(s.route) {
			t.Errorf("M4 FAILED at %s (t+%s): the container has NO IPv6 default route. "+
				"On-link traffic still works, which is what hides this; nothing off-link "+
				"is reachable over IPv6:\n%s", s.label, s.elapsed, s.route)
		}
	}

	// ---- M5: off-link reachability ----
	//
	// On-link ping6 succeeds even with no default route, so it proves
	// nothing. This pings an address in a DIFFERENT /64 to force the
	// route to be consulted, and reads the ERROR CLASS rather than
	// reachability -- nothing answers at the far end either way, and
	// that is fine. "Network unreachable" is the kernel refusing for
	// want of a route; a timeout means a route existed and was used.
	// No change to the fixture's network is needed for this.
	const offLink = "fd00:6470:6865:ffff::1"
	ping := harness.ExecOutput(t, ctx, id, "ping", "-6", "-c", "1", "-W", "2", offLink)
	t.Logf("M5: ping -6 %s from the container:\n%s", offLink, ping)
	if strings.Contains(strings.ToLower(ping), "unreachable") {
		t.Errorf("M5 FAILED: sending to the off-link address %s failed with a "+
			"network-unreachable error, which is the kernel saying it has no route at "+
			"all — not the far end being silent. IPv6 is on-link only:\n%s", offLink, ping)
	}
}
