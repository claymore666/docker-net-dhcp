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

// The three tests below cover the EFFECT half of the config case. The
// ones above prove what a config event must NOT do; these prove what it
// must do, and they exist because each of the three effects could be
// deleted from the case with the whole suite still green.
//
// Each takes one effect and one observer, deliberately not combined: a
// single test asserting all three would go red for any of them and name
// none.

// A config event's whole payload is options. propagateDNS is the only
// thing in that branch that reaches the docker client, so the fake's
// call count IS the observation that it ran — audit is left opted out
// here precisely so nothing else can inspect and lend a false positive.
//
// Asserting the call rather than the written resolv.conf is deliberate:
// the write lands in another process's mount namespace and a unit test
// cannot see it. What is checkable here is that the reply reached the
// propagation path at all, which is the step a deletion removes.
func TestHandleEvent_ConfigPropagatesTheDNSItCarries(t *testing.T) {
	cases := []struct {
		name    string
		opts    DHCPNetworkOptions
		dns     []string
		wantHit bool
	}{
		{"opted in, servers supplied", DHCPNetworkOptions{PropagateDNS: true}, []string{"2001:db8::53"}, true},
		{"opted out", DHCPNetworkOptions{}, []string{"2001:db8::53"}, false},
		// An information reply that carried no servers must not clobber
		// what the container already has — propagateDNS' own empty-list
		// rule, and the control that keeps the case above from passing
		// merely because a config event touches docker at all.
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

// logObservedOptions is the only place a search list from an
// information reply becomes visible to an operator — it is not applied,
// not counted, and on a stateless network there is no lease line to
// carry it either. Deleting the call leaves the option silently
// received and silently discarded.
//
// The assertion reads the entry's FIELDS rather than its rendered text:
// the message alone would pass on any "DHCP options received" line,
// including one about a different family or a different event.
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

	// The other direction: a reply with nothing observable in it must
	// not produce the line. Without this the test above is satisfied by
	// a logObservedOptions that logs unconditionally, which is the
	// per-renewal noise it was written to avoid.
	hook.Reset()
	m.handleEvent(dhcp.Event{Type: "config", Data: dhcp.Info{DNSServers: []string{"2001:db8::53"}}}, true)
	for _, e := range hook.AllEntries() {
		if e.Message == "DHCP options received" {
			t.Errorf("a config event with no observable options still logged them: %#v", e.Data)
		}
	}
}

// The ledger is the forensic record, and it is keyed on the entry's
// KIND. "config" and "bound" are the same shape of row and differ only
// there, so mapping an information reply onto "bound" changes nothing a
// counter test can see: the health counters above already refuse that
// mis-mapping, and every one of them would still pass.
//
// The consequence is the one #815 names — the plugin reporting a lease
// it does not hold — recorded in the artifact an operator reaches for
// after the fact, where it is least likely to be questioned.
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
	// An information reply carries no address by definition. A
	// non-empty IP here would be an address the plugin does not hold,
	// written into the record that outlives the process.
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
