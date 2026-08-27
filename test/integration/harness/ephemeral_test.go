// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

// ---- host AppArmor confinement (#680) --------------------------------
//
// On a host with the distribution's kea package installed, six tests in
// this suite fail before they test anything. The host's
// /etc/apparmor.d/usr.sbin.kea-dhcp4 profile is loaded, AppArmor
// attaches on exec by executable path, and kea-dhcp4 in this container
// transitions into it — privileged or not, root or not. Kea then
// reports "Permission denied" against a path it can see, which sends the
// reader to look at a config that is fine.
//
// These cover the reading and the wording, which is all that can be
// covered without a confined host. The reading is the part that decides
// whether the message appears at all.

func TestParseAppArmorCurrent(t *testing.T) {
	cases := []struct {
		name, in, profile, mode string
	}{
		{"unconfined", "unconfined\n", "", ""},
		{"empty", "", "", ""},
		{"whitespace only", "  \n", "", ""},
		{"enforce", "kea-dhcp4 (enforce)\n", "kea-dhcp4", "enforce"},
		{"complain", "kea-dhcp4 (complain)\n", "kea-dhcp4", "complain"},
		{"path-shaped name", "/usr/sbin/kea-dhcp4 (enforce)\n", "/usr/sbin/kea-dhcp4", "enforce"},
		{"name with a space", "docker-default (enforce)", "docker-default", "enforce"},
		// Confined with no mode reported. Unusual, but "confined and I
		// cannot tell you how" must not read as "unconfined".
		{"no mode", "kea-dhcp4\n", "kea-dhcp4", ""},
		{"unterminated mode", "kea-dhcp4 (enforce\n", "kea-dhcp4 (enforce", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			profile, mode := parseAppArmorCurrent(c.in)
			if profile != c.profile || mode != c.mode {
				t.Errorf("parseAppArmorCurrent(%q) = (%q, %q), want (%q, %q)",
					c.in, profile, mode, c.profile, c.mode)
			}
		})
	}
}

// TestApparmorProfileOf_ReadsTheFileItClaimsTo is the non-vacuous half.
//
// On any host CI runs on, /proc/self/attr/current says "unconfined", so
// comparing the reader's answer with the kernel's compares "" with "" —
// two empty strings agreeing, which they would also do if the reader
// opened nothing at all. This drives a proc-shaped tree instead, so the
// path the reader builds is what decides the result.
func TestApparmorProfileOf_ReadsTheFileItClaimsTo(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "4242", "attr"), 0o755); err != nil {
		t.Fatalf("build the fake proc tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "4242", "attr", "current"),
		[]byte("kea-dhcp4 (enforce)\n"), 0o644); err != nil {
		t.Fatalf("write the fake attr/current: %v", err)
	}
	saved := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = saved })

	profile, mode := apparmorProfileOf(4242)
	if profile != "kea-dhcp4" || mode != "enforce" {
		t.Errorf("apparmorProfileOf(4242) = (%q, %q), want (\"kea-dhcp4\", \"enforce\"); "+
			"the reader did not open <root>/4242/attr/current", profile, mode)
	}
	// The control: a pid with no entry in the same tree must come back
	// empty, or the test above would pass for a reader that ignores its
	// argument entirely.
	if profile, mode := apparmorProfileOf(4243); profile != "" || mode != "" {
		t.Errorf("apparmorProfileOf(4243) = (%q, %q) in a tree that has no 4243; "+
			"the reader is not using the pid it was given", profile, mode)
	}
}

