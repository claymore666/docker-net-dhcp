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

// TestKeaLeaseAllocSeconds reads the granted lifetime off the one line
// that states it. The first case is the verbatim capture from
// keaACKLog above; the rest are that same message with only the MAC,
// address, number or wording varied, which is what a parser has to
// survive.
//
// The bool matters more than the number. GrantedLease and
// verifyLeaseGrants both key on "did the server say so at all", so a
// parser that answered 0/true for a line it did not understand would
// hold 0 against the configured lease and fail every run, and one that
// answered 0/false quietly for a line it should have read would
// disable the check instead of failing it. Both directions are here.
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
			// An ACK line names the same client and the same address
			// but says nothing about the lifetime. Reading a number off
			// it — the port, the type, the tid — would be worse than
			// reading nothing.
			name:   "an ACK line carries no lifetime",
			line:   `2026-08-02 09:53:52.326 INFO  [kea-dhcp4.packets/17.1401] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 192.168.101.10:68 on interface dh-itest-edhcp`,
			wantOK: false,
		},
		{
			// The unit is read, not assumed. If a future kea says
			// minutes, 2 must not be compared against 120.
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

// keaGrantLog is keaACKLog's allocation message repeated for a second
// client and a later renewal, so attribution and recency can be
// checked. Only the MAC, address and lifetime vary from the captured
// line; the surrounding format is that capture verbatim.
const keaGrantLog = `
2026-08-02 09:53:52.326 INFO  [kea-dhcp4.leases/17.140146471081664] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55], tid=0x517eb529: lease 192.168.101.10 has been allocated for 20 seconds
2026-08-02 09:53:53.001 INFO  [kea-dhcp4.leases/17.140146471081664] DHCP4_LEASE_ALLOC [hwtype=1 aa:bb:cc:dd:ee:ff], cid=[00], tid=0x1cc9f780: lease 192.168.101.42 has been allocated for 20 seconds
2026-08-02 09:54:02.136 INFO  [kea-dhcp4.leases/17.140146462688960] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22:33:44:55], tid=0x1cc9f77f: lease 192.168.101.11 has been allocated for 20 seconds
`

// TestGrantedLease_Kea pins per-client attribution and recency, the
// same two properties LastACKAddress has.
func TestGrantedLease_Kea(t *testing.T) {
	ef := newLogFixture(t, backendKea, keaGrantLog)

	if got, ok := ef.GrantedLease(keaMAC); !ok || got != 20 {
		t.Errorf("GrantedLease(%s) = %d, %v; want 20, true", keaMAC, got, ok)
	}
	if got, ok := ef.GrantedLease("aa:bb:cc:dd:ee:ff"); !ok || got != 20 {
		t.Errorf("GrantedLease(other MAC) = %d, %v; want 20, true — grants must be attributed per client", got, ok)
	}
	// A client that never bound must report "no evidence", not a zero
	// that a caller would read as a lifetime.
	if got, ok := ef.GrantedLease("de:ad:be:ef:00:00"); ok || got != 0 {
		t.Errorf("GrantedLease(unknown MAC) = %d, %v; want 0, false", got, ok)
	}
}

// TestKeaLeaseGrants_ReadsEveryAllocation guards what
// verifyLeaseGrants consumes: it checks EVERY allocation, not the
// first, because a lifetime that changes mid-run (a Restart on a
// different pool, a lease reloaded from a seeded DB) is exactly the
// divergence the check exists to catch.
func TestKeaLeaseGrants_ReadsEveryAllocation(t *testing.T) {
	grants := keaLeaseGrants(keaGrantLog)
	if len(grants) != 3 {
		t.Fatalf("keaLeaseGrants found %d allocation(s), want 3", len(grants))
	}
	for i, g := range grants {
		if g.seconds != 20 {
			t.Errorf("grant %d = %ds, want 20s", i, g.seconds)
		}
		// The line is carried so a mismatch can quote the server
		// rather than paraphrase it.
		if !strings.Contains(g.line, "DHCP4_LEASE_ALLOC") {
			t.Errorf("grant %d kept no evidence line: %q", i, g.line)
		}
	}
	// keaACKLog is mostly packet traffic with a single allocation in
	// it — the reader must not count ACKs or OFFERs as grants.
	if got := len(keaLeaseGrants(keaACKLog)); got != 1 {
		t.Errorf("keaLeaseGrants(keaACKLog) found %d, want 1; only DHCP4_LEASE_ALLOC states a lifetime", got)
	}
	if got := len(keaLeaseGrants("")); got != 0 {
		t.Errorf("keaLeaseGrants(empty) found %d, want 0", got)
	}
}

// TestCheckLeaseGrants is the negative control, and it is the reason
// the comparison lives in a pure function.
//
// #472 exists because the fixture's lease timings were asserted about
// without ever being confirmed. A check added to fix that, which had
// itself never been observed rejecting anything, would be the same
// mistake one layer up. So every direction is exercised here: agreement
// is silent, a clamped lifetime reports and names BOTH numbers, a
// lifetime that drifts partway through a run is not averaged away, and
// a run with no allocation at all fails rather than passing vacuously.
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
		// The shape #472 names: the fixture asks for 20s, something
		// (a min-valid-lifetime default, a key kea tolerates silently)
		// serves 60 instead, and every test built on T1 < outage <
		// lease stays green while crossing nothing.
		got := checkLeaseGrants([]keaLeaseGrant{grant(60)}, 20)
		if len(got) != 1 {
			t.Fatalf("checkLeaseGrants reported %d problem(s), want 1: %v", len(got), got)
		}
		// Naming only one number leaves the reader to guess which is
		// which, and the whole value of this check is that the two are
		// different.
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
		// The trap this whole issue is about: evidence that is absent
		// must not read as evidence that agrees.
		got := checkLeaseGrants(nil, 20)
		if len(got) != 1 {
			t.Fatalf("checkLeaseGrants(no grants) reported %d problem(s), want 1: %v", len(got), got)
		}
		if !strings.Contains(got[0], "DHCP4_LEASE_ALLOC") {
			t.Errorf("problem does not say which line was looked for:\n%s", got[0])
		}
	})
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

