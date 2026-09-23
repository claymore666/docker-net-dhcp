// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
)

// RFC 9915 section 18.2.11: a Reconfigure is answered by Renew, Rebind or Information-request, and #925 routes each
// through the existing changed-configuration path.
// Bound: no library event names the message that started an exchange, so these tests pin the path and cannot observe a
// Reconfigure.

// reconfV6Lease is a bound DHCPv6 lease, as lease.Lease.
func reconfV6Lease(addr, dns string, domain string, now time.Time) lease.Lease {
	return lease.Lease{
		Addr:      netip.MustParsePrefix(addr),
		DNS:       []netip.Addr{netip.MustParseAddr(dns)},
		Domain:    domain,
		IAID:      0xac110002,
		Expire:    now.Add(time.Hour),
		Preferred: now.Add(30 * time.Minute),
		Valid:     now.Add(time.Hour),
	}
}

func TestReconfigurePath_AChangedLeaseCarriesTheNewParametersToThePlugin(t *testing.T) {
	now := time.Now()
	old := reconfV6Lease("2001:db8::5/128", "2001:db8::53", "old.example", now)
	fresh := reconfV6Lease("2001:db8::5/128", "2001:db8::35", "new.example", now)

	outRenew, emit, renewedAt := translateOne(
		lease.Event{Kind: lease.Renewed, Lease: old}, now, time.Time{}, netip.Prefix{})
	if !emit {
		t.Fatal("a Renewed emitted nothing, so a reconfigured client's Renew never reaches the plugin")
	}
	if outRenew.Type != "renew" {
		t.Errorf("Renewed translated to %q, want \"renew\"", outRenew.Type)
	}

	// Outside the coalescing window this Changed carries the configuration on its own (#925).
	outChanged, emit, _ := translateOne(
		lease.Event{Kind: lease.Changed, Lease: fresh},
		now.Add(10*coalesceWindow), renewedAt, netip.Prefix{})
	if !emit {
		t.Fatal("a Changed outside the coalescing window emitted nothing. RFC 9915 section " +
			"18.2.11's Renew and Rebind answers land here, and a reconfigured lease that " +
			"emits nothing leaves the container configured with what the server just replaced")
	}
	if outChanged.Type != "renew" {
		t.Errorf("Changed translated to %q, want \"renew\": #925 asks for the EXISTING "+
			"changed-configuration path and no plugin-side state of its own", outChanged.Type)
	}
	if got := outChanged.Data.IP; got != "2001:db8::5/128" {
		t.Errorf("the event carries IP %q, want the reconfigured lease's address", got)
	}
	if len(outChanged.Data.DNSServers) != 1 || outChanged.Data.DNSServers[0] != "2001:db8::35" {
		t.Errorf("the event carries DNS %v, want the server's NEW resolver. A reconfiguration "+
			"that reached the plugin with the old values is a reconfiguration that did nothing, "+
			"and nothing in the plugin would say so", outChanged.Data.DNSServers)
	}
	if outChanged.Data.Domain != "new.example" {
		t.Errorf("the event carries domain %q, want %q", outChanged.Data.Domain, "new.example")
	}
}

// The library keeps a bound client's binding across an Information-request, so the event must carry no address (#925).

func TestReconfigurePath_AnInformationRequestAnswerIsAConfigEvent(t *testing.T) {
	now := time.Now()

	out, emit, _ := translateOne(lease.Event{
		Kind: lease.Configured,
		Config: lease.Configuration{
			DNS:    []netip.Addr{netip.MustParseAddr("2001:db8::35")},
			Search: []string{"new.example"},
		},
	}, now, time.Time{}, netip.Prefix{})
	if !emit {
		t.Fatal("a Configured emitted nothing, so the Information-request answer RFC 9915 " +
			"section 18.2.11 lets a server ask for never reaches the plugin")
	}
	if out.Type != "config" {
		t.Errorf("Configured translated to %q, want \"config\"", out.Type)
	}
	if out.Data.IP != "" {
		t.Errorf("the config event carries an address %q. Section 18.2.6's answer has none, and "+
			"a bound client answering a Reconfigure keeps the address it already holds",
			out.Data.IP)
	}
	if len(out.Data.DNSServers) != 1 || out.Data.DNSServers[0] != "2001:db8::35" {
		t.Errorf("the config event carries DNS %v, want the resolver the Reply named: that is "+
			"the whole content of a stateless reconfiguration", out.Data.DNSServers)
	}
}
