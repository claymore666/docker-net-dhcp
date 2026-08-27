// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// TestRenew_LeaseChangedCounter pins the v0.9.0 / T1-4 counter
// behaviour: when dhcpcd returns a different IP than the manager's
// recorded lastIP, p.leaseChangedV4.Add(1) fires.
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

		if got := p.leaseChangedV4.Load(); got != 1 {
			t.Errorf("leaseChangedV4 = %d, want 1", got)
		}
	})

	t.Run("same IP does not bump counter", func(t *testing.T) {
		p := &Plugin{}
		m := &dhcpManager{plugin: p}
		m.setLastIP(false, addr1)

		if err := m.renew(false, dhcp.Info{IP: addr1.String()}); err != nil {
			t.Fatalf("renew: %v", err)
		}

		if got := p.leaseChangedV4.Load(); got != 0 {
			t.Errorf("leaseChangedV4 = %d, want 0 (same IP shouldn't count as a change)", got)
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

		if got := p.leaseChangedV4.Load(); got != 0 {
			t.Errorf("leaseChangedV4 = %d, want 0 (first bind shouldn't count as a change)", got)
		}
	})

	t.Run("v6 changed IP bumps the v6 half only", func(t *testing.T) {
		// Since #730 each family owns a counter and a v6 event bumps
		// exactly one of them. The v4 half staying at 0 is the whole
		// point: it used to move on every event, which is what made
		// the v4 number something that had to be subtracted back out.
		p := &Plugin{}
		m := &dhcpManager{plugin: p}
		m.setLastIP(true, addr1)

		if err := m.renew(true, dhcp.Info{IP: addr2.String()}); err != nil {
			t.Fatalf("renew: %v", err)
		}

		if got := p.leaseChangedV4.Load(); got != 0 {
			t.Errorf("leaseChangedV4 = %d, want 0 — a v6 event must not move the v4 half", got)
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

		if got := p.leaseChangedV4.Load(); got != 1 {
			t.Errorf("leaseChangedV4 aggregate = %d, want 1", got)
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
		event string
		v4    func(p *Plugin) int32
		v6    func(p *Plugin) int32
	}{
		{"bound", func(p *Plugin) int32 { return p.leasesObtainedV4.Load() }, func(p *Plugin) int32 { return p.leasesObtainedV6.Load() }},
		{"renew", func(p *Plugin) int32 { return p.leasesRenewedV4.Load() }, func(p *Plugin) int32 { return p.leasesRenewedV6.Load() }},
		{"leasefail", func(p *Plugin) int32 { return p.dhcpTimeoutsV4.Load() }, func(p *Plugin) int32 { return p.dhcpTimeoutsV6.Load() }},
		{"nak", func(p *Plugin) int32 { return p.naksReceivedV4.Load() }, func(p *Plugin) int32 { return p.naksReceivedV6.Load() }},
	}
	// Each event under both families bumps EXACTLY ONE half (#212,
	// #730). Asserting both halves — one moved, the other did not — is
	// what makes this a contract rather than a count: before #730 the
	// un-suffixed counter moved on every event, so a v6 event bumped
	// two counters and the v4 number had to be recovered by
	// subtracting them at render time. That subtraction is the defect
	// #730 removed, and it is unreachable only while this holds.
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

				wantV4, wantV6 := int32(1), int32(0)
				if v6 {
					wantV4, wantV6 = 0, 1
				}
				if got := c.v4(p); got != wantV4 {
					t.Errorf("%s v4 half = %d, want %d", c.event, got, wantV4)
				}
				if got := c.v6(p); got != wantV6 {
					t.Errorf("%s v6 half = %d, want %d", c.event, got, wantV6)
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
		// Both halves, not just the v4 one: these events are dispatched
		// with v6=false, so a bump mis-routed to the v6 half would
		// leave a v4-only sum at zero and pass.
		total := p.leasesObtainedV4.Load() + p.leasesRenewedV4.Load() +
			p.dhcpTimeoutsV4.Load() + p.naksReceivedV4.Load() +
			p.leasesObtainedV6.Load() + p.leasesRenewedV6.Load() +
			p.dhcpTimeoutsV6.Load() + p.naksReceivedV6.Load() +
			// #815's counter joins the sum for the same reason the v6
			// halves did: a case matching too broadly would bump it here
			// and a total that omitted it would call that clean.
			p.dhcpv6ConfigOnly.Load()
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

// stoppingManager builds a dhcpManager that Stop() can run against
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
func stoppingManager(t *testing.T, p *Plugin, opts DHCPNetworkOptions, errV4, errV6 error) *dhcpManager {
	t.Helper()

	m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"}, opts).withPlugin(p)
	m.nsHandle = netns.None()
	close(m.startedCh)

	v4, err := netlink.ParseAddr("192.168.99.50/24")
	if err != nil {
		t.Fatalf("ParseAddr v4: %v", err)
	}
	m.setLastIP(false, v4)
	// Every case in this file models a client that reached a bind and is
	// now being shut down, which is what makes "stopped" the honest
	// ledger entry. Without this the manager is in the never-bound state
	// instead, where Stop must NOT claim a release — see
	// TestStop_LeavingAndNotLeavingAreTheSame, whose
	// client_started_but_never_bound row drives exactly that state.
	m.boundV4.Store(true)

	m.errChan = make(chan error, 1)
	m.errChan <- errV4

	if opts.IPv6 {
		v6, err := netlink.ParseAddr("fd00::50/64")
		if err != nil {
			t.Fatalf("ParseAddr v6: %v", err)
		}
		m.setLastIP(true, v6)
		// Same reasoning as boundV4 above, for the v6 client (#608).
		m.boundV6.Store(true)

		m.errChanV6 = make(chan error, 1)
		m.errChanV6 <- errV6
	}
	return m
}

// TestStop_AuditsBothFamiliesIndependently pins the dual-drain contract
// (#325/#330). Stop must read BOTH consumer channels before returning —
// the old code returned early on a v4 stop failure, which left the
// v6 consumer live and mid-renew on m.netHandle while the deferred
// closeNetHandle nilled the socket out from under it, and additionally
// hid the v6 outcome from the audit ledger.
//
// The v4-fails-v6-succeeds row is the regression the old code failed:
// it recorded a failure for v4 and nothing at all for v6.
func TestStop_AuditsBothFamiliesIndependently(t *testing.T) {
	errV4 := errors.New("v4 stop boom")
	errV6 := errors.New("v6 stop boom")

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
			wantKinds: []string{"stopped"},
		},
		{
			name: "v4 only, failed release", ipv6: false,
			errV4:     errV4,
			wantKinds: []string{"stop_failed"}, wantFailures: 1, wantErr: errV4,
		},
		{
			name: "dual stack, both clean", ipv6: true,
			wantKinds: []string{"stopped", "stopped"},
		},
		{
			name: "dual stack, v4 fails — v6 outcome still audited", ipv6: true,
			errV4:     errV4,
			wantKinds: []string{"stop_failed", "stopped"}, wantFailures: 1, wantErr: errV4,
		},
		{
			name: "dual stack, v6 fails", ipv6: true,
			errV6:     errV6,
			wantKinds: []string{"stopped", "stop_failed"}, wantFailures: 1, wantErr: errV6,
		},
		{
			name: "dual stack, both fail — v4 error takes precedence", ipv6: true,
			errV4: errV4, errV6: errV6,
			wantKinds: []string{"stop_failed", "stop_failed"}, wantFailures: 2, wantErr: errV4,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)

			opts := DHCPNetworkOptions{AuditLog: true, IPv6: tc.ipv6}
			m := stoppingManager(t, p, opts, tc.errV4, tc.errV6)

			err := m.Stop()

			if tc.wantErr == nil && err != nil {
				t.Fatalf("Stop() = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Stop() = %v, want an error wrapping %v", err, tc.wantErr)
			}

			// wantFailures is the total across both families, which
			// since #730 is the sum of the two halves rather than a
			// counter of its own. Asserting the sum keeps this case
			// about Stop's auditing; which half moved is pinned by
			// TestStop_BoundV6StopFailureIsCountedPerFamily.
			if got := p.clientStopFailuresV4.Load() + p.clientStopFailuresV6.Load(); got != tc.wantFailures {
				t.Errorf("lease release failures (v4+v6) = %d, want %d", got, tc.wantFailures)
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
	if got := p.clientStopFailuresV4.Load(); got != 0 {
		t.Errorf("clientStopFailuresV4 = %d, want 0", got)
	}
	if _, err := os.Stat(p.ledger.path); !os.IsNotExist(err) {
		t.Errorf("ledger written for a manager that never held a lease (stat err: %v)", err)
	}
}

// failedStartManager builds the manager TestStop_FailedStartIsANoOp
// could not: Start errored, and the CreateEndpoint one-shot's address is
// still recorded on it.
//
// That combination is what #720 was about, and what #800 settled. The
// older test seeds no lastIP, so a stop path that acted on the lease
// would find nothing to act on and look correct for the wrong reason.
func failedStartManager(t *testing.T, p *Plugin) *dhcpManager {
	t.Helper()

	m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"},
		DHCPNetworkOptions{AuditLog: true}).withPlugin(p)

	v4, err := netlink.ParseAddr("192.168.99.50/24")
	if err != nil {
		t.Fatalf("ParseAddr v4: %v", err)
	}
	m.setLastIP(false, v4)

	m.startErr = errors.New("start boom")
	close(m.startedCh)
	return m
}

// ledgerKinds reads the audit ledger, tolerating a file that was never
// created — which is itself a result, and the one several tests below
// expect.
func ledgerKinds(t *testing.T, l *leaseLedger) []string {
	t.Helper()
	if _, err := os.Stat(l.path); os.IsNotExist(err) {
		return nil
	}
	var kinds []string
	for _, e := range readLedgerLines(t, l.path) {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// TestStop_LeavingAndNotLeavingAreTheSame is #800's rule stated as the
// thing a test can see.
//
// The plugin no longer distinguishes "this endpoint is going away" from
// "this manager is being shut down" when it comes to the lease, because
// at the moment of the decision those two are indistinguishable in the
// one case that matters: `docker restart` is a Leave immediately
// followed by a Join for the SAME MAC, and the tombstone exists to
// promise that Join the same address. Anything the leaving path did to
// the lease raced that promise.
//
// So both entry points must now produce IDENTICAL observable results,
// and this asserts equality rather than two hard-coded expectations. A
// change that reintroduces asymmetry fails here whichever side it
// favours — including a re-added reclaim, which is what the old
// behaviour was and therefore the strongest mutant this test faces.
//
// Every row seeds an address, because a stop path that acts on a lease
// finds nothing to act on when there is none and passes for the wrong
// reason.
func TestStop_LeavingAndNotLeavingAreTheSame(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func(t *testing.T, p *Plugin) *dhcpManager
	}{
		{
			name: "start_failed_with_the_oneshot_lease_outstanding",
			mk:   failedStartManager,
		},
		{
			name: "client_started_but_never_bound",
			mk: func(t *testing.T, p *Plugin) *dhcpManager {
				m := stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true}, nil, nil)
				m.boundV4.Store(false)
				return m
			},
		},
		{
			name: "client_bound_and_exited_cleanly",
			mk: func(t *testing.T, p *Plugin) *dhcpManager {
				return stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true}, nil, nil)
			},
		},
		{
			name: "client_never_bound_and_died_on_the_signal",
			mk: func(t *testing.T, p *Plugin) *dhcpManager {
				m := stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true},
					errors.New("signal: terminated"), nil)
				m.boundV4.Store(false)
				return m
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(leaving bool) (kinds []string, stopFailures int32) {
				var ledgerFailures atomic.Int32
				p := &Plugin{}
				p.ledger = testLedger(t, &ledgerFailures)
				m := tc.mk(t, p)

				var err error
				if leaving {
					err = m.StopForLeave()
				} else {
					err = m.Stop()
				}
				if err != nil {
					t.Fatalf("stop(leaving=%v) = %v, want nil", leaving, err)
				}
				return ledgerKinds(t, p.ledger), p.clientStopFailuresV4.Load()
			}

			leaveKinds, leaveFailures := run(true)
			stopKinds, stopFailures := run(false)

			if !slices.Equal(leaveKinds, stopKinds) {
				t.Errorf("ledger kinds differ by entry point: StopForLeave wrote %v, "+
					"Stop wrote %v. The lease is treated the same either way (#800) — "+
					"a Leave is not evidence the container is gone, it is what "+
					"`docker restart` does on its way back", leaveKinds, stopKinds)
			}
			if leaveFailures != stopFailures {
				t.Errorf("client_stop_failures differ by entry point: StopForLeave %d, "+
					"Stop %d", leaveFailures, stopFailures)
			}
		})
	}
}

