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
	// Every case in this file models a client that reached a bind and is
	// now being shut down, which is what makes "release" the honest
	// ledger entry. Without this the manager is in the never-bound state
	// instead, where Stop must NOT claim a release — see
	// TestStop_NeverBoundClientReclaimsInsteadOfClaimingRelease.
	m.boundV4.Store(true)

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

// TestStop_NeverBoundClientReclaimsInsteadOfClaimingRelease pins the
// third state Stop used to fall between (#549).
//
// CreateEndpoint's one-shot acquires the address and deliberately keeps
// it, because handing over is the persistent client's job. Stop knew two
// endings: Start failed (reclaim) and Start succeeded (let dhcpcd's
// `release` directive do it). A client that starts and is SIGTERMed
// before it ever binds is neither — dhcpcd releases only a lease it
// holds, so it sends nothing, and startErr is nil so the reclaim never
// ran. The address stayed held upstream with nobody responsible, while
// the ledger recorded a release the server never saw.
//
// Both halves are asserted, because the second is the one that kept the
// first invisible.
//
// Mode-independent by construction: this is a Join/Leave ordering race,
// not an ipvlan property. It was found through an ipvlan test only
// because that test deliberately races container exit against Join.
func TestStop_NeverBoundClientReclaimsInsteadOfClaimingRelease(t *testing.T) {
	var ledgerFailures atomic.Int32
	p := &Plugin{}
	p.ledger = testLedger(t, &ledgerFailures)

	// A network with neither parent nor bridge: the reclaim has no path
	// to the wire, so it lands on orphanedLeaseReleaseFailures. That is
	// what makes "the reclaim ran at all" observable in a unit test,
	// which is the fact under test here — whether it can reach a real
	// DHCP server is the integration test's job.
	m := releasingManager(t, p, DHCPNetworkOptions{AuditLog: true}, nil, nil)
	m.boundV4.Store(false)

	if err := m.StopForLeave(); err != nil {
		t.Fatalf("StopForLeave() = %v, want nil", err)
	}
	p.orphanReleases.Wait()

	if got := p.orphanedLeaseReleaseFailures.Load(); got != 1 {
		t.Errorf("orphaned_lease_release_failures = %d, want 1 — the lease the "+
			"one-shot acquired was never handed to the reclaim", got)
	}
	if got := p.leaseReleaseFailures.Load(); got != 0 {
		t.Errorf("leaseReleaseFailures = %d, want 0 — nothing we were running "+
			"failed to release; no client ever held this lease", got)
	}

	for _, e := range readLedgerLines(t, p.ledger.path) {
		if e.Kind == "release" {
			t.Fatalf("ledger recorded %q for %s, but the client never held a "+
				"binding and no DHCPRELEASE was sent", e.Kind, e.IP)
		}
	}
}

// A bound client is the unchanged path, asserted alongside the above so
// the reclaim cannot start firing on every ordinary teardown — that
// would send a second DHCPRELEASE for an address the server has already
// freed and may have reallocated.
func TestStop_BoundClientDoesNotReclaim(t *testing.T) {
	var ledgerFailures atomic.Int32
	p := &Plugin{}
	p.ledger = testLedger(t, &ledgerFailures)

	m := releasingManager(t, p, DHCPNetworkOptions{AuditLog: true}, nil, nil)

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	p.orphanReleases.Wait()

	if got := p.orphanedLeasesReleased.Load() + p.orphanedLeaseReleaseFailures.Load(); got != 0 {
		t.Errorf("reclaim ran %d time(s) for a client that held its lease; "+
			"dhcpcd already released it", got)
	}
}

// TestStop_NeverBoundIsNotReclaimedUnlessLeaving is the guard on the
// reclaim's blast radius, and it matters more than the reclaim itself.
//
// Plain Stop() is what plugin Close calls, once per live manager, so
// their dhcpcds can release before the process exits — on a plugin
// upgrade or `docker plugin disable`, with every container still
// running. A manager can be never-bound there for an ordinary reason:
// its persistent client came up against a DHCP server that is not
// answering. The address is still configured inside that container and
// still in use.
//
// Reclaiming it would tell the server an address is free while a live
// container holds it, and the server would be entitled to hand it to
// somebody else — the duplicate-assignment failure this release added
// conflict detection for (#524), manufactured by the plugin. A missed
// reclaim only leaves the leak that existed before; a wrong one creates
// an outage.
//
// The same applies to a displaced manager and to managers stopped on
// network removal, which is why the reclaim is opt-in at the call site
// rather than a flag that defaults to on.
func TestStop_NeverBoundIsNotReclaimedUnlessLeaving(t *testing.T) {
	var ledgerFailures atomic.Int32
	p := &Plugin{}
	p.ledger = testLedger(t, &ledgerFailures)

	m := releasingManager(t, p, DHCPNetworkOptions{AuditLog: true}, nil, nil)
	m.boundV4.Store(false)

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	p.orphanReleases.Wait()

	if got := p.orphanedLeasesReleased.Load() + p.orphanedLeaseReleaseFailures.Load(); got != 0 {
		t.Errorf("reclaim ran %d time(s) on a plain Stop; the container may still "+
			"be running and using this address", got)
	}

	// Still must not claim a release that did not happen. Nothing was
	// audited at all here, so the ledger was never even created — the
	// same shape TestStop_FailedStartIsANoOp asserts, and the honest
	// record: no DHCPRELEASE was sent, so none is written down.
	if _, err := os.Stat(p.ledger.path); !os.IsNotExist(err) {
		for _, e := range readLedgerLines(t, p.ledger.path) {
			if e.Kind == "release" {
				t.Errorf("ledger recorded %q for %s on a client that never bound", e.Kind, e.IP)
			}
		}
	}
}

