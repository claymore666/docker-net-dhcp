// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"testing"
	"time"
)

func TestDnsmasqLeaseExpiry_ReadsTheLineOfThatMAC(t *testing.T) {
	const leases = "1790000000 aa:bb:cc:dd:ee:ff 192.168.97.20 stolen-by *\n" +
		"1790000123 02:42:C0:A8:61:0B 192.168.97.21 dh-itest-bind2-ctr 01:02:42:c0:a8:61:0b\n"
	for _, tc := range []struct {
		mac    string
		want   time.Time
		wantOK bool
	}{
		{"02:42:c0:a8:61:0b", time.Unix(1790000123, 0), true},
		{"aa:bb:cc:dd:ee:ff", time.Unix(1790000000, 0), true},
		{"02:42:c0:a8:61:0c", time.Time{}, false},
		{"not-a-mac", time.Time{}, false},
	} {
		got, ok := DnsmasqLeaseExpiry(leases, tc.mac)
		if ok != tc.wantOK || !got.Equal(tc.want) {
			t.Errorf("DnsmasqLeaseExpiry(%s) = %v, %v; want %v, %v", tc.mac, got, ok, tc.want, tc.wantOK)
		}
	}
	if _, ok := DnsmasqLeaseExpiry("never 02:42:c0:a8:61:0b 192.168.97.21 x *\n", "02:42:c0:a8:61:0b"); ok {
		t.Error("a line whose expiry is not a number was read as a lease")
	}
}

func TestDnsmasqLease6Name_ReadsTheV6LineOfThatAddress(t *testing.T) {
	const leases = "1790000000 02:42:c0:a8:67:0b 192.168.103.11 web1 01:02:42:c0:a8:67:0b\n" +
		"duid 00:01:00:01:30:00:00:01:02:42:c0:a8:67:01\n" +
		"1790000100 3232261899 fd00:6470:6865::61 web1 00:03:00:01:02:42:c0:a8:67:0b\n" +
		"1790000200 3232261900 fd00:6470:6865::62 * 00:03:00:01:02:42:c0:a8:67:0c\n" +
		"1790000300 T77 fd00:6470:6865::63 temp 00:03:00:01:02:42:c0:a8:67:0d\n"
	for _, tc := range []struct {
		addr   string
		want   string
		wantOK bool
	}{
		{"fd00:6470:6865::61", "web1", true},
		{"fd00:6470:6865:0::62", "*", true},
		{"fd00:6470:6865::63", "temp", true},
		{"fd00:6470:6865::64", "", false},
		{"192.168.103.11", "", false},
		{"not-an-address", "", false},
	} {
		got, ok := DnsmasqLease6Name(leases, tc.addr)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("DnsmasqLease6Name(%s) = %q, %v; want %q, %v", tc.addr, got, ok, tc.want, tc.wantOK)
		}
	}
	if _, ok := DnsmasqLease6Name("1 3232261899 fd00:6470:6865::61 web1 *\n", "fd00:6470:6865::61"); ok {
		t.Error("a line before the duid line was read as a v6 lease")
	}
}