// The equality above is satisfied by a plugin that does nothing at all
// on either path, so this pins what the shared outcome actually IS.
//
// Without it, deleting every audit call would turn
// TestStop_LeavingAndNotLeavingAreTheSame green — the failure mode the
// #780 counters exist to name, one level up: two identical results are
// not evidence of correct behaviour unless at least one of them is
// known to be non-empty.
func TestStop_AuditsAStopWithoutClaimingARelease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bound     bool
		exitErr   error
		wantErr   bool
		wantKinds []string
		why       string
	}{
		{
			name: "bound_and_clean", bound: true, exitErr: nil,
			wantKinds: []string{"stopped"},
			why: "the client held a binding and shut down when asked; " +
				"`stopped` says that and claims nothing about the server",
		},
		{
			name: "bound_and_dirty", bound: true, exitErr: errors.New("boom"),
			wantErr:   true,
			wantKinds: []string{"stop_failed"},
			why:       "the client held a binding and did not shut down cleanly",
		},
		{
			name: "never_bound", bound: false, exitErr: nil,
			wantKinds: nil,
			why: "no binding ever existed, so there is nothing to write down; " +
				"an entry here would be the ledger inventing a lease event",
		},
		{
			name: "never_bound_killed_by_signal", bound: false,
			exitErr:   errors.New("signal: terminated"),
			wantKinds: nil,
			why: "we sent that SIGTERM and the client had no binding; the exit " +
				"status is not evidence of anything (#607)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)

			m := stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true}, tc.exitErr, nil)
			m.boundV4.Store(tc.bound)

			err := m.StopForLeave()
			switch {
			case tc.wantErr && err == nil:
				t.Errorf("StopForLeave() = nil, want an error — a client that held a "+
					"binding and did not exit cleanly is a real failure: %s", tc.why)
			case !tc.wantErr && err != nil:
				t.Errorf("StopForLeave() = %v, want nil — %s", err, tc.why)
			}

			kinds := ledgerKinds(t, p.ledger)
			if !slices.Equal(kinds, tc.wantKinds) {
				t.Errorf("ledger kinds = %v, want %v — %s", kinds, tc.wantKinds, tc.why)
			}
			// Whatever else it writes, it must never claim the server
			// saw a DHCPRELEASE. Nothing this plugin runs sends one.
			for _, k := range kinds {
				if strings.Contains(k, "release") {
					t.Errorf("ledger kind %q names a release; no client this plugin "+
						"runs releases a lease (#800)", k)
				}
			}
		})
	}
}

