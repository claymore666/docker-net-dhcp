// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import "testing"

// An empty hostname matches any tombstone, so a refused hostname must match none; the
// third case keeps the first two from passing vacuously (#692).
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

		hostname := p.safeHostname("attacker-host\x01")
		if hostname.name != "" || hostname.trusted() {
			t.Fatalf("safeHostname = (%q, trusted=%v), want (\"\", false)", hostname.name, hostname.trusted())
		}

		mac, ipv4, ipv6, ok := p.consumeTombstone(net, hostname)
		if ok {
			t.Fatalf("a refused hostname inherited another endpoint's identity: mac=%q ipv4=%q ipv6=%q", mac, ipv4, ipv6)
		}
	})

	t.Run("an honest mismatching hostname consumes nothing either", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		p := newPluginForTest()
		p.addTombstone(net, victim, victimMAC, victimIP, "fe80::99")

		hostname := p.safeHostname("attacker-host")
		if !hostname.trusted() {
			t.Fatalf("an ordinary hostname was refused")
		}
		if _, _, _, ok := p.consumeTombstone(net, hostname); ok {
			t.Error("a different container's tombstone was consumed by name")
		}
	})

	t.Run("the victim's own hostname still consumes it", func(t *testing.T) {
		withStateDir(t, t.TempDir())
		p := newPluginForTest()
		p.addTombstone(net, victim, victimMAC, victimIP, "fe80::99")

		hostname := p.safeHostname(victim)
		mac, ipv4, _, ok := p.consumeTombstone(net, hostname)
		if !ok || mac != victimMAC || ipv4 != victimIP {
			t.Fatalf("the tombstone was not consumable at all: (%q, %q, %v) — the refusals above prove nothing", mac, ipv4, ok)
		}
	})
}