// TestSampleConfinement_FirstReadingWins covers the property the poll
// loop depends on: kea's /proc entry vanishes when it dies, so a reading
// taken while it lived must survive the readings taken after it did not.
// Without this, the explanation disappears in exactly the case — a kea
// that dies at once — that it was written for.
func TestSampleConfinement_FirstReadingWins(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "4242", "attr"), 0o755); err != nil {
		t.Fatalf("build the fake proc tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "4242", "attr", "current"),
		[]byte("kea-dhcp4 (enforce)\n"), 0o644); err != nil {
		t.Fatalf("write the fake attr/current: %v", err)
	}
	saved := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = saved })

	// Nothing held yet: take the reading.
	if p, m := sampleConfinement("", "", 4242); p != "kea-dhcp4" || m != "enforce" {
		t.Errorf("sampleConfinement with nothing held = (%q, %q), want the profile it can read", p, m)
	}
	// Something held, and the process is now gone: keep what we have.
	if p, m := sampleConfinement("kea-dhcp4", "enforce", 4243); p != "kea-dhcp4" || m != "enforce" {
		t.Errorf("sampleConfinement dropped an earlier reading once the pid vanished: (%q, %q)", p, m)
	}
	// Nothing held and nothing readable stays nothing — the ordinary
	// unconfined case, which must not invent a confinement.
	if p, m := sampleConfinement("", "", 4243); p != "" || m != "" {
		t.Errorf("sampleConfinement invented a confinement for an unreadable pid: (%q, %q)", p, m)
	}
}

// TestApparmorProfileOf_ReadsTheKernelsView pins the reader to the file
// it is supposed to read, on whatever host this runs on. It cannot
// assert a particular profile — CI has none and a developer box may —
// so it asserts AGREEMENT with /proc/self/attr/current, which is the
// property: a reader wired to the wrong path, or parsing something
// else, disagrees here regardless of what the host is confining.
func TestApparmorProfileOf_ReadsTheKernelsView(t *testing.T) {
	raw, err := os.ReadFile("/proc/self/attr/current")
	gotProfile, gotMode := apparmorProfileOf(os.Getpid())
	if err != nil {
		// No AppArmor in this kernel. That is not a reason to skip — it
		// is an assertion: with nothing to read, the reader must say
		// nothing, because every caller treats a non-empty profile as an
		// explanation and would then blame a confinement that does not
		// exist.
		if gotProfile != "" || gotMode != "" {
			t.Errorf("this kernel has no /proc/self/attr/current (%v), but apparmorProfileOf "+
				"still reported (%q, %q)", err, gotProfile, gotMode)
		}
		return
	}
	wantProfile, wantMode := parseAppArmorCurrent(string(raw))
	if gotProfile != wantProfile || gotMode != wantMode {
		t.Errorf("apparmorProfileOf(self) = (%q, %q), but /proc/self/attr/current says (%q, %q)",
			gotProfile, gotMode, wantProfile, wantMode)
	}
	t.Logf("this process is confined by %q (%q); %q means unconfined", gotProfile, gotMode, "")
}

// A pid that cannot exist must read as "nothing to say", not as a
// finding. Every caller treats a non-empty profile as an explanation.
func TestApparmorProfileOf_AbsentPidSaysNothing(t *testing.T) {
	// -1 can never be a live pid, so /proc/-1/attr/current cannot exist.
	if profile, mode := apparmorProfileOf(-1); profile != "" || mode != "" {
		t.Errorf("apparmorProfileOf(-1) = (%q, %q), want empty; an unreadable pid is not a confinement",
			profile, mode)
	}
}

func TestApparmorDiagnosis(t *testing.T) {
	if d := apparmorDiagnosis("", ""); d != "" {
		t.Errorf("an unconfined process produced a diagnosis:\n%s", d)
	}
	// Complain mode logs and permits. Blaming it would send someone to
	// disable a profile that was never in their way.
	if d := apparmorDiagnosis("kea-dhcp4", "complain"); d != "" {
		t.Errorf("a complain-mode profile produced a diagnosis:\n%s", d)
	}

	d := apparmorDiagnosis("kea-dhcp4", "enforce")
	if d == "" {
		t.Fatal("an enforcing profile produced no diagnosis, so the failure would say nothing about it")
	}
	// The things a reader needs: what is confining it, that being root
	// and privileged does not help, and the one command that fixes it.
	for _, want := range []string{"kea-dhcp4 (enforce)", "exec by executable PATH", "aa-disable", "#680"} {
		if !strings.Contains(d, want) {
			t.Errorf("the diagnosis never mentions %q:\n%s", want, d)
		}
	}
	// Confined with no mode reported still explains the failure.
	if got := apparmorDiagnosis("kea-dhcp4", ""); got == "" {
		t.Error("a confinement with no mode produced no diagnosis; confined is confined")
	} else if strings.Contains(got, "()") {
		t.Errorf("the diagnosis renders an empty mode as \"()\":\n%s", got)
	}
}

