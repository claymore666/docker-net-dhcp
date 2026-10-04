// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

var wantResumedVendor17 = []VendorBlock{{Enterprise: 9, Data: "0001000200aa"}, {Enterprise: 3561, Data: "00020002bbcc"}}

func resumedVendorOptions6() wire.OptionsV6 {
	return wire.OptionsV6{
		vendorInstance(9, 0x00, 0x01, 0x00, 0x02, 0x00, 0xaa),
		vendorInstance(3561, 0x00, 0x02, 0x00, 0x02, 0xbb, 0xcc),
	}
}

// Two instances share option code 17, so the record's generic code and data list must keep both (#1203, #1033).
func TestRecords6_TwoOption17InstancesSurviveAReopen(t *testing.T) {
	r, path := testRecords(t)
	const network = "net-1"
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	id6 := testIdentity6(t, "02:42:ac:11:00:02")
	now := time.Now()
	held := vendorLease6(resumedVendorOptions6())
	held.Addrs = []lease.Addr6{{Addr: held.Addr, Preferred: now.Add(time.Hour), Valid: now.Add(2 * time.Hour)}}
	held.Acquired, held.Renew, held.Rebind, held.Expire = now, now.Add(30*time.Minute), now.Add(time.Hour), now.Add(2*time.Hour)

	if err := r.Created6("ep-v6", network, mac, id6.Bytes()); err != nil {
		t.Fatalf("Created6: %v", err)
	}
	if err := r.Bound("ep-v6"); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	if err := r.Observed("ep-v6", lease.Event{Kind: lease.Acquired, Lease: held}, nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := OpenRecords(path, "instance-b")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	_, res, _, ok := reopened.Resume6(network, mac, now.Add(time.Minute))
	if !ok || res.Lease == nil {
		t.Fatalf("the record did not resume after a reopen (ok=%v)", ok)
	}
	info, _ := infoFromLease(*res.Lease, proto.RouterObservation{}, time.Now(), netip.Prefix{})
	if !reflect.DeepEqual(info.VendorInformation, wantResumedVendor17) {
		t.Errorf("the resumed lease reads option 17 as %v, want %v", info.VendorInformation, wantResumedVendor17)
	}
}

// A confirmed lease's bind event has no Reply bag; the remembered option 17 instances ride to the options line.
func TestTranslate_AResumedV6EventCarriesTheRememberedOption17(t *testing.T) {
	out, _ := translateV6EventIntoRecord(t, &lease.Lease{OptionsV6: resumedVendorOptions6()},
		lease.Lease{Addr: netip.MustParsePrefix("fd00:6470:6865::61/128")})
	if !reflect.DeepEqual(out.Data.VendorInformation, wantResumedVendor17) {
		t.Errorf("the bound event of a confirmed lease has VendorInformation %v, want the remembered %v",
			out.Data.VendorInformation, wantResumedVendor17)
	}
}
