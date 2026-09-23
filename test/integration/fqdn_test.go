// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// dnsmasq --dhcp-fqdn registers only clients that send option 81 and ignores a bare option 12 hostname, so a resolved
// name proves the option was sent; the default-off case is in TestRenderConfig_FQDN and TestFQDNMode (#261).

// TestFQDN_RegistersInDNS checks that register_dns=true makes the container resolvable by name in the server's DNS (#261).
func TestFQDN_RegistersInDNS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const domain = "dh.test"
	netName := "dh-itest-fqdn"
	ctrName := "dh-itest-fqdn-ctr"

	ef := harness.NewEphemeralFixture(t, harness.WithDNS(domain))
	t.Cleanup(func() {
		if t.Failed() {
			ef.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	harness.CreateNetwork(t, ctx, netName, "macvlan", map[string]string{
		"parent":       harness.EphemeralHostVeth,
		"register_dns": "true",
	})
	id, ip, mac := harness.RunContainer(t, ctx, netName, ctrName)
	t.Logf("bound: ip=%s mac=%s; expecting %s.%s -> %s", ip, mac, ctrName, domain, ip)
	_ = id

	res := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", ef.DNSAddr())
		},
	}
	fqdn := ctrName + "." + domain

	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	var got []string
	for time.Now().Before(deadline) {
		lookupCtx, cancelLk := context.WithTimeout(ctx, 2*time.Second)
		got, lastErr = res.LookupHost(lookupCtx, fqdn)
		cancelLk()
		for _, a := range got {
			if a == ip {
				t.Logf("resolved %s -> %s after FQDN registration", fqdn, a)
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("FQDN %s never resolved to the leased IP %s within 30s (last result=%v err=%v) — "+
		"the server did not register the container via the DHCP FQDN option", fqdn, ip, got, lastErr)
}
