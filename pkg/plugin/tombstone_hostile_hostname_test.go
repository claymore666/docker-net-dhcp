// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import "testing"

// TestConsumeTombstone_ARefusedHostnameInheritsNothing pins the hole that
// the #692 fix opened while closing the one it was written for.
//
// safeHostname refuses a hostname carrying a control character and returns
// "". Independently, tombstoneStore.consume treats an EMPTY hostname as
// "match any tombstone on this network" — a deliberate carve-out for
// v0.5.0 tombstones and for the CreateEndpoint/container-registration
// race, both of which are honest absences. Routing a refusal into that
// same "" turned the sanitiser into a wildcard generator: one \x01 in
// `docker run --hostname` and the container inherited some other
// endpoint's MAC and asked the DHCP server for its address. On the
// deployment this plugin targets — a LAN DHCP server with MAC
// reservations — that is impersonation and lease theft, not a mix-up.
//
// The three cases below have to stay together. The last one is what stops
// this test passing vacuously: if the victim tombstone were not actually
// consumable, the first two would "pass" while proving nothing.
func TestConsumeTombstone_ARefusedHostnameInheritsNothing(t *testing.T) {
	const (
		net       = "net-A"
		victim    = "victim-host"
		victimMAC = "02:42:ac:11:00:99"
		victimIP  = "192.168.0.50"
	)

	t.Run("a refused hostname consumes nothing", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		p := newPluginForTest()
		p.addTombstone(net, victim, victimMAC, victimIP, "fe80::99")

		// Exactly what CreateEndpoint does with an attacker's hostname.
		hostname, trusted := p.safeHostname("attacker-host\x01")
		if hostname != "" || trusted {
			t.Fatalf("safeHostname = (%q, %v), want (\"\", false)", hostname, trusted)
		}

		mac, ipv4, ipv6, ok := p.consumeTombstone(net, hostname, trusted)
		if ok {
			t.Fatalf("a refused hostname inherited another endpoint's identity: mac=%q ipv4=%q ipv6=%q", mac, ipv4, ipv6)
		}
	})

	t.Run("an honest mismatching hostname consumes nothing either", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		p := newPluginForTest()
		p.addTombstone(net, victim, victimMAC, victimIP, "fe80::99")

		hostname, trusted := p.safeHostname("attacker-host")
		if !trusted {
			t.Fatalf("an ordinary hostname was refused")
		}
		if _, _, _, ok := p.consumeTombstone(net, hostname, trusted); ok {
			t.Error("a different container's tombstone was consumed by name")
		}
	})

	t.Run("the victim's own hostname still consumes it", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		p := newPluginForTest()
		p.addTombstone(net, victim, victimMAC, victimIP, "fe80::99")

		hostname, trusted := p.safeHostname(victim)
		mac, ipv4, _, ok := p.consumeTombstone(net, hostname, trusted)
		if !ok || mac != victimMAC || ipv4 != victimIP {
			t.Fatalf("the tombstone was not consumable at all: (%q, %q, %v) — the refusals above prove nothing", mac, ipv4, ok)
		}
	})
}
