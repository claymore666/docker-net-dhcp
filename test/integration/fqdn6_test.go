// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"bytes"
	"context"
	"net"
	"os"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// fqdn6Budget bounds each wait on the server's DNS and lease table, as TestFQDN_RegistersInDNS does on v4 (#261).
const fqdn6Budget = 30 * time.Second

// fqdn6Start starts a dual-stack container on a V6Managed segment whose server answers DNS, capturing its DHCPv6
// messages, and returns the capture, the container's v4 and v6 addresses and its name (#1029).
func fqdn6Start(t *testing.T, ctx context.Context, netName string, extra map[string]string) (*harness.V6Fixture, *harness.DHCPv6Capture, string, string, string) {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	f := harness.NewV6FixtureWithArgs(t, harness.V6Managed,
		append(harness.RangeArgsFor(harness.V6Managed), harness.V6DNSArgs()...))
	dumpOnFailure(t, f)
	cap6 := f.StartDHCPv6Capture()
	t.Cleanup(func() {
		if t.Failed() {
			cap6.Dump(func(s string) { t.Log(s) })
		}
	})

	opts := map[string]string{"ipv6": "", "ipv6_mode": "dhcp"}
	for k, v := range extra {
		opts[k] = v
	}
	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, netName, opts)
	if err != nil {
		t.Fatalf("ContainerStart on %s: %v", netName, err)
	}
	ins, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	ep := ins.NetworkSettings.Networks[netName]
	if ep == nil || ep.IPAddress == "" || ep.GlobalIPv6Address == "" {
		t.Fatalf("container on %s is not dual-stack: endpoint %+v", netName, ep)
	}
	if ip := net.ParseIP(ep.GlobalIPv6Address); ip == nil || !(&net.IPNet{IP: net.ParseIP(harness.V6Prefix), Mask: net.CIDRMask(64, 128)}).Contains(ip) {
		t.Fatalf("GlobalIPv6Address %q is not in the fixture's pool %s/64", ep.GlobalIPv6Address, harness.V6Prefix)
	}
	return f, cap6, ep.IPAddress, ep.GlobalIPv6Address, netName + "-ctr"
}

// fqdn6Resolver asks the fixture's own dnsmasq, which builds its A and AAAA records from its leases.
func fqdn6Resolver(f *harness.V6Fixture) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", f.DNSAddr())
		},
	}
}

