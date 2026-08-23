// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net"
	"reflect"
	"strings"
	"testing"
)

// The generated dhcpcd.conf is a line-oriented format with no quoting, so
// any value carrying a newline does not produce a malformed directive — it
// produces an extra one, and dhcpcd applies it. #692.
//
// The attack these tests describe was measured, not imagined: Docker does
// not validate a container's hostname (a newline survives into
// Config.Hostname), dhcpcd only warns about an unknown directive and keeps
// going, and a repeated directive resolves last-wins. renderConfig writes
// `duid` near the top and the hostname near the bottom, so an injected
// `duid` overrode the identity this plugin pinned.

const injected = "injected-marker"

// canary is a value that is harmless as a flat token and becomes a second
// directive the moment it is written unescaped.
var canary = "legit\nblacklist " + injected

// TestRenderConfig_NoValueCanIntroduceADirective is the structural
// guarantee, and it is written by REFLECTION on purpose.
//
// A hand-listed set of fields is the shape this repo keeps watching rot: a
// future dhcpcdParams field wired into the renderer with a bare Fprintf
// would leave a hand-written test passing. Walking the struct means the
// new field is covered the day it is added, and the test fails until it
// goes through directive().
func TestRenderConfig_NoValueCanIntroduceADirective(t *testing.T) {
	mac, err := net.ParseMAC("de:ad:be:ef:00:01")
	if err != nil {
		t.Fatal(err)
	}

	base := dhcpcdParams{Iface: "eth0", MAC: mac}
	v := reflect.ValueOf(base)
	typ := v.Type()

	covered := 0
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)

		p := base
		pv := reflect.ValueOf(&p).Elem()
		switch field.Type.Kind() {
		case reflect.String:
			pv.Field(i).SetString(canary)
		case reflect.Slice:
			// []string carries directive values (the server lists);
			// []byte is rendered as hex and cannot carry one.
			if field.Type.Elem().Kind() != reflect.String {
				continue
			}
			pv.Field(i).Set(reflect.ValueOf([]string{canary}))
		default:
			continue
		}
		covered++

		for _, line := range strings.Split(renderConfig(p), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "blacklist "+injected) {
				t.Errorf("field %s reached the config as its own directive:\n\t%q\n"+
					"every interpolated value must go through directive()", field.Name, line)
			}
		}
	}

	// Guard the guard: if the walk stopped finding fields, the loop above
	// would pass by testing nothing.
	if covered < 8 {
		t.Fatalf("walked only %d interpolatable fields; dhcpcdParams shrank or the walk broke", covered)
	}
}

// TestRenderConfig_HostnameCannotOverrideThePinnedIdentity is the concrete
// attack, kept alongside the general property because it is the one that
// has a victim: the DUID is derived from the endpoint MAC, every MAC on a
// bridge or macvlan segment is observable, and last-wins meant a container
// could claim another endpoint's binding.
func TestRenderConfig_HostnameCannotOverrideThePinnedIdentity(t *testing.T) {
	mac, err := net.ParseMAC("de:ad:be:ef:00:01")
	if err != nil {
		t.Fatal(err)
	}
	victim := "00:03:00:01:be:ef:be:ef:be:ef"

	conf := renderConfig(dhcpcdParams{
		Iface:    "eth0",
		MAC:      mac,
		Hostname: "web1\nduid " + victim,
	})

	var duids []string
	for _, line := range strings.Split(conf, "\n") {
		if strings.HasPrefix(line, "duid ") {
			duids = append(duids, line)
		}
	}
	if len(duids) != 1 {
		t.Fatalf("expected exactly one duid directive, got %d:\n%s", len(duids), conf)
	}
	if strings.Contains(duids[0], victim) {
		t.Errorf("the injected duid won: %q", duids[0])
	}
	if strings.Contains(conf, "web1\nduid") {
		t.Errorf("hostname was written raw:\n%s", conf)
	}
}

// TestRenderConfig_DenyListCannotBeVoidedByAHostname pins the #669
// interaction specifically. dhcpcd stops consulting a blacklist entirely
// once a whitelist exists, and the blacklist lines are written ABOVE the
// hostname — so an injected whitelist silently voided the operator's deny
// list. That combination is what makes shipping the deny list and this bug
// in the same release unacceptable.
func TestRenderConfig_DenyListCannotBeVoidedByAHostname(t *testing.T) {
	mac, err := net.ParseMAC("de:ad:be:ef:00:01")
	if err != nil {
		t.Fatal(err)
	}
	conf := renderConfig(dhcpcdParams{
		Iface:       "eth0",
		MAC:         mac,
		DenyServers: []string{"192.0.2.1"},
		Hostname:    "web1\nwhitelist 203.0.113.9",
	})
	if strings.Contains(conf, "whitelist") {
		t.Errorf("a hostname introduced a whitelist, voiding the deny list:\n%s", conf)
	}
	if !strings.Contains(conf, "blacklist 192.0.2.1") {
		t.Errorf("the operator's deny list did not survive:\n%s", conf)
	}
}

// TestSafeDirectiveValue_AcceptsWhatRealDeploymentsUse guards the opposite
// failure: a check strict enough to reject ordinary hostnames would turn a
// security fix into an outage. Docker accepts all of these, so we must too
// — the rule is about the file's STRUCTURE, not about well-formedness.
func TestSafeDirectiveValue_AcceptsWhatRealDeploymentsUse(t *testing.T) {
	for _, ok := range []string{
		"web1", "my_app", "MY-APP.example.com", "hôte", "a.b.c.d.e.f",
		strings.Repeat("x", 253), "container-1234567890abcdef",
	} {
		if !SafeDirectiveValue(ok) {
			t.Errorf("rejected a value Docker accepts and dhcpcd can carry: %q", ok)
		}
	}
	for _, bad := range []string{
		"a\nblacklist 1.2.3.4", "a\r\nduid 00", "a\rb", "a\x00b", "a\x7fb", "a\vb",
	} {
		if SafeDirectiveValue(bad) {
			t.Errorf("accepted a value that can restructure the file: %q", bad)
		}
	}
}
