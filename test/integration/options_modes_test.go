// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// Cases for the per-mode option matrix in docs/reference.md that had only a macvlan (or bridge) case (#1321).

func optionModesDump(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			fixture.DumpBridgeLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})
}

// TestClientID_OverrideReachesTheServerOnBridgeAndIpvlan checks that client_id arrives as option 61 on the lease line for this container's address (#1321).
func TestClientID_OverrideReachesTheServerOnBridgeAndIpvlan(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		lease func() string
	}{
		{"bridge", fixture.BridgeLeaseFile},
		{"ipvlan", fixture.LeaseFile},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			optionModesDump(t)

			// A per-run suffix keeps a line left by an earlier run on this address from satisfying the wait.
			clientID := fmt.Sprintf("dh-itest-cid-%s-%d", tc.mode, time.Now().UnixNano()%1000000)
			netName := "dh-itest-cid-" + tc.mode
			harness.CreateNetwork(t, ctx, netName, tc.mode, map[string]string{"client_id": clientID})
			_, ipv4, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")

			want := "00:" + colonHex([]byte(clientID))
			var got string
			for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
				fields, err := harness.LeaseFileLine(tc.lease(), ipv4)
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("read lease file %s: %v", tc.lease(), err)
				}
				if fields != nil {
					if got = fields[len(fields)-1]; strings.EqualFold(got, want) {
						return
					}
				}
			}
			t.Errorf("the lease line for %s carries client-id %q, want %q (the client_id option)", ipv4, got, want)
		})
	}
}

// TestVendorClass_BridgeAndIpvlan checks that vendor_class steers the offered router on bridge and ipvlan, and that the default class does not (#1321).
func TestVendorClass_BridgeAndIpvlan(t *testing.T) {
	for _, tc := range []struct {
		mode, tagged, untagged string
	}{
		{"bridge", harness.BridgeTaggedGateway, bridgeServerIP()},
		{"ipvlan", harness.TestTaggedGateway, harness.DefaultGateway},
	} {
		for _, override := range []bool{true, false} {
			name, want := tc.mode+"-default", tc.untagged
			var opts map[string]string
			if override {
				name, want = tc.mode+"-tagged", tc.tagged
				opts = map[string]string{"vendor_class": harness.TestVendorClass}
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				optionModesDump(t)

				netName := "dh-itest-vc-" + name
				harness.CreateNetwork(t, ctx, netName, tc.mode, opts)
				id, _, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")

				out := harness.ExecOutput(t, ctx, id, "ip", "route", "show", "default")
				if gw := defaultRouteGateway(t, out); gw != want {
					t.Errorf("default route via %s, want %s (vendor_class override=%v):\n%s", gw, want, override, out)
				}
			})
		}
	}
}

// TestUserClass_IpvlanRoutesViaTaggedGateway checks that user_class reaches the ipvlan endpoint's DISCOVER, and that a network without it does not match the rule (#1321).
func TestUserClass_IpvlanRoutesViaTaggedGateway(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts map[string]string
		want string
	}{
		{"tagged", map[string]string{"user_class": harness.TestUserClass}, harness.TestUserClassGateway},
		{"default", nil, harness.DefaultGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			optionModesDump(t)

			netName := "dh-itest-uc-ipv-" + tc.name
			harness.CreateNetwork(t, ctx, netName, "ipvlan", tc.opts)
			id, _, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")

			out := harness.ExecOutput(t, ctx, id, "ip", "route", "show", "default")
			if gw := defaultRouteGateway(t, out); gw != tc.want {
				t.Errorf("default route via %s, want %s:\n%s", gw, tc.want, out)
			}
		})
	}
}

// serverKindsSince is serverKinds over the log bytes written after off: every ipvlan child carries the parent's MAC, so
// the MAC alone would also pick up the lines of every earlier ipvlan container (#1321).
func serverKindsSince(logPath string, off int64, mac string) []string {
	data, err := os.ReadFile(logPath)
	if err != nil || int64(len(data)) < off {
		return nil
	}
	var kinds []string
	for _, line := range strings.Split(string(data[off:]), "\n") {
		if !strings.Contains(strings.ToLower(line), strings.ToLower(mac)) {
			continue
		}
		if m := rapidKindLine.FindStringSubmatch(line); m != nil {
			kinds = append(kinds, m[1])
		}
	}
	return kinds
}

