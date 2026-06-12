package plugin

import (
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/devplayer0/docker-net-dhcp/pkg/udhcpc"
)

// TestRenew_LeaseChangedCounter pins the v0.9.0 / T1-4 counter
// behaviour: when udhcpc returns a different IP than the manager's
// recorded lastIP, p.leaseChanged.Add(1) fires.
//
// We don't need a live netlink/netns fixture — the counter bump
// happens in the early part of renew, before any kernel-touching
// branches. The MTU / DNS / gateway side-paths are gated on
// PropagateMTU / PropagateDNS / info.Gateway, all of which we leave
// off so they don't try to dereference a nil m.netHandle.
func TestRenew_LeaseChangedCounter(t *testing.T) {
	addr1, err := netlink.ParseAddr("192.168.0.10/24")
	if err != nil {
		t.Fatalf("ParseAddr addr1: %v", err)
	}
	addr2, err := netlink.ParseAddr("192.168.0.11/24")
	if err != nil {
		t.Fatalf("ParseAddr addr2: %v", err)
	}

	t.Run("changed IP bumps counter", func(t *testing.T) {
		p := &Plugin{}
		m := &dhcpManager{plugin: p}
		m.setLastIP(false, addr1)

		if err := m.renew(false, udhcpc.Info{IP: addr2.String()}); err != nil {
			t.Fatalf("renew: %v", err)
		}

		if got := p.leaseChanged.Load(); got != 1 {
			t.Errorf("leaseChanged = %d, want 1", got)
		}
	})

	t.Run("same IP does not bump counter", func(t *testing.T) {
		p := &Plugin{}
		m := &dhcpManager{plugin: p}
		m.setLastIP(false, addr1)

		if err := m.renew(false, udhcpc.Info{IP: addr1.String()}); err != nil {
			t.Fatalf("renew: %v", err)
		}

		if got := p.leaseChanged.Load(); got != 0 {
			t.Errorf("leaseChanged = %d, want 0 (same IP shouldn't count as a change)", got)
		}
	})

	t.Run("first bind (no prior lastIP) does not bump counter", func(t *testing.T) {
		// On the very first bound event lastIP is nil; that's a fresh
		// lease, not a change. The condition `lastIP != nil && ...`
		// guards this. Pin the contract so a future refactor doesn't
		// regress to the old `lastIP == nil || !ip.Equal(*lastIP)`
		// shape that bumped the counter on every initial bind.
		p := &Plugin{}
		m := &dhcpManager{plugin: p}
		// no setLastIP — lastIP is nil

		if err := m.renew(false, udhcpc.Info{IP: addr1.String()}); err != nil {
			t.Fatalf("renew: %v", err)
		}

		if got := p.leaseChanged.Load(); got != 0 {
			t.Errorf("leaseChanged = %d, want 0 (first bind shouldn't count as a change)", got)
		}
	})

	t.Run("nil plugin is safe", func(t *testing.T) {
		// Tests that drive renew without wiring a Plugin (pre-v0.9.0
		// shape) must keep working — production callers always set
		// it via withPlugin, but the safety check is cheap.
		m := &dhcpManager{plugin: nil}
		m.setLastIP(false, addr1)

		if err := m.renew(false, udhcpc.Info{IP: addr2.String()}); err != nil {
			t.Fatalf("renew: %v", err)
		}
	})
}

// TestHandleEvent_Counters pins which health counter each udhcpc
// lifecycle event bumps (#128). The "nak" arm matters most: dnsmasq
// silently ignores refused renewals in several shapes instead of
// emitting DHCPNAK, so this contract cannot be pinned reliably at the
// integration level — when a real server does NAK (busybox udhcpc
// emits the event verbatim), this is the path that counts it.
func TestHandleEvent_Counters(t *testing.T) {
	addr, err := netlink.ParseAddr("192.168.0.10/24")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}

	cases := []struct {
		event string
		read  func(p *Plugin) int32
	}{
		{"bound", func(p *Plugin) int32 { return p.leasesObtained.Load() }},
		{"renew", func(p *Plugin) int32 { return p.leasesRenewed.Load() }},
		{"leasefail", func(p *Plugin) int32 { return p.dhcpTimeouts.Load() }},
		{"nak", func(p *Plugin) int32 { return p.naksReceived.Load() }},
	}
	for _, c := range cases {
		t.Run(c.event, func(t *testing.T) {
			p := &Plugin{}
			m := &dhcpManager{plugin: p}
			m.setLastIP(false, addr)

			m.handleEvent(udhcpc.Event{Type: c.event, Data: udhcpc.Info{IP: addr.String()}}, false)

			if got := c.read(p); got != 1 {
				t.Errorf("%s counter = %d, want 1", c.event, got)
			}
		})
	}

	t.Run("deconfig and unknown bump nothing", func(t *testing.T) {
		p := &Plugin{}
		m := &dhcpManager{plugin: p}
		for _, evt := range []string{"deconfig", "something-new"} {
			m.handleEvent(udhcpc.Event{Type: evt}, false)
		}
		total := p.leasesObtained.Load() + p.leasesRenewed.Load() +
			p.dhcpTimeouts.Load() + p.naksReceived.Load()
		if total != 0 {
			t.Errorf("counters moved on non-counting events: %d", total)
		}
	})

	t.Run("nil plugin is safe for every event", func(t *testing.T) {
		m := &dhcpManager{plugin: nil}
		m.setLastIP(false, addr)
		for _, evt := range []string{"bound", "renew", "leasefail", "nak", "deconfig"} {
			m.handleEvent(udhcpc.Event{Type: evt, Data: udhcpc.Info{IP: addr.String()}}, false)
		}
	})
}