// TestStop_NeverBoundClientKilledBySignalIsStillNeverBound covers the
// case #558 left behind, which shipped as a lease leak (#607).
//
// The #549 tests above all model a client that exits cleanly, and the
// code they pinned read the exit error FIRST — so it only ever reached
// the never-bound branch when errV4 was nil. dhcpcd exits 0 on SIGTERM
// once it has installed its handler; signalled before that it dies ON
// the signal and Finish reaps an exit status instead. A Leave that
// arrives immediately after Join produces exactly that, and the lease
// was then classified as a failed release and never reclaimed.
//
// The exit status is not evidence of anything here: we sent that
// SIGTERM. What decides the outcome is that the client never held a
// binding, so it cannot have failed to release one.
//
// Both `leaving` values are asserted, because the fix must not widen
// the reclaim's blast radius while it is widening its reach — see
// TestStop_NeverBoundIsNotReclaimedUnlessLeaving for why a wrong
// reclaim is worse than a missed one.
func TestStop_NeverBoundClientKilledBySignalIsStillNeverBound(t *testing.T) {
	// Stands in for the *exec.ExitError that Finish reaps when dhcpcd
	// dies on the signal. The production path distinguishes nothing
	// beyond non-nil, so the concrete type is not what is under test —
	// the string is the one operators see in the plugin log.
	errSignalled := errors.New("signal: terminated")

	for _, tc := range []struct {
		name        string
		leaving     bool
		wantReclaim int32
		// The ledger cannot tell these two writers apart on its own: an
		// unfixed stop() writes release_failed here, and so does a
		// reclaim that could not reach the wire. wantReclaim is what
		// separates them, and it is the assertion that goes red on the
		// bug. The kinds are asserted alongside it so a fix that
		// reclaims AND still audits from stop() is caught too.
		wantKinds []string
	}{
		{
			name:    "leaving reclaims the lease nobody took",
			leaving: true, wantReclaim: 1,
			wantKinds: []string{"release_failed"}, // the reclaim's own, honest: not handed back
		},
		{
			name:    "not leaving leaves a live container's address alone",
			leaving: false, wantReclaim: 0,
			wantKinds: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerFailures atomic.Int32
			p := &Plugin{}
			p.ledger = testLedger(t, &ledgerFailures)

			// As in the #549 test: no parent and no bridge, so the
			// reclaim cannot reach the wire and lands on
			// orphanedLeaseReleaseFailures. That it ran at all is the
			// fact under test.
			m := releasingManager(t, p, DHCPNetworkOptions{AuditLog: true}, errSignalled, nil)
			m.boundV4.Store(false)

			err := m.stop(tc.leaving)
			// Errorf, not Fatalf: the reclaim assertion below is the
			// one that fails on the leak itself, and a run that stopped
			// here would hide it behind the cosmetic half.
			if err != nil {
				t.Errorf("stop(%v) = %v, want nil — we sent the SIGTERM and the "+
					"lease was handled, so nothing failed; returning this is what "+
					"made Leave answer 500 for a correct teardown", tc.leaving, err)
			}
			p.orphanReleases.Wait()

			if got := p.orphanedLeaseReleaseFailures.Load(); got != tc.wantReclaim {
				t.Errorf("reclaim ran %d time(s), want %d — the client died on the "+
					"signal before it bound, which does not change what is owed",
					got, tc.wantReclaim)
			}
			if got := p.leaseReleaseFailures.Load(); got != 0 {
				t.Errorf("leaseReleaseFailures = %d, want 0 — no client we were "+
					"running failed to hand a lease back; none ever held one", got)
			}

			var kinds []string
			if _, err := os.Stat(p.ledger.path); !os.IsNotExist(err) {
				for _, e := range readLedgerLines(t, p.ledger.path) {
					kinds = append(kinds, e.Kind)
					if e.Kind == "release" {
						t.Errorf("ledger recorded %q for %s, but the client never held "+
							"a binding, so no DHCPRELEASE was ever sent", e.Kind, e.IP)
					}
				}
			}
			if !slices.Equal(kinds, tc.wantKinds) {
				t.Errorf("ledger kinds = %v, want %v", kinds, tc.wantKinds)
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

// TestManagerClientID_IsStableAcrossCallSites pins the property the
// whole helper exists for: the renewal client (setupClient) and the
// synthesised release of an orphaned lease (synthesiseRelease) must
// present the SAME option-61 id, or the server treats them as
// different clients and the lease is neither renewed nor freed. Both
// now route through m.clientID(), so the id must not depend on whether
// a container link is still around — which it is not, by the time an
// orphan release runs.
func TestManagerClientID_IsStableAcrossCallSites(t *testing.T) {
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
