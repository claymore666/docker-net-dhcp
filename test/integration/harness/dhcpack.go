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

// LastACKedAddress is ACKedTo's other half: not "was this address ever
// handed to this client" but "which address does the server say this
// client holds NOW".
//
// THE DIFFERENCE IS A WHOLE CLASS OF DEFECT. A restarting container's
// address is claimed twice, once by the reservation and once by the
// container's own client, and an assertion that some ACK exists is
// satisfied by the first of the two even when the second was NAKed and
// handed a different address. That is exactly how an address that
// moved on the wire read as green while Docker went on publishing the
// address the reservation was given: measured on the pool, ACK .10 to
// the reservation, NAK then .11 to the client, Docker reporting .10.
//
// Empty when the server never ACKed this client, which a caller must
// treat as a failure and never as a pass: absent data is not evidence.
func LastACKedAddress(logData []byte, mac string) string {
	// The token is the spelling the reader looks for, lowercased: the
	// line is lowercased before the comparison, so a token with a
	// capital in it matches nothing at all and every caller reads "this
	// client was never ACKed". dnsmasq spells it one way, which is why
	// this is a literal here and a per-log choice on the ephemeral
	// fixture.
	addr, _ := lastACKAddressFrom(backendDnsmasq, string(logData), "dhcpack", mac)
	return addr
}