// The logger key is the one part of the kea config that differs by
// server version: renamed from output_options to output-options in Kea
// 2.5.4, and Debian/Ubuntu stable still ships the older 2.4.x. The
// fixture asks the installed binary which it accepts rather than
// deciding from a version string, so what is testable without a kea on
// PATH is the substitution itself — that both spellings render, and
// that neither leaks the other.
//
// This is not hypothetical tidiness. The hosted portability lane failed
// with `got unexpected keyword "output-options" in loggers map` on a
// stock runner, while every self-hosted run stayed green on a newer kea
// from our own image (#612).
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

			// The other spelling must be absent, not merely
			// unused: kea rejects the key it does not know, so a
			// config carrying both fails on every version.
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

// Real kea-dhcp4 2.4.1 output at severity INFO — what Ubuntu 24.04
// ships — captured from the hosted portability lane on 2026-08-18
// (#612). Kept verbatim for the same reason as keaACKLog above.
//
// The shape to notice, because it is why the parser needs a per-log
// decision: this version writes NO line containing DHCPACK. Every
// bind and every renewal is a DHCP4_LEASE_ALLOC, and nothing else. A
// parser that only knew the 2.6.3 shape answered "0 ACKs" for the
// client below, which had visibly bound and renewed twice.
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

// TestCountLogLines_Kea24CountsLeaseAllocs pins the 2.4.x side of the
// parser: with no DHCPACK line anywhere in the log, LEASE_ALLOC stands
// in, and it counts one per bind or renewal — three for this client
// — attributed per MAC, with ADVERT and RELEASE lines not counted.
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

// TestLastACKAddress_Kea24 pins that the granted address is read off
// the LEASE_ALLOC line when that is all the server writes.
func TestLastACKAddress_Kea24(t *testing.T) {
	ef := newLogFixture(t, backendKea, kea24Log)
	if got := ef.LastACKAddress(kea24MAC); got != "192.168.101.10" {
		t.Errorf("LastACKAddress on kea 2.4 = %q, want 192.168.101.10", got)
	}
	if got := ef.LastACKAddress("aa:bb:cc:dd:ee:ff"); got != "192.168.101.42" {
		t.Errorf("LastACKAddress(other MAC) on kea 2.4 = %q, want 192.168.101.42", got)
	}
}