// The event that flips the manager out of the never-bound state. Only a
// v4 bind counts: the reclaim hands back the v4 address and there is no
// v6 equivalent, so a v6-only bind must leave the v4 lease unclaimed.
func TestHandleEvent_BoundOwnershipIsV4Only(t *testing.T) {
	for _, tc := range []struct {
		event string
		v6    bool
		want  bool
	}{
		{event: "bound", v6: false, want: true},
		{event: "renew", v6: false, want: true},
		{event: "bound", v6: true, want: false},
		{event: "renew", v6: true, want: false},
		{event: "leasefail", v6: false, want: false},
		{event: "nak", v6: false, want: false},
	} {
		t.Run(tc.event+familySuffix(tc.v6), func(t *testing.T) {
			m := newDHCPManager(nil, JoinRequest{NetworkID: "net1", EndpointID: "ep1"},
				DHCPNetworkOptions{})
			m.handleEvent(dhcp.Event{Type: tc.event, Data: dhcp.Info{IP: "192.168.99.50/24"}}, tc.v6)

			if got := m.boundV4.Load(); got != tc.want {
				t.Errorf("after %q (v6=%v): boundV4 = %v, want %v", tc.event, tc.v6, got, tc.want)
			}
		})
	}
}

func familySuffix(v6 bool) string {
	if v6 {
		return "_v6"
	}
	return "_v4"
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

// TestStart_RecordsPhasesForTheCaller is the check that #411's timing
// actually reaches a reader. #411 logged it on its own Debug line and
// the line went unread: the health floor's evidence dump prints error
// and warning lines, so a run with six "context deadline exceeded"
// failures showed no timing anywhere near them. Instrumentation that
// lands somewhere other than the failure it explains is not
// instrumentation, so the summary is now recorded on the manager and
// the Join failure log folds it in (#406).
func TestStart_RecordsPhasesForTheCaller(t *testing.T) {
	const (
		netID = "net-1"
		epID  = "ep-abcdef"
		ctrID = "container-1"
	)
	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			netID: {Containers: map[string]dNetwork.EndpointResource{
				ctrID: {EndpointID: epID},
			}},
		},
		// The failure under investigation: the container resolves, then
		// inspecting it never answers.
		containerErr: errors.New("context deadline exceeded"),
	}
	m := newDHCPManager(docker, JoinRequest{NetworkID: netID, EndpointID: epID}, DHCPNetworkOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := m.Start(ctx); err == nil {
		t.Fatal("Start succeeded against a daemon that never answers ContainerInspect")
	}

	if m.startPhases == "" {
		t.Fatal("Start recorded no phase summary; the Join failure line will say only 'context deadline exceeded' again")
	}
	if !strings.Contains(m.startPhases, "resolve_container_id=") {
		t.Errorf("phase summary %q does not name the phase that completed", m.startPhases)
	}
	if m.startTotal == "" {
		t.Error("Start recorded no total; a reader cannot tell a 10s timeout from a 200ms one")
	}
}

