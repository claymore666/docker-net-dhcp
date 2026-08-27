// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// #815: a DHCPv6 information reply is address-less configuration. The
// consumer must record it and propagate what it carries WITHOUT touching
// the address state machine — every assertion below is about something
// the lease path would have done and this path must not.

func TestHandleEvent_ConfigCountsAndTouchesNoLeaseState(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{plugin: p}

	m.handleEvent(dhcp.Event{
		Type: "config",
		Data: dhcp.Info{
			DNSServers: []string{"2001:db8::53"},
			SearchList: []string{"corp.example"},
		},
	}, true)

	if got := p.dhcpv6ConfigOnly.Load(); got != 1 {
		t.Errorf("dhcpv6_config_only = %d, want 1", got)
	}

	// Not a lease. Before #815 the reasonable-looking fix was to map
	// INFORM6 onto "bound"; had that shipped, every one of these would
	// have moved and the plugin would report leases it does not hold.
	if p.leasesObtainedV6.Load() != 0 || p.leasesRenewedV6.Load() != 0 ||
		p.leasesObtainedV4.Load() != 0 || p.leasesRenewedV4.Load() != 0 {
		t.Errorf("a config event moved a lease counter")
	}
	if p.dhcpTimeoutsV6.Load() != 0 || p.naksReceivedV6.Load() != 0 {
		t.Errorf("a config event moved a failure counter")
	}

	// markBound is the proof of ownership Stop reads to decide whether
	// dhcpcd may release a binding. A config event owns no binding.
	if m.boundV6.Load() {
		t.Errorf("a config event claimed ownership of a v6 binding")
	}
	if v4, v6 := m.lastIPs(); v4 != nil || v6 != nil {
		t.Errorf("a config event recorded an address: v4=%v v6=%v", v4, v6)
	}
}

// The counter is v6-only and has no v4 half. Dispatching a config event
// with v6=false — which the production path never does, but a future
// refactor could — must not silently invent a v4 series.
func TestHandleEvent_ConfigHasNoFamilySplit(t *testing.T) {
	p := &Plugin{}
	m := &dhcpManager{plugin: p}
	m.handleEvent(dhcp.Event{Type: "config"}, false)
	m.handleEvent(dhcp.Event{Type: "config"}, true)
	if got := p.dhcpv6ConfigOnly.Load(); got != 2 {
		t.Errorf("dhcpv6_config_only = %d, want 2 — the counter is not family-split", got)
	}
}

func TestHandleEvent_ConfigWithNilPluginIsSafe(t *testing.T) {
	m := &dhcpManager{plugin: nil}
	m.handleEvent(dhcp.Event{Type: "config", Data: dhcp.Info{DNSServers: []string{"2001:db8::53"}}}, true)
}

// The outage watchdog derives "are we currently trying to get a lease?"
// from the event stream. An information reply proves the server is
// reachable; it does NOT prove we hold a lease. If it were treated as
// one, a network that answers information requests and hands out no
// addresses would look healthy forever and dhcp_timeouts would never
// move — the precise failure #816 describes.
func TestOutageTracker_ConfigDoesNotAffirmALease(t *testing.T) {
	now := time.Now()
	o := &outageTracker{}

	// Drop into the acquiring state the way a real failure does.
	o.observe("leasefail", dhcp.Info{}, now)
	if !o.acquiring {
		t.Fatalf("leasefail should leave the tracker acquiring")
	}
	before := o.lastAffirmed

	o.observe("config", dhcp.Info{DNSServers: []string{"2001:db8::53"}, LeaseSeconds: 600}, now.Add(time.Minute))

	if !o.acquiring {
		t.Errorf("a config event cleared the acquiring state — the watchdog would go quiet " +
			"on a network that configures but never leases")
	}
	if o.lastAffirmed != before {
		t.Errorf("a config event restarted the lease deadline: %v -> %v", before, o.lastAffirmed)
	}
	if o.lapseAfter != 0 {
		t.Errorf("a config event installed a lease deadline from LeaseSeconds: %v", o.lapseAfter)
	}

	// The control: the same tracker, same instant, with the event that
	// IS proof of a lease. Without this, a tracker that ignored every
	// event would pass the assertions above.
	o.observe("bound", dhcp.Info{LeaseSeconds: 600}, now.Add(2*time.Minute))
	if o.acquiring {
		t.Errorf("bound should have cleared the acquiring state")
	}
	if o.lapseAfter == 0 {
		t.Errorf("bound should have installed a lease deadline")
	}
}

// The counter has to reach /Plugin.Health, not just exist. The
// reflection test that walks HealthResponse proves the FIELD is exposed;
// this proves the atom is the one behind it.
func TestHealthSnapshot_CarriesDHCPv6ConfigOnly(t *testing.T) {
	p := &Plugin{}
	if got := p.healthSnapshot().DHCPv6ConfigOnly; got != 0 {
		t.Fatalf("fresh plugin reports %d config-only events", got)
	}
	p.dhcpv6ConfigOnly.Add(3)
	if got := p.healthSnapshot().DHCPv6ConfigOnly; got != 3 {
		t.Errorf("healthSnapshot DHCPv6ConfigOnly = %d, want 3", got)
	}
}
