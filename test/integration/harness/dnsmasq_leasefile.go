// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: a pure parser, driven in the unit job.

package harness

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// DnsmasqLeaseExpiry returns the expiry dnsmasq recorded for mac. dnsmasq(8) writes one lease per line as
// "expiry MAC IP hostname client-id", expiry in Unix seconds, and rewrites the line on every grant (#1089).
func DnsmasqLeaseExpiry(leases, mac string) (time.Time, bool) {
	want, err := net.ParseMAC(mac)
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(leases, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		got, err := net.ParseMAC(fields[1])
		if err != nil || got.String() != want.String() {
			continue
		}
		secs, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(secs, 0), true
	}
	return time.Time{}, false
}
