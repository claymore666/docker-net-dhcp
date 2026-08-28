// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"path"
)

// The Router-Advertisement guard: keep the container's KERNEL doing
// RFC 4861 §6.3.4 and RFC 4862 §5.5.3, and keep dhcpcd from turning it
// off again (#875).
//
// # WHY THE PLUGIN HAS TO DO ANYTHING AT ALL
//
// DHCPv6 carries no router. The option catalogue is RFC 8415 §21 and
// nothing in it has a next hop, so router discovery is RFC 4861 §6.3.4
// and advertisements are its only source. On-link determination does
// not come from the assigned address either: RFC 5942 §4 rule 1 says
// assigning an address -- "whether through IPv6 stateless address
// autoconfiguration, DHCPv6, or manual configuration" -- "MUST NOT
// implicitly cause a prefix derived from that address to be treated as
// on-link", and RFC 8415 §18.2.10.1 repeats it inside the DHCPv6
// specification. So advertisement processing is MANDATORY on the
// managed path too. "No gateways in DHCPv6" is the reason this matters,
// not a reason it can be skipped.
//
// # WHAT WAS TURNING IT OFF
//
// MEASURED, dhcpcd 10.3.2 on the pinned base image: dhcpcd writes
// net.ipv6.conf.<if>.accept_ra=0 and .autoconf=0 on the interface it
// manages, in if_setup_inet6() (if-linux.c). `--noconfigure` does not
// gate that write -- the plugin passes it on every client -- and under
// `--noconfigure` dhcpcd also skips ipv6nd_applyra(), ipv6_addaddrs()
// and rt_build(). So nobody in the container performed §6.3.4 or
// §5.5.3 at all: the address and route that existed at t+0 came from
// the single advertisement the kernel accepted before dhcpcd got there,
// and then nothing refreshed them.
//
// Only the `-6` client does this. MEASURED: a `-4` dhcpcd left
// accept_ra and autoconf at 1/1 on the same link.
//
// # WHY A ONE-SHOT WRITE AFTER THE CLIENT STARTS IS NOT ENOUGH
//
// MEASURED, and this is the part that would have shipped broken:
// dhcpcd re-runs if_setup_inet6() on EVERY CARRIER ACQUISITION, not
// only at startup. Re-asserting the sysctls once after Start returns
// holds until the first carrier event -- a switch reboot, a parent NIC
// reset, an operator bouncing the host-side veth -- and then IPv6 dies
// for the rest of the container's life, silently, which is the exact
// failure mode #875 is about.
//
// So the guard has two halves and needs both:
//
//   - WRITE the values, per-interface, before dhcpcd is exec'd. The
//     `all` node does not propagate to an existing interface (MEASURED:
//     conf/all/accept_ra=2 left conf/<if>/accept_ra at 0), so this is
//     per-interface or it is nothing.
//   - SHIELD them, by bind-mounting each sysctl file over itself
//     read-only inside the client's OWN mount namespace. dhcpcd's write
//     then fails with EROFS, which it reports and carries on from
//     (MEASURED: "if_setup_inet6: .../accept_ra: Read-only file
//     system", followed by a normal router solicitation, SOLICIT,
//     ADVERTISE and REPLY). The mount is invisible to the host, to the
//     container and to every other client, because unshare(1) -m makes
//     its propagation private -- the same property mountPrep already
//     relies on for the /proc/sys remount.
//
// The shield is what makes it durable, and it has no race with dhcpcd:
// both halves run before the exec, so there is no window in which
// dhcpcd's value is the live one.
//
// # WHY accept_ra=2 AND NOT 1
//
// Value 1 accepts advertisements only while forwarding is disabled.
// Containers that enable forwarding -- VPN, NAT, router and
// docker-in-docker images do it routinely -- would silently lose
// advertisement processing, with exactly the symptom above. Value 2
// overrules that. RFC 7084 §4.2 is the IETF precedent for a node that
// is a router on one interface and a host on another: W-1 requires such
// a router to "act as an IPv6 host" on its WAN side and W-3 requires it
// to "use Router Discovery ... to discover a default router(s) and
// install a default route(s)".
//
// # WHY autoconf=1, WHICH LOOKS WRONG FOR A MANAGED ENDPOINT
//
// Because the ROUTER decides, not us. RFC 4862 §5.5.3 gates address
// formation on the prefix option's A flag; `autoconf` is the host-side
// veto on top of it. Setting it to 0 overrides the router; setting it
// to 1 defers to the router, which is what a host is supposed to do.
//
// MEASURED against the shape the integration fixture calls "managed"
// (a dnsmasq DHCPv6 pool plus --enable-ra): with autoconf=1 the
// container formed NO autoconfigured address, because that server
// advertises the prefix with A=0. So this costs a managed endpoint
// nothing, and it is what lets a stateless or SLAAC segment -- where
// the address is SUPPOSED to come from the advertisement -- have an
// address at all.
//
// Where a segment really does advertise A=1 alongside stateful DHCPv6,
// the container ends up with both addresses. That is what any other
// host on that segment does, and it is the bound on this paragraph
// rather than a case that has been ruled out.
//
// # WHY keep_addr_on_down
//
// MEASURED with a control that runs no dhcpcd at all: a link down/up
// flushes every global IPv6 address on the interface (kernel default
// keep_addr_on_down=0) while the IPv4 address on the same link
// survives. libnetwork applies the DHCPv6 address once, at Join, and
// nothing re-applies it -- so one carrier flap costs the container its
// IPv6 address permanently, with the lease still valid on the server.
// The routes come back on the next advertisement once the two knobs
// above are in force; the statically applied address cannot, because
// nothing re-advertises it. Keeping it is the smallest thing that makes
// a flap survivable, and it is the same guard, so it rides here.
//
// # WHAT THE SHIELD CANNOT REACH: addr_gen_mode
//
// The shield is a MOUNT. It makes a write through /proc/sys fail, and
// it is blind to netlink. That is not a theoretical boundary, it is a
// measured one, and it decided a knob:
//
// MEASURED, single-variable control (identical netns, identical link,
// dhcpcd present or absent): addr_gen_mode goes 0 -> 1 when the `-6`
// client starts and stays 0 for the whole run when no client is
// started. 1 is IN6_ADDR_GEN_MODE_NONE, so after the next carrier flap
// the kernel does not regenerate the link-local address and the
// interface comes back with none -- MEASURED, link-local count 1 -> 0
// across a flap with dhcpcd running, 1 -> 1 without it. RFC 4291 §2.8
// requires an interface to have a link-local address.
//
// addr_gen_mode was in this table, as a shield-only knob, and it was
// taken out because IT DID NOT WORK. MEASURED, both routes driven by
// hand inside the client's own mount namespace with the shield in
// force: the /proc/sys write is refused ("Read-only file system") and
// `ip link set dev eth0 addrgenmode none` SUCCEEDS, after which the
// container reads 1. dhcpcd sets this one over netlink
// (IFLA_INET6_ADDR_GEN_MODE), where a bind mount has no say.
//
// A guard step that reports success while the value it guards changes
// anyway is worse than an absent one -- it puts a green step and a
// counter at zero over a knob nothing is holding. So it is gone, the
// residual is stated rather than papered over, and
// TestRAGuard_DoesNotClaimKnobsTheShieldCannotHold keeps it out.
//
// The residual, precisely: after a carrier flap a guarded container has
// no link-local address. MEASURED, that does not break what #875 is
// about -- the global address is kept, advertisements are still
// received, and the default route is restored within seconds -- but it
// is a conformance gap and closing it needs a netlink re-assertion
// driven by carrier events, which is a different mechanism with a race
// this design does not have.
//
// # THE BOUND ON ALL OF THIS
//
// The guard changes what the container's kernel is allowed to do about
// advertisements. It does not make anything re-apply an address that
// the plugin itself applied statically -- keep_addr_on_down is what
// covers that, and only across a carrier flap, not across a plugin
// restart. And it cannot help a segment with no advertising router at
// all: RFC 4861 §6.3.4 has no source of a default route other than an
// advertisement, so a DHCPv6-only segment with RAs disabled has no
// default route to discover and this guard will not invent one.
const (
	// sysctlIPv6ConfDir is the per-interface IPv6 configuration tree.
	// /proc/sys/net is per-NETWORK-NAMESPACE: the same path names a
	// different switch depending on the reader's netns, which is why
	// this is only ever used from inside the client's own netns (see
	// raGuardRequired).
	sysctlIPv6ConfDir = "/proc/sys/net/ipv6/conf"

	// raAcceptValue overrules forwarding: accept advertisements whether
	// or not the container routes. See the block comment.
	raAcceptValue = "2"
	// raAutoconfValue defers address formation to the advertisement's A
	// flag rather than vetoing it host-side.
	raAutoconfValue = "1"
	// raKeepAddrValue keeps configured addresses across a carrier loss.
	raKeepAddrValue = "1"
)

