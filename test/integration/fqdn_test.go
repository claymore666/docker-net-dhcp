//go:build integration

package integration

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/devplayer0/docker-net-dhcp/test/integration/harness"
)

// TestFQDN_RegistersInDNS verifies the opt-in FQDN option (#261): with
// `-o register_dns=true`, the plugin's dhcpcd sends the DHCP FQDN option
// (81), the DHCP server registers <hostname>.<domain> in its DNS, and
// the container becomes resolvable by name to its leased IP.
//
// The fixture runs dnsmasq with DNS enabled, a domain, and --dhcp-fqdn
// (WithDNS). --dhcp-fqdn registers ONLY clients that send the FQDN
// option, ignoring plain option-12 hostnames — so this resolving is
// itself the proof that the FQDN option (not the bare hostname hint) is
// what landed. The default-off case (no `fqdn` directive emitted at all)
// is pinned by the unit tests (TestRenderConfig_FQDN, TestFQDNMode).
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

	// Resolve <hostname>.<domain> against the fixture's own DNS (a custom
	// resolver dialing the fixture, since it listens on a private veth and
	// a high port). Poll: registration lands once the client's FQDN-
	// carrying bind reaches dnsmasq, a beat after the lease.
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
