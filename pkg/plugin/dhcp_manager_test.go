package plugin

import (
	"testing"

	"github.com/vishvananda/netlink"

	"github.com/devplayer0/docker-net-dhcp/pkg/udhcpc"
)

// TestRenew_LeaseChangedCounter pins the v0.9.0 / T1-4 counter
// behaviour: when dhcpcd returns a different IP than the manager's
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

	t.Run("v6 changed IP bumps aggregate and v6 sibling", func(t *testing.T) {
		p := &Plugin{}
		m := &dhcpManager{plugin: p}
		m.setLastIP(true, addr1)

		if err := m.renew(true, udhcpc.Info{IP: addr2.String()}); err != nil {
			t.Fatalf("renew: %v", err)
		}

		if got := p.leaseChanged.Load(); got != 1 {
			t.Errorf("leaseChanged aggregate = %d, want 1", got)
		}
		if got := p.leaseChangedV6.Load(); got != 1 {
			t.Errorf("leaseChangedV6 = %d, want 1", got)
		}
	})

	t.Run("v4 changed IP leaves v6 sibling at zero", func(t *testing.T) {
		p := &Plugin{}
		m := &dhcpManager{plugin: p}
		m.setLastIP(false, addr1)

		if err := m.renew(false, udhcpc.Info{IP: addr2.String()}); err != nil {
			t.Fatalf("renew: %v", err)
		}

		if got := p.leaseChanged.Load(); got != 1 {
			t.Errorf("leaseChanged aggregate = %d, want 1", got)
		}
		if got := p.leaseChangedV6.Load(); got != 0 {
			t.Errorf("leaseChangedV6 = %d, want 0 (v4 change must not touch the v6 sibling)", got)
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

// TestHandleEvent_Counters pins which health counter each dhcpcd
// lifecycle event bumps (#128). The "nak" arm matters most: dnsmasq
// silently ignores refused renewals in several shapes instead of
// emitting DHCPNAK, so this contract cannot be pinned reliably at the
// integration level — when a real server does NAK (dhcpcd maps the
// NAK reason to the event), this is the path that counts it.
func TestHandleEvent_Counters(t *testing.T) {
	addr, err := netlink.ParseAddr("192.168.0.10/24")
	if err != nil {
		t.Fatalf("ParseAddr: %v", err)
	}

	cases := []struct {
		event     string
		aggregate func(p *Plugin) int32
		v6        func(p *Plugin) int32
	}{
		{"bound", func(p *Plugin) int32 { return p.leasesObtained.Load() }, func(p *Plugin) int32 { return p.leasesObtainedV6.Load() }},
		{"renew", func(p *Plugin) int32 { return p.leasesRenewed.Load() }, func(p *Plugin) int32 { return p.leasesRenewedV6.Load() }},
		{"leasefail", func(p *Plugin) int32 { return p.dhcpTimeouts.Load() }, func(p *Plugin) int32 { return p.dhcpTimeoutsV6.Load() }},
		{"nak", func(p *Plugin) int32 { return p.naksReceived.Load() }, func(p *Plugin) int32 { return p.naksReceivedV6.Load() }},
	}
	// Each event under both families: the aggregate always moves; the v6
	// sibling moves only for v6 events and stays put for v4 ones (#212).
	for _, c := range cases {
		for _, v6 := range []bool{false, true} {
			family := "v4"
			if v6 {
				family = "v6"
			}
			t.Run(c.event+"/"+family, func(t *testing.T) {
				p := &Plugin{}
				m := &dhcpManager{plugin: p}
				m.setLastIP(v6, addr)

				m.handleEvent(udhcpc.Event{Type: c.event, Data: udhcpc.Info{IP: addr.String()}}, v6)

				if got := c.aggregate(p); got != 1 {
					t.Errorf("%s aggregate = %d, want 1", c.event, got)
				}
				wantV6 := int32(0)
				if v6 {
					wantV6 = 1
				}
				if got := c.v6(p); got != wantV6 {
					t.Errorf("%s v6 sibling = %d, want %d", c.event, got, wantV6)
				}
			})
		}
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

// TestNextAcquiring pins the DHCP-outage watchdog state machine. dhcpcd
// emits no per-attempt failure hook, so the persistent-client goroutine
// derives an "acquiring" flag from the event stream: a bound/renew means
// we hold a lease; a leasefail (dhcpcd EXPIRE/TIMEOUT) drops back to
// acquiring; anything else (NAK) is left unchanged.
func TestNextAcquiring(t *testing.T) {
	cases := []struct {
		name      string
		prev      bool
		eventType string
		want      bool
	}{
		{"bound clears acquiring", true, "bound", false},
		{"renew clears acquiring", true, "renew", false},
		{"leasefail sets acquiring", false, "leasefail", true},
		{"leasefail while acquiring stays acquiring", true, "leasefail", true},
		{"bound while bound stays bound", false, "bound", false},
		{"nak leaves acquiring=true unchanged", true, "nak", true},
		{"nak leaves acquiring=false unchanged", false, "nak", false},
		{"unknown leaves state unchanged", true, "carrier", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextAcquiring(tc.prev, tc.eventType); got != tc.want {
				t.Errorf("nextAcquiring(%v, %q) = %v, want %v", tc.prev, tc.eventType, got, tc.want)
			}
		})
	}
}