// TestStart_LeavesNoPhaseRecordOnSuccess keeps the field honest. A
// stale summary from a previous failure, or timing on every successful
// Join, would put noise on the operator's log for no diagnostic gain.
func TestStart_LeavesNoPhaseRecordOnSuccess(t *testing.T) {
	m := newDHCPManager(&fakeDocker{}, JoinRequest{}, DHCPNetworkOptions{})
	if m.startPhases != "" || m.startTotal != "" {
		t.Error("a fresh manager already carries phase timing")
	}
}

// TestStop_CancelsAnInFlightAttach is the guard on the risk the #406
// fix introduces rather than the bug it fixes.
//
// The attach budget grew from AwaitTimeout to AwaitTimeout+60s so a
// daemon that is busy with the container being joined stops being read
// as a plugin failure. Stop waits for Start to finish, so without a
// cancellation path that same 60s would be charged to every Leave that
// arrives during an attach — libnetwork would block for a minute
// waiting on an attach whose container is already going away. A longer
// wait that becomes a longer teardown is not a fix, it is a trade, and
// nobody agreed to that one.
func TestStop_CancelsAnInFlightAttach(t *testing.T) {
	m := newDHCPManager(&fakeDocker{}, JoinRequest{}, DHCPNetworkOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	m.attachCancel = cancel

	// Stand in for an attach parked on an unresponsive daemon: it
	// finishes only when its context is cancelled.
	attachReturned := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(m.startedCh)
		close(attachReturned)
	}()

	m.startErr = errors.New("attach cancelled")
	done := make(chan struct{})
	go func() {
		_ = m.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return; it is waiting out the attach grace instead of cancelling the attach")
	}

	select {
	case <-attachReturned:
	case <-time.After(time.Second):
		t.Error("Stop returned but the attach was never cancelled; the goroutine outlives the endpoint")
	}
}

