// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

func v6Lease(o wire.OptionsV6) lease.Lease {
	return lease.Lease{
		Addr:      netip.MustParsePrefix("2001:db8:1::42/128"),
		OptionsV6: o,
	}
}

func v6Info(t *testing.T, o wire.OptionsV6) (Info, int) {
	t.Helper()
	return infoFromLease(v6Lease(o), proto.RouterObservation{}, time.Now(), netip.Prefix{})
}

// Distinct strings per option, so a swapped pair of fields shows in the assertion (#1033).
func TestInfoFromLease_V6TimezonesAreRecorded(t *testing.T) {
	info, dropped := v6Info(t, wire.OptionsV6{
		{Code: wire.OptV6PosixTimezone, Data: []byte("CET-1CEST,M3.5.0,M10.5.0/3")},
		{Code: wire.OptV6TZDatabase, Data: []byte("Europe/Berlin")},
	})

	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if info.PosixTimezone != "CET-1CEST,M3.5.0,M10.5.0/3" {
		t.Errorf("PosixTimezone = %q, want option 41's string", info.PosixTimezone)
	}
	if info.TZDBTimezone != "Europe/Berlin" {
		t.Errorf("TZDBTimezone = %q, want option 42's string", info.TZDBTimezone)
	}
	if info.IP != "2001:db8:1::42/128" {
		t.Errorf("Info.IP = %q; the lease was lost along with the options", info.IP)
	}
}

func TestInfoFromLease_V4TimezonesAreUnchangedByTheV6Reader(t *testing.T) {
	l := lease.Lease{
		Addr: netip.MustParsePrefix("192.168.99.10/24"),
		Options: wire.Options{
			wire.OptPosixTimezone: []byte("PST8PDT"),
			wire.OptTZDatabase:    []byte("America/Los_Angeles"),
		},
	}

	info, dropped := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	if dropped != 0 || info.PosixTimezone != "PST8PDT" || info.TZDBTimezone != "America/Los_Angeles" {
		t.Errorf("v4 timezones = %q / %q (dropped %d), want options 100 and 101 as before",
			info.PosixTimezone, info.TZDBTimezone, dropped)
	}
}

// A v4 lease never carries OptionsV6 (nil), but a stray one must not leak into a v4 Info (#1033).
func TestInfoFromLease_V4LeaseIgnoresV6OptionBag(t *testing.T) {
	l := lease.Lease{
		Addr:      netip.MustParsePrefix("192.168.99.10/24"),
		Options:   wire.Options{wire.OptPosixTimezone: []byte("v4-wins")},
		OptionsV6: wire.OptionsV6{{Code: wire.OptV6PosixTimezone, Data: []byte("v6-loses")}},
	}

	info, _ := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	if info.PosixTimezone != "v4-wins" {
		t.Errorf("PosixTimezone = %q, want the v4 option 100 value", info.PosixTimezone)
	}
}

func TestInfoFromLease_V6WithoutTheOptionsLeavesThemEmpty(t *testing.T) {
	for name, o := range map[string]wire.OptionsV6{
		"nil bag (a lease resumed from its record)": nil,
		"bag with other options only": {
			{Code: wire.OptV6DNSServers, Data: make([]byte, 16)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			info, dropped := v6Info(t, o)

			if info.PosixTimezone != "" || info.TZDBTimezone != "" || len(info.NTPServers) != 0 || dropped != 0 {
				t.Errorf("timezones %q/%q ntp %v dropped %d, want all empty so no options line is logged",
					info.PosixTimezone, info.TZDBTimezone, info.NTPServers, dropped)
			}
		})
	}
}

// Options 41 and 42 go through the sanitiser exactly as 100 and 101 do: whatever v4 does with a byte string, v6 does
// the same, so a divergence in either direction fails here (#1033).
func TestInfoFromLease_V6TimezoneStringsMatchTheV4Sanitiser(t *testing.T) {
	for name, raw := range map[string]string{
		"clean":             "Europe/Berlin",
		"trailing NUL":      "Europe/Berlin\x00",
		"two trailing NULs": "Europe/Berlin\x00\x00",
		"interior NUL":      "Europe/\x00Berlin",
		"newline":           "CET\nlevel=error msg=FORGED",
		"control byte":      "CET\x01",
		"DEL":               "CET\x7f",
		"invalid UTF-8":     "Europe/\xffBerlin",
		"empty":             "",
		"only NUL":          "\x00",
	} {
		t.Run(name, func(t *testing.T) {
			v4 := lease.Lease{
				Addr: netip.MustParsePrefix("192.168.99.10/24"),
				Options: wire.Options{
					wire.OptPosixTimezone: []byte(raw),
					wire.OptTZDatabase:    []byte(raw),
				},
			}
			want, wantDropped := infoFromLease(v4, proto.RouterObservation{}, time.Now(), netip.Prefix{})

			got, gotDropped := v6Info(t, wire.OptionsV6{
				{Code: wire.OptV6PosixTimezone, Data: []byte(raw)},
				{Code: wire.OptV6TZDatabase, Data: []byte(raw)},
			})

			if got.PosixTimezone != want.PosixTimezone || got.TZDBTimezone != want.TZDBTimezone || gotDropped != wantDropped {
				t.Errorf("v6 gave %q/%q (dropped %d), the v4 sanitiser gave %q/%q (dropped %d)",
					got.PosixTimezone, got.TZDBTimezone, gotDropped, want.PosixTimezone, want.TZDBTimezone, wantDropped)
			}
		})
	}
}

// The control-character cases must really drop, or the differential test above could pass with both sides leaking.
func TestInfoFromLease_V6TimezoneWithControlCharactersIsDropped(t *testing.T) {
	info, dropped := v6Info(t, wire.OptionsV6{
		{Code: wire.OptV6PosixTimezone, Data: []byte("CET\nlevel=error msg=FORGED")},
		{Code: wire.OptV6TZDatabase, Data: []byte("Europe/\x00Berlin")},
	})

	if info.PosixTimezone != "" || info.TZDBTimezone != "" {
		t.Errorf("timezones %q / %q reached the Info, want both dropped", info.PosixTimezone, info.TZDBTimezone)
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2 (feeds unsafe_option_values_dropped)", dropped)
	}
}

func TestInfoFromLease_V6TrailingNULIsStrippedLikeV4(t *testing.T) {
	info, dropped := v6Info(t, wire.OptionsV6{{Code: wire.OptV6TZDatabase, Data: []byte("Europe/Berlin\x00")}})

	if info.TZDBTimezone != "Europe/Berlin" || dropped != 0 {
		t.Errorf("TZDBTimezone = %q (dropped %d), want the name without its terminator", info.TZDBTimezone, dropped)
	}
}
