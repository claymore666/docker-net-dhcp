// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import "testing"

// Real dnsmasq output, trimmed. The trailing token on an ACK is the
// client hostname when the client sent one — the static-IP container
// reached ACK without one on the run that exposed all this, which is
// why the reservation keys on the MAC.
const ackLog = `
Aug  1 15:36:42 dnsmasq-dhcp[5432]: 3202957726 DHCPDISCOVER(dh-itest-dhcp) 192.168.99.95 b6:53:0e:19:10:83
Aug  1 15:36:42 dnsmasq-dhcp[5432]: 3202957726 DHCPACK(dh-itest-dhcp) 192.168.99.95 b6:53:0e:19:10:83
Aug  1 15:37:03 dnsmasq-dhcp[5432]: 3836155040 DHCPOFFER(dh-itest-dhcp) 192.168.99.89 de:13:77:9c:ab:5c
Aug  1 15:37:03 dnsmasq-dhcp[5432]: 3836155040 DHCPACK(dh-itest-dhcp) 192.168.99.89 de:13:77:9c:ab:5c
`

func TestACKedTo(t *testing.T) {
	tests := []struct {
		name     string
		log      string
		ip, mac  string
		wantOK   bool
		wantACKs int
	}{
		{
			name: "the reserved MAC got the address",
			log:  ackLog, ip: "192.168.99.95", mac: "b6:53:0e:19:10:83",
			wantOK: true, wantACKs: 1,
		},
		{
			// The failure this assertion exists for: the address was
			// handed out, just not to us. Docker's view cannot see the
			// difference; the server's log can.
			name: "the address was ACKed to somebody else",
			log:  ackLog, ip: "192.168.99.95", mac: "02:00:00:00:99:95",
			wantOK: false, wantACKs: 1,
		},
		{
			name: "the address was never ACKed at all",
			log:  ackLog, ip: "192.168.99.42", mac: "02:00:00:00:99:95",
			wantOK: false, wantACKs: 0,
		},
		{
			// An unreadable or empty log must never read as success —
			// absent data is not evidence of the happy path.
			name: "an empty log is not a pass",
			log:  "", ip: "192.168.99.95", mac: "b6:53:0e:19:10:83",
			wantOK: false, wantACKs: 0,
		},
		{
			// A DISCOVER naming the address is not an ACK; only the
			// ACK says the server committed to it.
			name: "a DISCOVER for the address is not an ACK",
			log: "Aug  1 15:37:03 dnsmasq-dhcp[5432]: 1 DHCPDISCOVER(dh-itest-dhcp) " +
				"192.168.99.95 02:00:00:00:99:95\n",
			ip: "192.168.99.95", mac: "02:00:00:00:99:95",
			wantOK: false, wantACKs: 0,
		},
		{
			// .9 must not match .95 — a prefix match would let a
			// neighbouring lease vouch for this one.
			name: "a shorter address is not a prefix match",
			log:  "Aug  1 15:37:03 dnsmasq-dhcp[5432]: 1 DHCPACK(dh-itest-dhcp) 192.168.99.95 aa:bb:cc:dd:ee:ff\n",
			ip:   "192.168.99.9", mac: "aa:bb:cc:dd:ee:ff",
			wantOK: false, wantACKs: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, acks := ACKedTo([]byte(tc.log), tc.ip, tc.mac)
			if ok != tc.wantOK {
				t.Errorf("ACKedTo(_, %q, %q) ok = %v, want %v", tc.ip, tc.mac, ok, tc.wantOK)
			}
			if len(acks) != tc.wantACKs {
				t.Errorf("ACKedTo(_, %q, %q) returned %d ACK line(s), want %d: %v",
					tc.ip, tc.mac, len(acks), tc.wantACKs, acks)
			}
		})
	}
}
