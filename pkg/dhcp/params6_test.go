// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"errors"
	"net/netip"
	"strings"
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

// firstSolicit6 builds the library machine from p and returns the Solicit it sends once the RFC 9915 section 18.2.1
// delay fires, the message option 39 must ride on (RFC 4704 section 5, #1029).
func firstSolicit6(t *testing.T, p proto.Params6) *wire.MessageV6 {
	t.Helper()
	m, err := proto.New6(p)
	if err != nil {
		t.Fatalf("proto.New6: %v", err)
	}
	m.Step(0, 0, proto.Simple(proto.EvStart))
	_, acts := m.Step(proto.Instant(proto.Second), 1, proto.TimerFired(proto.Timer6Delay))
	for _, a := range acts {
		if a.Kind == proto.ActSendV6 && a.MsgV6 != nil && a.MsgV6.Type == wire.MsgSolicit {
			return a.MsgV6
		}
	}
	t.Fatalf("no Solicit after the delay: %v", acts)
	return nil
}

func TestBuildParams6_RegisterDNSPutsTheNameInOption39(t *testing.T) {
	opts := testOpts6(t)
	opts.Hostname, opts.FQDN = "web1", "both"
	p, err := buildParams6(opts, false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if p.Hostname != "web1" {
		t.Fatalf("Params6.Hostname = %q on a register_dns network, want %q: the v6 client would ask the server "+
			"for no AAAA while the v4 client asks for the A record (#1029)", p.Hostname, "web1")
	}
	f, ok, err := firstSolicit6(t, p).Options.ClientFQDN()
	if err != nil || !ok {
		t.Fatalf("the Solicit carries option 39 %v (err %v), want it present", ok, err)
	}
	if f.Name != "web1" || f.Flags != wire.ClientFQDNFlagS {
		t.Errorf("the Solicit's option 39 is %+v, want the partial name %q with S=1 O=0 N=0 (RFC 4704 section 5.2)",
			f, "web1")
	}
}

func TestBuildParams6_NoRegisterDNSSendsNoName(t *testing.T) {
	opts := testOpts6(t)
	opts.Hostname = "web1"
	p, err := buildParams6(opts, false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if p.Hostname != "" {
		t.Fatalf("Params6.Hostname = %q without register_dns, want empty: the library sends a name only as "+
			"option 39 with S=1, which asks the server to register an AAAA nobody opted into (#1029 (a))", p.Hostname)
	}
	if _, ok, _ := firstSolicit6(t, p).Options.ClientFQDN(); ok {
		t.Errorf("the Solicit carries option 39 on a network without register_dns")
	}
}

// The label rule is unchanged: a label over 63 octets is refused on register_dns networks, both families, and a v6
// network without register_dns never meets it (RFC 1035 section 2.3.4, #1029).
func TestBuildParams6_LongLabelIsRefusedWhereV4RefusesIt(t *testing.T) {
	long := strings.Repeat("a", 64)
	mac := testMAC(t)

	v4, err := buildParams(&DHCPClientOptions{MAC: mac, Hostname: long, FQDN: "both"}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if _, err := proto.New(v4); !errors.Is(err, proto.ErrBadFQDN) {
		t.Fatalf("v4 with register_dns took a 64-octet label: %v; this test pins v6 to the v4 rule", err)
	}

	opts := testOpts6(t)
	opts.Hostname, opts.FQDN = long, "both"
	p, err := buildParams6(opts, false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if _, err := proto.New6(p); err == nil {
		t.Errorf("v6 with register_dns took a 64-octet label that v4 refuses on the same network")
	}

	opts.FQDN = ""
	if p, err = buildParams6(opts, false); err != nil {
		t.Fatalf("buildParams6 without register_dns: %v", err)
	}
	if _, err := proto.New6(p); err != nil {
		t.Errorf("v6 without register_dns refused a 64-octet hostname it never sends: %v", err)
	}
}
