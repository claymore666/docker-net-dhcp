// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Real kea-dhcp4 2.6.3 INFO output captured on the integration runner image (#356), kept verbatim.
const keaACKLog = `
2026-08-02 09:53:52.326 INFO  [kea-dhcp4.packets/17.140146479474368] DHCP4_PACKET_RECEIVED [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x517eb529: DHCPDISCOVER (type 1) received from 0.0.0.0 to 255.255.255.255 on interface dh-itest-edhcp
2026-08-02 09:53:52.326 INFO  [kea-dhcp4.packets/17.140146479474368] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x517eb529: trying to send packet DHCPOFFER (type 2) from 192.168.101.1:67 to 192.168.101.10:68 on interface dh-itest-edhcp
2026-08-02 09:53:52.326 INFO  [kea-dhcp4.leases/17.140146471081664] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x517eb529: lease 192.168.101.10 has been allocated for 20 seconds
2026-08-02 09:53:52.326 INFO  [kea-dhcp4.packets/17.140146471081664] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x517eb529: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 192.168.101.10:68 on interface dh-itest-edhcp
2026-08-02 09:54:02.136 INFO  [kea-dhcp4.packets/17.140146462688960] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x1cc9f77f: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 192.168.101.11:68 on interface dh-itest-edhcp
2026-08-02 09:54:02.136 INFO  [kea-dhcp4.packets/17.140146462688960] DHCP4_PACKET_SEND [hwtype=1 aa:bb:cc:dd:ee:ff], cid=[00], tid=0x1cc9f780: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 192.168.101.42:68 on interface dh-itest-edhcp
`

const keaMAC = "02:11:22:33:44:55"

func newLogFixture(t *testing.T, backend ephemeralBackend, log string) *EphemeralFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dhcp-server.log")
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return &EphemeralFixture{t: t, backend: backend, tmpDir: dir, logFile: path}
}

func TestAckAddress_Kea(t *testing.T) {
	const line = `2026-08-02 09:53:52.326 INFO  [kea-dhcp4.packets/17.1401] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 192.168.101.10:68 on interface dh-itest-edhcp`
	if got := ackAddress(backendKea, line); got != "192.168.101.10" {
		t.Errorf("ackAddress(kea) = %q, want the ACK recipient 192.168.101.10", got)
	}
	// Kea names its own address first on the line, as the `from`.
	if got := ackAddress(backendKea, line); got == "192.168.101.1" {
		t.Error("ackAddress(kea) returned the server's own address; it must return the recipient, not the sender")
	}
}

func TestAckAddress_Dnsmasq(t *testing.T) {
	const line = `Aug  1 15:36:42 dnsmasq-dhcp[5432]: 3202957726 DHCPACK(dh-itest-dhcp) 192.168.99.95 b6:53:0e:19:10:83 mycontainer`
	if got := ackAddress(backendDnsmasq, line); got != "192.168.99.95" {
		t.Errorf("ackAddress(dnsmasq) = %q, want 192.168.99.95", got)
	}
}

func TestLastACKAddress_KeaTracksLatest(t *testing.T) {
	ef := newLogFixture(t, backendKea, keaACKLog)
	if got := ef.LastACKAddress(keaMAC); got != "192.168.101.11" {
		t.Errorf("LastACKAddress = %q, want 192.168.101.11 (the most recent ACK for this MAC)", got)
	}
	if got := ef.LastACKAddress("aa:bb:cc:dd:ee:ff"); got != "192.168.101.42" {
		t.Errorf("LastACKAddress(other MAC) = %q, want 192.168.101.42; ACKs must be attributed per client", got)
	}
	if got := ef.LastACKAddress("de:ad:be:ef:00:00"); got != "" {
		t.Errorf("LastACKAddress(unknown MAC) = %q, want \"\"", got)
	}
}