// TestAttachBudget_ExceedsTheDaemonBusyWindow states the relationship
// the fix depends on as an assertion rather than as a comment. If
// someone later tunes AwaitTimeout or the grace to the point where the
// attach budget no longer clears the client-timeout window that
// produced #406, this says so.
func TestAttachBudget_ExceedsTheDaemonBusyWindow(t *testing.T) {
	// What was measured: five Docker client requests, each giving up at
	// its own 2s timeout, filled a 10s attach budget end to end.
	const observedBusyWindow = 10 * time.Second
	if attachDaemonBusyGrace <= observedBusyWindow {
		t.Errorf("attachDaemonBusyGrace = %v, which does not clear the %v window measured in #406; "+
			"the attach would still be abandoned while the daemon is merely busy",
			attachDaemonBusyGrace, observedBusyWindow)
	}
}

// TestJoin_AttachCancelIsSetBeforeRegistration pins an ordering that a
// reader cannot see from either line on its own.
//
// registerDHCPManager publishes the manager so a fast Leave can find
// it — its own comment says as much. Stop then reads attachCancel. So
// the assignment has to happen before the registration, or a Leave that
// wins the race reads nil, does not cancel, and waits out the full
// attach grace: the exact behaviour TestStop_CancelsAnInFlightAttach
// forbids, reintroduced by a line that merely sits in the wrong place.
//
// Checked as source order because there is no runtime seam between the
// two statements to test against, and the failure mode is a race that a
// unit test would reproduce only occasionally (#406).
func TestJoin_AttachCancelIsSetBeforeRegistration(t *testing.T) {
	src, err := os.ReadFile("network.go")
	if err != nil {
		t.Fatalf("read network.go: %v", err)
	}
	text := string(src)

	assign := strings.Index(text, "m.attachCancel = cancelAttach")
	register := strings.Index(text, "p.registerDHCPManager(r.EndpointID, m)")
	if assign < 0 || register < 0 {
		t.Fatal("could not find both statements; this guard has gone stale and is passing vacuously")
	}
	if assign > register {
		t.Error("m.attachCancel is assigned AFTER registerDHCPManager publishes the manager. " +
			"A Leave arriving in between reads a nil cancel and blocks for the whole attach grace.")
	}
}

