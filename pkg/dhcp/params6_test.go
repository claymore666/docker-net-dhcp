// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

func testOpts6(t *testing.T) *DHCPClientOptions {
	t.Helper()
	return &DHCPClientOptions{
		V6: true,
		Identity6: Identity6{
			DUID: []byte{0, 3, 0, 1, 0x02, 0x42, 0xac, 0x11, 0, 2},
			IAID: 0xac110002,
		},
	}
}

func TestBuildParams6_Refusals(t *testing.T) {
	cases := []struct {
		name string
		opts func() *DHCPClientOptions
	}{
		{"a v4 endpoint", func() *DHCPClientOptions {
			o := testOpts6(t)
			o.V6 = false
			return o
		}},
		{"no identity at all", func() *DHCPClientOptions {
			o := testOpts6(t)
			o.Identity6 = Identity6{}
			return o
		}},
		{"an IAID but no DUID", func() *DHCPClientOptions {
			o := testOpts6(t)
			o.Identity6 = Identity6{IAID: 9}
			return o
		}},
		{"a preferred address that is not an address", func() *DHCPClientOptions {
			o := testOpts6(t)
			o.PreferredV6 = "not-an-address"
			return o
		}},
		// A v4 hint parses and the server answers another address, so the `ipv6` option would silently mean nothing
		// (#213).
		{"a v4 address as the v6 hint", func() *DHCPClientOptions {
			o := testOpts6(t)
			o.PreferredV6 = "192.168.0.10"
			return o
		}},
		{"a v4-mapped address as the v6 hint", func() *DHCPClientOptions {
			o := testOpts6(t)
			o.PreferredV6 = "::ffff:192.168.0.10"
			return o
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildParams6(tc.opts(), false); err == nil {
				t.Error("buildParams6 accepted it")
			}
			if _, err := buildParams6(tc.opts(), true); err == nil {
				t.Error("buildParams6 accepted it as a one-shot")
			}
		})
	}
}

// Measured: dnsmasq 2.91 answers an Information-request with exactly the options asked for, and proto.DefaultParams6
// leaves ORO nil (#911).

func TestBuildParams6_CarriesTheIdentityAndAsksForConfiguration(t *testing.T) {
	opts := testOpts6(t)
	p, err := buildParams6(opts, false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if !bytes.Equal(p.DUID, opts.Identity6.DUID) {
		t.Errorf("DUID = %x, want %x", p.DUID, opts.Identity6.DUID)
	}
	if p.IAID != opts.Identity6.IAID {
		t.Errorf("IAID = %#x, want %#x", p.IAID, opts.Identity6.IAID)
	}
	if len(p.ORO) == 0 {
		t.Fatal("the ORO is empty: dnsmasq answers an Information-request with exactly " +
			"what was asked for, so an empty ORO makes every stateless segment look " +
			"like it offers no resolver")
	}
	want := map[uint16]string{
		uint16(wire.OptV6DNSServers): "DNS servers (RFC 3646 option 23)",
		uint16(wire.OptV6DomainList): "the domain search list (RFC 3646 option 24)",
	}
	for opt, why := range want {
		found := false
		for _, got := range p.ORO {
			if uint16(got) == opt {
				found = true
			}
		}
		if !found {
			t.Errorf("the ORO does not ask for %s; the container would get no %s", why, why)
		}
	}

	opts.Identity6.DUID[0] = 0xff
	if p.DUID[0] == 0xff {
		t.Error("buildParams6 aliases the caller's DUID")
	}
}

// The library reads an invalid Hint as no IA_ADDR, while "::" is an IA_ADDR the server is asked to honour (#911).

func TestBuildParams6_HintIsOptional(t *testing.T) {
	p, err := buildParams6(testOpts6(t), false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if p.Hint.IsValid() {
		t.Errorf("Hint = %v with no PreferredV6 set, want the zero address", p.Hint)
	}

	opts := testOpts6(t)
	opts.PreferredV6 = "2001:db8::5"
	p, err = buildParams6(opts, false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if want := netip.MustParseAddr("2001:db8::5"); p.Hint != want {
		t.Errorf("Hint = %v, want %v", p.Hint, want)
	}
}

// RFC 9915 section 18.2.1 gives Solicit no MRC and no MRD, so the container's deadline decides when to give up.

func TestBuildParams6_KeepsTheLibraryRetransmissionPolicy(t *testing.T) {
	p, err := buildParams6(testOpts6(t), false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	d := proto.DefaultParams6()
	if p.SolMaxRT != d.SolMaxRT {
		t.Errorf("SolMaxRT = %v, want the library default %v", p.SolMaxRT, d.SolMaxRT)
	}
	if p.RouterSolicitations != d.RouterSolicitations {
		t.Errorf("RouterSolicitations = %v, want the library default %v",
			p.RouterSolicitations, d.RouterSolicitations)
	}
	if p.RouterSolicitInterval != d.RouterSolicitInterval {
		t.Errorf("RouterSolicitInterval = %v, want the library default %v",
			p.RouterSolicitInterval, d.RouterSolicitInterval)
	}
}