// fqdn6Lookup polls the fixture's DNS for name in family ("ip4" or "ip6") until want answers or budget ends (#1029).
func fqdn6Lookup(ctx context.Context, res *net.Resolver, family, name, want string, budget time.Duration) ([]net.IP, error) {
	deadline := time.Now().Add(budget)
	for {
		lk, cancel := context.WithTimeout(ctx, 2*time.Second)
		got, err := res.LookupIP(lk, family, name)
		cancel()
		for _, a := range got {
			if a.Equal(net.ParseIP(want)) {
				return got, nil
			}
		}
		if !time.Now().Before(deadline) {
			return got, err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// fqdn6LeaseName polls the fixture's lease file for the name on the v6 lease of addr until it equals want.
func fqdn6LeaseName(t *testing.T, f *harness.V6Fixture, addr, want string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		data, err := os.ReadFile(f.LeaseFile())
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read lease file: %v", err)
		}
		got, _ := harness.DnsmasqLease6Name(string(data), addr)
		if got == want || !time.Now().Before(deadline) {
			return got
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestFQDN_V6RegistersAAAA checks that register_dns sends the name in option 39 and the server answers its AAAA (#1029).
func TestFQDN_V6RegistersAAAA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	f, cap6, ip4, ip6, ctr := fqdn6Start(t, ctx, "dh-itest-fqdn6", map[string]string{"register_dns": "true"})
	name := ctr + "." + harness.V6DNSDomain
	res := fqdn6Resolver(f)

	if got, err := fqdn6Lookup(ctx, res, "ip6", name, ip6, fqdn6Budget); !containsIP(got, ip6) {
		t.Errorf("%s has AAAA %v (err %v) after %s, want %s: the DHCPv6 server did not register the name the "+
			"lease was given", name, got, err, fqdn6Budget, ip6)
	}
	if got, err := fqdn6Lookup(ctx, res, "ip4", name, ip4, fqdn6Budget); !containsIP(got, ip4) {
		t.Errorf("%s has A %v (err %v), want %s: the v4 half of the same name is missing", name, got, err, ip4)
	}
	if got := fqdn6LeaseName(t, f, ip6, ctr, fqdn6Budget); got != ctr {
		t.Errorf("the server's v6 lease line for %s names %q, want %q", ip6, got, ctr)
	}
	if n := f.CountLogLines("DHCPREPLY", ip6, ctr); n == 0 {
		t.Errorf("the server logged no DHCPREPLY naming %s for %s", ctr, ip6)
	}

	// The plugin's bind report, read after the server's own evidence above so it is the report of a real S=1 Reply.
	if n := harness.CountPluginLogLines(t, ctx, "registers the AAAA record for this name", ctr); n == 0 {
		t.Errorf("the plugin logged no report of the server's option 39 reply for %s", ctr)
	}

	// RFC 4704 section 5: Solicit and Request carry it, with S set, and the name the lease table shows.
	msgs, ok := cap6.AwaitClientMessages([]uint8{harness.DHCPv6Solicit, harness.DHCPv6Request}, harness.IPAcquisitionBudget)
	if !ok {
		t.Fatalf("no Solicit and Request captured: %s\n%s", cap6.SeenTally(), harness.FormatDHCPv6Messages(msgs))
	}
	want := harness.ClientFQDNOption(0x01, ctr)
	for _, m := range msgs {
		if !m.FromClient {
			continue
		}
		carries := m.Type == harness.DHCPv6Solicit || m.Type == harness.DHCPv6Request ||
			m.Type == harness.DHCPv6Renew || m.Type == harness.DHCPv6Rebind
		if carries && !bytes.Equal(m.ClientFQDN, want) {
			t.Errorf("client message type %d carries option 39 %x, want %x (S=1, name %s)", m.Type, m.ClientFQDN, want, ctr)
		}
		if !carries && m.ClientFQDN != nil {
			t.Errorf("client message type %d carries option 39 %x; RFC 4704 section 5 allows it only in Solicit, "+
				"Request, Renew and Rebind", m.Type, m.ClientFQDN)
		}
	}
}

// TestFQDN_V6UnsetRegistersNoAAAA checks that without register_dns no name goes out on v6, so <ctr>.dh6.test has an A
// record from option 12 and no AAAA (#1029, decision (a)).
func TestFQDN_V6UnsetRegistersNoAAAA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	f, cap6, ip4, ip6, ctr := fqdn6Start(t, ctx, "dh-itest-fqdn6u", nil)
	name := ctr + "." + harness.V6DNSDomain
	res := fqdn6Resolver(f)

	// The A answer is the gate: once option 12 reached the server, on either attach route (#961), a v6 name would have too.
	if got, err := fqdn6Lookup(ctx, res, "ip4", name, ip4, fqdn6Budget); !containsIP(got, ip4) {
		t.Fatalf("%s has A %v (err %v) after %s, want %s: the v4 name never reached the server, so nothing "+
			"below would be a measurement", name, got, err, fqdn6Budget, ip4)
	}
	if got, _ := fqdn6Lookup(ctx, res, "ip6", name, ip6, 5*time.Second); len(got) != 0 {
		t.Errorf("%s has AAAA %v on a network without register_dns, want none (#1029 (a))", name, got)
	}
	if got := fqdn6LeaseName(t, f, ip6, "*", fqdn6Budget); got != "*" {
		t.Errorf("the server's v6 lease line for %s names %q, want \"*\": no name goes out on v6 without "+
			"register_dns", ip6, got)
	}
	msgs, ok := cap6.AwaitClientMessages([]uint8{harness.DHCPv6Solicit, harness.DHCPv6Request}, harness.IPAcquisitionBudget)
	if !ok {
		t.Fatalf("no Solicit and Request captured, so no absence of option 39 is shown: %s\n%s", cap6.SeenTally(),
			harness.FormatDHCPv6Messages(msgs))
	}
	for _, m := range msgs {
		if m.FromClient && m.ClientFQDN != nil {
			t.Errorf("client message type %d carries option 39 %x on a network without register_dns", m.Type, m.ClientFQDN)
		}
	}
}

func containsIP(got []net.IP, want string) bool {
	w := net.ParseIP(want)
	for _, a := range got {
		if a.Equal(w) {
			return true
		}
	}
	return false
}