// TestStart_SurvivesADaemonThatWillNotAnswer makes the #406 condition
// happen on demand instead of waiting for CI to be unlucky.
//
// Every integration run so far has been a sample: unchanged code has
// scored 6, 5, 4, 3 and 0 Join failures, and a run that scores 0 says
// only that the condition did not arise. That is not a basis for
// deciding whether the grace earns its place, and hoping the next run
// hits it is not a method.
//
// So the fake daemon stalls the way the real one does — accepting the
// call and never answering — and the two budgets are compared directly.
// The measured window was 10s (five Docker client requests, each giving
// up at its own 2s timeout); 15s here is comfortably past it.
func TestStart_SurvivesADaemonThatWillNotAnswer(t *testing.T) {
	const (
		netID     = "net-1"
		epID      = "ep-abcdef"
		ctrID     = "container-1"
		stall     = 120 * time.Millisecond
		oldBudget = 40 * time.Millisecond
	)
	// Scaled down through the same seam the recovery tests use. The
	// ratio is what is being tested — a stall that outlasts the old
	// budget and not the new one — and it holds at any scale. That the
	// SHIPPED constant clears the measured 10s window is a separate
	// assertion, in TestAttachBudget_ExceedsTheDaemonBusyWindow, so
	// shrinking it here cannot quietly weaken that.
	prev := attachDaemonBusyGrace
	attachDaemonBusyGrace = 400 * time.Millisecond
	t.Cleanup(func() { attachDaemonBusyGrace = prev })
	newDocker := func() *fakeDocker {
		return &fakeDocker{
			inspectResult: map[string]dNetwork.Inspect{
				netID: {Containers: map[string]dNetwork.EndpointResource{
					ctrID: {EndpointID: epID},
				}},
			},
			containerResult: map[string]dContainer.InspectResponse{
				ctrID: {ContainerJSONBase: &dContainer.ContainerJSONBase{
					State: &dContainer.State{Pid: 1},
				}, Config: &dContainer.Config{Hostname: "h"}},
			},
			containerDelay: stall,
		}
	}

	t.Run("the old budget gives up on it", func(t *testing.T) {
		m := newDHCPManager(newDocker(), JoinRequest{NetworkID: netID, EndpointID: epID}, DHCPNetworkOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), oldBudget)
		defer cancel()
		err := m.Start(ctx)
		if err == nil {
			t.Fatal("Start succeeded on the pre-#406 budget; the stall is not reproducing the condition")
		}
		if !strings.Contains(err.Error(), "Docker container info") {
			t.Errorf("failed somewhere other than the inspect: %v", err)
		}
	})

	t.Run("the grace outlasts it", func(t *testing.T) {
		d := newDocker()
		m := newDHCPManager(d, JoinRequest{NetworkID: netID, EndpointID: epID}, DHCPNetworkOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), oldBudget+attachDaemonBusyGrace)
		defer cancel()
		err := m.Start(ctx)
		// Start goes on to open a netns and locate a link, neither of
		// which exists here, so it still fails — but it must get PAST
		// the inspect. Reaching a later phase is the whole claim.
		if err != nil && strings.Contains(err.Error(), "Docker container info") {
			t.Fatalf("still gave up at the inspect with the grace applied: %v", err)
		}
		if d.containerCalls == 0 {
			t.Error("the inspect was never attempted")
		}
	})
}

// hintMAC / linkMAC are deliberately different so a test can tell which
// source a derivation actually used.
var (
	hintMAC = net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x03}
	linkMAC = net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x99}
)

func managerWithMACs(mode string, hint, link net.HardwareAddr) *dhcpManager {
	m := &dhcpManager{
		joinReq:    JoinRequest{EndpointID: "0123456789abcdef0123456789abcdef"},
		opts:       DHCPNetworkOptions{Mode: mode},
		MacAddress: hint,
	}
	if link != nil {
		m.ctrLink = &netlink.Device{LinkAttrs: netlink.LinkAttrs{HardwareAddr: link}}
	}
	return m
}