func TestKeaLeaseAllocSeconds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		line   string
		want   int
		wantOK bool
	}{
		{
			name:   "the captured allocation line",
			line:   `2026-08-02 09:53:52.326 INFO  [kea-dhcp4.leases/17.140146471081664] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x517eb529: lease 192.168.101.10 has been allocated for 20 seconds`,
			want:   20,
			wantOK: true,
		},
		{
			name:   "the fixture default lifetime",
			line:   `2026-08-02 09:53:52.326 INFO  [kea-dhcp4.leases/17.1401] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: lease 192.168.101.10 has been allocated for 120 seconds`,
			want:   120,
			wantOK: true,
		},
		{
			name:   "an ACK line carries no lifetime",
			line:   `2026-08-02 09:53:52.326 INFO  [kea-dhcp4.packets/17.1401] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 192.168.101.10:68 on interface dh-itest-edhcp`,
			wantOK: false,
		},
		{
			name:   "a lifetime in another unit does not parse as seconds",
			line:   `2026-08-02 09:53:52.326 INFO  [kea-dhcp4.leases/17.1401] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: lease 192.168.101.10 has been allocated for 2 minutes`,
			wantOK: false,
		},
		{
			name:   "a reworded allocation message reports no reading",
			line:   `2026-08-02 09:53:52.326 INFO  [kea-dhcp4.leases/17.1401] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: lease 192.168.101.10 has been allocated`,
			wantOK: false,
		},
		{
			name:   "an empty line is not an allocation",
			line:   "",
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := keaLeaseAllocSeconds(tc.line)
			if ok != tc.wantOK {
				t.Fatalf("keaLeaseAllocSeconds ok = %v, want %v (line: %s)", ok, tc.wantOK, tc.line)
			}
			if ok && got != tc.want {
				t.Errorf("keaLeaseAllocSeconds = %d, want %d", got, tc.want)
			}
			if !ok && got != 0 {
				t.Errorf("keaLeaseAllocSeconds returned %d alongside ok=false; a no-match must not carry a number", got)
			}
		})
	}
}

// keaGrantLog is keaACKLog's allocation line repeated with only the MAC, address and lifetime varied.
const keaGrantLog = `
2026-08-02 09:53:52.326 INFO  [kea-dhcp4.leases/17.140146471081664] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55], tid=0x517eb529: lease 192.168.101.10 has been allocated for 20 seconds
2026-08-02 09:53:53.001 INFO  [kea-dhcp4.leases/17.140146471081664] DHCP4_LEASE_ALLOC [hwtype=1 aa:bb:cc:dd:ee:ff], cid=[00], tid=0x1cc9f780: lease 192.168.101.42 has been allocated for 20 seconds
2026-08-02 09:54:02.136 INFO  [kea-dhcp4.leases/17.140146462688960] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55], tid=0x1cc9f77f: lease 192.168.101.11 has been allocated for 20 seconds
`

func TestGrantedLease_Kea(t *testing.T) {
	ef := newLogFixture(t, backendKea, keaGrantLog)

	if got, ok := ef.GrantedLease(keaMAC); !ok || got != 20 {
		t.Errorf("GrantedLease(%s) = %d, %v; want 20, true", keaMAC, got, ok)
	}
	if got, ok := ef.GrantedLease("aa:bb:cc:dd:ee:ff"); !ok || got != 20 {
		t.Errorf("GrantedLease(other MAC) = %d, %v; want 20, true — grants must be attributed per client", got, ok)
	}
	if got, ok := ef.GrantedLease("de:ad:be:ef:00:00"); ok || got != 0 {
		t.Errorf("GrantedLease(unknown MAC) = %d, %v; want 0, false", got, ok)
	}
}

func TestKeaLeaseGrants_ReadsEveryAllocation(t *testing.T) {
	grants := keaLeaseGrants(keaGrantLog)
	if len(grants) != 3 {
		t.Fatalf("keaLeaseGrants found %d allocation(s), want 3", len(grants))
	}
	for i, g := range grants {
		if g.seconds != 20 {
			t.Errorf("grant %d = %ds, want 20s", i, g.seconds)
		}
		if !strings.Contains(g.line, "DHCP4_LEASE_ALLOC") {
			t.Errorf("grant %d kept no evidence line: %q", i, g.line)
		}
	}
	if got := len(keaLeaseGrants(keaACKLog)); got != 1 {
		t.Errorf("keaLeaseGrants(keaACKLog) found %d, want 1; only DHCP4_LEASE_ALLOC states a lifetime", got)
	}
	if got := len(keaLeaseGrants("")); got != 0 {
		t.Errorf("keaLeaseGrants(empty) found %d, want 0", got)
	}
}

