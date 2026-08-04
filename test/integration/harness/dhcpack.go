// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import "strings"

// ACKedTo reports whether dnsmasq's log records a DHCPACK handing ip to
// mac, and returns every ACK seen for ip so a caller can show what
// happened instead when it didn't.
//
// This exists because Docker's endpoint view cannot distinguish "the
// server reserved this address for us" from "the address happened to be
// free". TestStaticIP_DriverOpt lived its whole life on the second and
// looked identical to the first, until it drew .89 and .12 on two runs
// of a commit that had already passed three times.
func ACKedTo(logData []byte, ip, mac string) (bool, []string) {
	var acks []string
	for _, line := range strings.Split(string(logData), "\n") {
		if !strings.Contains(line, "DHCPACK") || !strings.Contains(line, ip+" ") {
			continue
		}
		acks = append(acks, strings.TrimSpace(line))
		if strings.Contains(line, mac) {
			return true, acks
		}
	}
	return false, acks
}
