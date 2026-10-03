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
	log "github.com/sirupsen/logrus"
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

// ntpOpt builds one option 56 instance from its sub-options (RFC 5908 section 4): code, 16-bit length, body.
func ntpOpt(subs ...[]byte) wire.OptionV6 {
	var d []byte
	for _, s := range subs {
		d = append(d, s...)
	}
	return wire.OptionV6{Code: wire.OptV6NTPServer, Data: d}
}

func ntpSub(code byte, body []byte) []byte {
	return append([]byte{0, code, 0, byte(len(body))}, body...)
}

func ntpAddrSub(code byte, addr string) []byte {
	a := netip.MustParseAddr(addr).As16()
	return ntpSub(code, a[:])
}

// ntpNameSub encodes a name as RFC 1035 labels with no compression (RFC 5908 section 4.3).
func ntpNameSub(labels ...string) []byte {
	var b []byte
	for _, l := range labels {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return ntpSub(3, append(b, 0))
}

// Two distinct addresses, so a reversed or deduplicated list shows in the assertion (#859).
func TestInfoFromLease_V6NTPServersKeepWireOrder(t *testing.T) {
	info, dropped := v6Info(t, wire.OptionsV6{
		ntpOpt(ntpAddrSub(1, "2001:db8::123")),
		ntpOpt(ntpAddrSub(1, "2001:db8::45")),
	})

	want := []string{"2001:db8::123", "2001:db8::45"}
	if !reflect.DeepEqual(info.NTPServers, want) || dropped != 0 {
		t.Errorf("NTPServers = %v (dropped %d), want %v in wire order", info.NTPServers, dropped, want)
	}
}

func TestInfoFromLease_V6NTPServersCarryUnicastMulticastAndName(t *testing.T) {
	info, _ := v6Info(t, wire.OptionsV6{
		ntpOpt(ntpAddrSub(1, "2001:db8::123")),
		ntpOpt(ntpNameSub("ntp", "example", "test")),
		ntpOpt(ntpAddrSub(2, "ff05::101")),
	})

	want := []string{"2001:db8::123", "ntp.example.test", "ff05::101"}
	if !reflect.DeepEqual(info.NTPServers, want) {
		t.Errorf("NTPServers = %v, want %v: an address as its string, a name as the name", info.NTPServers, want)
	}
}

func TestInfoFromLease_V6NTPNameWithControlCharacterIsDropped(t *testing.T) {
	info, dropped := v6Info(t, wire.OptionsV6{
		ntpOpt(ntpAddrSub(1, "2001:db8::123")),
		ntpOpt(ntpNameSub("ntp\nlevel=error msg=FORGED", "test")),
	})

	if !reflect.DeepEqual(info.NTPServers, []string{"2001:db8::123"}) || dropped != 1 {
		t.Errorf("NTPServers = %v (dropped %d), want the address kept and the forged name dropped", info.NTPServers, dropped)
	}
}

// dhcp-golib wire.OptionsV6.NTPServers returns nil and ErrMalformedNTP for the whole bag when any instance is bad, so
// a good instance beside it is lost too; the plugin keeps what the library returned and says so once (#859).
func TestInfoFromLease_V6MalformedNTPInstanceGivesNoListAndOneWarning(t *testing.T) {
	hook := captureLog(t)
	resetNTPWarnings(t)
	o := wire.OptionsV6{
		ntpOpt(ntpAddrSub(1, "2001:db8::123")),
		ntpOpt(ntpSub(1, []byte{1, 2, 3})),
	}

	var info Info
	for range 3 {
		info, _ = v6Info(t, o)
	}

	if len(info.NTPServers) != 0 {
		t.Errorf("NTPServers = %v, want none: the library returns no list beside ErrMalformedNTP", info.NTPServers)
	}
	warns := 0
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "#859") {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("%d warnings over three renderings of one lease, want exactly 1; the router-advert watch renders every 750 ms", warns)
	}
}

func TestInfoFromLease_V6NTPInstanceWithOnlyAnUnknownSubOptionIsSilent(t *testing.T) {
	hook := captureLog(t)
	resetNTPWarnings(t)

	info, _ := v6Info(t, wire.OptionsV6{
		ntpOpt(ntpSub(9, []byte{1, 2, 3})),
		ntpOpt(ntpAddrSub(1, "2001:db8::123")),
	})

	if !reflect.DeepEqual(info.NTPServers, []string{"2001:db8::123"}) {
		t.Errorf("NTPServers = %v, want the one readable instance", info.NTPServers)
	}
	for _, e := range hook.AllEntries() {
		if e.Level <= log.WarnLevel {
			t.Errorf("unexpected warning %q: the library drops such an instance without an error", e.Message)
		}
	}
}

func TestInfoFromLease_V4NTPServersAreUnchangedByTheV6Reader(t *testing.T) {
	l := lease.Lease{
		Addr:    netip.MustParsePrefix("192.168.99.10/24"),
		Options: wire.Options{wire.OptNTPServer: {192, 168, 99, 1, 192, 168, 99, 2}},
	}

	info, _ := infoFromLease(l, proto.RouterObservation{}, time.Now(), netip.Prefix{})

	want := []string{"192.168.99.1", "192.168.99.2"}
	if !reflect.DeepEqual(info.NTPServers, want) {
		t.Errorf("v4 NTPServers = %v, want %v", info.NTPServers, want)
	}
}

func resetNTPWarnings(t *testing.T) {
	t.Helper()
	ntpMalformedSeen.Lock()
	clear(ntpMalformedSeen.m)
	ntpMalformedSeen.Unlock()
}