func TestCheckLeaseGrants(t *testing.T) {
	grant := func(seconds int) keaLeaseGrant {
		return keaLeaseGrant{
			line:    "DHCP4_LEASE_ALLOC ...: lease 192.168.101.10 has been allocated for " + strconv.Itoa(seconds) + " seconds",
			seconds: seconds,
		}
	}

	t.Run("granted equals asked", func(t *testing.T) {
		if got := checkLeaseGrants([]keaLeaseGrant{grant(20), grant(20)}, 20); len(got) != 0 {
			t.Errorf("checkLeaseGrants reported %v on a clean run; it must be silent", got)
		}
	})

	t.Run("a clamped lifetime reports both numbers", func(t *testing.T) {
		// The #472 shape: the fixture asks for 20 s and the server silently serves 60 s.
		got := checkLeaseGrants([]keaLeaseGrant{grant(60)}, 20)
		if len(got) != 1 {
			t.Fatalf("checkLeaseGrants reported %d problem(s), want 1: %v", len(got), got)
		}
		for _, want := range []string{"20s", "60s"} {
			if !strings.Contains(got[0], want) {
				t.Errorf("problem does not name %s, so it cannot be acted on:\n%s", want, got[0])
			}
		}
		if !strings.Contains(got[0], "DHCP4_LEASE_ALLOC") {
			t.Errorf("problem does not quote the server's own line:\n%s", got[0])
		}
	})

	t.Run("a lifetime that drifts mid-run is not averaged away", func(t *testing.T) {
		got := checkLeaseGrants([]keaLeaseGrant{grant(20), grant(60), grant(90)}, 20)
		if len(got) != 2 {
			t.Fatalf("checkLeaseGrants reported %d problem(s), want 2 (first offender + tally): %v", len(got), got)
		}
		if !strings.Contains(got[1], "2 of 3") {
			t.Errorf("tally does not say how many of how many:\n%s", got[1])
		}
	})

	t.Run("no allocation at all is a failure, not a pass", func(t *testing.T) {
		got := checkLeaseGrants(nil, 20)
		if len(got) != 1 {
			t.Fatalf("checkLeaseGrants(no grants) reported %d problem(s), want 1: %v", len(got), got)
		}
		if !strings.Contains(got[0], "DHCP4_LEASE_ALLOC") {
			t.Errorf("problem does not say which line was looked for:\n%s", got[0])
		}
	})
}

// Kea logs OFFER and ACK through the same DHCP4_PACKET_SEND message, and at DEBUG DHCP4_RESPONSE_DATA repeats "DHCPACK".
func TestCountLogLines_KeaCountsAcksNotOffers(t *testing.T) {
	ef := newLogFixture(t, backendKea, keaACKLog)
	if got := ef.CountLogLines("DHCPACK", keaMAC); got != 2 {
		t.Errorf("CountLogLines(DHCPACK, %s) = %d, want 2", keaMAC, got)
	}
	if got := ef.CountLogLines("DHCPOFFER", keaMAC); got != 1 {
		t.Errorf("CountLogLines(DHCPOFFER, %s) = %d, want 1", keaMAC, got)
	}
	if got := ef.CountLogLines("DHCPACK", "aa:bb:cc:dd:ee:ff"); got != 1 {
		t.Errorf("CountLogLines(DHCPACK, other MAC) = %d, want 1", got)
	}
}