// TestEndpointMAC_PrefersTheRecordedMAC is the guard against the drift
// #371 made possible. The DHCP identity is keyed to the MAC the
// CreateEndpoint one-shot ran under; that MAC is recorded on the join
// hint. Reading it off whatever link happens to be in hand instead
// would produce a different identity the moment the two disagree — and
// the orphan-release path (#370) runs when there is no link at all.
func TestEndpointMAC_PrefersTheRecordedMAC(t *testing.T) {
	t.Run("recorded MAC wins over the live link", func(t *testing.T) {
		m := managerWithMACs("", hintMAC, linkMAC)
		if got := m.endpointMAC(); got.String() != hintMAC.String() {
			t.Errorf("got %v, want the recorded %v — the lease is keyed to the one-shot's MAC, not the link's", got, hintMAC)
		}
	})

	t.Run("falls back to the live link when nothing was recorded", func(t *testing.T) {
		m := managerWithMACs("", nil, linkMAC)
		if got := m.endpointMAC(); got.String() != linkMAC.String() {
			t.Errorf("got %v, want fallback %v", got, linkMAC)
		}
	})

	t.Run("nil when neither is available", func(t *testing.T) {
		m := managerWithMACs("", nil, nil)
		if got := m.endpointMAC(); len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}

// TestJoin_AttachBudgetIncludesTheGrace closes the gap between the two
// tests above.
//
// TestStart_SurvivesADaemonThatWillNotAnswer proves a longer budget
// outlasts a stalled daemon, but it builds its own context, so deleting
// the grace from the Join path would not make it fail.
// TestAttachBudget_ExceedsTheDaemonBusyWindow proves the constant is
// large enough, but not that anything uses it. Between them sits the
// line that actually matters, and neither covers it.
//
// Static, like the ordering guard: the budget is built inside a
// goroutine in a handler that needs a live libnetwork request, and a
// check this cheap should not need one.
func TestJoin_AttachBudgetIncludesTheGrace(t *testing.T) {
	src, err := os.ReadFile("network.go")
	if err != nil {
		t.Fatalf("read network.go: %v", err)
	}
	const want = "p.awaitTimeout+attachDaemonBusyGrace"
	if !strings.Contains(string(src), want) {
		t.Errorf("the Join attach budget is no longer %s. Either the grace was removed "+
			"(in which case #406 is back: a daemon busy with the container being joined "+
			"leaves it without a renewal client) or it moved, and this guard needs updating "+
			"deliberately rather than by deleting it.", want)
	}
}

// TestManagerClientID_DoesNotDependOnALiveLink pins the property the
// whole helper exists for: the id must be the same whether or not the
// container's link is still around.
//
// It used to be phrased as "the same across call sites", because the
// removed orphaned-lease reclaim was a second caller that ran after the
// link was gone. There is one caller now, and the property is if
// anything more load-bearing than it was (#800): the plugin no longer
// releases, so a restarting container gets its address back only by
// presenting the same option-61 identity the one-shot used and being
// recognised. An id that quietly changed when the link went away would
// mean a different address on every restart.
func TestManagerClientID_DoesNotDependOnALiveLink(t *testing.T) {
	withLink := managerWithMACs("", hintMAC, linkMAC)
	afterContainerGone := managerWithMACs("", hintMAC, nil)

	joined := withLink.clientID()
	releasing := afterContainerGone.clientID()
	if string(joined) != string(releasing) {
		t.Fatalf("id drifted once the container link was gone: join=%x release=%x", joined, releasing)
	}
	if string(joined) != string(hintMAC) {
		t.Errorf("got %x, want MAC-derived %x", joined, []byte(hintMAC))
	}
}

// TestManagerClientID_ModeAndOverride checks the manager-level helper
// honours the same rules resolveClientID does, so routing every call
// site through it changes no semantics.
func TestManagerClientID_ModeAndOverride(t *testing.T) {
	eid := "0123456789abcdef0123456789abcdef"

	t.Run("macvlan derives from the MAC", func(t *testing.T) {
		m := managerWithMACs("macvlan", hintMAC, nil)
		if got := m.clientID(); string(got) != string(hintMAC) {
			t.Errorf("got %x, want %x", got, []byte(hintMAC))
		}
	})

	t.Run("ipvlan stays endpoint-derived", func(t *testing.T) {
		// ipvlan slaves share the parent's MAC, so a MAC-derived id
		// would be identical for every container on the network.
		m := managerWithMACs("ipvlan", hintMAC, nil)
		got := m.clientID()
		if want := clientIDFromEndpoint(eid); string(got) != string(want) {
			t.Errorf("got %x, want endpoint-derived %x", got, want)
		}
		if string(got) == string(hintMAC) {
			t.Error("ipvlan derived from the shared parent MAC; every container would claim one lease")
		}
	})

	t.Run("operator override wins", func(t *testing.T) {
		m := managerWithMACs("", hintMAC, nil)
		m.opts.ClientID = "my-id"
		if got := m.clientID(); string(got) != "my-id" {
			t.Errorf("got %q, want %q", got, "my-id")
		}
	})
}

// TestStop_NeverBoundV6ClientIsNotAuditedAsReleased is the v6 half of
// the honesty rule (#608).
//
// Until #608 the v6 client was judged on its exit error alone: signalled
// before it bound it exits cleanly, so the ledger recorded a release for
// the IA_NA address the one-shot had taken — the ledger asserting the
// server saw a DHCPv6 RELEASE for an address no client ever held a
// binding to release.
//
// The v4 client is bound in every row, so exactly one honest v4 entry is
// expected and the v6 half must add nothing. Both entry points are
// asserted and expect the SAME result: since #800 the lease is treated
// identically whether or not the endpoint is leaving, and a row here
// that differed would mean that rule had been quietly reintroduced on
// the v6 side.
func TestStop_NeverBoundV6ClientIsNotAuditedAsReleased(t *testing.T) {
	errSignalled := errors.New("signal: terminated")

	for _, tc := range []struct {
		name    string
		errV6   error
		leaving bool
	}{
		{name: "clean exit, leaving", leaving: true},
		{name: "clean exit, not leaving", leaving: false},
		{name: "killed on the signal, leaving", errV6: errSignalled, leaving: true},
		{name: "killed on the signal, not leaving", errV6: errSignalled, leaving: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)

			m := stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true, IPv6: true}, nil, tc.errV6)
			m.boundV6.Store(false)

			if err := m.stop(tc.leaving); err != nil {
				t.Errorf("stop(%v) = %v, want nil — a v6 client that never bound "+
					"cannot have failed to release, whatever its exit status", tc.leaving, err)
			}

			if got := p.clientStopFailuresV4.Load() + p.clientStopFailuresV6.Load(); got != 0 {
				t.Errorf("client_stop_failures(+v6) = %d, want 0 — no client we were "+
					"running failed to shut down", got)
			}

			var kinds []string
			for _, e := range readLedgerLines(t, p.ledger.path) {
				kinds = append(kinds, e.Kind)
				if e.IP == "fd00::50" {
					t.Errorf("ledger recorded %q for %s, but the v6 client never held "+
						"a binding; there is no v6 lease event to write down", e.Kind, e.IP)
				}
			}
			// The v4 client DID bind and DID shut down cleanly, so
			// exactly one honest entry is expected. Asserting it here
			// is what stops this test passing on a plugin that audits
			// nothing at all.
			if !slices.Equal(kinds, []string{"stopped"}) {
				t.Errorf("ledger kinds = %v, want [stopped] — the bound v4 client's "+
					"own entry, and nothing from v6", kinds)
			}
		})
	}
}

