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
