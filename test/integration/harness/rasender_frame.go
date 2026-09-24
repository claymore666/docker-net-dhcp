// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"encoding/binary"
	"net"
	"strings"
)

// RASpec is the content of one router advertisement the harness sends.
type RASpec struct {
	Managed        bool
	OtherConfig    bool
	RouterLifetime uint16
	Prefixes       []RAPrefix
	Routes         []RARoute
	DNSServers     []net.IP
	SearchDomains  []string
	// DNSLifetime covers both the RDNSS and the DNSSL option.
	DNSLifetime uint32
}

// RARoute is one Route Information option (RFC 4191 section 2.3).
type RARoute struct {
	Prefix    net.IP
	PrefixLen uint8
	Lifetime  uint32
}

const (
	raOptRouteInfo = 24
	raOptRDNSS     = 25
	raOptDNSSL     = 31
	raCurHopLimit  = 64
)

// EncodeRA returns the ICMPv6 message; the checksum stays zero for the kernel to fill (RFC 3542 section 3.1).
func EncodeRA(s RASpec) []byte {
	b := make([]byte, raOptionOffset)
	b[0] = icmpTypeRA
	b[4] = raCurHopLimit
	if s.Managed {
		b[raFlagsOffset] |= raFlagManaged
	}
	if s.OtherConfig {
		b[raFlagsOffset] |= raFlagOtherConfig
	}
	binary.BigEndian.PutUint16(b[6:8], s.RouterLifetime)

	for _, p := range s.Prefixes {
		o := make([]byte, 32)
		o[0], o[1], o[2] = raOptPrefixInfo, 4, p.PrefixLen
		if p.OnLink {
			o[3] |= raPrefixFlagOnLink
		}
		if p.Autonomous {
			o[3] |= raPrefixFlagAutonom
		}
		binary.BigEndian.PutUint32(o[raPrefixValidOffset:], p.ValidLifetime)
		binary.BigEndian.PutUint32(o[raPrefixPreferredOffset:], p.PreferredLifetime)
		copy(o[16:], p.Prefix.To16())
		b = append(b, o...)
	}
	for _, r := range s.Routes {
		o := make([]byte, 24)
		o[0], o[1], o[2] = raOptRouteInfo, 3, r.PrefixLen
		binary.BigEndian.PutUint32(o[4:], r.Lifetime)
		copy(o[8:], r.Prefix.To16())
		b = append(b, o...)
	}
	if len(s.DNSServers) > 0 {
		o := make([]byte, 8+16*len(s.DNSServers))
		o[0], o[1] = raOptRDNSS, byte(len(o)/8)
		binary.BigEndian.PutUint32(o[4:], s.DNSLifetime)
		for i, ip := range s.DNSServers {
			copy(o[8+16*i:], ip.To16())
		}
		b = append(b, o...)
	}
	if len(s.SearchDomains) > 0 {
		o := make([]byte, 8)
		o[0] = raOptDNSSL
		binary.BigEndian.PutUint32(o[4:], s.DNSLifetime)
		for _, d := range s.SearchDomains {
			o = append(o, encodeDNSName(d)...)
		}
		for len(o)%8 != 0 {
			o = append(o, 0)
		}
		o[1] = byte(len(o) / 8)
		b = append(b, o...)
	}
	return b
}

func encodeDNSName(name string) []byte {
	var out []byte
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	return append(out, 0)
}

// decodeDNSSL reads RFC 8106 section 5.2's names; the zero bytes after the last name are padding.
func decodeDNSSL(b []byte) []string {
	var out, labels []string
	for len(b) > 0 {
		n := int(b[0])
		b = b[1:]
		if n == 0 {
			if len(labels) == 0 {
				continue
			}
			out = append(out, strings.Join(labels, "."))
			labels = nil
			continue
		}
		if n > len(b) {
			return out
		}
		labels = append(labels, string(b[:n]))
		b = b[n:]
	}
	return out
}