// TestStop_BoundV6StopFailureIsCountedPerFamily guards the other
// direction of #608: a v6 client that DID hold its binding and failed to
// shut down is still a real failure — audited as such, returned as an
// error, and counted on the v6 split so a dual-stack operator can tell
// which family failed. The v4 row pins that the split does not move on a
// v4 failure.
//
// What it no longer means is that a lease was not handed back. Nothing
// is handed back on any path since #800; this counts a client that did
// not exit cleanly when signalled, which is why it is
// client_stop_failures and not the lease_release_failures it was called
// when the plugin still released.
func TestStop_BoundV6StopFailureIsCountedPerFamily(t *testing.T) {
	boom := errors.New("release boom")
	for _, tc := range []struct {
		name          string
		errV4, errV6  error
		wantAgg, want int32
	}{
		{name: "v6 fails", errV6: boom, wantAgg: 1, want: 1},
		{name: "v4 fails", errV4: boom, wantAgg: 1, want: 0},
		{name: "both fail", errV4: boom, errV6: boom, wantAgg: 2, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)
			m := stoppingManager(t, p, DHCPNetworkOptions{AuditLog: true, IPv6: true}, tc.errV4, tc.errV6)

			if err := m.StopForLeave(); !errors.Is(err, boom) {
				t.Errorf("StopForLeave() = %v, want an error wrapping %v — a bound client "+
					"that fails to shut down is a real failure, not swallowed by the "+
					"never-bound handling", err, boom)
			}

			// wantAgg is the total across both families. Since #730 it
			// is their sum, so assert the sum AND the v4 half: the two
			// together say which counter moved, not merely how many
			// bumps happened.
			if got := p.clientStopFailuresV4.Load() + p.clientStopFailuresV6.Load(); got != tc.wantAgg {
				t.Errorf("client_stop_failures (v4+v6) = %d, want %d", got, tc.wantAgg)
			}
			if got, wantV4 := p.clientStopFailuresV4.Load(), tc.wantAgg-tc.want; got != wantV4 {
				t.Errorf("client_stop_failures_v4 = %d, want %d", got, wantV4)
			}
			if got := p.clientStopFailuresV6.Load(); got != tc.want {
				t.Errorf("client_stop_failures_v6 = %d, want %d", got, tc.want)
			}
		})
	}
}