// TestKeaStartFailure_CarriesTheDiagnosis is the assembly: the reading
// only matters if it reaches the message the reader sees.
func TestKeaStartFailure_CarriesTheDiagnosis(t *testing.T) {
	const config, log = "CONFIG-SENTINEL", "LOG-SENTINEL"

	plain := keaStartFailure("headline", "", "", config, log)
	for _, want := range []string{"headline", config, log} {
		if !strings.Contains(plain, want) {
			t.Errorf("the unconfined message dropped %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "AppArmor") {
		t.Errorf("an unconfined failure blamed AppArmor:\n%s", plain)
	}

	confined := keaStartFailure("headline", "kea-dhcp4", "enforce", config, log)
	for _, want := range []string{"headline", config, log, "AppArmor", "aa-disable"} {
		if !strings.Contains(confined, want) {
			t.Errorf("the confined message dropped %q:\n%s", want, confined)
		}
	}
	// The explanation must come BEFORE the config and log. The whole
	// defect is that a reader who sees the config first goes looking at
	// JSON that is fine.
	if strings.Index(confined, "AppArmor") > strings.Index(confined, config) {
		t.Error("the AppArmor explanation appears after the config, which is where the reader stops reading")
	}
}

// TestEphemeral_DoesNotSkipOnConfinement holds the decision #680 made
// explicitly, from the other side.
//
// The tempting fix is to skip when a profile is detected. That would
// silently remove six tests from every run on any developer box with the
// kea package installed — the condition is "the host has a package", not
// "this test is inapplicable" — and a broad silent skip is the shape
// that hid #402 and #408 behind an honest comment for months.
//
// Failing with an accurate message is the decision. Reach for a skip and
// this goes red, which is the point.
func TestEphemeral_DoesNotSkipOnConfinement(t *testing.T) {
	src, err := os.ReadFile("ephemeral.go")
	if err != nil {
		t.Fatalf("read ephemeral.go: %v", err)
	}
	// Find the subject by its condition, not by the file being
	// non-empty: if this fixture is ever renamed or the diagnosis moved
	// elsewhere, this guard must go red rather than pass over a file
	// that no longer contains what it is guarding.
	if !strings.Contains(string(src), "func apparmorDiagnosis(") {
		t.Fatal("ephemeral.go no longer defines apparmorDiagnosis; this guard is watching the wrong file")
	}
	for _, banned := range []string{"t.Skip", "Skipf", "SkipNow"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("ephemeral.go calls %s. #680 decided this fixture fails with an accurate "+
				"message rather than skipping: the condition is 'the host has kea installed', "+
				"which would remove six tests from every local run without saying so.", banned)
		}
	}
}

// ---- the readiness loop itself (#680) --------------------------------
//
// The helpers above can all be correct while this loop forgets to sample
// the confinement or forgets to pass it on. Measured: both of those
// mutations survived a suite that tested only the helpers, which is why
// keaReadiness is a function rather than a loop inside startKea.

// keaProbeEnv points procRoot at a tree containing one confined pid and
// returns that pid plus a log path to write into.
func keaProbeEnv(t *testing.T, confined bool) (logPath string, pid int) {
	t.Helper()
	root := t.TempDir()
	pid = 4242
	if confined {
		if err := os.MkdirAll(filepath.Join(root, "4242", "attr"), 0o755); err != nil {
			t.Fatalf("build the fake proc tree: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "4242", "attr", "current"),
			[]byte("kea-dhcp4 (enforce)\n"), 0o644); err != nil {
			t.Fatalf("write the fake attr/current: %v", err)
		}
	}
	saved := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = saved })
	return filepath.Join(root, "kea.log"), pid
}

func writeLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const keaReadyLog = "DHCP4_STARTED version 2.6.3\nDHCPSRV_CFGMGR_ADD_IFACE adding iface eth0\n"

func TestKeaReadiness_ServingIsSilent(t *testing.T) {
	logPath, pid := keaProbeEnv(t, true)
	writeLog(t, logPath, keaReadyLog)
	got := keaReadiness(logPath, 0, pid, time.Now().Add(2*time.Second), "CONFIG",
		func() string { return "LOG" })
	if got != "" {
		t.Errorf("a serving kea produced a failure message:\n%s", got)
	}
}

// The startMark contract: markers written by a PREVIOUS instance must
// not count. The log is appended to across every Stop/StartAgain.
func TestKeaReadiness_IgnoresTheLogBeforeStartMark(t *testing.T) {
	logPath, pid := keaProbeEnv(t, false)
	writeLog(t, logPath, keaReadyLog+"NOTHING FROM THIS INSTANCE\n")
	got := keaReadiness(logPath, len(keaReadyLog), pid, time.Now().Add(200*time.Millisecond),
		"CONFIG", func() string { return "LOG" })
	if got == "" {
		t.Error("readiness accepted markers written before startMark, so a restarted kea " +
			"would report ready on its predecessor's log")
	}
}

func TestKeaReadiness_SocketFailureIsReported(t *testing.T) {
	logPath, pid := keaProbeEnv(t, false)
	writeLog(t, logPath, "DHCPSRV_NO_SOCKETS_OPEN no interfaces\n"+keaReadyLog)
	got := keaReadiness(logPath, 0, pid, time.Now().Add(2*time.Second), "CONFIG",
		func() string { return "LOG" })
	if !strings.Contains(got, "opened no DHCP socket") {
		t.Errorf("a kea that bound nothing was reported as ready or as something else:\n%s", got)
	}
	if strings.Contains(got, "AppArmor") {
		t.Errorf("an unconfined socket failure blamed AppArmor:\n%s", got)
	}
}

// THE CASE THE WHOLE ISSUE IS ABOUT, and the one that observes the call
// site: kea never becomes ready, and the kernel says a host profile is
// confining it. Without this, a loop that never samples the confinement
// — or samples it and drops it on the way to the message — passes.
func TestKeaReadiness_ConfinedNotReadyExplainsItself(t *testing.T) {
	logPath, pid := keaProbeEnv(t, true)
	writeLog(t, logPath, "some kea noise that is not readiness\n")
	got := keaReadiness(logPath, 0, pid, time.Now().Add(300*time.Millisecond), "CONFIG",
		func() string { return "LOG" })
	if got == "" {
		t.Fatal("a kea that never became ready reported success")
	}
	for _, want := range []string{"did not become ready", "AppArmor", "kea-dhcp4 (enforce)", "aa-disable", "CONFIG", "LOG"} {
		if !strings.Contains(got, want) {
			t.Errorf("the failure never mentions %q:\n%s", want, got)
		}
	}
}

// The control that makes the case above mean something: the same
// failure on an unconfined host must NOT blame AppArmor. A loop that
// blamed it unconditionally would pass the test above and be a worse
// message than the one it replaced.
func TestKeaReadiness_UnconfinedNotReadyBlamesNothing(t *testing.T) {
	logPath, pid := keaProbeEnv(t, false)
	writeLog(t, logPath, "some kea noise that is not readiness\n")
	got := keaReadiness(logPath, 0, pid, time.Now().Add(300*time.Millisecond), "CONFIG",
		func() string { return "LOG" })
	if !strings.Contains(got, "did not become ready") {
		t.Errorf("an unconfined kea that never started was reported as something else:\n%s", got)
	}
	if strings.Contains(got, "AppArmor") {
		t.Errorf("an unconfined failure blamed AppArmor, which sends the reader to disable "+
			"a profile that was never in their way:\n%s", got)
	}
}