func TestKeaConfig_IsValidJSON(t *testing.T) {
	for _, tc := range []struct {
		name     string
		t1, t2   int
		wantKeys []string
	}{
		{name: "no timers", wantKeys: nil},
		{name: "t1 only", t1: 12, wantKeys: []string{"renew-timer"}},
		{name: "both", t1: 12, t2: 25, wantKeys: []string{"renew-timer", "rebind-timer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ef := &EphemeralFixture{
				t:            t,
				backend:      backendKea,
				poolStart:    EphemeralPoolStart,
				poolEnd:      EphemeralPoolEnd,
				serverCIDR:   EphemeralServerAddr,
				leaseSeconds: EphemeralOutageLeaseSeconds,
				leaseFile:    "/tmp/leases4.csv",
				renewT1:      tc.t1,
				renewT2:      tc.t2,
			}
			raw := ef.keaConfig(keaLoggerOutputModern)
			var cfg struct {
				Dhcp4 struct {
					ValidLifetime int `json:"valid-lifetime"`
					RenewTimer    int `json:"renew-timer"`
					RebindTimer   int `json:"rebind-timer"`
					Authoritative bool
					Subnet4       []struct {
						Subnet string
						Pools  []struct{ Pool string }
					}
				}
			}
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				t.Fatalf("keaConfig is not valid JSON: %v\n%s", err, raw)
			}
			if cfg.Dhcp4.ValidLifetime != EphemeralOutageLeaseSeconds {
				t.Errorf("valid-lifetime = %d, want %d", cfg.Dhcp4.ValidLifetime, EphemeralOutageLeaseSeconds)
			}
			if cfg.Dhcp4.RenewTimer != tc.t1 {
				t.Errorf("renew-timer = %d, want %d", cfg.Dhcp4.RenewTimer, tc.t1)
			}
			if cfg.Dhcp4.RebindTimer != tc.t2 {
				t.Errorf("rebind-timer = %d, want %d", cfg.Dhcp4.RebindTimer, tc.t2)
			}
			// Authoritative makes Kea refuse an unknown REQUEST instead of staying silent (#356).
			if !cfg.Dhcp4.Authoritative {
				t.Error("authoritative is false; the failure tests need a server that owns its subnet")
			}
			if len(cfg.Dhcp4.Subnet4) != 1 {
				t.Fatalf("want exactly one subnet4 entry, got %d", len(cfg.Dhcp4.Subnet4))
			}
			// Kea rejects a subnet4 whose pool falls outside it, so the subnet must be the network address.
			if got := cfg.Dhcp4.Subnet4[0].Subnet; got != "192.168.101.0/24" {
				t.Errorf("subnet = %q, want the network 192.168.101.0/24", got)
			}
			if got := cfg.Dhcp4.Subnet4[0].Pools[0].Pool; !strings.Contains(got, EphemeralPoolStart) {
				t.Errorf("pool = %q, want it to start at %s", got, EphemeralPoolStart)
			}
			for _, k := range tc.wantKeys {
				if !strings.Contains(raw, k) {
					t.Errorf("config is missing %q:\n%s", k, raw)
				}
			}
		})
	}
}

// A lease row whose subnet_id matches no configured subnet loads into nothing, silently (#356).
func TestSeedStolenLease_Kea(t *testing.T) {
	dir := t.TempDir()
	ef := &EphemeralFixture{
		t:         t,
		backend:   backendKea,
		tmpDir:    dir,
		leaseFile: filepath.Join(dir, "leases4.csv"),
	}
	ef.SeedStolenLease("192.168.101.10")

	data, err := os.ReadFile(ef.leaseFile)
	if err != nil {
		t.Fatalf("read seeded lease DB: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want header + one lease row, got %d line(s):\n%s", len(lines), data)
	}
	if lines[0] != keaLeaseCSVHeader {
		t.Errorf("header = %q, want the schema kea writes", lines[0])
	}
	cols := strings.Split(lines[1], ",")
	if len(cols) != len(strings.Split(keaLeaseCSVHeader, ",")) {
		t.Fatalf("lease row has %d columns, header has %d:\n%s",
			len(cols), len(strings.Split(keaLeaseCSVHeader, ",")), lines[1])
	}
	if cols[0] != "192.168.101.10" {
		t.Errorf("seeded address = %q, want 192.168.101.10", cols[0])
	}
	if cols[1] == "" {
		t.Error("seeded lease has no hwaddr; it must belong to a FOREIGN client for the address to read as taken")
	}
	if cols[5] != "1" {
		t.Errorf("subnet_id = %q, want 1 to match keaConfig's subnet4 id; a mismatch seeds nothing silently", cols[5])
	}
}

