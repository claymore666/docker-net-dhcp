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

func vendorLease(o wire.Options) lease.Lease {
	return lease.Lease{
		Addr:    netip.MustParsePrefix("192.168.99.10/24"),
		Gateway: netip.MustParseAddr("192.168.99.1"),
		Options: o,
	}
}

// Distinct bytes per option, so a swapped pair of fields shows in the assertion (#1034).
func TestInfoFromLease_RecordsOptions43And125AsHex(t *testing.T) {
	l := vendorLease(wire.Options{
		wire.OptVendorSpecific: {0x01, 0x04, 0xc0, 0xa8, 0x63, 0x01},
		// Enterprise 9 with two data octets, 3561 with none, then 66051 (above 16 bits) with one.
		wire.OptVIVSO: {0, 0, 0, 9, 2, 0xaa, 0xbb, 0, 0, 0x0d, 0xe9, 0, 0, 1, 2, 3, 1, 0xcc},
	})

	info, dropped := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	if dropped != 0 {
		t.Errorf("dropped = %d, want 0: hex is never a control character", dropped)
	}
	if info.VendorSpecific != "0104c0a86301" {
		t.Errorf("VendorSpecific = %q, want option 43's bytes as hex", info.VendorSpecific)
	}
	want := []VendorBlock{{Enterprise: 9, Data: "aabb"}, {Enterprise: 3561, Data: ""}, {Enterprise: 66051, Data: "cc"}}
	if !reflect.DeepEqual(info.VendorIdentifying, want) {
		t.Errorf("VendorIdentifying = %+v, want %+v (wire order, one block per enterprise)", info.VendorIdentifying, want)
	}
	if info.IP != "192.168.99.10/24" || info.Gateway != "192.168.99.1" {
		t.Errorf("the lease was lost along with the vendor options: %+v", info)
	}
}

func TestInfoFromLease_Option125AloneIsRecorded(t *testing.T) {
	l := vendorLease(wire.Options{wire.OptVIVSO: {0, 0, 0x01, 0x37, 1, 0x7f}})

	info, _ := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	if info.VendorSpecific != "" {
		t.Errorf("VendorSpecific = %q on a lease with no option 43", info.VendorSpecific)
	}
	want := []VendorBlock{{Enterprise: 311, Data: "7f"}}
	if !reflect.DeepEqual(info.VendorIdentifying, want) {
		t.Errorf("VendorIdentifying = %+v, want %+v", info.VendorIdentifying, want)
	}
}

func TestInfoFromLease_Option43AloneIsRecorded(t *testing.T) {
	l := vendorLease(wire.Options{wire.OptVendorSpecific: {0xde, 0xad}})

	info, _ := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	if info.VendorSpecific != "dead" {
		t.Errorf("VendorSpecific = %q, want dead", info.VendorSpecific)
	}
	if len(info.VendorIdentifying) != 0 {
		t.Errorf("VendorIdentifying = %+v on a lease with no option 125", info.VendorIdentifying)
	}
}

// A block cut short is left out whole, as the library refuses it whole, and option 43 is not lost beside it (#1034).
func TestInfoFromLease_MalformedOption125IsLeftOutAndNothingElse(t *testing.T) {
	l := vendorLease(wire.Options{
		wire.OptVendorSpecific: {0x01},
		wire.OptVIVSO:          {0, 0, 0, 9, 5, 0xaa},
	})

	info, _ := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	if len(info.VendorIdentifying) != 0 {
		t.Errorf("VendorIdentifying = %+v, want none from a malformed option", info.VendorIdentifying)
	}
	if info.VendorSpecific != "01" {
		t.Errorf("VendorSpecific = %q, want 01", info.VendorSpecific)
	}
}

// A present option 43 of zero octets has nothing to show, and a plain lease records neither field.
func TestInfoFromLease_NoVendorValueNoVendorField(t *testing.T) {
	for name, o := range map[string]wire.Options{
		"absent":       {},
		"empty 43":     {wire.OptVendorSpecific: {}},
		"empty 125":    {wire.OptVIVSO: {}},
		"nil options":  nil,
		"unrelated 42": {wire.OptNTPServer: {192, 168, 99, 1}},
	} {
		info, _ := infoFromLease(vendorLease(o), proto.RouterObservation{}, time.Now(), netip.Prefix{})
		if info.VendorSpecific != "" || len(info.VendorIdentifying) != 0 {
			t.Errorf("%s: vendor fields set: %q %+v", name, info.VendorSpecific, info.VendorIdentifying)
		}
	}
}

// A server's bytes are control characters as text and hex as recorded, so no forged line can come out (#1034, #703).
func TestInfoFromLease_VendorBytesWithControlCharactersStayHex(t *testing.T) {
	forged := []byte("a\nlevel=error msg=FORGED\r")
	l := vendorLease(wire.Options{
		wire.OptVendorSpecific: forged,
		wire.OptVIVSO:          append([]byte{0, 0, 0, 9, byte(len(forged))}, forged...),
	})

	info, dropped := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	for _, s := range []string{info.VendorSpecific, info.VendorIdentifying[0].Data} {
		if s == "" || strings.ContainsAny(s, "\n\r") || !SafeValue(s) {
			t.Errorf("recorded value %q is not plain hex", s)
		}
	}
}