// A confined kea can also fail at the socket stage, and that message
// carries the explanation too — the diagnosis belongs to the fixture's
// failures, not to one of them.
func TestKeaReadiness_ConfinedSocketFailureAlsoExplains(t *testing.T) {
	logPath, pid := keaProbeEnv(t, true)
	writeLog(t, logPath, "DHCPSRV_NO_SOCKETS_OPEN no interfaces\n")
	got := keaReadiness(logPath, 0, pid, time.Now().Add(2*time.Second), "CONFIG",
		func() string { return "LOG" })
	if !strings.Contains(got, "opened no DHCP socket") || !strings.Contains(got, "aa-disable") {
		t.Errorf("a confined socket failure lost one of its two halves:\n%s", got)
	}
}

// A missing log file must time out and say so, not panic and not report
// ready. This is the shape of a kea that died before writing anything.
func TestKeaReadiness_MissingLogTimesOut(t *testing.T) {
	logPath, pid := keaProbeEnv(t, false)
	got := keaReadiness(logPath, 0, pid, time.Now().Add(200*time.Millisecond), "CONFIG",
		func() string { return "LOG" })
	if !strings.Contains(got, "did not become ready") {
		t.Errorf("a kea that wrote no log at all was not reported as never ready:\n%s", got)
	}
}

// DHCP4_STARTED alone is not readiness, and the socket-failure check is
// not what proves it.
//
// #356's comment says a probe keyed on DHCP4_STARTED alone returns ready
// for a server that will never answer — but every log that says so in
// this suite also carries DHCPSRV_NO_SOCKETS_OPEN, which is caught one
// branch earlier. So dropping the interface requirement changed no
// verdict and the rule was unobserved. This is the input that isolates
// it: a kea that started and configured no interface, with nothing the
// socket-failure check recognises.
func TestKeaReadiness_StartedWithoutAnInterfaceIsNotReady(t *testing.T) {
	logPath, pid := keaProbeEnv(t, false)
	writeLog(t, logPath, "DHCP4_STARTED version 2.6.3\n")
	got := keaReadiness(logPath, 0, pid, time.Now().Add(300*time.Millisecond), "CONFIG",
		func() string { return "LOG" })
	if got == "" {
		t.Error("a kea that logged DHCP4_STARTED but configured no interface was reported " +
			"ready; every test built on that fails later, somewhere else, looking like a plugin bug (#356)")
	}
}

// The other half of the conjunction. Driving only the DHCP4_STARTED
// side leaves "key on the interface line alone" alive: a kea that has
// added an interface but has not finished starting is not serving
// either, and a probe that accepts it hands the suite a server that is
// not yet listening.
func TestKeaReadiness_InterfaceWithoutStartedIsNotReady(t *testing.T) {
	logPath, pid := keaProbeEnv(t, false)
	writeLog(t, logPath, "DHCPSRV_CFGMGR_ADD_IFACE adding iface eth0\n")
	got := keaReadiness(logPath, 0, pid, time.Now().Add(300*time.Millisecond), "CONFIG",
		func() string { return "LOG" })
	if got == "" {
		t.Error("a kea that added an interface but never logged DHCP4_STARTED was reported ready")
	}
}

// TestKeaPid_NoProcessIsZero keeps a false attribution out of the
// message. keaReadiness only asks the kernel when the pid is positive,
// so a helper that answered 1 for "no process" would report the
// confinement of the container's init as if it were kea's — a
// confident, specific and wrong explanation, which is worse than none.
func TestKeaPid_NoProcessIsZero(t *testing.T) {
	if got := (&EphemeralFixture{}).keaPid(); got != 0 {
		t.Errorf("keaPid() with no command = %d, want 0", got)
	}
	if got := (&EphemeralFixture{cmd: &exec.Cmd{}}).keaPid(); got != 0 {
		t.Errorf("keaPid() with an unstarted command = %d, want 0", got)
	}
}