// Kea 2.5.4 renamed the logger key output_options to output-options, and Debian and Ubuntu stable ship 2.4.x; the
// hosted lane failed with `got unexpected keyword "output-options" in loggers map` (#612).
func TestKeaConfig_LoggerOutputKeyIsSubstituted(t *testing.T) {
	for _, key := range []string{keaLoggerOutputModern, keaLoggerOutputLegacy} {
		t.Run(key, func(t *testing.T) {
			ef := &EphemeralFixture{
				t:            t,
				backend:      backendKea,
				poolStart:    EphemeralPoolStart,
				poolEnd:      EphemeralPoolEnd,
				serverCIDR:   EphemeralServerAddr,
				leaseSeconds: EphemeralOutageLeaseSeconds,
				leaseFile:    "/tmp/leases4.csv",
			}
			raw := ef.keaConfig(key)

			var cfg struct {
				Dhcp4 struct {
					Loggers []map[string]any
				}
			}
			if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
				t.Fatalf("keaConfig(%q) is not valid JSON: %v\n%s", key, err, raw)
			}
			if len(cfg.Dhcp4.Loggers) != 1 {
				t.Fatalf("want exactly one logger, got %d", len(cfg.Dhcp4.Loggers))
			}
			if _, ok := cfg.Dhcp4.Loggers[0][key]; !ok {
				t.Errorf("logger has no %q key; got keys %v", key, cfg.Dhcp4.Loggers[0])
			}

			// Kea rejects a logger key it does not know, so a config carrying both spellings fails on every version.
			other := keaLoggerOutputLegacy
			if key == keaLoggerOutputLegacy {
				other = keaLoggerOutputModern
			}
			if _, ok := cfg.Dhcp4.Loggers[0][other]; ok {
				t.Errorf("logger also carries %q; kea rejects the spelling it does not know", other)
			}
		})
	}
}

// Real kea-dhcp4 2.4.1 INFO output, as Ubuntu 24.04 ships, from the hosted lane on 2026-08-18 (#612). This version
// writes no DHCPACK line: every bind and renewal is a DHCP4_LEASE_ALLOC.
const kea24Log = `
2026-08-18 16:18:35.302 INFO  [kea-dhcp4.leases/17955.139959131666112] DHCP4_LEASE_ADVERT [hwtype=1 e6:ea:00:d4:55:60], cid=[00:e6:ea:00:d4:55:60], tid=0x8a179707: lease 192.168.101.10 will be advertised
2026-08-18 16:18:35.303 INFO  [kea-dhcp4.leases/17955.139959123273408] DHCP4_LEASE_ALLOC [hwtype=1 e6:ea:00:d4:55:60], cid=[00:e6:ea:00:d4:55:60], tid=0x8a179707: lease 192.168.101.10 has been allocated for 120 seconds
2026-08-18 16:18:37.190 INFO  [kea-dhcp4.leases/17955.139959114880704] DHCP4_LEASE_ADVERT [hwtype=1 e6:ea:00:d4:55:60], cid=[00:e6:ea:00:d4:55:60], tid=0x456b68e2: lease 192.168.101.10 will be advertised
2026-08-18 16:18:37.191 INFO  [kea-dhcp4.leases/17955.139959106488000] DHCP4_LEASE_ALLOC [hwtype=1 e6:ea:00:d4:55:60], cid=[00:e6:ea:00:d4:55:60], tid=0x456b68e2: lease 192.168.101.10 has been allocated for 120 seconds
2026-08-18 16:18:49.203 INFO  [kea-dhcp4.leases/17955.139959131666112] DHCP4_LEASE_ALLOC [hwtype=1 e6:ea:00:d4:55:60], cid=[00:e6:ea:00:d4:55:60], tid=0xac7971a5: lease 192.168.101.10 has been allocated for 120 seconds
2026-08-18 16:18:50.000 INFO  [kea-dhcp4.leases/17955.139959131666112] DHCP4_LEASE_ALLOC [hwtype=1 aa:bb:cc:dd:ee:ff], cid=[00], tid=0xac7971a6: lease 192.168.101.42 has been allocated for 120 seconds
2026-08-18 16:18:53.506 INFO  [kea-dhcp4.leases/17955.139959123273408] DHCP4_RELEASE [hwtype=1 e6:ea:00:d4:55:60], cid=[00:e6:ea:00:d4:55:60], tid=0xf24c63e4: address 192.168.101.10 was released properly.
`

const kea24MAC = "e6:ea:00:d4:55:60"

func TestCountLogLines_Kea24CountsLeaseAllocs(t *testing.T) {
	ef := newLogFixture(t, backendKea, kea24Log)
	if got := ef.CountLogLines("DHCPACK", kea24MAC); got != 3 {
		t.Errorf("CountLogLines(DHCPACK, %s) on kea 2.4 = %d, want 3 (one bind, two renewals)", kea24MAC, got)
	}
	if got := ef.CountLogLines("DHCPACK", "aa:bb:cc:dd:ee:ff"); got != 1 {
		t.Errorf("CountLogLines(DHCPACK, other MAC) on kea 2.4 = %d, want 1", got)
	}
	if got := ef.CountLogLines("DHCPACK", "de:ad:be:ef:00:00"); got != 0 {
		t.Errorf("CountLogLines(DHCPACK, unknown MAC) = %d, want 0", got)
	}
}

