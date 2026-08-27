// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Getenv reads one environment variable. The handler binary supplies
// os.Getenv at runtime; tests inject a closure over a fixed map so
// they can exercise every branch of BuildEvent without setenv churn.
type Getenv func(string) string

// mapReason translates a dhcpcd hook `$reason` into the small set of
// event types the persistent-client goroutine acts on, plus the
// address family the lease vars should be read from.
//
// dhcpcd fires the hook for far more reasons than we care about
// (PREINIT, CARRIER, ROUTERADVERT, STOP, DELEGATED6, …); only the
// lease-bearing and lease-loss transitions map to an emitted event.
// Everything else returns emit=false and is silently dropped — the
// same "best-effort: never block the DHCP exchange on our own
// bookkeeping" contract the busybox handler had.
//
//   - BOUND / REBOOT      -> "bound"  (v4 lease acquired / confirmed)
//   - RENEW / REBIND      -> "renew"  (v4 lease extended / re-bound)
//   - BOUND6 / REBOOT6    -> "bound"  (v6 IA_NA acquired / confirmed)
//   - RENEW6 / REBIND6    -> "renew"  (v6 IA_NA extended / re-bound)
//   - NAK                 -> "nak"    (server refused; treat as loss)
//   - EXPIRE / TIMEOUT    -> "leasefail" (v4 lease lapsed / no lease)
//   - EXPIRE6 / TIMEOUT6  -> "leasefail" (v6 lease lapsed / no lease)
//
// REBIND(6) maps to "renew" rather than "bound" because the consumer's
// renew path already re-applies a possibly-changed address; a rebind is
// exactly that case. dhcpcd's man page says NAK "should be treated as
// EXPIRE" — we keep them distinct only so the naks_received counter can
// separate a server refusal from a quiet timeout (#128 / #212).
//
// RELEASE is deliberately NOT mapped, and that is load-bearing (#353).
// Under `--noconfigure` — which this plugin always runs — dhcpcd reports
// a lease that lapsed with the server unreachable as RELEASE, not
// EXPIRE. Verified against dhcpcd 10.3.2: two identical clients, one
// with `--noconfigure` and one without, both bound to a 2m lease with
// the server then killed; at the expiry instant the first fired RELEASE
// and the second EXPIRE.
//
// Up to v1.8.x there was a second reason, and it was the decisive one:
// a graceful stop (Leave / plugin shutdown → SIGTERM → `release`
// directive) fired the SAME reason, so mapping RELEASE would have
// counted every clean teardown as a DHCP failure.
//
// #800 REMOVED THE `release` DIRECTIVE, AND THAT CHANGES BOTH HALVES.
// The paragraph above was true, and it was true because of a co-factor
// it does not name: `release`, not `--noconfigure` alone. Measured on
// the shipped pair (alpine 3.24.1 + dhcpcd 10.3.2-r0) with this
// function's own argv from renderArgs, a 2m lease and the server then
// killed, all four cells run (#855):
//
//	--noconfigure + release     -> lapse fires RELEASE   <- the v1.8.x client
//	--noconfigure, no release   -> lapse fires EXPIRE    <- this build
//	no --noconfigure + release  -> lapse fires EXPIRE
//	no --noconfigure, no release-> lapse fires EXPIRE
//
// The original experiment compared --noconfigure against not, with both
// clients carrying `release`, so it could not see that the directive was
// doing the work. Confirmed with the plugin in the loop, by the failure
// suite's own logs across the two trees: on dev the outage shows
// "+0 leasefail / +1 watchdog", on this branch "+1 leasefail / +0
// watchdog". dhcpcd now speaks, and EXPIRE below already maps to a lease
// loss.
//
// RELEASE stays unmapped anyway, and the reason is now trivial rather
// than load-bearing: no client this build starts can produce it. Mapping
// it would be dead code; TestBuildEvent_ReleaseIsNotALeaseLoss keeps the
// contract pinned in case a `release` directive ever comes back.
// LeaseSeconds' deadline remains as the backstop for a lapse dhcpcd does
// not report at all.
func mapReason(reason string) (eventType string, v6 bool, emit bool) {
	switch reason {
	case "BOUND", "REBOOT":
		return "bound", false, true
	case "RENEW", "REBIND":
		return "renew", false, true
	case "BOUND6", "REBOOT6":
		return "bound", true, true
	case "RENEW6", "REBIND6":
		return "renew", true, true
	case "NAK":
		return "nak", false, true
	case "EXPIRE", "TIMEOUT":
		return "leasefail", false, true
	case "EXPIRE6", "TIMEOUT6":
		return "leasefail", true, true
	default:
		return "", false, false
	}
}

