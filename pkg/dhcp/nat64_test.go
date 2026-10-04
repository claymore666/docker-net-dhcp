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
)

func TestNAT64Prefixes_RendersCanonicalIPv6CIDROnly(t *testing.T) {
	got := nat64Prefixes(proto.RouterObservation{PREF64: []netip.Prefix{
		pfx(t, "64:ff9b::/96"),
		pfx(t, "2001:db8:64::7/64"),
		pfx(t, "64:ff9b::/96"),
		pfx(t, "192.0.2.0/24"),
		pfx(t, "::ffff:192.0.2.0/120"),
		{},
		netip.PrefixFrom(netip.MustParseAddr("2001:db8:65::"), 200),
	}})
	want := []string{"64:ff9b::/96", "2001:db8:64::/64"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nat64Prefixes = %v, want %v", got, want)
	}
	if got := nat64Prefixes(proto.RouterObservation{}); got != nil {
		t.Errorf("an observation with no PREF64 gave %v, want nil", got)
	}
}

func TestInfoFromLease_CarriesThePREF64Prefixes(t *testing.T) {
	r := proto.RouterObservation{Seen: true, PREF64: []netip.Prefix{pfx(t, "64:ff9b::/96")}}
	info, _ := infoFromLease(lease.Lease{}, r, time.Now(), netip.Prefix{})
	if want := []string{"64:ff9b::/96"}; !reflect.DeepEqual(info.NAT64Prefixes, want) {
		t.Errorf("Info.NAT64Prefixes = %v, want %v", info.NAT64Prefixes, want)
	}
	info, _ = infoFromLease(lease.Lease{}, proto.RouterObservation{Seen: true}, time.Now(), netip.Prefix{})
	if len(info.NAT64Prefixes) != 0 {
		t.Errorf("Info.NAT64Prefixes = %v for an advertisement without PREF64, want none", info.NAT64Prefixes)
	}
}

func TestTakeAdvertChange_TheEventCarriesPREF64(t *testing.T) {
	c, ra := advertWatch(t)
	ra.PREF64 = []netip.Prefix{pfx(t, "64:ff9b::/96")}
	c.takeAdvertChange(time.Now())

	ra.MTU = 1280
	ev, ok := c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("a lowered MTU was not reported")
	}
	if want := []string{"64:ff9b::/96"}; !reflect.DeepEqual(ev.Data.NAT64Prefixes, want) {
		t.Errorf("routeradvert Info.NAT64Prefixes = %v, want %v", ev.Data.NAT64Prefixes, want)
	}
}

// The bound the docs state (#1028).
func TestTakeAdvertChange_APREF64OnlyChangeIsNotAnEvent(t *testing.T) {
	c, ra := advertWatch(t)
	c.takeAdvertChange(time.Now())

	ra.PREF64 = []netip.Prefix{pfx(t, "64:ff9b::/96")}
	if ev, ok := c.takeAdvertChange(time.Now()); ok {
		t.Fatalf("a PREF64-only change produced a routeradvert event: %+v", ev.Data)
	}
}