func TestLastACKAddress_Kea24(t *testing.T) {
	ef := newLogFixture(t, backendKea, kea24Log)
	if got := ef.LastACKAddress(kea24MAC); got != "192.168.101.10" {
		t.Errorf("LastACKAddress on kea 2.4 = %q, want 192.168.101.10", got)
	}
	if got := ef.LastACKAddress("aa:bb:cc:dd:ee:ff"); got != "192.168.101.42" {
		t.Errorf("LastACKAddress(other MAC) on kea 2.4 = %q, want 192.168.101.42", got)
	}
}

func TestCountLogLines_KeaTokenChoiceIsPerLog(t *testing.T) {
	ef26 := newLogFixture(t, backendKea, keaACKLog)
	if got := ef26.CountLogLines("DHCPACK", keaMAC); got != 2 {
		t.Errorf("2.6.3: = %d, want 2 — a bind on 2.6.3 writes both lines and must count once", got)
	}
	const oneBindOneRenew = `
2026-08-18 16:18:35.303 INFO  [kea-dhcp4.leases/1.1] DHCP4_LEASE_ALLOC [hwtype=1 e6:ea:00:d4:55:60], cid=[00], tid=0x1: lease 192.168.101.10 has been allocated for 120 seconds
2026-08-18 16:18:49.203 INFO  [kea-dhcp4.leases/1.1] DHCP4_LEASE_ALLOC [hwtype=1 e6:ea:00:d4:55:60], cid=[00], tid=0x2: lease 192.168.101.10 has been allocated for 120 seconds
`
	ef24 := newLogFixture(t, backendKea, oneBindOneRenew)
	if got := ef24.CountLogLines("DHCPACK", kea24MAC); got != 2 {
		t.Errorf("2.4.1: = %d, want 2 — the same history must count the same on both versions", got)
	}
}

// keaBroadcastACKLog is keaACKLog's bind as Kea 2.6.3 logs it for a client setting RFC 2131 section 2's BROADCAST flag:
// the ACK goes to 255.255.255.255, not the granted address.
const keaBroadcastACKLog = `
2026-09-03 15:41:02.326 INFO  [kea-dhcp4.packets/17.14014647] DHCP4_PACKET_RECEIVED [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: DHCPDISCOVER (type 1) received from 0.0.0.0 to 255.255.255.255 on interface dh-itest-edhcp
2026-09-03 15:41:02.326 INFO  [kea-dhcp4.leases/17.14014647] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: lease 192.168.101.10 has been allocated for 20 seconds
2026-09-03 15:41:02.326 INFO  [kea-dhcp4.packets/17.14014647] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 255.255.255.255:68 on interface dh-itest-edhcp
`

func TestLastACKAddress_BroadcastACKStillNamesTheGrant(t *testing.T) {
	bc := newLogFixture(t, backendKea, keaBroadcastACKLog)
	if got := bc.LastACKAddress(keaMAC); got != "192.168.101.10" {
		t.Errorf("broadcast ACK: LastACKAddress = %q, want 192.168.101.10 — the address the "+
			"server GRANTED. 255.255.255.255 is where the packet went and says nothing about "+
			"which address this client holds.", got)
	}
	uc := newLogFixture(t, backendKea, keaACKLog)
	if got := uc.LastACKAddress("aa:bb:cc:dd:ee:ff"); got != "192.168.101.42" {
		t.Errorf("unicast ACK: LastACKAddress = %q, want 192.168.101.42; the fix must not "+
			"break the path that was working", got)
	}
}

func TestLastACKAddressFrom_ACKedButUnreadableIsNotEmpty(t *testing.T) {
	const unreadable = `
2026-09-03 15:41:02.326 INFO  [kea-dhcp4.packets/1.1] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x1: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 255.255.255.255:68 on interface dh-itest-edhcp
`
	addr, matched := lastACKAddressFrom(backendKea, unreadable, "dhcpack", keaMAC)
	if addr != "" {
		t.Fatalf("addr = %q, want empty: the only line names a broadcast destination and no grant", addr)
	}
	if matched == 0 {
		t.Error("matched = 0 for a log containing an ACK for this MAC. The caller cannot then " +
			"tell a never-ACKed client from a reader that failed, and the divergence check " +
			"would pass by being skipped.")
	}

	if _, m := lastACKAddressFrom(backendKea, keaACKLog, "dhcpack", "de:ad:be:ef:00:00"); m != 0 {
		t.Errorf("matched = %d for a MAC with no ACK, want 0", m)
	}
}