// v4PrefixLen returns the CIDR prefix length for a dhcpcd v4 lease.
// dhcpcd usually exports new_subnet_cidr (the prefix length directly);
// when it doesn't, we derive it from the dotted-quad new_subnet_mask.
// A non-contiguous mask (Size()==0,0) is rejected so a garbage value
// can't produce a bogus prefix downstream.
func v4PrefixLen(getenv Getenv) (string, bool) {
	if c := getenv("new_subnet_cidr"); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n >= 0 && n <= 32 {
			return c, true
		}
		return "", false
	}
	mask := getenv("new_subnet_mask")
	if mask == "" {
		return "", false
	}
	ip := net.ParseIP(mask)
	if ip == nil {
		return "", false
	}
	v4 := ip.To4()
	if v4 == nil {
		return "", false
	}
	ones, bits := net.IPMask(v4).Size()
	if bits == 0 {
		// Non-contiguous mask: net.IPMask.Size() signals this as (0, 0).
		return "", false
	}
	return strconv.Itoa(ones), true
}

// parseClasslessRoutes parses a dhcpcd classless-static-routes value
// (DHCP option 121, RFC 3442) — a space-separated sequence of
// "destination/prefix gateway" pairs, e.g.
// "10.0.0.0/8 192.168.99.1 0.0.0.0/0 192.168.99.254". It returns the
// non-default routes plus, separately, the gateway of a 0.0.0.0/0
// default entry ("" when absent) so the caller can apply RFC 3442
// default-route supersession over option 3.
//
// dhcpcd reports an on-link route with the gateway 0.0.0.0; that becomes
// a Route with an empty Gateway. Malformed entries (unparseable
// destination or gateway) are skipped best-effort — mirroring the MTU
// guard, a single bad route must not drop the whole lease event — as is
// a trailing unpaired token.
func parseClasslessRoutes(raw string) (routes []Route, defaultGW string) {
	fields := strings.Fields(raw)
	for i := 0; i+1 < len(fields); i += 2 {
		dest, gw := fields[i], fields[i+1]
		_, ipNet, err := net.ParseCIDR(dest)
		if err != nil {
			log.WithField("route", dest).Warn("Skipping classless static route with unparseable destination")
			continue
		}
		if net.ParseIP(gw) == nil {
			log.WithField("gateway", gw).Warn("Skipping classless static route with unparseable gateway")
			continue
		}
		onlink := gw == "0.0.0.0"

		// A 0.0.0.0/0 destination is the default route: its gateway
		// supersedes option 3 (RFC 3442) and is returned separately, not
		// added as a static route. An on-link default (gw 0.0.0.0) is
		// meaningless, so it is simply dropped.
		if ones, bits := ipNet.Mask.Size(); ones == 0 && bits == 32 && ipNet.IP.Equal(net.IPv4zero) {
			if !onlink {
				defaultGW = gw
			}
			continue
		}

		r := Route{Destination: ipNet.String()}
		if !onlink {
			r.Gateway = gw
		}
		routes = append(routes, r)
	}
	return routes, defaultGW
}

