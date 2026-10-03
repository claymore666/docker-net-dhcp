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

var (
	iidTestMAC     = []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	iidTestNetwork = []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	iidTestSecret  = []byte("0123456789abcdef0123456789ABCDEF")
)

func TestParseIPv6IID_IsTheLibrarysOwnEnumeration(t *testing.T) {
	want := proto.AllIIDModes()
	got := IPv6IIDs()
	if len(got) != len(want) {
		t.Fatalf("IPv6IIDs() = %v, want one spelling per proto.AllIIDModes() entry %v (#1032)", got, want)
	}
	for i, m := range want {
		if got[i] != m.String() {
			t.Errorf("IPv6IIDs()[%d] = %q, want %q", i, got[i], m.String())
		}
		parsed, err := ParseIPv6IID(m.String())
		if err != nil || parsed != m {
			t.Errorf("ParseIPv6IID(%q) = %v, %v, want %v", m.String(), parsed, err, m)
		}
	}
}

func TestParseIPv6IID_UnsetIsEUI64(t *testing.T) {
	m, err := ParseIPv6IID("")
	if err != nil || m != proto.IIDModeEUI64 {
		t.Fatalf("ParseIPv6IID(\"\") = %v, %v, want eui64: an unset option must leave every existing address where it is (#1032)", m, err)
	}
}

func TestParseIPv6IID_RefusesAValueOutsideTheSet(t *testing.T) {
	for _, v := range []string{"EUI64", "eui-64", "stable_privacy", "stable-privacy ", "random", "opaque", "0", "true"} {
		m, err := ParseIPv6IID(v)
		if err == nil {
			t.Errorf("ParseIPv6IID(%q) = %v, want a refusal: a typo would run as eui64 and the operator would never learn (#1032)", v, m)
			continue
		}
		for _, need := range IPv6IIDs() {
			if !strings.Contains(err.Error(), need) {
				t.Errorf("the refusal %q does not list %q", err, need)
			}
		}
	}
}

func stableOpts6(t *testing.T) *DHCPClientOptions {
	t.Helper()
	o := testOpts6(t)
	o.Mode6 = proto.Mode6SLAAC
	o.MAC = append([]byte(nil), iidTestMAC...)
	o.IPv6IID = proto.IIDModeStablePrivacy
	o.IPv6IIDSecret = append([]byte(nil), iidTestSecret...)
	o.IPv6IIDNetworkID = append([]byte(nil), iidTestNetwork...)
	return o
}

