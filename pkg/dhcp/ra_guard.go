// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

// The Router-Advertisement guard: keep the container's KERNEL doing RFC
// 4861 section 6.3.4 and RFC 4862 section 5.5.3 (#875).
//
// # WHY THE PLUGIN HAS TO DO ANYTHING AT ALL
//
// DHCPv6 carries no router. The option catalogue is RFC 9915 section 21
// and nothing in it has a next hop, so router discovery is RFC 4861
// section 6.3.4 and advertisements are its only source. On-link
// determination does not come from the assigned address either: RFC
// 5942 section 4 rule 1 says assigning an address -- "whether through
// IPv6 stateless address autoconfiguration, DHCPv6, or manual
// configuration" -- "MUST NOT implicitly cause a prefix derived from
// that address to be treated as on-link", and RFC 9915 section
// 18.2.10.1 repeats it inside the DHCPv6 specification. So
// advertisement processing is MANDATORY on the managed path too. "No
// gateways in DHCPv6" is the reason this matters, not a reason it can
// be skipped.
//
// # WHAT CHANGED IN 2.0, AND WHAT DID NOT
//
// 1.9.0's guard had two halves: WRITE the three sysctls, and SHIELD
// them by remounting /proc/sys read-only inside dhcpcd's own mount
// namespace so dhcpcd's if_setup_inet6() write was refused. The shield
// existed because dhcpcd wrote accept_ra=0 and autoconf=0 on every
// carrier acquisition, and it is gone from this build with dhcpcd: this
// plugin execs nothing, and no process inside the container's namespace
// writes these knobs unless somebody in the container does.
//
// So D30 Q3: the writes stay, the shield does not come back, and there
// is no option to bring it back. The knobs are reachable over netlink
// as well as through /proc/sys (MEASURED at 1.9.0 for addr_gen_mode,
// which is why that knob was already out of the table), so a mount-based
// shield promises a protection it cannot deliver while keeping the mount
// privilege in the plugin's manifest for every user. The bound is stated
// on the reference row instead: a privileged process inside the
// container can set these back, and this guard neither prevents that nor
// counts it.
//
// # WHY accept_ra=2 AND NOT 1
//
// A KERNEL-BEHAVIOUR argument, not a standards one: no RFC assigns
// meanings to the values of a Linux sysctl.
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
// MEASURED at 1.9.0, precondition-gated so the treatment was only
// applied to a container that had actually received an advertisement
// first, three trials per arm:
//
//	accept_ra=1: default route purged 3/3 when forwarding was enabled
//	accept_ra=2: default route survived 3/3
//
// # WHY autoconf=1, WHICH LOOKS WRONG FOR A MANAGED ENDPOINT
//
// Because the ROUTER decides, not us. RFC 4862 section 5.5.3(a) gates
// address formation on the prefix option's A flag -- a host processes
// the prefix for autoconfiguration only if that flag is set; `autoconf`
// is the host-side veto on top of it. Setting it to 0 overrides the
// router; setting it to 1 defers to the router, which is what a host is
// supposed to do.
//
// Deferring is also what keeps the two mechanisms from being read as
// alternatives. RFC 4861 section 6.3.4 has a host apply the
// advertisement's contents as a UNION with whatever else configured it.
// A managed endpoint that vetoed autoconf would be deciding, on the
// router's behalf, that its segment is stateful-only.
//
// MEASURED at 1.9.0 against the shape the integration fixture calls
// "managed" (a dnsmasq DHCPv6 pool plus --enable-ra): with autoconf=1
// the container formed NO autoconfigured address, because that server
// advertises the prefix with A=0. So this costs a managed endpoint
// nothing, and it is what lets a stateless or SLAAC segment -- where
// the address is SUPPOSED to come from the advertisement -- have an
// address at all. Where a segment really does advertise A=1 alongside
// stateful DHCPv6 the container ends up with both addresses; that is
// what any other host on that segment does, and it is the bound on this
// paragraph rather than a case that has been ruled out.
//
// # WHY keep_addr_on_down
//
// MEASURED at 1.9.0 with a control that runs no client at all: a link
// down/up flushes every global IPv6 address on the interface (kernel
// default keep_addr_on_down=0) while the IPv4 address on the same link
// survives. The DHCPv6 address is applied once and nothing re-applies
// it, so one carrier flap costs the container its IPv6 address
// permanently with the lease still valid on the server. The routes come
// back on the next advertisement once the two knobs above are in force;
// the applied address cannot, because nothing re-advertises it.
//
// # THE RESIDUAL: addr_gen_mode
//
// Not in the table, and it was not in 1.9.0's either. The knob is set
// over netlink (IFLA_INET6_ADDR_GEN_MODE), so /proc/sys does not reach
// it. With dhcpcd gone nothing in this build sets it, which removes the
// 1.9.0 symptom rather than guarding against it: MEASURED at 1.9.0,
// addr_gen_mode went 0 -> 1 when dhcpcd's -6 client started and stayed
// 0 for the whole run when no client was started.
//
// # THE BOUND ON ALL OF THIS
//
// The guard changes what the container's kernel is allowed to do about
// advertisements. It does not make anything re-apply an address; it
// cannot help a segment with no advertising router at all (RFC 4861
// section 6.3.4 has no other source of a default route); and after a
// carrier flap the recovery time is a property of the SEGMENT, bounded
// by the router's MaxRtrAdvInterval, whose RFC 4861 section 6.2.1
// default is 600 s and whose permitted maximum is 1800 s. The
// integration fixture recovers in seconds only because its dnsmasq
// advertises far more often than that.
const (
	// sysctlIPv6ConfDir is the per-interface IPv6 configuration tree.
	// /proc/sys/net is per-NETWORK-NAMESPACE: the same path names a
	// different switch depending on the reader's netns, which is why
	// this is only ever used from inside the client's own netns.
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

// raGuardKnob is one sysctl the guard writes and reads back.
//
// A table rather than three hand-written pairs so that the write and
// the read-back for a knob cannot drift apart, and so a fourth knob
// cannot be added with only one of its two steps -- the
// enumeration-beside-the-code failure this repository has paid for more
// than once. raGuardSteps derives every step from this table.
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

// RouterAdvertGuardContract is the guard's sysctl contract as data:
// knob name to the value the guard holds it at.
//
// Exported for ONE reason (#875): the integration suite asserts the
// same contract from inside the container, and it held a second,
// hand-written copy of this table. Value drift between the two went
// red, which is why nobody noticed the half that did not -- a knob
// ADDED here was silently unobserved, because the assertion iterated
// the copy. The observer now iterates this.
//
// A fresh map per call: a package-level map would be reachable and
// mutable from any importer, and an observer whose expectations can be
// rewritten by the thing it observes is not an observer.
func RouterAdvertGuardContract() map[string]string {
	knobs := raGuardKnobs()
	out := make(map[string]string, len(knobs))
	for _, k := range knobs {
		out[k.name] = k.value
	}
	return out
}

// raGuardPath is one knob's sysctl path for iface.
func raGuardPath(dir, iface, knob string) string {
	return path.Join(dir, iface, knob)
}

// RouterAdvertGuardResult is what one application of the guard did.
//
// Failures is a count of STEPS, not of knobs and not of endpoints: each
// knob contributes a write and a read-back, so three knobs are at most
// six. It is the number router_advert_guard_failures moves by.
type RouterAdvertGuardResult struct {
	Failures int
	// Err joins every step that failed, or nil.
	Err error
}

// ApplyRouterAdvertGuard writes the three knobs on iface and reads each
// one back, in whatever network and mount namespaces the caller is in.
//
// THE CALLER SUPPLIES BOTH NAMESPACES, and they are needed for different
// reasons: the NETWORK namespace decides which link the path names,
// because /proc/sys/net is per-netns; the MOUNT namespace decides
// whether the tree can be written at all, because /proc/sys is mounted
// read-only in the managed-plugin rootfs. The plugin enters both once,
// for the disable_ipv6 clear this guard runs beside — see
// pkg/plugin/v6_link.go, which is the only production caller.
//
// dir is /proc/sys/net/ipv6/conf in production and a temporary
// directory in a test. It is a parameter and not a constant because the
// read-back is the half most worth driving, and driving it against the
// real tree would mean writing to the host's sysctls.
//
// THE READ-BACK IS NOT CEREMONY. A write to /proc/sys reports success
// for a value the kernel then clamps or ignores, and the whole point of
// this guard is that the value is there afterwards -- so "we wrote it"
// is precisely the claim that must not be trusted.
//
// IT IS NEVER FATAL, and the caller is what makes that true: a
// container with a working lease and stale advertisement settings is
// better than a container with no address at all, so every failure is
// counted and the endpoint proceeds. What matters is that a failure is
// LOUD, because a container whose guard did not take looks completely
// healthy: it keeps the address and route the kernel accepted in the
// first seconds and loses everything through the router at the end of
// that advertisement's router lifetime, minutes or hours later, with no
// error anywhere.
func ApplyRouterAdvertGuard(dir, iface string) RouterAdvertGuardResult {
	var (
		res  RouterAdvertGuardResult
		errs []error
	)
	for _, k := range raGuardKnobs() {
		p := raGuardPath(dir, iface, k.name)
		if err := os.WriteFile(p, []byte(k.value+"\n"), 0o644); err != nil {
			res.Failures++
			errs = append(errs, fmt.Errorf("write %v=%v: %w", p, k.value, err))
			// The read-back is still taken. A write that failed
			// against a knob already holding the right value is not a
			// knob left wrong, and reporting it as two failures would
			// double-count one cause.
		}
		got, err := os.ReadFile(p)
		if err != nil {
			res.Failures++
			errs = append(errs, fmt.Errorf("read back %v: %w", p, err))
			continue
		}
		if strings.TrimSpace(string(got)) != k.value {
			res.Failures++
			errs = append(errs, fmt.Errorf("%v reads %q after a write of %q",
				p, strings.TrimSpace(string(got)), k.value))
		}
	}
	res.Err = errors.Join(errs...)
	return res
}