// BuildEvent assembles an Event from a dhcpcd hook invocation: the
// `$reason` string plus the `new_*` lease variables dhcpcd exports to
// its --script. Returns (event, true) when the caller should emit the
// event downstream; (zero, false) when the reason is one we don't act
// on, or when a lease-bearing event carries an unparseable address.
//
// Migration note (#152): this replaced busybox udhcpc/udhcpc6. busybox
// passed the event type as argv and a flat set of env vars
// (ip/mask/router/ipv6/dns6/…); dhcpcd passes the reason in $reason and
// the lease as the documented new_* variables, with the DHCPv6 IA_NA
// address in the indexed new_dhcp6_ia_na1_ia_addr1. The downstream
// Event/Info contract is unchanged, so the plugin's renew()/counter
// paths did not move.
//
// The #128 hardening is preserved: an emitted bound/renew NEVER carries
// an IP string that netlink.ParseAddr would later reject — malformed
// input skips the event instead of blowing up mid-renewal.
func BuildEvent(reason string, getenv Getenv) (Event, bool) {
	event, emit := buildEvent(reason, getenv)
	if !emit {
		return event, false
	}

	// Filter EVERY string value the server chose, at the one point all
	// of them pass through, before the event reaches a log field or the
	// FIFO. dhcpcd validates only its dname-typed options; the
	// string-typed ones (67, 100, 101, and our own `define 252 string
	// wpad`) carry \n and \r through untouched. See sanitizeInfo for
	// why an accidental single layer was not enough (#703).
	//
	// The count rides the event so the plugin -- a different process
	// from this hook -- can put it on a health counter. A drop that
	// leaves no trace is indistinguishable from a value that was never
	// sent.
	event.UnsafeValuesDropped = sanitizeInfo(&event.Data)

	// Option 15 needs its own rule, and 0x20 is why: SafeDirectiveValue
	// rejects control characters, and a SPACE is the field separator of
	// the sink this protects. One domain becomes several -- with the
	// attacker's first in the search order. See FirstSearchDomain (#704).
	if kept, truncated := FirstSearchDomain(event.Data.Domain); truncated {
		log.WithField("domain", quoteForLog(event.Data.Domain)).
			WithField("kept", kept).
			Warn("DHCP domain name (option 15) carried more than one domain; keeping only the first")
		event.Data.Domain = kept
		event.UnsafeValuesDropped++
	}

	// Option 3 (routers) is the one path into Gateway that is not
	// ParseIP-validated on the way in. The option-121 default gateway a
	// few lines above comes out of parseClasslessRoutes, which parses
	// every address it returns; this one is `strings.Fields(...)[0]`,
	// taken verbatim from the wire.
	//
	// Downstream, net.ParseIP's nil is not a refusal -- it is a valid
	// netlink argument meaning "no gateway", and reconcileDefaultRoute
	// hands it straight to RouteAdd/RouteReplace. The result is
	// `default dev ethX scope link`: an ON-LINK default route, which
	// makes the container ARP for every off-net destination and hands
	// interception to anyone on the L2 segment -- the attacker this
	// threat model already assumes (#728).
	//
	// Refuse it here instead, where the value enters. An empty Gateway
	// is a state the sink already knows: it returns without touching
	// the route, leaving whatever the container already had. Failing
	// closed costs a default route the server garbled; failing open
	// costs the container's traffic.
	if event.Data.Gateway != "" && net.ParseIP(event.Data.Gateway) == nil {
		log.WithField("gateway", quoteForLog(event.Data.Gateway)).
			Warn("DHCP gateway (option 3) is not an IP address; leaving the existing default route alone")
		event.Data.Gateway = ""
		event.UnsafeValuesDropped++
	}

	return event, true
}

