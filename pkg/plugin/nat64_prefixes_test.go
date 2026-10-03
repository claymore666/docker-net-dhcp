// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

func nat64Manager() *dhcpManager {
	return newDHCPManager(nil, JoinRequest{EndpointID: "aaaaaaaaaaaabbbbbbbbbbbb", NetworkID: "111111111111222222222222"},
		DHCPNetworkOptions{Mode: ModeMacvlan})
}

func observedOptionsEntries(hook *logtest.Hook) []*log.Entry {
	var out []*log.Entry
	for _, e := range hook.AllEntries() {
		if e.Message == "DHCP options received" {
			out = append(out, e)
		}
	}
	return out
}

func TestLogObservedOptions_NamesThePREF64PrefixesAndStaysSilentWithoutThem(t *testing.T) {
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()
	m := nat64Manager()

	m.logObservedOptions(true, dhcp.Info{NAT64Prefixes: []string{"64:ff9b::/96", "2001:db8:64::/64"}})
	got := observedOptionsEntries(hook)
	if len(got) != 1 {
		t.Fatalf("an event carrying PREF64 logged %d observed-options lines, want 1", len(got))
	}
	if want := []string{"64:ff9b::/96", "2001:db8:64::/64"}; !reflect.DeepEqual(got[0].Data["nat64"], want) {
		t.Errorf("nat64 log field = %#v, want %v", got[0].Data["nat64"], want)
	}

	hook.Reset()
	m.logObservedOptions(true, dhcp.Info{NAT64Prefixes: []string{"64:ff9b::/96"}})
	if e := observedOptionsEntries(hook); len(e) != 1 || !reflect.DeepEqual(e[0].Data["nat64"], []string{"64:ff9b::/96"}) {
		t.Errorf("an event whose only observed option is one PREF64 prefix logged %d lines: %v", len(e), e)
	}

	hook.Reset()
	m.logObservedOptions(true, dhcp.Info{DNSServers: []string{"2001:db8::53"}})
	if n := len(observedOptionsEntries(hook)); n != 0 {
		t.Errorf("an event with no observable option, PREF64 included, logged %d observed-options lines", n)
	}

	hook.Reset()
	m.logObservedOptions(true, dhcp.Info{NTPServers: []string{"2001:db8::123"}})
	e := observedOptionsEntries(hook)
	if len(e) != 1 {
		t.Fatalf("an NTP-only event logged %d observed-options lines, want 1", len(e))
	}
	if _, ok := e[0].Data["nat64"]; ok {
		t.Errorf("an event without PREF64 logged an empty nat64 field: %#v", e[0].Data["nat64"])
	}
}

func TestHealthView_ShowsThePREF64PrefixesOfTheLastV6Event(t *testing.T) {
	prefixes := []string{"64:ff9b::/96"}

	t.Run("a v6 bound event carrying PREF64", func(t *testing.T) {
		m := nat64Manager()
		m.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{NAT64Prefixes: prefixes}}, true)
		if got := m.healthView().NAT64Prefixes; !reflect.DeepEqual(got, prefixes) {
			t.Errorf("nat64_prefixes = %v, want %v", got, prefixes)
		}
	})
	t.Run("a v6 renew replaces the list", func(t *testing.T) {
		m := nat64Manager()
		m.handleEvent(dhcp.Event{Type: "renew", Data: dhcp.Info{NAT64Prefixes: prefixes}}, true)
		next := []string{"2001:db8:64::/64"}
		m.handleEvent(dhcp.Event{Type: "renew", Data: dhcp.Info{NAT64Prefixes: next}}, true)
		if got := m.healthView().NAT64Prefixes; !reflect.DeepEqual(got, next) {
			t.Errorf("nat64_prefixes = %v after a renew naming %v", got, next)
		}
	})
	t.Run("a v6 event without PREF64 clears it", func(t *testing.T) {
		m := nat64Manager()
		m.handleEvent(dhcp.Event{Type: "renew", Data: dhcp.Info{NAT64Prefixes: prefixes}}, true)
		m.handleEvent(dhcp.Event{Type: "renew", Data: dhcp.Info{}}, true)
		if got := m.healthView().NAT64Prefixes; len(got) != 0 {
			t.Errorf("nat64_prefixes = %v after a lease event whose router state had none, want empty", got)
		}
	})
	t.Run("a v4 event does not clear it", func(t *testing.T) {
		m := nat64Manager()
		m.handleEvent(dhcp.Event{Type: "renew", Data: dhcp.Info{NAT64Prefixes: prefixes}}, true)
		m.handleEvent(dhcp.Event{Type: "renew", Data: dhcp.Info{IP: "192.0.2.17/24"}}, false)
		if got := m.healthView().NAT64Prefixes; !reflect.DeepEqual(got, prefixes) {
			t.Errorf("nat64_prefixes = %v after a v4 renew, want %v kept", got, prefixes)
		}
	})
	t.Run("a config event does not clear it", func(t *testing.T) {
		m := nat64Manager()
		m.handleEvent(dhcp.Event{Type: "renew", Data: dhcp.Info{NAT64Prefixes: prefixes}}, true)
		m.handleEvent(dhcp.Event{Type: "config", Data: dhcp.Info{DNSServers: []string{"2001:db8::53"}}}, true)
		if got := m.healthView().NAT64Prefixes; !reflect.DeepEqual(got, prefixes) {
			t.Errorf("nat64_prefixes = %v after an address-less config event, want %v kept", got, prefixes)
		}
	})
	t.Run("a router advert event carrying it", func(t *testing.T) {
		m := nat64Manager()
		m.handleEvent(dhcp.Event{Type: "routeradvert", Data: dhcp.Info{NAT64Prefixes: prefixes}}, true)
		if got := m.healthView().NAT64Prefixes; !reflect.DeepEqual(got, prefixes) {
			t.Errorf("nat64_prefixes = %v, want %v", got, prefixes)
		}
	})
	t.Run("the list is a copy", func(t *testing.T) {
		m := nat64Manager()
		in := []string{"64:ff9b::/96"}
		m.handleEvent(dhcp.Event{Type: "renew", Data: dhcp.Info{NAT64Prefixes: in}}, true)
		in[0] = "changed"
		m.healthView().NAT64Prefixes[0] = "changed"
		if got := m.healthView().NAT64Prefixes; !reflect.DeepEqual(got, prefixes) {
			t.Errorf("nat64_prefixes = %v, a caller's slice reached the record", got)
		}
	})
}

func TestEndpointHealth_NAT64PrefixesKeyIsAbsentWhenEmpty(t *testing.T) {
	empty, err := json.Marshal(EndpointHealth{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "nat64_prefixes") {
		t.Errorf("an endpoint with no PREF64 renders the key: %s", empty)
	}
	set, err := json.Marshal(EndpointHealth{NAT64Prefixes: []string{"64:ff9b::/96"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(set), `"nat64_prefixes":["64:ff9b::/96"]`) {
		t.Errorf("the key is missing or renamed: %s", set)
	}
}