// kea24ConflictLog is verbatim from hosted run 34537348413 (#942): kea 2.4.1 through an offer, a decline and a second
// offer, with no DHCPOFFER and no DHCPACK line.
const kea24ConflictLog = `
2026-09-10 22:29:00.539 INFO  [kea-dhcp4.leases/11572.139789029430976] DHCP4_LEASE_ADVERT [hwtype=1 4e:b2:23:e0:6f:f9], cid=[00:4e:b2:23:e0:6f:f9], tid=0x5a1e48c7: lease 192.168.101.42 will be advertised
2026-09-10 22:29:00.540 INFO  [kea-dhcp4.leases/11572.139789021038272] DHCP4_LEASE_ALLOC [hwtype=1 4e:b2:23:e0:6f:f9], cid=[00:4e:b2:23:e0:6f:f9], tid=0x5a1e48c7: lease 192.168.101.42 has been allocated for 120 seconds
2026-09-10 22:29:01.298 INFO  [kea-dhcp4.leases/11572.139789012645568] DHCP4_DECLINE_LEASE Received DHCPDECLINE for addr 192.168.101.42 from client [hwtype=1 4e:b2:23:e0:6f:f9], cid=[00:4e:b2:23:e0:6f:f9], tid=0xaeb7d074. The lease will be unavailable for 86400 seconds.
2026-09-10 22:29:11.308 INFO  [kea-dhcp4.leases/11572.139789004252864] DHCP4_LEASE_ADVERT [hwtype=1 4e:b2:23:e0:6f:f9], cid=[00:4e:b2:23:e0:6f:f9], tid=0xad73c779: lease 192.168.101.43 will be advertised
2026-09-10 22:29:11.308 INFO  [kea-dhcp4.leases/11572.139789029430976] DHCP4_LEASE_ALLOC [hwtype=1 4e:b2:23:e0:6f:f9], cid=[00:4e:b2:23:e0:6f:f9], tid=0xad73c779: lease 192.168.101.43 has been allocated for 120 seconds
`

const kea24ConflictMAC = "4e:b2:23:e0:6f:f9"

func TestCountLogLines_Kea24CountsLeaseAdverts(t *testing.T) {
	ef := newLogFixture(t, backendKea, kea24ConflictLog)
	if got := ef.CountLogLines("DHCPOFFER", kea24ConflictMAC); got != 2 {
		t.Errorf("CountLogLines(DHCPOFFER, %s) on kea 2.4 = %d, want 2 (the offer before the "+
			"decline and the one after it)", kea24ConflictMAC, got)
	}
	if got := ef.CountLogLines("DHCPDECLINE", kea24ConflictMAC); got != 1 {
		t.Errorf("CountLogLines(DHCPDECLINE, %s) = %d, want 1; this token is spelt the same on "+
			"both versions and must not be disturbed by the stand-in", kea24ConflictMAC, got)
	}
	if got := ef.CountLogLines("DHCPOFFER", "de:ad:be:ef:00:00"); got != 0 {
		t.Errorf("CountLogLines(DHCPOFFER, unknown MAC) = %d, want 0", got)
	}
}

