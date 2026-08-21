// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/binary"
	"math"
	"net"
	"sort"
	"time"
)

// Bounds on the values a DHCP server chooses for us.
//
// The plugin trusts the server for *values* — that is what a DHCP client
// is for, and refusing a legitimate-but-unusual value breaks real
// deployments. What it must not do is let a server-chosen number switch
// a safety mechanism off, or push a link into a state no legitimate
// server asks for, with nothing but an Info log to show for it (#699).
//
// Each bound here is paired with a counter, because a refusal that
// leaves no trace is indistinguishable from a value that was never sent.

const (
	// maxLeaseDeadline caps the outage watchdog's deadline, NOT the
	// lease time reported to operators or written to the ledger.
	//
	// leaseDeadline is the only trigger that can detect a silently
	// lapsed lease under `--noconfigure` (see outageTracker) — the
	// acquiring trigger never fires for it. So option 51 is a number
	// the server picks that decides whether our own watchdog runs at
	// all: dhcpcd exports 0xFFFFFFFF verbatim, which is a 136-year
	// deadline, and dhcp_timeouts then stays at zero through a total
	// outage for that endpoint. That is exactly the failure #353 exists
	// to catch, re-opened from the wire.
	//
	// 24h is longer than any renewal interval that matters and short
	// enough that a silent lapse is noticed the same day. A server that
	// legitimately grants longer still renews long before this, so the
	// clamp costs a healthy client nothing.
	maxLeaseDeadline = 24 * time.Hour

	// minPropagatedMTU is the smallest MTU we will apply from option 26.
	// 576 is the IPv4 minimum reassembly buffer (RFC 791) and the
	// smallest value any real deployment uses. Below it, throughput is
	// destroyed and path MTU discovery black-holes, re-applied on every
	// renewal — measured: dhcpcd exported 68 unchanged and the kernel
	// accepted it.
	minPropagatedMTU = 576

	// maxPropagatedMTU is the largest value the kernel's own MTU field
	// can hold. The *device* ceiling is lower and the kernel enforces it
	// for us ("mtu greater than device maximum"), which is why this is
	// not the parent link's MTU: raising a container link above its
	// current MTU is the documented jumbo-frame use case, so the parent
	// is not a bound we may impose here. This end of the range is held
	// by the kernel; the constant exists so the accepted range is stated
	// in one place rather than half-stated.
	maxPropagatedMTU = 65535
)

// clampLeaseDeadline bounds a watchdog deadline to maxLeaseDeadline,
// reporting whether it had to. See maxLeaseDeadline for why the bound is
// only on the deadline and never on the value we report.
func clampLeaseDeadline(d time.Duration) (time.Duration, bool) {
	if d > maxLeaseDeadline {
		return maxLeaseDeadline, true
	}
	return d, false
}

// mtuAcceptable reports whether a server-supplied MTU is inside the
// range propagateMTU will apply. Split out as a pure function so the
// hostile values have a table test that needs no netlink.
func mtuAcceptable(mtu int) bool {
	return mtu >= minPropagatedMTU && mtu <= maxPropagatedMTU
}

// routesSupersedeDefault reports whether the union of these IPv4 static
// route destinations covers 0.0.0.0/0 — i.e. whether, taken together,
// they beat the container's default route on longest-prefix match.
//
// This is NOT the literal-default test parseClasslessRoutes already
// does. A server that wants the traffic without touching the reported
// gateway sends `0.0.0.0/1 <gw> 128.0.0.0/1 <gw>`: neither half is a
// default route, both install as ordinary static routes, and between
// them they take every destination while res.Gateway, `docker inspect`
// and the log all still name the legitimate router.
//
// The routes are still applied — that is correct client behaviour, and
// legitimate split-tunnel setups rely on it. The point is that the
// operator gets a counter and a log line naming the next hops, so
// "where did this container's traffic go" is answerable afterwards.
//
// IPv6 destinations are ignored: they cannot cover the v4 default.
func routesSupersedeDefault(routes []*StaticRoute) bool {
	type span struct{ lo, hi uint32 }

	spans := make([]span, 0, len(routes))
	for _, r := range routes {
		if r == nil {
			continue
		}
		_, n, err := net.ParseCIDR(r.Destination)
		if err != nil {
			continue
		}
		v4 := n.IP.To4()
		if v4 == nil {
			continue
		}
		ones, bits := n.Mask.Size()
		if bits != 32 {
			// Non-contiguous mask, or a v4-mapped address carrying a
			// 128-bit mask: not a prefix we can reason about.
			continue
		}
		lo := binary.BigEndian.Uint32(v4)
		// ones == 32 shifts the whole width, which Go defines as 0 for
		// unsigned operands — a /32 is correctly a single address.
		spans = append(spans, span{lo: lo, hi: lo | (math.MaxUint32 >> uint(ones))})
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })

	// Walk the sorted spans looking for one contiguous run from
	// 0.0.0.0 to 255.255.255.255. Any gap ends it.
	var next uint32
	for _, s := range spans {
		if s.lo > next {
			return false
		}
		if s.hi == math.MaxUint32 {
			return true
		}
		if s.hi+1 > next {
			next = s.hi + 1
		}
	}
	return false
}
