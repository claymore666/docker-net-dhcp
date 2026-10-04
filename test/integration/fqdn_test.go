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
// name proves the option was sent; the default-off half is TestFQDN_Option81OnlyWithRegisterDNS below (#261, #861).

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

// With --log-dhcp dnsmasq logs every option of a reply, and it echoes option 81 only to a client that sent one, so a
// "sent size ... option: 81" line is the server's own evidence that option 81 arrived. The control runs first: if the
// observer cannot see option 81 on a register_dns network, the absence on the default network proves nothing (#861).

// TestFQDN_Option81OnlyWithRegisterDNS checks that a v4 client sends option 81 with register_dns=true and omits it by default (#261, #861).
func TestFQDN_Option81OnlyWithRegisterDNS(t *testing.T) {
	cases := []struct {
		name        string
		opts        map[string]string
		wantOption  bool
		netName     string
		ctrName     string
		failureHint string
	}{
		{
			name:       "control_register_dns_true",
			opts:       map[string]string{"parent": harness.EphemeralHostVeth, "register_dns": "true"},
			wantOption: true,
			netName:    "dh-itest-fqdn81-on",
			ctrName:    "dh-itest-fqdn81-on-ctr",
			failureHint: "the observer cannot see option 81 even on a register_dns=true network, " +
				"so the absence case below would pass whatever the client sends",
		},
		{
			name:       "default_sends_no_option_81",
			opts:       map[string]string{"parent": harness.EphemeralHostVeth},
			wantOption: false,
			netName:    "dh-itest-fqdn81-off",
			ctrName:    "dh-itest-fqdn81-off-ctr",
			failureHint: "a client on a network without register_dns sent option 81, which asks the " +
				"server to register the container in DNS",
		},
	}
	for _, c := range cases {
		if !t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			ef := harness.NewEphemeralFixture(t, harness.WithDNS("dh.test"))
			t.Cleanup(func() {
				if t.Failed() {
					ef.DumpLogs(func(s string) { t.Log(s) })
					harness.DumpPluginLog(t)
				}
			})

			harness.CreateNetwork(t, ctx, c.netName, "macvlan", c.opts)
			_, ip, mac := harness.RunContainer(t, ctx, c.netName, c.ctrName)
			t.Logf("bound: ip=%s mac=%s", ip, mac)

			// The request carried the container name in option 12 or 81; without this line the server never read a
			// name and "no option 81 echoed" would be about an empty exchange (#861).
			deadline := time.Now().Add(15 * time.Second)
			for ef.CountLogLines("client provides name", c.ctrName) == 0 && time.Now().Before(deadline) {
				time.Sleep(200 * time.Millisecond)
			}
			if ef.CountLogLines("client provides name", c.ctrName) == 0 {
				t.Fatalf("the server never logged a name for %s within 15s: no request was observed", c.ctrName)
			}

			if c.wantOption {
				deadline = time.Now().Add(15 * time.Second)
				for ef.CountLogLines("sent size", "option: 81") == 0 && time.Now().Before(deadline) {
					time.Sleep(200 * time.Millisecond)
				}
			}
			got := ef.CountLogLines("sent size", "option: 81")
			if c.wantOption && got == 0 {
				t.Errorf("no reply option 81 within 15s of the lease: %s", c.failureHint)
			}
			if !c.wantOption && got != 0 {
				t.Errorf("%d reply lines carry option 81: %s", got, c.failureHint)
			}
		}) {
			return
		}
	}
}
