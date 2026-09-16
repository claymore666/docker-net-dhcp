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
// # WHY accept_ra=0 (#821), WHICH IS THE OPPOSITE OF WHAT 2.0 SHIPPED
//
// Because the plugin now supplies everything the kernel was reading out
// of the advertisement, and two writers on one interface is the defect,
// not the belt.
//
// Until #821 the kernel owned the v6 default route, the on-link prefix
// route and the link MTU, all learned from the advertisement, while the
// plugin owned the address. That split has no owner for the question an
// operator actually asks -- `docker inspect` showed no IPv6 gateway at
// all, because libnetwork only knows what the driver returns at Join and
// the driver had nothing to return. The library reads the same
// advertisements on an AF_PACKET socket and hands the router, the MTU,
// the advertised resolvers, the search list and RFC 4191's more-specific
// routes over as an ordinary lease, so the plugin can answer Join with a
// gateway and apply the rest itself.
//
// With one side able to answer, leaving the kernel switched on is not
// redundancy. It is a second writer installing its own `proto ra`
// default route beside Docker's, expiring on the router's schedule
// rather than on the plugin's, and a container whose routing table
// changes under it for reasons nothing in this plugin can report.
//
// accept_ra=0 IS NOT A RETREAT TO 1.9.0's SHAPE. 1.9.0 wrote the same
// value and had nothing that supplied a route, which is #875: the
// container ended up with an address and no way off the link. The value
// is the same and the reason is its opposite -- the route comes from the
// Join answer now, and the RFC 4861 section 6.3.4 processing that
// produces it happens in the library rather than in the kernel.
//
// WHAT THE VALUE DOES NOT DO: it does not purge. MEASURED (see
// pkg/plugin/v6_link.go): a route the kernel installed from an
// advertisement before this write survives it and expires on the
// router's lifetime. The link is live in the sandbox at the kernel
// default before this guard can reach it, so purging what it left is
// part of the guard's job and lives at the one caller, which has the
// netlink handle this package does not.
//
// # WHY autoconf=0
//
// One writer, read for addresses rather than for routes. The plugin
// installs the address it holds a lease for; a kernel forming a second
// one from the same advertisement gives the container an address
// nothing in this plugin knows about, that `docker inspect` cannot
// show and that no record can release.
//
// It is also inert at accept_ra=0, which is the point worth stating
// rather than leaving to be rediscovered: `autoconf` is the host-side
// veto on a prefix the kernel has already decided to process, and at
// accept_ra=0 it processes none. MEASURED on this box, kernel
// 6.12.107+deb13-amd64, in an unprivileged user namespace: with
// accept_ra=0 and autoconf=0 the link keeps its link-local, forms no
// global address, installs no default route and no on-link route, and
// keeps its own MTU, through twelve seconds of advertisements. Writing
// it is what makes the intent a value a reader can check instead of a
// consequence of another knob.
//
// The 1.9.0 reasoning this replaces said the opposite -- defer to the
// router's A flag, because a host that vetoed autoconf would be
// deciding on the router's behalf that its segment is stateful-only.
// That argument was right for a plugin that formed no addresses of its
// own. Forming them from the advertisement is #818, in the library,
// under the plugin's ownership; until it lands a SLAAC-only segment
// gets no global address from this plugin, which is what
// dhcpv6_not_offered has always counted.
//
// # WHY keep_addr_on_down
//
// MEASURED at 1.9.0 with a control that runs no client at all: a link
// down/up flushes every global IPv6 address on the interface (kernel
// default keep_addr_on_down=0) while the IPv4 address on the same link
// survives. The DHCPv6 address is applied once and nothing re-applies
// it, so one carrier flap costs the container its IPv6 address
// permanently with the lease still valid on the server.
//
// Its bound is stated rather than implied (design note finding (a),
// 2026-09-11): keep_addr_on_down=1 keeps a PERMANENT address, and the
// plugin installs its DHCPv6 address with finite lifetimes, so the
// address is gone after a flap until the library's next renewal
// re-installs it. The knob costs nothing and is not the fix.
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
// The guard decides who configures the container's link from an
// advertisement. It does not make anything re-apply an address; it
// cannot help a segment with no advertising router at all (RFC 4861
// section 6.3.4 has no other source of a default route, and with
// accept_ra=0 neither has the kernel); and a container whose routes now
// come from the Join answer depends on the plugin's own client for
// every later change, which is what the ipv6_router_withdrawn counter
// and the live route path exist to make visible.
const (
	// sysctlIPv6ConfDir is the per-interface IPv6 configuration tree.
	// /proc/sys/net is per-NETWORK-NAMESPACE: the same path names a
	// different switch depending on the reader's netns, which is why
	// this is only ever used from inside the client's own netns.
	sysctlIPv6ConfDir = "/proc/sys/net/ipv6/conf"

	// raAcceptValue leaves advertisement processing to the plugin's own
	// client. See the block comment.
	raAcceptValue = "0"
	// raAutoconfValue keeps the kernel from forming an address beside
	// the one the plugin holds a lease for.
	raAutoconfValue = "0"
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
// healthy: the kernel keeps processing advertisements beside the
// plugin, so the container carries a second default route that expires
// on the router's schedule and an address nothing here can report,
// minutes or hours before anybody notices.
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