// TestRapidCommit_IpvlanTakesTheTwoMessageLease checks that rapid_commit reaches the ipvlan endpoint's DISCOVER, and that a network without it keeps the four-message exchange (#1321).
func TestRapidCommit_IpvlanTakesTheTwoMessageLease(t *testing.T) {
	for _, rapid := range []bool{true, false} {
		name, opts := "off", map[string]string(nil)
		if rapid {
			name, opts = "on", map[string]string{"rapid_commit": "true"}
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			optionModesDump(t)

			netName := "dh-itest-rc-ipv-" + name
			harness.CreateNetwork(t, ctx, netName, "ipvlan", opts)
			// Marked after the create, whose preflight probe is also logged, and before the endpoint's exchange.
			var off int64
			if fi, err := os.Stat(fixture.DnsmasqLog()); err == nil {
				off = fi.Size()
			}
			id, ipv4, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
			mac := containerLinkMAC(t, ctx, id)
			if mac == "" {
				t.Fatal("could not read the container link's MAC, so the exchange cannot be told from other ipvlan children's")
			}
			assertPoolAddress(t, ipv4, harness.IsInPool)

			var got []string
			for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
				if got = serverKindsSince(fixture.DnsmasqLog(), off, mac); len(got) >= 2 && got[len(got)-1] == "ACK" {
					break
				}
			}
			if rapid {
				if len(got) < 2 || got[0] != "DISCOVER" || got[1] != "ACK" {
					t.Errorf("the server logged %v for %s, want it to start with [DISCOVER ACK]", got, mac)
				}
				noOffer(t, got, mac)
				return
			}
			if err := harness.FourMessageExchange(got); err != nil {
				t.Errorf("%s: %v", mac, err)
			}
		})
	}
}

// tmp6DUIDOf waits for the container to hold a global IPv6 address the server's lease file lists and returns the DUID
// it lists it under. An ipvlan endpoint's DUID is a per-endpoint DUID-UUID, not the MAC form (#895), so the address is the key.
func tmp6DUIDOf(t *testing.T, ctx context.Context, ef *harness.EphemeralFixture, ctrID string) string {
	t.Helper()
	for end := time.Now().Add(harness.IPAcquisitionBudget); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		for a := range tmp6GlobalSet(t, ctx, ctrID) {
			if duid := ef.V6LeaseDUID(a.String()); duid != "" {
				return duid
			}
		}
	}
	t.Fatal("no global IPv6 address of the container appeared in the server's lease file, so the server's view of this endpoint cannot be read")
	return ""
}

// TestIPv6Temporary_IpvlanHoldsAStableAndATemporaryAddress checks that ipv6_temporary reaches the ipvlan endpoint's Solicit and installs the IA_TA address, and that a network without it asks for none (#1321).
func TestIPv6Temporary_IpvlanHoldsAStableAndATemporaryAddress(t *testing.T) {
	for _, temporary := range []bool{true, false} {
		name := "off"
		opts := map[string]string{"parent": harness.EphemeralHostVeth, "ipv6_mode": "dhcp"}
		if temporary {
			name, opts["ipv6_temporary"] = "on", "true"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
			defer cancel()
			ef := tmp6Ephemeral(t)

			netName := "dh-itest-tmp6-ipv-" + name
			netID := harness.CreateNetwork(t, ctx, netName, "ipvlan", opts)
			id, _, _ := harness.RunContainer(t, ctx, netName, netName+"-ctr")
			duid := tmp6DUIDOf(t, ctx, ef, id)
			if temporary {
				tmp6AssertHeld(t, ctx, ef.V6LeaseAddrs, id, netName, netID, duid)
				return
			}
			tmp6AssertNone(t, ctx, ef.V6LeaseAddrs, id, netID, duid)
		})
	}
}
