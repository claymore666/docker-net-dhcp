// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import "testing"

// Real dnsmasq output, trimmed: the trailing token on an ACK is the client hostname only when the client sent one.
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
			name: "an empty log is not a pass",
			log:  "", ip: "192.168.99.95", mac: "b6:53:0e:19:10:83",
			wantOK: false, wantACKs: 0,
		},
		{
			name: "a DISCOVER for the address is not an ACK",
			log: "Aug  1 15:37:03 dnsmasq-dhcp[5432]: 1 DHCPDISCOVER(dh-itest-dhcp) " +
				"192.168.99.95 02:00:00:00:99:95\n",
			ip: "192.168.99.95", mac: "02:00:00:00:99:95",
			wantOK: false, wantACKs: 0,
		},
		{
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

// A restart on the standing fixture as dnsmasq logs it: the reservation's exchange under the removed container's
// identity, then the container's own client refused and taking another address (#1047).
const restartAckLog = `
Sep 21 15:16:02 dnsmasq-dhcp[5432]: 1 DHCPREQUEST(dh-itest) 192.168.99.10 ea:a9:52:1b:95:ab
Sep 21 15:16:02 dnsmasq-dhcp[5432]: 1 DHCPACK(dh-itest) 192.168.99.10 ea:a9:52:1b:95:ab
Sep 21 15:16:10 dnsmasq-dhcp[5432]: 2 DHCPREQUEST(dh-itest) 192.168.99.10 ea:a9:52:1b:95:ab
Sep 21 15:16:10 dnsmasq-dhcp[5432]: 2 DHCPNAK(dh-itest) 192.168.99.10 ea:a9:52:1b:95:ab
Sep 21 15:16:10 dnsmasq-dhcp[5432]: 3 DHCPACK(dh-itest) 192.168.99.11 ea:a9:52:1b:95:ab
`

func TestLastACKedAddress(t *testing.T) {
	tests := []struct {
		name string
		log  string
		mac  string
		want string
	}{
		{
			name: "the newest ACK wins over an older one for the same client",
			log:  restartAckLog, mac: "ea:a9:52:1b:95:ab",
			want: "192.168.99.11",
		},
		{
			name: "one ACK is its own answer",
			log:  ackLog, mac: "b6:53:0e:19:10:83",
			want: "192.168.99.95",
		},
		{
			name: "a client the server never ACKed has no address",
			log:  ackLog, mac: "02:00:00:00:99:95",
			want: "",
		},
		{
			name: "an empty log is not an address",
			log:  "", mac: "b6:53:0e:19:10:83",
			want: "",
		},
		{
			name: "a NAK is not an ACK",
			log: "Sep 21 15:16:10 dnsmasq-dhcp[5432]: 2 DHCPNAK(dh-itest) 192.168.99.10 " +
				"ea:a9:52:1b:95:ab\n",
			mac:  "ea:a9:52:1b:95:ab",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LastACKedAddress([]byte(tc.log), tc.mac); got != tc.want {
				t.Errorf("LastACKedAddress(_, %q) = %q, want %q", tc.mac, got, tc.want)
			}
		})
	}
}
