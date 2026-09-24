// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/binary"
	"math"
	"net"
	"sort"
)

// Bounds on server-chosen values that could switch a safety mechanism off, each paired with a counter (#699).

const (
	// minPropagatedMTU is 576, the IPv4 minimum reassembly size (RFC 791), since a server-supplied 68
	// once reached the kernel unchanged (#699).
	minPropagatedMTU = 576

	// maxPropagatedMTU is the kernel's MTU field ceiling; the kernel enforces the device maximum
	// itself, and a link above its parent's MTU is the jumbo-frame case (#699).
	maxPropagatedMTU = 65535
)

func mtuAcceptable(mtu int) bool {
	return mtu >= minPropagatedMTU && mtu <= maxPropagatedMTU
}

type v4Span struct{ lo, hi uint32 }

// routableUnicastV4 is IPv4 minus 0/8, 127/8, 169.254/16, 224/4 and 240/4 (RFC 6890), so a route
// set reaching 239.255.255.255 counts as a takeover; RFC 1918 space stays in (#700).
var routableUnicastV4 = [...]v4Span{
	{0x01000000, 0x7EFFFFFF}, // 1.0.0.0      – 126.255.255.255
	{0x80000000, 0xA9FDFFFF}, // 128.0.0.0    – 169.253.255.255
	{0xA9FF0000, 0xDFFFFFFF}, // 169.255.0.0  – 223.255.255.255
}

// routesSupersedeDefault reports whether these IPv4 destinations cover routableUnicastV4, as
// `0.0.0.0/1 128.0.0.0/1` does while the reported gateway still names the router (#700).
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
			continue
		}
		lo := binary.BigEndian.Uint32(v4)
		// A shift by the full width is 0 for unsigned operands in Go, so a /32 is one address.
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

func spansCover(spans []v4Span, lo, hi uint32) bool {
	next := lo
	for _, s := range spans {
		if s.lo > next {
			return false
		}
		if s.hi >= hi {
			// Checked before s.hi+1, so a span ending at 255.255.255.255 cannot wrap to 0.
			return true
		}
		if s.hi+1 > next {
			next = s.hi + 1
		}
	}
	return false
}
