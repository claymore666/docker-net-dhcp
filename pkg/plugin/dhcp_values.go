// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/binary"
	"math"
	"net"
	"sort"
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
