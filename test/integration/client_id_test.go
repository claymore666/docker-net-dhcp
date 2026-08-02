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

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestClientID_OverrideAppearsInLeaseFile is the v0.9.0 / T2-3
// proof-of-wire-encoding for #106's client_id driver-opt.
//
// dnsmasq's lease file stores option-61 client identifiers as
// `<type-byte-hex>:<payload-hex>` in the last column. The plugin
// always sends type 0x00 (opaque, RFC 2132), so an operator
// override of `client_id=<string>` must show up as
// `00:<hex(string)>` in the lease line for the container's MAC.
//
// Asserting on the lease file is more robust than scraping dnsmasq
// stdout: the lease file format is stable across dnsmasq versions,
// and the line is written exactly once per ACK so we don't need to
// chase log-stream timing.
//
// Closes the integration-coverage gap noted in the post-T2-3 audit:
// the dhcpcd config-shape unit tests only check that the `clientid`
// directive is emitted; this test proves the bytes also reach
// the upstream server.
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

	// Wire-format expectation: type byte 0x00 + ASCII bytes,
	// rendered by dnsmasq as colon-separated hex pairs.
	wantHex := "00:" + colonHex([]byte(operatorClientID))

	// Lease line lands when dnsmasq writes the lease database. That
	// happens immediately on ACK; the only delay is the FS sync.
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

// TestClientID_DefaultIsMACDerived proves on the wire what #371
// changed: without an operator override, a macvlan endpoint's option-61
// client-id is the container's MAC.
//
// This is the property the whole restart-stability fix rests on. The
// tombstone preserves the MAC, so a MAC-derived client-id is an identity
// the server still recognises after a restart — which is why IPv6, whose
// DUID/IAID has always been MAC-derived, never lost its address the way
// IPv4 did.
//
// Asserting the exact bytes (not just "isn't the override string") is
// deliberate. The previous version of this test only checked that the
// operator's string was absent, which a MAC-derived id, an
// endpoint-derived id, and an empty id would all have satisfied equally.
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

	// Find the lease line for our IP. Without an override the client-id
	// is the MAC, rendered by dnsmasq as 00:<mac hex> (type byte 0x00,
	// "opaque", is what the plugin always sends).
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

	// Lease format: <expiry> <mac> <ip> <hostname> <client-id>.
	// Last whitespace-separated field is the client-id.
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

	// The override must not leak into a network that didn't ask for it.
	if strings.Contains(lineForIP, "dh-itest-cid") {
		t.Errorf("default network should NOT carry the override string in client-id; got line: %s", lineForIP)
	}
}

// colonHex renders bytes as `aa:bb:cc:...` — matches dnsmasq's
// lease-file rendering of binary fields. encoding/hex's bare form
// is `aabbcc`, so we splice colons in.
func colonHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	enc := hex.EncodeToString(b)
	// Two hex chars per byte; insert ':' between every pair.
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
