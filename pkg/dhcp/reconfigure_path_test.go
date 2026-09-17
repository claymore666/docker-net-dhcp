// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
)

// The plugin end of #925's first test: the two library events a
// server-initiated Reconfigure arrives on already carry its result into
// the plugin.
//
// RFC 9915 section 18.2.11 gives a Reconfigure three answers — "the
// client responds with a Renew message, a Rebind message, or an
// Information-request message as indicated by the Reconfigure Message
// option" — and each of the three ends in a library event this chassis
// already translates. A Renew or Rebind whose Reply differs from the
// lease in hand produces lease.Renewed and lease.Changed
// (proto/machine6.go stamps ActLeaseRenewed and, when the contents
// differ, ActLeaseChanged); an Information-request produces
// lease.Configured. #925's scope line says the reconfigured lease
// "flows through the existing changed-configuration path, the same one
// a renewal with new parameters uses. No new plugin-side state." These
// tests are what makes that claim checkable from this side.
//
// WHAT THESE TESTS CANNOT DO, said here rather than left to be
// discovered. They cannot distinguish their own subject. A constructed
// lease.Changed drives exactly the path a T1 renewal with new
// parameters drives, because nothing on a library event says which
// timer or which message started the exchange — and nothing should:
// #925 asks for no new plugin-side state, so the plugin is DESIGNED not
// to be able to tell a Reconfigure-driven renewal from any other. The
// value here is therefore a pin and not a demonstration: it holds the
// path #925 depends on, and it fails if that path stops carrying
// changed parameters through. A test that claimed to observe a
// Reconfigure would be claiming something this plugin cannot see.

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

// A Renew a Reconfigure asked for comes back with different parameters,
// and the plugin is told about the NEW ones.
//
// THE ASSERTION IS ON THE CONTENTS AND NOT ON THE EVENT TYPE. A
// translation that emitted "renew" carrying the OLD lease would pass
// any check that only read out.Type, and the container would keep
// resolving against a DNS server the segment has moved off — which is
// the entire operational content of a reconfiguration. So the new
// resolver and the new address are read out of the event.
func TestReconfigurePath_AChangedLeaseCarriesTheNewParametersToThePlugin(t *testing.T) {
	now := time.Now()
	old := reconfV6Lease("2001:db8::5/128", "2001:db8::53", "old.example", now)
	fresh := reconfV6Lease("2001:db8::5/128", "2001:db8::35", "new.example", now)

	// The Renewed that accompanies the same Reply, first, so renewedAt
	// is set the way the live path sets it.
	outRenew, emit, renewedAt := translateOne(
		lease.Event{Kind: lease.Renewed, Lease: old}, now, time.Time{}, netip.Prefix{})
	if !emit {
		t.Fatal("a Renewed emitted nothing, so a reconfigured client's Renew never reaches the plugin")
	}
	if outRenew.Type != "renew" {
		t.Errorf("Renewed translated to %q, want \"renew\"", outRenew.Type)
	}

	// The Changed that follows a renewal whose contents differ, OUTSIDE
	// the coalescing window. Inside it the plugin has already applied
	// this Reply on the "renew" above, which
	// TestTranslate_ARenewalIsNotCountedTwice pins; outside it this is
	// the event that carries a changed configuration on its own.
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

// The third of section 18.2.11's answers. An Information-request a
// Reconfigure asked for is answered with a Reply carrying configuration
// and no address, which the library stamps as ActConfigured and emits
// as lease.Configured.
//
// THE LEASE IS UNTOUCHED AND THE EVENT MUST NOT CARRY ONE. The library
// keeps a bound client's binding and its timers across that detour
// (proto's TestAReconfigureNamingInformationRequestKeepsTheLease); an
// event that arrived here carrying an empty address would have the
// plugin reconfigure a container's interface to have none.
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