// raGuardKnob is one sysctl the guard writes and then shields.
//
// A table rather than three hand-written triples so that the write, the
// read-back and the shield for a knob cannot drift apart, and so a
// fourth knob cannot be added with only two of its three steps -- the
// enumeration-beside-the-code failure this repo has paid for more than
// once. raGuardSteps derives every step from this table.
type raGuardKnob struct {
	// name is the sysctl leaf under /proc/sys/net/ipv6/conf/<iface>/.
	name string
	// value is what the guard writes and then verifies. Every knob in
	// the table has one: a knob the guard cannot name a value for is a
	// knob it has no business claiming to hold.
	value string
}

// raGuardKnobs is the complete set, in write order.
func raGuardKnobs() []raGuardKnob {
	return []raGuardKnob{
		{name: "accept_ra", value: raAcceptValue},
		{name: "autoconf", value: raAutoconfValue},
		{name: "keep_addr_on_down", value: raKeepAddrValue},
	}
}

// raGuardPath is one knob's sysctl path for iface.
//
// path.Join, and iface has already been through ValidIfaceName
// (NewDHCPClient refuses anything else), whose character class --
// alphanumerics, dot, dash, underscore -- contains no shell
// metacharacter and no path separator. That is what makes it safe to
// interpolate into the shell mountPrep builds.
func raGuardPath(iface, knob string) string {
	return path.Join(sysctlIPv6ConfDir, iface, knob)
}