// TestCountLogLines_KeaTokenChoiceIsPerLog is the property the whole
// design rests on: the two Kea shapes must give the SAME answer for
// the same history. keaACKLog (2.6.3) has one bind and one renewal for
// keaMAC and answers 2; a 2.4 log with one bind and one renewal must
// also answer 2, and must not double count the bind on 2.6.3, where
// the DHCPACK line and the LEASE_ALLOC line describe the same event.
func TestCountLogLines_KeaTokenChoiceIsPerLog(t *testing.T) {
	// 2.6.3: DHCPACK lines exist, so LEASE_ALLOC must be ignored.
	ef26 := newLogFixture(t, backendKea, keaACKLog)
	if got := ef26.CountLogLines("DHCPACK", keaMAC); got != 2 {
		t.Errorf("2.6.3: = %d, want 2 — a bind on 2.6.3 writes both lines and must count once", got)
	}
	// 2.4.1: no DHCPACK line anywhere, so LEASE_ALLOC stands in.
	const oneBindOneRenew = `
2026-08-18 16:18:35.303 INFO  [kea-dhcp4.leases/1.1] DHCP4_LEASE_ALLOC [hwtype=1 e6:ea:00:d4:55:60], cid=[00], tid=0x1: lease 192.168.101.10 has been allocated for 120 seconds
2026-08-18 16:18:49.203 INFO  [kea-dhcp4.leases/1.1] DHCP4_LEASE_ALLOC [hwtype=1 e6:ea:00:d4:55:60], cid=[00], tid=0x2: lease 192.168.101.10 has been allocated for 120 seconds
`
	ef24 := newLogFixture(t, backendKea, oneBindOneRenew)
	if got := ef24.CountLogLines("DHCPACK", kea24MAC); got != 2 {
		t.Errorf("2.4.1: = %d, want 2 — the same history must count the same on both versions", got)
	}
}

// keaBroadcastACKLog is keaACKLog's bind, as Kea 2.6.3 actually logs it
// once the client sets the BROADCAST flag of RFC 2131 section 2 — which
// every client on this plugin's raw transport does. The ONLY difference
// from the unicast capture is the ACK's destination: 255.255.255.255
// instead of the address being granted.
const keaBroadcastACKLog = `
2026-09-03 15:41:02.326 INFO  [kea-dhcp4.packets/17.14014647] DHCP4_PACKET_RECEIVED [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: DHCPDISCOVER (type 1) received from 0.0.0.0 to 255.255.255.255 on interface dh-itest-edhcp
2026-09-03 15:41:02.326 INFO  [kea-dhcp4.leases/17.14014647] DHCP4_LEASE_ALLOC [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: lease 192.168.101.10 has been allocated for 20 seconds
2026-09-03 15:41:02.326 INFO  [kea-dhcp4.packets/17.14014647] DHCP4_PACKET_SEND [hwtype=1 02:11:22:33:44:55], cid=[ff:22], tid=0x517eb529: trying to send packet DHCPACK (type 5) from 192.168.101.1:67 to 255.255.255.255:68 on interface dh-itest-edhcp
`

// TestLastACKAddress_BroadcastACKStillNamesTheGrant is the regression.
//
// The reader used to take Kea's `DHCPACK ... to <addr>:68` and treat the
// recipient as the granted address, and ackAddress's own comment
// asserted the two were the same thing. They coincide only while the
// reply is unicast. Setting the BROADCAST flag made Kea send to
// 255.255.255.255, so LastACKAddress answered 255.255.255.255 and
// TestFailure_ServerLossDuringRenewal reported the container and the
// server as diverged when in fact they agreed — a red on the ONE
// assertion in that file that checks the product's real claim, caused
// entirely by the observer.
//
// Driven against the unicast capture too, in the same test: a fix that
// read the allocation line but broke the unicast path would move the
// failure rather than remove it.
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

// TestLastACKAddressFrom_ACKedButUnreadableIsNotEmpty pins the
// difference between the two ways of answering "".
//
// "" is legitimate for a client the server never ACKed, and the caller
// guards on `acked != ""` precisely so an un-ACKed client does not fail
// the divergence check. That same guard means a PARSER failure returning
// "" would disable the check silently instead of failing it. matched is
// what separates the two, so it is asserted here directly rather than
// through the t.Errorf the method raises from it.
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

	// The other side of it: a MAC the server never ACKed must report
	// zero, or every un-ACKed client becomes a harness error.
	if _, m := lastACKAddressFrom(backendKea, keaACKLog, "dhcpack", "de:ad:be:ef:00:00"); m != 0 {
		t.Errorf("matched = %d for a MAC with no ACK, want 0", m)
	}
}
