//go:build integration

package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real kea-dhcp4 2.6.3 output at severity INFO, captured on the
// integration runner image while migrating this fixture (#356). Kept
// verbatim — the point of these tests is that the parser matches what
// the server actually writes, so paraphrasing them would defeat them.
const keaACKLog = `
2026-08-02 09:53:52.326 INFO  [kea-dhcp4.packets/17.140146479474368] DHCP4_PACKET_RECEIVED [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x517eb529: DHCPDISCOVER (type 1) received from 0.0.0.0 to 255.255.255.255 on interface dh-itest-edhcp
2026-08-02 09:53:52.326 INFO  [kea-dhcp4.packets/17.140146479474368] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x517eb529: trying to send packet DHCPOFFER (type 2) from 192.168.101.1:67 to 192.168.101.10:68 on interface dh-itest-edhcp
2026-08-02 09:53:52.326 INFO  [kea-dhcp4.leases/17.140146471081664] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x517eb529: lease 192.168.101.10 has been allocated for 20 seconds
2026-08-02 09:53:52.326 INFO  [kea-dhcp4.packets/17.140146471081664] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x517eb529: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 192.168.101.10:68 on interface dh-itest-edhcp
2026-08-02 09:54:02.136 INFO  [kea-dhcp4.packets/17.140146462688960] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55:00:01:00:01:32:01:d0:2e:02:11:22:33:44:55], tid=0x1cc9f77f: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 192.168.101.11:68 on interface dh-itest-edhcp
2026-08-02 09:54:02.136 INFO  [kea-dhcp4.packets/17.140146462688960] DHCP4_PACKET_SEND [hwtype=1 aa:bb:cc:dd:ee:ff], cid=[00], tid=0x1cc9f780: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 192.168.101.42:68 on interface dh-itest-edhcp
`

const keaMAC = "02:11:22:33:44:55"

// newLogFixture builds an EphemeralFixture backed by a log file with
// the given contents, for exercising the log readers without a server.
func newLogFixture(t *testing.T, backend ephemeralBackend, log string) *EphemeralFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dhcp-server.log")
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	return &EphemeralFixture{t: t, backend: backend, tmpDir: dir, logFile: path}
}

// TestAckAddress_Kea is the guard on the riskiest part of the #356
// migration.
//
// LastACKAddress is the fixture's outside evidence: the server's own
// statement of which address the client holds, which no health counter
// can supply. TestFailure_ServerLossDuringRenewal consumes it as
//
//	if acked := ef.LastACKAddress(mac); acked != "" && acked != live {
//
// so a parser that returns "" for every Kea line does not fail that
// test — it silently deletes the assertion, and the test goes on
// passing while checking one thing less. That is the exact failure
// shape this repo has been bitten by before, so the parser gets a
// test that fails loudly instead.
func TestAckAddress_Kea(t *testing.T) {
	const line = `2026-08-02 09:53:52.326 INFO  [kea-dhcp4.packets/17.1401] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 192.168.101.10:68 on interface dh-itest-edhcp`
	if got := ackAddress(backendKea, line); got != "192.168.101.10" {
		t.Errorf("ackAddress(kea) = %q, want the ACK recipient 192.168.101.10", got)
	}
	// The server's own address appears FIRST on the line, as the
	// `from`. Returning it would make every divergence check compare
	// the container's address against the gateway and fail confusingly.
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

// TestLastACKAddress_KeaTracksLatest pins the "last" in the name: a
// renewal that moves the client to another address must win over the
// original bind, and another client's ACK must not.
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

// TestCountLogLines_KeaCountsAcksNotOffers guards the counter the
// renewal and re-bind tests poll on. Kea logs OFFER and ACK through
// the same DHCP4_PACKET_SEND message, so a substring match on the
// message id — or running the logger at DEBUG, where
// DHCP4_RESPONSE_DATA repeats "DHCPACK" for the same packet — would
// double or inflate every count in the failure suite.
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

// TestKeaConfig_IsValidJSON guards the hand-rolled config template.
// The optional renew-timer / rebind-timer lines carry their own commas,
// so an empty timer set is the case where a stray or missing comma
// would produce JSON Kea rejects at startup — cheap to catch here
// rather than in a CI round trip.
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
			raw := ef.keaConfig()
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
			// Authoritative is what makes an unknown REQUEST a refusal
			// rather than silence; the failure suite's semantics rest
			// on it, so it is not optional.
			if !cfg.Dhcp4.Authoritative {
				t.Error("authoritative is false; the failure tests need a server that owns its subnet")
			}
			if len(cfg.Dhcp4.Subnet4) != 1 {
				t.Fatalf("want exactly one subnet4 entry, got %d", len(cfg.Dhcp4.Subnet4))
			}
			// The subnet must be the NETWORK, not the server's host
			// address — Kea rejects a subnet4 whose pool falls outside
			// it, and "192.168.101.1/24" is the easy mistake.
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

// TestSeedStolenLease_Kea checks the seeded lease DB carries the
// schema header Kea writes and pins the address to a foreign client.
// A row whose subnet_id does not match the configured subnet loads
// into no subnet and seeds nothing — silently — so that field is
// asserted rather than assumed.
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