// buildEvent is BuildEvent without the filter, so every one of its
// early returns is covered by the filter rather than each having to
// remember it.
func buildEvent(reason string, getenv Getenv) (Event, bool) {
	eventType, v6, emit := mapReason(reason)
	if !emit {
		log.Debugf("Ignoring dhcpcd reason %q", reason)
		return Event{}, false
	}

	event := Event{Type: eventType}

	// Lease-loss events (nak / leasefail) carry no data — emit Type only
	// so the consumer goroutine can match on them for its counters.
	if eventType == "nak" || eventType == "leasefail" {
		return event, true
	}

	if v6 {
		// DHCPv6 IA_NA address. dhcpcd indexes addresses as
		// new_dhcp6_ia_na<N>_ia_addr<M>; we pin a single IAID and
		// request one IA_NA, so the first address is the lease. A
		// missing/garbage address skips the event (the v6 analogue of
		// the #128 v4 guard).
		addr := getenv("new_dhcp6_ia_na1_ia_addr1")
		if addr == "" {
			log.WithField("reason", reason).Debug("DHCPv6 event with no IA_NA address; skipping")
			return Event{}, false
		}
		// dhcpcd emits a bare address; defensively strip any /prefix and
		// canonicalise via ParseCIDR to /128 (stable compressed form for
		// downstream string comparisons).
		bare := strings.SplitN(addr, "/", 2)[0]
		_, netV6, err := net.ParseCIDR(bare + "/128")
		if err != nil {
			log.WithError(err).WithField("ipv6", addr).Error("Failed to parse DHCPv6 address; skipping event")
			return Event{}, false
		}
		event.Data.IP = netV6.String()
		// DHCPv6 option 23 (recursive DNS servers).
		if dns := getenv("new_dhcp6_name_servers"); dns != "" {
			event.Data.DNSServers = strings.Fields(dns)
		}
		// IA_NA valid lifetime — the v6 lease clock the outage detector
		// uses (#353). DHCPv6 exposes no separate T1 through dhcpcd's
		// variables, so RenewSeconds stays 0 and the consumer falls back
		// to the lease itself.
		event.Data.LeaseSeconds = envSeconds(getenv, "new_dhcp6_ia_na1_ia_addr1_vltime")
		// No gateway in DHCPv6 (it comes from Router Advertisements,
		// sourced from the host routing table at Join) and no DHCPv6
		// MTU option — both are intentionally left zero.
		return event, true
	}

	// IPv4 lease. Compose CIDR from new_ip_address + the prefix length
	// and validate it as a whole, mirroring the v6 guard above.
	ipAddr := getenv("new_ip_address")
	prefix, ok := v4PrefixLen(getenv)
	if ipAddr == "" || !ok {
		log.WithField("ip", ipAddr).Error("Incomplete IPv4 lease (missing address or mask); skipping event")
		return Event{}, false
	}
	ipMask := ipAddr + "/" + prefix
	if _, _, err := net.ParseCIDR(ipMask); err != nil {
		log.WithError(err).WithField("ip", ipMask).Error("Failed to parse IPv4 lease; skipping event")
		return Event{}, false
	}
	event.Data.IP = ipMask

	// Option 121 (classless static routes, RFC 3442). Parsed before the
	// routers gateway below so an opt-121 default route (0.0.0.0/0) can
	// supersede option 3, as RFC 3442 requires.
	routes, classlessDefaultGW := parseClasslessRoutes(getenv("new_classless_static_routes"))
	event.Data.Routes = routes

	// Default gateway: an opt-121 default route supersedes the routers
	// option (opt 3) per RFC 3442; otherwise dhcpcd exports routers as a
	// space-separated list and the plugin applies a single default route,
	// so take the first.
	if classlessDefaultGW != "" {
		event.Data.Gateway = classlessDefaultGW
	} else if routers := strings.Fields(getenv("new_routers")); len(routers) > 0 {
		event.Data.Gateway = routers[0]
	}
	event.Data.Domain = getenv("new_domain_name")
	// Option 6 (DNS servers).
	if dns := getenv("new_domain_name_servers"); dns != "" {
		event.Data.DNSServers = strings.Fields(dns)
	}
	// Option 42 (NTP servers).
	if ntp := getenv("new_ntp_servers"); ntp != "" {
		event.Data.NTPServers = strings.Fields(ntp)
	}
	// Option 119 (DNS domain search list).
	if search := getenv("new_domain_search"); search != "" {
		event.Data.SearchList = strings.Fields(search)
	}
	// Option 66 (TFTP server name) / 67 (boot file name) — surfaced via
	// plugin logs, not auto-applied.
	event.Data.TFTPServer = getenv("new_tftp_server_name")
	event.Data.BootFile = getenv("new_bootfile_name")

	// Option 252 (WPAD URL), 100/101 (RFC 4833 timezone PCode/TCode) and
	// the legacy option 2 (time offset, seconds from UTC) — observe-only,
	// surfaced via plugin logs like TFTP/bootfile, never pushed into the
	// container (#262). Raw string values; dhcpcd already validated the
	// option types, so no parsing/guard is needed here.
	event.Data.WPAD = getenv("new_wpad")
	event.Data.PosixTimezone = getenv("new_posix_timezone")
	event.Data.TZDBTimezone = getenv("new_tzdb_timezone")
	event.Data.TimeOffset = getenv("new_time_offset")

	// Option 26 (interface MTU). Best-effort: a garbage or non-positive
	// value must not block IP propagation — the consumer treats 0 as
	// "no MTU info".
	if raw := getenv("new_interface_mtu"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			event.Data.MTU = n
		} else {
			log.WithField("mtu", raw).Warn("Failed to parse new_interface_mtu; skipping MTU propagation for this event")
		}
	}

	// Option 51 (lease time). Drives the plugin's outage detection, which
	// cannot rely on a lease-loss hook (#353). Option 58 (T1) is
	// deliberately not read — see Info.LeaseSeconds for why it is not a
	// usable deadline under --noconfigure.
	event.Data.LeaseSeconds = envSeconds(getenv, "new_dhcp_lease_time")

	return event, true
}

// envSeconds reads a positive integer seconds value from a dhcpcd lease
// variable, returning 0 when absent or unusable. Best-effort by design:
// a server that omits or mangles a lifetime must not cost us the lease
// event itself — the consumer treats 0 as "no deadline known" and falls
// back to event-driven detection alone.
func envSeconds(getenv Getenv, name string) int {
	raw := getenv(name)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.WithField(name, raw).Debug("Unusable lease-lifetime variable; outage deadline will fall back")
		return 0
	}
	return n
}
