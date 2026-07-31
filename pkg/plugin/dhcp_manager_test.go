package plugin

import (
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/devplayer0/docker-net-dhcp/pkg/dhcp"
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

		if err := m.renew(false, dhcp.Info{IP: addr2.String()}); err != nil {
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

		if err := m.renew(false, dhcp.Info{IP: addr1.String()}); err != nil {
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

		if err := m.renew(false, dhcp.Info{IP: addr1.String()}); err != nil {
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

		if err := m.renew(true, dhcp.Info{IP: addr2.String()}); err != nil {
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

		if err := m.renew(false, dhcp.Info{IP: addr2.String()}); err != nil {
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

		if err := m.renew(false, dhcp.Info{IP: addr2.String()}); err != nil {
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

				m.handleEvent(dhcp.Event{Type: c.event, Data: dhcp.Info{IP: addr.String()}}, v6)

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
			m.handleEvent(dhcp.Event{Type: evt}, false)
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
			m.handleEvent(dhcp.Event{Type: evt, Data: dhcp.Info{IP: addr.String()}}, false)
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

// releasingManager builds a dhcpManager that Stop() can run against
// without a live netns/netlink fixture: Start is marked complete with
// no error, and the two consumer goroutines are simulated by
// pre-filling their exit channels.
//
// nsHandle is set to netns.None() deliberately. NsHandle.IsOpen() is
// `ns != -1`, so the zero value (0) reports *open* and Stop's deferred
// cleanup would close file descriptor 0 — the test process's stdin.
// Real managers can't hit that (Start sets the handle, and a Start that
// failed earlier short-circuits Stop via startErr), but a test that
// hand-builds the struct has to say so.
func releasingManager(t *testing.T, p *Plugin, opts DHCPNetworkOptions, errV4, errV6 error) *dhcpManager {
	t.Helper()

	m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"}, opts).withPlugin(p)
	m.nsHandle = netns.None()
	close(m.startedCh)

	v4, err := netlink.ParseAddr("192.168.99.50/24")
	if err != nil {
		t.Fatalf("ParseAddr v4: %v", err)
	}
	m.setLastIP(false, v4)

	m.errChan = make(chan error, 1)
	m.errChan <- errV4

	if opts.IPv6 {
		v6, err := netlink.ParseAddr("fd00::50/64")
		if err != nil {
			t.Fatalf("ParseAddr v6: %v", err)
		}
		m.setLastIP(true, v6)

		m.errChanV6 = make(chan error, 1)
		m.errChanV6 <- errV6
	}
	return m
}

// TestStop_AuditsBothFamiliesIndependently pins the dual-drain contract
// (#325/#330). Stop must read BOTH consumer channels before returning —
// the old code returned early on a v4 release failure, which left the
// v6 consumer live and mid-renew on m.netHandle while the deferred
// closeNetHandle nilled the socket out from under it, and additionally
// hid the v6 outcome from the audit ledger.
//
// The v4-fails-v6-succeeds row is the regression the old code failed:
// it recorded release_failed for v4 and nothing at all for v6.
func TestStop_AuditsBothFamiliesIndependently(t *testing.T) {
	errV4 := errors.New("v4 release boom")
	errV6 := errors.New("v6 release boom")

	cases := []struct {
		name         string
		ipv6         bool
		errV4, errV6 error
		wantKinds    []string
		wantFailures int32
		wantErr      error
	}{
		{
			name: "v4 only, clean release", ipv6: false,
			wantKinds: []string{"release"},
		},
		{
			name: "v4 only, failed release", ipv6: false,
			errV4:     errV4,
			wantKinds: []string{"release_failed"}, wantFailures: 1, wantErr: errV4,
		},
		{
			name: "dual stack, both clean", ipv6: true,
			wantKinds: []string{"release", "release"},
		},
		{
			name: "dual stack, v4 fails — v6 outcome still audited", ipv6: true,
			errV4:     errV4,
			wantKinds: []string{"release_failed", "release"}, wantFailures: 1, wantErr: errV4,
		},
		{
			name: "dual stack, v6 fails", ipv6: true,
			errV6:     errV6,
			wantKinds: []string{"release", "release_failed"}, wantFailures: 1, wantErr: errV6,
		},
		{
			name: "dual stack, both fail — v4 error takes precedence", ipv6: true,
			errV4: errV4, errV6: errV6,
			wantKinds: []string{"release_failed", "release_failed"}, wantFailures: 2, wantErr: errV4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)

			opts := DHCPNetworkOptions{AuditLog: true, IPv6: tc.ipv6}
			m := releasingManager(t, p, opts, tc.errV4, tc.errV6)

			err := m.Stop()

			if tc.wantErr == nil && err != nil {
				t.Fatalf("Stop() = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Stop() = %v, want an error wrapping %v", err, tc.wantErr)
			}

			if got := p.leaseReleaseFailures.Load(); got != tc.wantFailures {
				t.Errorf("leaseReleaseFailures = %d, want %d", got, tc.wantFailures)
			}

			entries := readLedgerLines(t, p.ledger.path)
			var kinds []string
			for _, e := range entries {
				kinds = append(kinds, e.Kind)
			}
			if len(kinds) != len(tc.wantKinds) {
				t.Fatalf("ledger kinds = %v, want %v", kinds, tc.wantKinds)
			}
			for i := range kinds {
				if kinds[i] != tc.wantKinds[i] {
					t.Fatalf("ledger kinds = %v, want %v", kinds, tc.wantKinds)
				}
			}

			// Each family's entry must carry its own address — the
			// point of auditing them separately.
			if entries[0].IP != "192.168.99.50" {
				t.Errorf("v4 entry IP = %q, want 192.168.99.50", entries[0].IP)
			}
			if tc.ipv6 && entries[1].IP != "fd00::50" {
				t.Errorf("v6 entry IP = %q, want fd00::50", entries[1].IP)
			}
		})
	}
}

// TestStop_FailedStartIsANoOp pins the short-circuit: a manager whose
// Start errored has nothing to release, so Stop must not touch the
// ledger, the counters, or the (never-populated) exit channels.
func TestStop_FailedStartIsANoOp(t *testing.T) {
	var ledgerFailures atomic.Int32
	p := &Plugin{}
	p.ledger = testLedger(t, &ledgerFailures)

	m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"},
		DHCPNetworkOptions{AuditLog: true}).withPlugin(p)
	m.startErr = errors.New("start boom")
	close(m.startedCh)

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() on a failed-Start manager = %v, want nil", err)
	}
	if got := p.leaseReleaseFailures.Load(); got != 0 {
		t.Errorf("leaseReleaseFailures = %d, want 0", got)
	}
	if _, err := os.Stat(p.ledger.path); !os.IsNotExist(err) {
		t.Errorf("ledger written for a manager that never held a lease (stat err: %v)", err)
	}
}

// #406: when a Join runs out of budget, every phase reports the same
// "context deadline exceeded" and the useful question — which phase
// consumed it — has no answer. These pin the rendering, since the
// phases themselves are only reachable with a live daemon.
func TestJoinPhases(t *testing.T) {
	t.Run("renders each phase with its own time", func(t *testing.T) {
		p := newJoinPhases()
		p.mark("resolve_container_id")
		p.mark("inspect_container")
		got := p.summary()
		for _, want := range []string{"resolve_container_id=", "inspect_container="} {
			if !strings.Contains(got, want) {
				t.Errorf("summary %q is missing %q", got, want)
			}
		}
		if strings.Count(got, "=") != 2 {
			t.Errorf("want exactly the two phases that completed, got %q", got)
		}
	})

	t.Run("says so when nothing completed", func(t *testing.T) {
		// The most interesting failure of all: the budget went entirely
		// to the first phase. An empty string here would read as "no
		// timing available" rather than "it never got past step one".
		if got := newJoinPhases().summary(); !strings.Contains(got, "no phase completed") {
			t.Errorf("summary with no marks = %q; want an explicit note", got)
		}
	})

	t.Run("attributes elapsed time to the phase that spent it", func(t *testing.T) {
		p := newJoinPhases()
		time.Sleep(60 * time.Millisecond)
		p.mark("slow_phase")
		p.mark("fast_phase")
		var slow, fast joinPhaseSpan
		for _, s := range p.spans {
			switch s.name {
			case "slow_phase":
				slow = s
			case "fast_phase":
				fast = s
			}
		}
		if slow.took < 50*time.Millisecond {
			t.Errorf("the slow phase was charged %v; the whole point is that it carries the time", slow.took)
		}
		if fast.took > 20*time.Millisecond {
			t.Errorf("the fast phase was charged %v; time is being double-counted", fast.took)
		}
	})

	t.Run("a nil tracker is inert", func(t *testing.T) {
		var p *joinPhases
		p.mark("x")
		if got := p.summary(); got == "" {
			t.Error("nil tracker should still render something rather than an empty field")
		}
		if p.total() != 0 {
			t.Error("nil tracker total should be zero")
		}
	})
}