func TestBuildParams6_IIDLeavesEUI64AloneWhenTheOptionIsUnset(t *testing.T) {
	o := testOpts6(t)
	o.Mode6 = proto.Mode6SLAAC
	o.MAC = iidTestMAC
	// A secret and a network id on an eui64 network must not reach the machine: eui64 reads neither (#1032).
	o.IPv6IIDSecret = iidTestSecret
	o.IPv6IIDNetworkID = iidTestNetwork
	p, err := buildParams6(o, false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if p.IID != proto.IIDModeEUI64 {
		t.Errorf("Params6.IID = %v on a network with no ipv6_iid, want %v", p.IID, proto.IIDModeEUI64)
	}
	if len(p.IIDSecret) != 0 || len(p.IIDNetIface) != 0 || len(p.IIDNetworkID) != 0 {
		t.Errorf("identifier inputs are secret %x, interface %x, network %x on an eui64 network, want all empty",
			p.IIDSecret, p.IIDNetIface, p.IIDNetworkID)
	}
}

func TestBuildParams6_StablePrivacyCarriesTheThreeInputs(t *testing.T) {
	p, err := buildParams6(stableOpts6(t), false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if p.IID != proto.IIDModeStablePrivacy {
		t.Errorf("Params6.IID = %v, want %v: the option never reached the machine (#1032)", p.IID, proto.IIDModeStablePrivacy)
	}
	if !bytes.Equal(p.IIDSecret, iidTestSecret) {
		t.Errorf("Params6.IIDSecret = %x, want %x", p.IIDSecret, iidTestSecret)
	}
	if !bytes.Equal(p.IIDNetIface, iidTestMAC) {
		t.Errorf("Params6.IIDNetIface = %x, want the endpoint MAC %x", p.IIDNetIface, iidTestMAC)
	}
	if !bytes.Equal(p.IIDNetworkID, iidTestNetwork) {
		t.Errorf("Params6.IIDNetworkID = %q, want the Docker network id", p.IIDNetworkID)
	}
	if _, err := proto.New6(p); err != nil {
		t.Errorf("the library refuses the stable-privacy Params6 this plugin builds: %v", err)
	}
}

func TestBuildParams6_StablePrivacyWithoutASecretIsRefusedByTheLibrary(t *testing.T) {
	o := stableOpts6(t)
	o.IPv6IIDSecret = nil
	p, err := buildParams6(o, false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if _, err := proto.New6(p); !errors.Is(err, proto.ErrShortIIDSecret) {
		t.Errorf("New6 = %v, want ErrShortIIDSecret: a stable-privacy network must not start on an empty secret (#1032)", err)
	}
}

func TestBuildParams6_StablePrivacyDoesNotAliasTheCallersSlices(t *testing.T) {
	o := stableOpts6(t)
	p, err := buildParams6(o, false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	o.IPv6IIDSecret[0] ^= 0xff
	o.IPv6IIDNetworkID[0] ^= 0xff
	o.MAC[0] ^= 0xff
	if !bytes.Equal(p.IIDSecret, iidTestSecret) || !bytes.Equal(p.IIDNetworkID, iidTestNetwork) || !bytes.Equal(p.IIDNetIface, iidTestMAC) {
		t.Errorf("Params6 changed with the options it was built from: a later write would move a running machine's addresses (#1032)")
	}
}

// formedAddress drives the machine the way the chassis does: start, one advertisement, and the address duplicate
// address detection is asked about is the one the interface identifier produced (RFC 4862 section 5.4, #1032).
func formedAddress(t *testing.T, p proto.Params6, prefix string) netip.Addr {
	t.Helper()
	m, err := proto.New6(p)
	if err != nil {
		t.Fatalf("proto.New6: %v", err)
	}
	m.Step(0, 0, proto.Simple(proto.EvStart))
	pio := make([]byte, 32)
	pio[0], pio[1], pio[2], pio[3] = 3, 4, 64, 0xc0
	pio[4], pio[5], pio[6], pio[7] = 0, 1, 0x51, 0x80
	pio[8], pio[9], pio[10], pio[11] = 0, 0, 0x38, 0x40
	copy(pio[16:], netip.MustParseAddr(prefix).AsSlice())
	raw := append([]byte{134, 0, 0, 0, 64, 0, 0x07, 0x08, 0, 0, 0, 0, 0, 0, 0, 0}, pio...)
	ra, err := wire.DecodeRouterAdvert(raw)
	if err != nil {
		t.Fatalf("DecodeRouterAdvert: %v", err)
	}
	ra.Router = netip.MustParseAddr("fe80::1")
	_, acts := m.Step(proto.Instant(proto.Second), 0, proto.RouterAdvertRaw(ra, raw))
	for _, a := range acts {
		if a.Kind == proto.ActStartDAD {
			return a.Target
		}
	}
	t.Fatalf("no duplicate address detection after an advertisement of %s: %v", prefix, acts)
	return netip.Addr{}
}

func TestBuildParams6_StablePrivacyFormsTheRFC7217AddressAndNotTheEUI64One(t *testing.T) {
	const prefix = "2001:db8:1::"
	p, err := buildParams6(stableOpts6(t), false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	got := formedAddress(t, p, prefix)

	iid := proto.StablePrivacyIID(netip.MustParsePrefix(prefix+"/64"), iidTestMAC, iidTestNetwork, 0, iidTestSecret)
	want := netip.MustParseAddr(prefix).As16()
	copy(want[8:], iid[:])
	if got != netip.AddrFrom16(want) {
		t.Errorf("the machine formed %s, want %s: RFC 7217 section 5's F(prefix, MAC, network id, 0, secret) (#1032)", got, netip.AddrFrom16(want))
	}
	eui, err := proto.ModifiedEUI64(iidTestMAC)
	if err != nil {
		t.Fatal(err)
	}
	if low := got.As16(); [8]byte(low[8:]) == eui {
		t.Errorf("%s carries the MAC's modified EUI-64 %x, the identifier stable-privacy exists to replace", got, eui)
	}

	// The eui64 control on the same MAC and prefix is the address #818 shipped.
	o := stableOpts6(t)
	o.IPv6IID = proto.IIDModeEUI64
	p, err = buildParams6(o, false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	wantEUI := netip.MustParseAddr(prefix).As16()
	copy(wantEUI[8:], eui[:])
	if got := formedAddress(t, p, prefix); got != netip.AddrFrom16(wantEUI) {
		t.Errorf("the eui64 control formed %s, want %s", got, netip.AddrFrom16(wantEUI))
	}
}

func TestBuildParams6_StablePrivacyDependsOnTheNetworkAndTheSecret(t *testing.T) {
	const prefix = "2001:db8:1::"
	base := func(edit func(*DHCPClientOptions)) netip.Addr {
		o := stableOpts6(t)
		edit(o)
		p, err := buildParams6(o, false)
		if err != nil {
			t.Fatalf("buildParams6: %v", err)
		}
		return formedAddress(t, p, prefix)
	}
	ref := base(func(*DHCPClientOptions) {})
	if again := base(func(*DHCPClientOptions) {}); again != ref {
		t.Fatalf("the same inputs formed %s and then %s: the address must be a pure function of its inputs (#1032)", ref, again)
	}
	for name, edit := range map[string]func(*DHCPClientOptions){
		"the network id": func(o *DHCPClientOptions) { o.IPv6IIDNetworkID = []byte("another-network-id-another-network-id") },
		"the secret":     func(o *DHCPClientOptions) { o.IPv6IIDSecret = bytes.Repeat([]byte{7}, 32) },
		"the MAC":        func(o *DHCPClientOptions) { o.MAC = []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x03} },
	} {
		if got := base(edit); got == ref {
			t.Errorf("changing %s left the address at %s: that input does not reach the identifier (#1032)", name, ref)
		}
	}
}
