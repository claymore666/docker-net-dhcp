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
// §5.5.3 at all.
//
// The loss is ACTIVE, not merely a failure to refresh. MEASURED, in the
// ordering that is least favourable to this claim -- an advertisement
// accepted and a `proto ra` default route installed BEFORE dhcpcd is
// started at all:
//
//	unguarded: route present before dhcpcd, GONE after   (accept_ra 0)
//	guarded:   route present before dhcpcd, still present (accept_ra 2)
//
// So the reported symptom is not "the route eventually ages out". The
// route the container already had disappears once the `-6` client
// starts, and nothing can replace it, because the same write also
// stopped the kernel accepting the advertisement that would.
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
// This is a KERNEL-BEHAVIOUR argument, not a standards one, and it is
// stated that way on purpose: no RFC assigns meanings to the values of
// a Linux sysctl.
//
// Linux ties advertisement processing to forwarding. At accept_ra=1 the
// kernel refuses advertisements once forwarding is enabled on the
// interface, and rt6_purge_dflt_routers() removes the default routes it
// had already learned from them. accept_ra=2 is the only value that
// overrules the forwarding check. Containers enable forwarding
// routinely -- VPN, NAT, router and docker-in-docker images all do --
// and at accept_ra=1 doing so silently reproduces the #875 symptom the
// guard exists to prevent.
//
// MEASURED, precondition-gated so the treatment is only applied to a
// container that had actually received an advertisement first, three
// trials per arm:
//
//	accept_ra=1: default route purged 3/3 when forwarding was enabled
//	accept_ra=2: default route survived 3/3
//
// An earlier draft of this paragraph cited RFC 7084 §4.2 (W-1, W-3) as
// precedent. That citation was WRONG and is recorded here so it is not
// reinstated: RFC 7084 governs IPv6 CE routers, which is not what a
// container is, and W-2 in that same requirement list expects the
// link-local address that this guard's own residual (below) admits to
// losing. The value is right; the reason it is right is the paragraph
// above, which is measurable rather than analogical.
//
// # WHY autoconf=1, WHICH LOOKS WRONG FOR A MANAGED ENDPOINT
//
// Because the ROUTER decides, not us. RFC 4862 §5.5.3(a) gates address
// formation on the prefix option's A flag -- a host processes the
// prefix for autoconfiguration only if that flag is set; `autoconf` is
// the host-side veto on top of it. Setting it to 0 overrides the
// router; setting it to 1 defers to the router, which is what a host is
// supposed to do.
//
// Deferring is also what keeps the two mechanisms from being read as
// alternatives. RFC 4861 §6.3.4 has a host apply the advertisement's
// contents as a UNION with whatever else configured it -- stateful
// DHCPv6 does not suppress advertisement-derived configuration and
// advertisement-derived configuration does not suppress DHCPv6. A
// managed endpoint that vetoed autoconf would be deciding, on the
// router's behalf, that its segment is stateful-only.
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
// (IFLA_INET6_ADDR_GEN_MODE), which a read-only /proc/sys does not
// reach at all.
//
// A guard step that reports success while the value it guards changes
// anyway is worse than an absent one -- it puts a green step and a
// counter at zero over a knob nothing is holding. So it is gone, the
// residual is stated rather than papered over, and
// TestRAGuard_DoesNotClaimKnobsTheShieldCannotHold keeps it out.
//
// The residual, precisely: after a carrier flap a guarded container has
// no link-local address, and the consequence is sharper than "a
// conformance gap".
//
// MEASURED, single-variable control across a flap, addr_gen_mode being
// the only thing that differs:
//
//	addr_gen_mode=eui64: link-local re-formed, and the container sent a
//	                     Router Solicitation (ICMPv6 type 133, observed
//	                     on the router side with tcpdump) -- 1 seen
//	addr_gen_mode=none:  no link-local, and NO Router Solicitation -- 0
//	                     seen
//
// RFC 4861 §6.3.7 lets a host solicit from the unspecified address, but
// Linux drives solicitation off link-local DAD completion, so with no
// link-local there is nothing to trigger it. So the container cannot
// ASK for an advertisement after a flap; it can only wait for the next
// unsolicited one.
//
// That is why the recovery time is a property of the SEGMENT, not of
// this design. It is bounded by the router's MaxRtrAdvInterval, whose
// RFC 4861 §6.2.1 default is 600s and whose permitted maximum is 1800s.
// The integration fixture recovers in seconds only because its dnsmasq
// advertises far more often than that; a real segment on the defaults
// can leave a flapped container without a default route for ten
// minutes. Do not restate that as "restored within seconds" -- an
// earlier draft of this comment did, and it was reading the fixture's
// cadence as if it were the design's guarantee.
//
// The global address itself is kept across the flap (keep_addr_on_down,
// above), and advertisements are still processed when they arrive.
// Closing the residual needs a netlink re-assertion driven by carrier
// events, which is a different mechanism with a race this design does
// not have.
//
// # WHEN THE SHIELD IS REFUSED
//
// Every step is non-fatal by construction: the client starts either
// way, because a container with a working lease and stale advertisement
// settings is better than a container with no address at all. What
// matters is that a refusal is LOUD.
//
// The table below is the evidence that CLOSED route A, kept because it
// is what the current shape is a response to. Each arm ran against a
// live interface with the precondition asserted (accept_ra=1) first:
//
//	leaf bind honoured   accept_ra=2   0 markers   dhcpcd saw EROFS
//	leaf bind denied     accept_ra=0   3 markers   (the three -shield steps)
//	/proc/sys absent     accept_ra=0   9 markers   (write+verify+shield x3)
//	guard not asked      accept_ra=0   0 markers   (control)
//
// Row 2 is why a write-only design would have shipped broken: the write
// succeeds, the shield does not, and accept_ra reads 0 afterwards
// because dhcpcd overwrote it. The shield is not belt-and-braces on top
// of the write; it is the half that works.
//
// And row 2 is not hypothetical. MEASURED on the CI runner, with the
// per-leaf bind in force and the assertion correctly anchored: write 3
// of 3 succeeded, verify 3 of 3 succeeded, and the shield was refused 3
// of 3, after which the container read accept_ra=0 and autoconf=0 --
// dhcpcd's values -- while keep_addr_on_down held at 1 because dhcpcd
// never writes that knob. A mechanism that reports success where it
// works and is silently absent where it does not is the failure this
// package exists to avoid, which is what sent route A to the
// enumeration in raGuardSteps.
//
// For the mechanism now in force, MEASURED against a control in the
// shipped image:
//
//	/proc/sys returned ro   accept_ra=2  autoconf=1   dhcpcd exit 0, no abort
//	/proc/sys left rw       accept_ra=0  autoconf=0   (control: the defect)
//
// KNOWINGLY OPEN, and the honest boundary of this comment: a host that
// refuses the read-only remount itself has not been observed. It would
// emit one procsys-ro marker rather than three, because one operation
// covers all the knobs -- read the marker as "none of the knobs are
// held", never as "one of them is not". The prologue's own
// read-WRITE remount of the same mount succeeds on every host the
// suite has run on, including the one that refuses the leaf bind, which
// is the reason to expect the reverse to be available too; expecting is
// not measuring, and the integration lane is where it gets decided.
//
// Also not measured, and open: a rootfs mounted read-only, and a
// runtime that denies CAP_SYS_ADMIN. The second cannot reach these
// steps at all -- unshare(1) -m fails first and the prologue never
// runs -- so it is a mountPrep concern rather than a guard one.
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
//
// The shield's own escape, stated because a read-only /proc/sys reads
// like a stronger promise than it is: it constrains DHCPCD, because
// dhcpcd is what runs inside that mount namespace. It constrains
// nothing else. Any other writer in the container's network namespace
// -- a process in the container running as root, an `nsenter` from the
// host, a `docker exec` -- sees the container's ordinary /proc/sys and
// can set accept_ra back to 0. The guard neither prevents that nor
// counts it, and the health counter will still read zero afterwards,
// because the counter observes the guard's own steps and not the
// current value of the knob. The integration assertion in
// test/integration/ipv6_test.go is what reads the live value.
//
// Two writers reach these paths in this codebase, and they compose.
// The read-only remount is confined to the client: MEASURED for THIS
// mechanism rather than inherited from route A's -- with the parent's
// /proc/sys writable, a child that remounts it read-only sees its own
// write blocked while the parent's write still succeeds, both before
// and after the child exits. pkg/plugin's v6_link.go writes
// disable_ipv6 from the plugin process, outside that namespace, so it
// is unaffected: the guard cannot break that path and that path cannot
// break the guard. That containment is not incidental -- it is the same
// unshare(1) -m boundary the prologue has always relied on for its
// read-WRITE remount of this very mount, which has never leaked to the
// host or to another client.
//
// Worth noting where the two agree: v6_link.go records that in a
// --privileged runtime the /proc/sys remount fails with "can't find
// /proc/sys in /proc/mounts" because /proc/sys is not a separate mount
// there. That is the same phenomenon, from the other side, that closed
// route A -- busybox's mount resolves a remount through /proc/mounts,
// so a target with no entry there cannot be remounted whatever the
// kernel would allow.
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

	// raGuardShieldStep names the single step that returns /proc/sys to
	// read-only. One name for one operation: the marker a host emits
	// when the shield is refused must not be mistakable for a per-knob
	// failure, because the consequence differs -- a refused shield
	// leaves ALL the knobs writable, not one.
	raGuardShieldStep = "procsys-ro"
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
// raGuardSteps emits, per knob, a write and a read-back verify; then a
// SINGLE step that returns /proc/sys to read-only for the rest of
// dhcpcd's life.
//
// THE ROUTES TO DURABILITY, AND WHY THIS ONE (#875)
//
// Setting the knobs once is not enough: dhcpcd re-runs if_setup_inet6()
// on every carrier acquisition, so whatever we write it writes back.
// Durability is the whole guarantee, not a refinement of it. These are
// the routes considered, each marked closed or knowingly open.
//
//	A. Read-only bind mount over each sysctl LEAF (what this used to
//	   do). CLOSED as unreliable: MEASURED on the CI runner, `mount -o
//	   bind P P` returns 0 but leaves no /proc/mounts entry, so the
//	   following `remount,bind,ro` reports "can't find P in
//	   /proc/mounts" and the leaf stays writable. It works on other
//	   hosts, which is exactly what made it dangerous -- it reported
//	   success where it worked and its absence was invisible where it
//	   did not.
//	B. Stop the WRITER: dhcpcd's `noipv6rs`. MEASURED to work on the
//	   sysctls -- accept_ra stays 2 and autoconf stays 1, while an
//	   unrelated directive (`slaac private`) does not have that effect,
//	   so the effect is specific rather than "any config". CLOSED
//	   anyway, on protocol risk: noipv6rs also stops dhcpcd processing
//	   Router Advertisements, and MEASURED it emits no Router
//	   Solicitation at all. dhcpcd learns to start DHCPv6 from the RA's
//	   M flag, so suppressing RA handling risks never starting the
//	   DHCPv6 exchange this plugin exists to run. Trading a lease for a
//	   sysctl is not a trade worth making.
//	C. Re-assert in a loop after each of dhcpcd's writes. CLOSED: racy
//	   by construction (the container is exposed between the write and
//	   the re-assert), and it needs a process per endpoint.
//	D. Set the knobs over netlink instead of /proc. CLOSED: addresses
//	   the write, not the DURABILITY -- dhcpcd overwrites either way.
//	E. Overmount a leaf with a DIFFERENT file (tmpfs). CLOSED, and it
//	   is a correctness hazard rather than merely ineffective: the
//	   container would then read our file instead of the kernel's
//	   sysctl, so the guard would look like it held while the kernel
//	   value underneath was whatever dhcpcd last wrote.
//	F. seccomp/LSM to deny the write. KNOWINGLY OPEN, out of scope: it
//	   needs machinery this plugin does not have, for a case the route
//	   below already covers.
//	G. Drop CAP_NET_ADMIN from dhcpcd so its sysctl writes fail. OPEN,
//	   not taken: plausible, but it changes what dhcpcd may do for
//	   every other purpose, and the failure mode if it needs the
//	   capability elsewhere is a broken lease rather than a loud error.
//	H. Return /proc/sys to READ-ONLY in the client's private mount
//	   namespace, after our writes and before dhcpcd starts. TAKEN.
//
// H is chosen because it is the one route that uses an operation
// already MEASURED to work in the environment where A fails: the
// prologue's own `remount,bind,rw /proc/sys` succeeds on that runner
// (zero procsys-remount markers). It is also the conceptually correct
// shape -- /proc/sys is read-only in the managed-plugin rootfs, and it
// is OUR prologue that widens it so dhcpcd's v4 setup can write
// promote_secondaries (#247). Leaving it open for the whole of dhcpcd's
// life is what let dhcpcd write accept_ra back to 0. This closes the
// door we opened, rather than adding a new lock beside it.
//
// MEASURED against a control, in the shipped image: with /proc/sys left
// read-write dhcpcd takes accept_ra 2 -> 0 and autoconf 1 -> 0; with it
// returned to read-only both hold at 2 and 1, dhcpcd exits 0, prints no
// abort signature, and its protocol behaviour is unchanged (it still
// solicits, unlike under route B).
//
// Confirmed on the CI runner by an absence drive with BOTH arms, which
// is the only form that says anything: the pre-fix control (this same
// tree with HonorRouterAdverts forced false) read accept_ra=0,
// autoconf=0, keep_addr_on_down=0 on BOTH the macvlan and bridge
// endpoints -- dhcpcd's if_setup_inet6 signature -- and failed exactly
// the two v6 golden-path tests and nothing else. The fixed tree read
// the guarded 2/1/1 on both. Route H therefore holds where route A was
// refused, on the host where A was refused.
//
// One bound worth carrying: the DEFAULT-ROUTE half of that observer did
// NOT discriminate -- it passed on the unfixed tree too, because a
// route already installed outlives the test even once accept_ra is 0.
// The knobs are the discriminating assertion. A future change that
// keeps only the route check is back to an observer that cannot see the
// defect.
//
// Scope, and it is load-bearing: this runs ONLY for the persistent v6
// client (pkg/dhcp refuses the combination otherwise). The v4 client
// keeps a writable /proc/sys, because its promote_secondaries write is
// FATAL if it fails -- that is #247, and re-breaking it would trade one
// user-visible bug for another.
//
// Order within a knob is still write, then read back, then shield: a
// shield applied before the write would make the write fail, and a
// verify after the shield would only prove the shield's own view.
func raGuardSteps(iface string) string {
	out := ""
	for _, k := range raGuardKnobs() {
		p := raGuardPath(iface, k.name)
		out += prepStep(fmt.Sprintf("%s %s > %s", echoBin, k.value, p),
			raGuardFailMarker, k.name+"-write")
		out += prepStep(fmt.Sprintf("%s -qxF %s %s", grepBin, k.value, p),
			raGuardFailMarker, k.name+"-verify")
	}
	// One shield for all three knobs, last. Not per-knob: the thing
	// being made read-only is the /proc/sys mount itself, so repeating
	// it per knob would be the same operation three times and would
	// report three failures for one cause.
	out += prepStep(fmt.Sprintf("%s -o remount,bind,ro %s", mountBin, procSysPath),
		raGuardFailMarker, raGuardShieldStep)
	return out
}
