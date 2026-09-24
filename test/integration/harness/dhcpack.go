// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import "strings"

// ACKedTo reports whether dnsmasq logged a DHCPACK of ip to mac, and returns every ACK seen for ip (#425).
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

// LastACKedAddress is the address the server last ACKed to mac, empty when none. A restarting container's address is
// claimed by the reservation and by the container's own client; measured on the pool: ACK .10 to the reservation,
// NAK then .11 to the client, Docker reporting .10 (#1047).
func LastACKedAddress(logData []byte, mac string) string {
	// dnsmasq spells the token one way; lines are lowercased before comparison, so it must be lowercase.
	addr, _ := lastACKAddressFrom(backendDnsmasq, string(logData), "dhcpack", mac)
	return addr
}