// raGuardSteps renders the guard as shell, one prepared step per line
// of work, each carrying its own failure marker.
//
// The order within a knob is load-bearing and is the reason this is
// generated rather than written out: write, then VERIFY BY READING IT
// BACK, then shield. Shielding before the write would make the write
// fail; verifying after the shield would verify the shield's own view.
//
// The read-back is not ceremony. A write to /proc/sys reports success
// for a value the kernel then clamps or ignores, and the whole point of
// this guard is that the value is still there later -- so "we wrote it"
// is precisely the claim that must not be trusted. grep -qxF: fixed
// string, whole line, no pattern.
//
// EVERY step goes through prepStep, so every step has a marker and a
// counter; TestRAGuard_EveryStepReportsItsFailure asserts that by
// counting markers against commands rather than by naming the steps
// that exist today.
func raGuardSteps(iface string) string {
	out := ""
	for _, k := range raGuardKnobs() {
		p := raGuardPath(iface, k.name)
		out += prepStep(fmt.Sprintf("%s %s > %s", echoBin, k.value, p),
			raGuardFailMarker, k.name+"-write")
		out += prepStep(fmt.Sprintf("%s -qxF %s %s", grepBin, k.value, p),
			raGuardFailMarker, k.name+"-verify")
		// bind-then-remount: `-o bind` makes the file its own mount,
		// and only then can `remount,bind,ro` make that mount
		// read-only. One step, because a half-applied shield is not a
		// state worth reporting separately -- either dhcpcd's write
		// fails or it does not.
		out += prepStep(fmt.Sprintf("%s -o bind %s %s && %s -o remount,bind,ro %s",
			mountBin, p, p, mountBin, p),
			raGuardFailMarker, k.name+"-shield")
	}
	return out
}
