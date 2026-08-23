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
	// maxLeaseDeadline is the deadline substituted for a lease the
	// client will never renew from. It caps the outage watchdog's
	// deadline, NOT the lease time reported to operators or written to
	// the ledger.
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
	// enough that a silent lapse is noticed the same day.
	maxLeaseDeadline = 24 * time.Hour

	// maxRenewableLease decides WHICH leases maxLeaseDeadline is applied
	// to, and it is the whole correctness of the clamp.
	//
	// Applying the 24h deadline to every long lease is wrong, and wrong
	// in the direction that costs the most. Under `--noconfigure` the
	// interface carries no address, so dhcpcd's T1 unicast renewal
	// cannot succeed and every renewal lands at T2 — RFC 2131's
	// 0.875 × lease (measured: 105s on a 120s lease, dhcpcd 10.3.2). A
	// deadline of 24h therefore falls BEFORE the healthy client's own
	// next contact with the server for any lease longer than about
	// 27.4h, and `due` then counts a dhcp_timeout on every tick from
	// 24h until the rebind actually happens. On a 7-day lease — an
	// ordinary value, not an exotic one — that is roughly 14,800
	// fabricated timeouts per endpoint per lease, each with its own
	// "DHCP server still unreachable" log line, for a client that is
	// working perfectly.
	//
	// So the clamp applies only where there is no renewal to wait for.
	// A year is far above any operational lease anyone grants and far
	// below what "permanent" encodes: 0xFFFFFFFF is 136 years. Above
	// this line the client will never come back to the server on its
	// own, so substituting a deadline invents nothing — below it, the
	// client's own rebind restarts the deadline and the lease is the
	// only instant that needs no assumption about which retry
	// succeeded (see leaseDeadline).
	maxRenewableLease = 365 * 24 * time.Hour

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

// clampLeaseDeadline substitutes maxLeaseDeadline for a lease lifetime
// the client will never renew from, reporting whether it had to.
//
// A lease at or below maxRenewableLease is returned UNCHANGED, however
// long it is: the client rebinds at T2 and that restarts the deadline,
// so shortening it here would count a healthy client as an outage on
// every tick in between. See maxRenewableLease.
//
// The bound is on the deadline only, never on the value we report.
func clampLeaseDeadline(d time.Duration) (time.Duration, bool) {
	if d > maxRenewableLease {
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

// v4Span is a closed interval of IPv4 address space, as unsigned
// integers, so that prefix coverage is an arithmetic question rather
// than a mask one.
type v4Span struct{ lo, hi uint32 }

// routableUnicastV4 is what "takes every destination that matters" means
// for routesSupersedeDefault, and it is the whole reason that function
// is not a test for covering 0.0.0.0/0.
//
// Demanding an exact cover of 0.0.0.0/0 is evadable by ARITHMETIC, with
// no hole a container would ever notice. `0.0.0.0/1 128.0.0.0/2
// 192.0.0.0/3 224.0.0.0/4` reaches 239.255.255.255 — every routable
// unicast address plus all of multicast — leaving only 240.0.0.0/4,
// which is reserved and unroutable. Three routes are enough if the
// sender does not care about multicast. Under the old predicate all of
// those returned false, so the one shape the detector existed to catch
// was also the easiest one to slip past: add a prefix.
//
// These are therefore the ranges a full takeover must cover, with the
// blocks no container routes through excluded: 0.0.0.0/8 (this
// network), 127.0.0.0/8 (loopback), 169.254.0.0/16 (link-local),
// 224.0.0.0/4 (multicast) and 240.0.0.0/4 (reserved). RFC 1918 space is
// deliberately NOT excluded — a route set that omits the container's own
// private ranges is a split tunnel, which is the legitimate case.
var routableUnicastV4 = [...]v4Span{
	{0x01000000, 0x7EFFFFFF}, // 1.0.0.0      – 126.255.255.255
	{0x80000000, 0xA9FDFFFF}, // 128.0.0.0    – 169.253.255.255
	{0xA9FF0000, 0xDFFFFFFF}, // 169.255.0.0  – 223.255.255.255
}

// routesSupersedeDefault reports whether the union of these IPv4 static
// route destinations takes every routable unicast destination — i.e.
// whether, taken together, they beat the container's default route on
// longest-prefix match for any address it would actually talk to.
//
// This is NOT the literal-default test parseClasslessRoutes already
// does. A server that wants the traffic without touching the reported
// gateway sends `0.0.0.0/1 <gw> 128.0.0.0/1 <gw>`: neither half is a
// default route, both install as ordinary static routes, and between
// them they take every destination while res.Gateway, `docker inspect`
// and the log all still name the legitimate router.
//
// It remains a HEURISTIC, and the direction of its remaining error is
// worth being explicit about: a sender that leaves a genuine hole in
// routable space — one prefix it does not claim — is not reported, no
// matter how small the hole is. That cannot be closed by making the
// predicate stricter, because at some hole size the route set really is
// a split tunnel and reporting it would be wrong. What bounds the damage
// is that describeStaticRoutes logs every destination and next hop
// regardless of this verdict (see network.go), so the evidence for
// "where did this container's traffic go" is on record either way. This
// function only decides whether a counter also moves.
//
// The routes are still applied — that is correct client behaviour, and
// legitimate split-tunnel setups rely on it. The point is that the
// operator gets a counter and a log line naming the next hops, so
// "where did this container's traffic go" is answerable afterwards.
//
// IPv6 destinations are ignored: they cannot cover the v4 default.
func routesSupersedeDefault(routes []*StaticRoute) bool {
	spans := make([]v4Span, 0, len(routes))
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
		spans = append(spans, v4Span{lo: lo, hi: lo | (math.MaxUint32 >> uint(ones))})
	}

	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })

	for _, req := range routableUnicastV4 {
		if !spansCover(spans, req.lo, req.hi) {
			return false
		}
	}
	return true
}

// spansCover reports whether the union of spans, ALREADY SORTED BY lo,
// contains every address in [lo, hi].
//
// The walk carries `next`, the first address not yet covered. A span
// starting beyond it is a gap and ends the question; anything else
// extends the run. Overlap and containment need no special case, which
// matters because a real route set repeats and nests prefixes freely.
func spansCover(spans []v4Span, lo, hi uint32) bool {
	next := lo
	for _, s := range spans {
		if s.lo > next {
			return false
		}
		if s.hi >= hi {
			// Reached before computing s.hi+1, so a span ending at
			// 255.255.255.255 cannot wrap to 0 and restart the walk.
			return true
		}
		if s.hi+1 > next {
			next = s.hi + 1
		}
	}
	return false
}
