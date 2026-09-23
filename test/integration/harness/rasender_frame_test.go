// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"encoding/binary"
	"net"
	"reflect"
	"testing"
)

func ethernetFrameAround(icmp []byte) []byte {
	b := make([]byte, ethHeaderLen+ipv6HeaderLen)
	copy(b[6:12], []byte{0x02, 0, 0, 0, 0, 1})
	binary.BigEndian.PutUint16(b[12:14], ethertypeIPv6)
	ip := b[ethHeaderLen:]
	ip[0] = 6 << 4
	binary.BigEndian.PutUint16(ip[4:6], uint16(len(icmp)))
	ip[6], ip[7] = protoICMPv6, 255
	return append(b, icmp...)
}

func TestEncodeRA_RoundTripsThroughParseRA(t *testing.T) {
	spec := RASpec{
		Managed:        true,
		RouterLifetime: 1800,
		Prefixes: []RAPrefix{
			{Prefix: net.ParseIP("fd00:1::"), PrefixLen: 64, OnLink: true, Autonomous: true, ValidLifetime: 30, PreferredLifetime: 20},
			{Prefix: net.ParseIP("fd00:2::"), PrefixLen: 64, OnLink: true, ValidLifetime: RAInfiniteLifetime},
		},
		Routes:        []RARoute{{Prefix: net.ParseIP("fd00:77::"), PrefixLen: 48, Lifetime: 600}},
		DNSServers:    []net.IP{net.ParseIP("fd00:1::53"), net.ParseIP("fd00:1::54")},
		SearchDomains: []string{"ra.example", "b.ra.example"},
		DNSLifetime:   600,
	}
	f, ok := ParseRA(ethernetFrameAround(EncodeRA(spec)))
	if !ok {
		t.Fatal("ParseRA refused the encoded advertisement")
	}
	if !f.Managed || f.OtherConfig || f.RouterLifetime.Seconds() != 1800 || f.CurHopLimit != raCurHopLimit {
		t.Errorf("header decoded as %s", f)
	}
	if !reflect.DeepEqual(f.Prefixes, normalisedPrefixes(spec.Prefixes)) {
		t.Errorf("prefixes = %+v, want %+v", f.Prefixes, spec.Prefixes)
	}
	if len(f.Routes) != 1 || !f.Routes[0].Prefix.Equal(spec.Routes[0].Prefix) ||
		f.Routes[0].PrefixLen != 48 || f.Routes[0].Lifetime != 600 {
		t.Errorf("routes = %+v, want %+v", f.Routes, spec.Routes)
	}
	if len(f.DNSServers) != 2 || !f.DNSServers[0].Equal(spec.DNSServers[0]) || !f.DNSServers[1].Equal(spec.DNSServers[1]) {
		t.Errorf("rdnss = %v, want %v", f.DNSServers, spec.DNSServers)
	}
	if !reflect.DeepEqual(f.SearchDomains, spec.SearchDomains) {
		t.Errorf("dnssl = %v, want %v", f.SearchDomains, spec.SearchDomains)
	}
}

func normalisedPrefixes(in []RAPrefix) []RAPrefix {
	out := make([]RAPrefix, len(in))
	for i, p := range in {
		p.Prefix = p.Prefix.To16()
		out[i] = p
	}
	return out
}

func TestEncodeRA_OptionsAreWholeOctetsAndTheListEndsExactly(t *testing.T) {
	b := EncodeRA(RASpec{SearchDomains: []string{"a.example"}, DNSServers: []net.IP{net.ParseIP("fd00::1")}})
	o := b[raOptionOffset:]
	for len(o) > 0 {
		n := int(o[1]) * 8
		if n == 0 || n > len(o) {
			t.Fatalf("option type %d has length %d with %d bytes left", o[0], n, len(o))
		}
		o = o[n:]
	}
}

func TestEncodeRA_AnEmptySpecCarriesNoOptions(t *testing.T) {
	f, ok := ParseRA(ethernetFrameAround(EncodeRA(RASpec{})))
	if !ok || len(f.Prefixes)+len(f.Routes)+len(f.DNSServers)+len(f.SearchDomains) != 0 || f.RouterLifetime != 0 {
		t.Errorf("an empty advertisement decoded as %s (ok=%v)", f, ok)
	}
}
