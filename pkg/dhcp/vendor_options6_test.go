// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

func vendorLease6(o wire.OptionsV6) lease.Lease {
	return lease.Lease{
		Addr:       netip.MustParsePrefix("2001:db8::5/128"),
		ServerDUID: []byte{0, 3, 0, 1, 2, 2, 2, 2, 2, 2},
		OptionsV6:  o,
	}
}

func vendorInstance(enterprise uint32, data ...byte) wire.OptionV6 {
	return wire.OptionV6{
		Code: wire.OptV6VendorOpts,
		Data: append([]byte{byte(enterprise >> 24), byte(enterprise >> 16), byte(enterprise >> 8), byte(enterprise)}, data...),
	}
}

// Enterprise 9 with data, 3561 with none, 66051 (above 16 bits) with data (#1203).
func TestInfoFromLease_RecordsEveryOption17InstanceAsHex(t *testing.T) {
	l := vendorLease6(wire.OptionsV6{
		vendorInstance(9, 0x00, 0x01, 0x00, 0x02, 0xaa, 0xbb),
		vendorInstance(3561),
		vendorInstance(66051, 0xcc),
	})

	info, _ := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	want := []VendorBlock{{Enterprise: 9, Data: "00010002aabb"}, {Enterprise: 3561, Data: ""}, {Enterprise: 66051, Data: "cc"}}
	if !reflect.DeepEqual(info.VendorInformation, want) {
		t.Errorf("VendorInformation = %+v, want %+v (wire order, one block per instance)", info.VendorInformation, want)
	}
	if info.VendorSpecific != "" || len(info.VendorIdentifying) != 0 {
		t.Errorf("option 17 landed in a v4 vendor field: %q %+v", info.VendorSpecific, info.VendorIdentifying)
	}
	if info.IP != "2001:db8::5/128" {
		t.Errorf("the lease was lost along with option 17: %+v", info)
	}
}

// Pins the library's no-list-beside-ErrMalformedVendor behaviour, as for option 125, not a goal (#1203).
func TestInfoFromLease_AMalformedOption17InstanceLeavesTheWholeOptionOut(t *testing.T) {
	l := vendorLease6(wire.OptionsV6{
		vendorInstance(9, 0xaa),
		{Code: wire.OptV6VendorOpts, Data: []byte{0, 0, 1}},
	})

	info, _ := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	if len(info.VendorInformation) != 0 {
		t.Errorf("VendorInformation = %+v, want none beside a malformed instance", info.VendorInformation)
	}
}

func TestInfoFromLease_NoOption17NoVendorInformation(t *testing.T) {
	for name, l := range map[string]lease.Lease{
		"v6 lease, no options":      vendorLease6(nil),
		"v6 lease, unrelated 42":    vendorLease6(wire.OptionsV6{{Code: wire.OptV6TZDatabase, Data: []byte("Europe/Berlin")}}),
		"v4 lease with 43 and 125":  vendorLease(wire.Options{wire.OptVendorSpecific: {0x01}, wire.OptVIVSO: {0, 0, 0, 9, 1, 0x7f}}),
		"v4 lease with no options":  vendorLease(nil),
		"v6 lease, ORO-only option": vendorLease6(wire.OptionsV6{{Code: wire.OptV6DNSServers}}),
	} {
		info, _ := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})
		if len(info.VendorInformation) != 0 {
			t.Errorf("%s: VendorInformation = %+v, want none", name, info.VendorInformation)
		}
	}
}

// A server's bytes are control characters as text and hex as recorded, so no forged line can come out (#1203, #703).
func TestInfoFromLease_Option17BytesWithControlCharactersStayHex(t *testing.T) {
	forged := []byte("a\nlevel=error msg=FORGED\r")
	l := vendorLease6(wire.OptionsV6{vendorInstance(9, forged...)})

	info, dropped := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if len(info.VendorInformation) != 1 {
		t.Fatalf("VendorInformation = %+v, want one block", info.VendorInformation)
	}
	if s := info.VendorInformation[0].Data; s == "" || strings.ContainsAny(s, "\n\r") || !SafeValue(s) {
		t.Errorf("recorded value %q is not plain hex", s)
	}
}
