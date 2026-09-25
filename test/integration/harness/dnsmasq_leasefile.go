// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: a pure parser, driven in the unit job.

package harness

import (
	"net"
	"net/netip"
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

// DnsmasqLease6Name reads the name ("*" for none) on addr's v6 line, after the "duid" line (dnsmasq lease.c, #1029).
func DnsmasqLease6Name(leases, addr string) (string, bool) {
	want, err := netip.ParseAddr(addr)
	if err != nil || !want.Is6() {
		return "", false
	}
	v6 := false
	for _, line := range strings.Split(leases, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 && fields[0] == "duid" {
			v6 = true
			continue
		}
		if !v6 || len(fields) < 4 {
			continue
		}
		if got, err := netip.ParseAddr(fields[2]); err == nil && got == want {
			return fields[3], true
		}
	}
	return "", false
}