func TestCountLogLines_OfferTokenChoiceIsPerLog(t *testing.T) {
	ef26 := newLogFixture(t, backendKea, keaACKLog)
	if got := ef26.CountLogLines("DHCPOFFER", keaMAC); got != 1 {
		t.Errorf("2.6.3: CountLogLines(DHCPOFFER) = %d, want 1", got)
	}
	const oneOffer = `
2026-08-18 16:18:35.302 INFO  [kea-dhcp4.leases/1.1] DHCP4_LEASE_ADVERT [hwtype=1 02:11:22:33:44:55], cid=[00], tid=0x1: lease 192.168.101.10 will be advertised
2026-08-18 16:18:35.303 INFO  [kea-dhcp4.leases/1.1] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[00], tid=0x1: lease 192.168.101.10 has been allocated for 120 seconds
`
	ef24 := newLogFixture(t, backendKea, oneOffer)
	if got := ef24.CountLogLines("DHCPOFFER", keaMAC); got != 1 {
		t.Errorf("2.4.1: CountLogLines(DHCPOFFER) = %d, want 1 — the same history must count "+
			"the same on both versions", got)
	}
	// No released Kea writes both spellings for one event; this rule counts such an event once.
	both := keaACKLog + `
2026-08-02 09:53:52.325 INFO  [kea-dhcp4.leases/17.1401] DHCP4_LEASE_ADVERT [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: lease 192.168.101.10 will be advertised
`
	efBoth := newLogFixture(t, backendKea, both)
	if got := efBoth.CountLogLines("DHCPOFFER", keaMAC); got != 1 {
		t.Errorf("both spellings for one offer: CountLogLines(DHCPOFFER) = %d, want 1", got)
	}
}

func TestCountLogLines_Kea24AdvertWithoutAllocIsStillAnOffer(t *testing.T) {
	const advertThenDecline = `
2026-09-10 22:29:00.539 INFO  [kea-dhcp4.leases/11572.139789029430976] DHCP4_LEASE_ADVERT [hwtype=1 4e:b2:23:e0:6f:f9], cid=[00:4e:b2:23:e0:6f:f9], tid=0x5a1e48c7: lease 192.168.101.42 will be advertised
2026-09-10 22:29:01.298 INFO  [kea-dhcp4.leases/11572.139789012645568] DHCP4_DECLINE_LEASE Received DHCPDECLINE for addr 192.168.101.42 from client [hwtype=1 4e:b2:23:e0:6f:f9], cid=[00:4e:b2:23:e0:6f:f9], tid=0xaeb7d074. The lease will be unavailable for 86400 seconds.
`
	ef := newLogFixture(t, backendKea, advertThenDecline)
	if got := ef.CountLogLines("DHCPOFFER", kea24ConflictMAC); got != 1 {
		t.Errorf("CountLogLines(DHCPOFFER, %s) = %d, want 1: the server advertised an address "+
			"and the client declined it, so the offer happened and no allocation followed it. "+
			"A 0 here is an offer count keyed on the bind", kea24ConflictMAC, got)
	}
	if got := ef.CountLogLines("DHCPACK", kea24ConflictMAC); got != 0 {
		t.Errorf("CountLogLines(DHCPACK, %s) = %d, want 0: nothing in this log grants the "+
			"address", kea24ConflictMAC, got)
	}
}

func TestCountLogLines_DnsmasqOfferIsUntouched(t *testing.T) {
	const mac = "1e:c1:60:88:5a:ef"
	const dnsmasqLog = `
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPOFFER(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPACK(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef
`
	ef := newLogFixture(t, backendDnsmasq, dnsmasqLog)
	if got := ef.CountLogLines("DHCPOFFER", mac); got != 1 {
		t.Errorf("dnsmasq: CountLogLines(DHCPOFFER) = %d, want 1", got)
	}
	if got := ef.CountLogLines("DHCPACK", mac); got != 1 {
		t.Errorf("dnsmasq: CountLogLines(DHCPACK) = %d, want 1", got)
	}

	const dnsmasqNoOffer = `
Aug 20 11:04:07 dnsmasq-dhcp[1]: DHCPACK(dh-itest-mv) 192.168.99.34 1e:c1:60:88:5a:ef
Aug 20 11:04:07 dnsmasq-dhcp[1]: relayed text DHCP4_LEASE_ADVERT for 1e:c1:60:88:5a:ef
`
	efNoOffer := newLogFixture(t, backendDnsmasq, dnsmasqNoOffer)
	if got := efNoOffer.CountLogLines("DHCPOFFER", mac); got != 0 {
		t.Errorf("dnsmasq log with no offer line: CountLogLines(DHCPOFFER) = %d, want 0. A Kea "+
			"stand-in was applied to a log this fixture's server did not write", got)
	}
	if got := efNoOffer.CountLogLines("DHCPACK", mac); got != 1 {
		t.Errorf("dnsmasq log with no offer line: CountLogLines(DHCPACK) = %d, want 1. Without "+
			"this the zero above would be an unreadable log and not an absent offer", got)
	}
}
