// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// dnsmasq's lease file stores option 61 as <type hex>:<payload hex> in its last field, written once per ACK; the
// plugin always sends type 0x00, opaque (RFC 2132, #106).

// TestClientID_OverrideAppearsInLeaseFile checks that a client_id override reaches the server as 00:<hex of the string> (#106).
func TestClientID_OverrideAppearsInLeaseFile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const operatorClientID = "dh-itest-cid"
	netName := "dh-itest-cid-override"
	ctrName := "dh-itest-cid-override-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"client_id": operatorClientID,
	})
	id, ipv4, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s: id=%s ip=%s mac=%s", ctrName, id[:12], ipv4, mac)

	wantHex := "00:" + colonHex([]byte(operatorClientID))

	deadline := time.Now().Add(5 * time.Second)
	var leaseContents string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fixture.LeaseFile())
		if err == nil {
			leaseContents = string(data)
			if strings.Contains(leaseContents, wantHex) {
				t.Logf("client_id %q surfaced in lease file as %s", operatorClientID, wantHex)
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Errorf("expected client_id encoded as %q in lease file %s within 5s\nfile contents:\n%s",
		wantHex, fixture.LeaseFile(), leaseContents)
}

// The tombstone keeps the MAC, so a MAC-derived client-id survives a restart; the exact bytes are asserted, since an
// absent override string is also true of an endpoint-derived or empty id (#371).

// TestClientID_DefaultIsMACDerived checks that without an override a macvlan endpoint's option 61 is 00:<its MAC> (#371).
func TestClientID_DefaultIsMACDerived(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	netName := "dh-itest-cid-default"
	ctrName := "dh-itest-cid-default-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, ipv4, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("container %s: id=%s ip=%s mac=%s", ctrName, id[:12], ipv4, mac)

	deadline := time.Now().Add(5 * time.Second)
	var lineForIP string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fixture.LeaseFile())
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, ipv4) {
					lineForIP = line
					break
				}
			}
		}
		if lineForIP != "" {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if lineForIP == "" {
		t.Fatalf("no lease line found for IP %s within 5s", ipv4)
	}
	t.Logf("lease line: %s", lineForIP)

	// dnsmasq lease line: <expiry> <mac> <ip> <hostname> <client-id>.
	fields := strings.Fields(lineForIP)
	if len(fields) < 5 {
		t.Fatalf("unexpected lease line shape (want >=5 fields): %q", lineForIP)
	}
	gotClientID := fields[len(fields)-1]

	macBytes, err := net.ParseMAC(mac)
	if err != nil {
		t.Fatalf("docker inspect returned an unparseable MAC %q: %v", mac, err)
	}
	wantClientID := "00:" + colonHex(macBytes)

	if !strings.EqualFold(gotClientID, wantClientID) {
		t.Errorf("client-id on the wire is %q, want %q (the MAC, #371).\nlease line: %s\n"+
			"If this is the endpoint-derived id instead, IPv4 no longer survives a restart and #370 is back.",
			gotClientID, wantClientID, lineForIP)
	}

	if strings.Contains(lineForIP, "dh-itest-cid") {
		t.Errorf("default network should NOT carry the override string in client-id; got line: %s", lineForIP)
	}
}

// colonHex renders bytes as aa:bb:cc, dnsmasq's lease-file form of a binary field.
func colonHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	enc := hex.EncodeToString(b)
	var out strings.Builder
	out.Grow(len(enc) + len(b) - 1)
	for i := 0; i < len(enc); i += 2 {
		if i > 0 {
			out.WriteByte(':')
		}
		out.WriteString(enc[i : i+2])
	}
	return out.String()
}
