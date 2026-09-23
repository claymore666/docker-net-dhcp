// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// A DHCPv6 information reply is address-less configuration: it is recorded and
// propagated without touching the address state machine (#815).

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

	if p.leasesObtainedV6.Load() != 0 || p.leasesRenewedV6.Load() != 0 ||
		p.leasesObtainedV4.Load() != 0 || p.leasesRenewedV4.Load() != 0 {
		t.Errorf("a config event moved a lease counter")
	}
	if p.dhcpTimeoutsV6.Load() != 0 || p.naksReceivedV6.Load() != 0 {
		t.Errorf("a config event moved a failure counter")
	}

	if m.boundV6.Load() {
		t.Errorf("a config event claimed ownership of a v6 binding")
	}
	if v4, v6 := m.lastIPs(); v4 != nil || v6 != nil {
		t.Errorf("a config event recorded an address: v4=%v v6=%v", v4, v6)
	}
}

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

func TestHandleEvent_ConfigPropagatesTheDNSItCarries(t *testing.T) {
	cases := []struct {
		name    string
		opts    DHCPNetworkOptions
		dns     []string
		wantHit bool
	}{
		{"opted in, servers supplied", DHCPNetworkOptions{PropagateDNS: true}, []string{"2001:db8::53"}, true},
		{"opted out", DHCPNetworkOptions{}, []string{"2001:db8::53"}, false},
		{"opted in, no servers", DHCPNetworkOptions{PropagateDNS: true}, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDocker{inspectErr: errors.New("no such network")}
			p := &Plugin{}
			m := newDHCPManager(f, JoinRequest{NetworkID: "net1", EndpointID: "ep1"}, tc.opts).withPlugin(p)

			m.handleEvent(dhcp.Event{
				Type: "config",
				Data: dhcp.Info{DNSServers: tc.dns, SearchList: []string{"corp.example"}},
			}, true)

			if got := f.inspectCalls > 0; got != tc.wantHit {
				t.Errorf("config event reached the DNS propagation path = %v, want %v "+
					"(NetworkInspect calls = %d). A stateless network's entire answer is "+
					"its options; dropping them leaves the container with no resolver and "+
					"nothing in the counters to say so.",
					got, tc.wantHit, f.inspectCalls)
			}
		})
	}
}

func TestHandleEvent_ConfigLogsTheOptionsItObserved(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	p := &Plugin{}
	m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"}, DHCPNetworkOptions{}).withPlugin(p)

	m.handleEvent(dhcp.Event{
		Type: "config",
		Data: dhcp.Info{SearchList: []string{"corp.example"}, NTPServers: []string{"2001:db8::123"}},
	}, true)

	var found *log.Entry
	for _, e := range hook.AllEntries() {
		if e.Message == "DHCP options received" {
			found = e
			break
		}
	}
	if found == nil {
		t.Fatalf("a config event carrying a search list and an NTP server logged no observed "+
			"options; entries: %v", messagesOf(hook.AllEntries()))
	}
	if got := found.Data["search"]; !reflect.DeepEqual(got, []string{"corp.example"}) {
		t.Errorf("observed-options search field = %#v, want [corp.example]", got)
	}
	if got := found.Data["ntp"]; !reflect.DeepEqual(got, []string{"2001:db8::123"}) {
		t.Errorf("observed-options ntp field = %#v, want [2001:db8::123]", got)
	}

	hook.Reset()
	m.handleEvent(dhcp.Event{Type: "config", Data: dhcp.Info{DNSServers: []string{"2001:db8::53"}}}, true)
	for _, e := range hook.AllEntries() {
		if e.Message == "DHCP options received" {
			t.Errorf("a config event with no observable options still logged them: %#v", e.Data)
		}
	}
}

func TestHandleEvent_ConfigIsAuditedAsConfigNotAsALease(t *testing.T) {
	var failures atomic.Int32
	p := &Plugin{}
	p.ledger = newLeaseLedger(filepath.Join(t.TempDir(), ledgerFileName), &failures)

	m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"},
		DHCPNetworkOptions{AuditLog: true}).withPlugin(p)

	m.handleEvent(dhcp.Event{
		Type: "config",
		Data: dhcp.Info{DNSServers: []string{"2001:db8::53"}, SearchList: []string{"corp.example"}},
	}, true)

	entries := readLedgerLines(t, p.ledger.path)
	if len(entries) != 1 {
		t.Fatalf("config event wrote %d ledger entries, want 1: %+v", len(entries), entries)
	}
	if entries[0].Kind != "config" {
		t.Errorf("ledger kind = %q, want \"config\". An information reply recorded as a lease "+
			"is the audit trail agreeing with a claim the plugin never had grounds to make.",
			entries[0].Kind)
	}
	if entries[0].IP != "" {
		t.Errorf("ledger IP = %q, want empty — a config event has no address", entries[0].IP)
	}
	if got := failures.Load(); got != 0 {
		t.Errorf("ledger_write_failures = %d, want 0", got)
	}
}

func messagesOf(entries []*log.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Message)
	}
	return out
}
