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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

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
		p := &Plugin{}
		m := &dhcpManager{plugin: p}

		if err := m.renew(false, dhcp.Info{IP: addr1.String()}); err != nil {
			t.Fatalf("renew: %v", err)
		}

		if got := p.leaseChangedV4.Load(); got != 0 {
			t.Errorf("leaseChangedV4 = %d, want 0 (first bind shouldn't count as a change)", got)
		}
	})

	t.Run("v6 changed IP bumps the v6 half only", func(t *testing.T) {
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
		m := &dhcpManager{plugin: nil}
		m.setLastIP(false, addr1)

		if err := m.renew(false, dhcp.Info{IP: addr2.String()}); err != nil {
			t.Fatalf("renew: %v", err)
		}
	})
}

// TestHandleEvent_Counters pins the nak arm here: dnsmasq ignores refused renewals in several shapes without a
// DHCPNAK (#128).
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
		total := p.leasesObtainedV4.Load() + p.leasesRenewedV4.Load() +
			p.dhcpTimeoutsV4.Load() + p.naksReceivedV4.Load() +
			p.leasesObtainedV6.Load() + p.leasesRenewedV6.Load() +
			p.dhcpTimeoutsV6.Load() + p.naksReceivedV6.Load() +
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

// NsHandle.IsOpen is `ns != -1`, so the zero value reports open and Stop would close fd 0; set netns.None().
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
	m.boundV4.Store(true)

	m.errChan = make(chan error, 1)
	m.errChan <- errV4

	if opts.IPv6 {
		v6, err := netlink.ParseAddr("fd00::50/64")
		if err != nil {
			t.Fatalf("ParseAddr v6: %v", err)
		}
		m.setLastIP(true, v6)
		m.boundV6.Store(true)

		m.errChanV6 = make(chan error, 1)
		m.errChanV6 <- errV6
	}
	return m
}

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

			if entries[0].IP != "192.168.99.50" {
				t.Errorf("v4 entry IP = %q, want 192.168.99.50", entries[0].IP)
			}
			if tc.ipv6 && entries[1].IP != "fd00::50" {
				t.Errorf("v6 entry IP = %q, want fd00::50", entries[1].IP)
			}
		})
	}
}

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

// TestStop_LeavingAndNotLeavingAreTheSame: `docker restart` is a Leave then a Join for the same MAC, so both stop
// paths leave the lease alone (#800).
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
			for _, k := range kinds {
				if strings.Contains(k, "release") {
					t.Errorf("ledger kind %q names a release; no client releases a "+
						"lease on a network that does not set release_lease, and none "+
						"of these does (#800, #962)", k)
				}
			}
		})
	}
}

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

func TestStart_CarriesNoPhaseRecordBeforeItRuns(t *testing.T) {
	m := newDHCPManager(&fakeDocker{}, JoinRequest{}, DHCPNetworkOptions{})
	if m.startPhases != "" || m.startTotal != "" {
		t.Error("a fresh manager already carries phase timing")
	}
}

func TestStop_CancelsAnInFlightAttach(t *testing.T) {
	m := newDHCPManager(&fakeDocker{}, JoinRequest{}, DHCPNetworkOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	m.attachCancel = cancel

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

func TestAttachBudget_ExceedsTheDaemonBusyWindow(t *testing.T) {
	// Measured: five Docker client requests, each timing out at 2 s, filled a 10 s attach budget (#406).
	const observedBusyWindow = 10 * time.Second
	if attachDaemonBusyGrace <= observedBusyWindow {
		t.Errorf("attachDaemonBusyGrace = %v, which does not clear the %v window measured in #406; "+
			"the attach would still be abandoned while the daemon is merely busy",
			attachDaemonBusyGrace, observedBusyWindow)
	}
}

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

func TestStart_SurvivesADaemonThatWillNotAnswer(t *testing.T) {
	const (
		netID     = "net-1"
		epID      = "ep-abcdef"
		ctrID     = "container-1"
		stall     = 120 * time.Millisecond
		oldBudget = 40 * time.Millisecond
	)
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
		if err != nil && strings.Contains(err.Error(), "Docker container info") {
			t.Fatalf("still gave up at the inspect with the grace applied: %v", err)
		}
		if d.containerCalls == 0 {
			t.Error("the inspect was never attempted")
		}
	})
}

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

func TestManagerClientID_DoesNotDependOnALiveLink(t *testing.T) {
	withLink := managerWithMACs("", hintMAC, linkMAC)
	afterContainerGone := managerWithMACs("", hintMAC, nil)

	joined := withLink.clientID(nil)
	releasing := afterContainerGone.clientID(nil)
	if string(joined) != string(releasing) {
		t.Fatalf("id drifted once the container link was gone: join=%x release=%x", joined, releasing)
	}
	if string(joined) != string(hintMAC) {
		t.Errorf("got %x, want MAC-derived %x", joined, []byte(hintMAC))
	}

	identity := dhcp.ClientIdentity([]byte("record-identity"))
	joinedRebound := withLink.clientID(identity)
	releasingRebound := afterContainerGone.clientID(identity)
	if string(joinedRebound) != string(releasingRebound) {
		t.Fatalf("a re-bound record's id drifted once the container link was gone: join=%x release=%x",
			joinedRebound, releasingRebound)
	}
	if string(joinedRebound) != "record-identity" {
		t.Errorf("got %x, want the record's identity %q", joinedRebound, "record-identity")
	}
}

func TestManagerClientID_ModeAndOverride(t *testing.T) {
	eid := "0123456789abcdef0123456789abcdef"

	t.Run("macvlan derives from the MAC", func(t *testing.T) {
		m := managerWithMACs("macvlan", hintMAC, nil)
		if got := m.clientID(nil); string(got) != string(hintMAC) {
			t.Errorf("got %x, want %x", got, []byte(hintMAC))
		}
	})

	t.Run("ipvlan stays endpoint-derived", func(t *testing.T) {
		// ipvlan slaves share the parent's MAC, so a MAC-derived id would be the same for every container (#219).
		m := managerWithMACs("ipvlan", hintMAC, nil)
		got := m.clientID(nil)
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
		if got := m.clientID(nil); string(got) != "my-id" {
			t.Errorf("got %q, want %q", got, "my-id")
		}
	})
}

// TestManagerClientID_TheRecordsIdentityWins: a client-id from the new MAC got a DHCPNAK after the reservation's ACK,
// and the container ran on another address (#1047).
func TestManagerClientID_TheRecordsIdentityWins(t *testing.T) {
	stored := dhcp.ClientIdentity([]byte{0xde, 0xad, 0xbe, 0xef})

	for _, tc := range []struct {
		name     string
		mode     string
		clientID string
		identity []byte
		want     string
		why      string
	}{
		{
			name: "no record identity leaves the derived id alone",
			want: string(hintMAC),
			why:  "a fresh endpoint has nothing to resume and must send what its MAC derives",
		},
		{
			name:     "a re-bound record's identity wins over the MAC",
			identity: stored,
			want:     "\xde\xad\xbe\xef",
			why:      "the server has the re-bound address filed under this and under nothing else",
		},
		{
			name:     "a re-bound record's identity wins over the operator's client_id",
			clientID: "operator-id",
			identity: stored,
			want:     "\xde\xad\xbe\xef",
			why: "the reservation half already prefers the record here; an operator setting that " +
				"won on this half alone would put the two exchanges back out of step and NAK the address",
		},
		{
			name:     "the operator's client_id still wins with no record identity",
			clientID: "operator-id",
			want:     "operator-id",
			why:      "a fresh endpoint on a network that sets client_id is unchanged by any of this",
		},
		{
			name:     "an identity this build never wrote is refused",
			identity: []byte{0x01, 0x02, 0x03},
			want:     string(hintMAC),
			why: "a DUID or any other shape is not an option-61 payload, and re-sending its tail " +
				"would put a value on the wire that no record describes",
		},
		{
			name:     "an identity of only the type byte is refused",
			identity: []byte{0x00},
			want:     string(hintMAC),
			why:      "there is no payload to send",
		},
		{
			name:     "ipvlan keeps the record's identity too",
			mode:     "ipvlan",
			identity: stored,
			want:     "\xde\xad\xbe\xef",
			why:      "the mode decides what is DERIVED, and a record's identity is not derived",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := managerWithMACs(tc.mode, hintMAC, nil)
			m.opts.ClientID = tc.clientID
			if got := m.clientID(tc.identity); string(got) != tc.want {
				t.Errorf("client identifier on the wire = %x, want %x. %s", got, tc.want, tc.why)
			}
		})
	}
}

func TestClientIDWiring_OneCallSiteAndTheV4IdentityOnly(t *testing.T) {
	src, err := os.ReadFile("dhcp_manager.go")
	if err != nil {
		t.Fatalf("read dhcp_manager.go: %v", err)
	}
	text := string(src)

	const call = "ClientID:    m.clientID(v4Identity),"
	if got := strings.Count(text, call); got != 1 {
		t.Errorf("%q appears %d time(s), want exactly 1. The persistent client's option 61 is "+
			"the identity the reservation used, and a second call site or a different argument "+
			"is how the two halves drifted apart in the first place", call, got)
	}
	if got := strings.Count(text, "m.clientID("); got != 1 {
		t.Errorf("m.clientID( is called %d time(s) in dhcp_manager.go, want exactly 1", got)
	}

	const assign = "v4Identity = resumption.Identity"
	if got := strings.Count(text, assign); got != 1 {
		t.Fatalf("%q appears %d time(s), want exactly 1", assign, got)
	}

	start := strings.Index(text, "m.recordID, resumption = m.resumeFromRecord()")
	if start < 0 {
		t.Fatal("the v4 resume moved; this guard needs updating deliberately")
	}
	end := strings.Index(text[start:], "m.recordID6, resumption, identity6 = m.resumeFromRecord6()")
	if end < 0 {
		t.Fatal("the v6 resume moved; this guard needs updating deliberately")
	}
	if at := strings.Index(text, assign); at < start || at > start+end {
		t.Errorf("%q is not inside the IPv4 branch of setupClient. A v6 record's identity is a "+
			"DUID and an IAID; sending its tail as option 61 would be a value no record describes",
			assign)
	}
}

func TestManagerClientID_IsTheReservationsIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		clientID string
	}{
		{name: "derived on both halves"},
		{name: "operator client_id on the network", clientID: "operator-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := DHCPNetworkOptions{ClientID: tc.clientID}

			stored := dhcp.ClientIdentity(resolveClientID(opts, "", hintMAC))

			reservation := ipamExchangeClientID(resolveClientID(opts, "", linkMAC), stored)

			m := managerWithMACs("", linkMAC, linkMAC)
			m.opts = opts
			client := m.clientID(stored)

			if string(client) != string(reservation) {
				t.Fatalf("the reservation asks as %x and the container's client asks as %x. "+
					"The server files the lease under the first and NAKs the second, so the "+
					"container comes back on a different address than Docker was told about",
					reservation, client)
			}
			if payload, _ := dhcp.ClientIDPayload(stored); string(client) != string(payload) {
				t.Errorf("both halves agree on %x, which is not the record's identity %x", client, payload)
			}
		})
	}
}

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
			if !slices.Equal(kinds, []string{"stopped"}) {
				t.Errorf("ledger kinds = %v, want [stopped] — the bound v4 client's "+
					"own entry, and nothing from v6", kinds)
			}
		})
	}
}

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

func TestRenew_ADeprecatedV6LeaseWithNoAddressListKeepsItsDeprecation(t *testing.T) {
	m := &dhcpManager{plugin: &Plugin{}}

	if err := m.renew(true, dhcp.Info{IP: "2001:db8:1::a/64", IPDeprecated: true}); err != nil {
		t.Fatalf("renew: %v", err)
	}

	_, got := m.lastIPs()
	if got == nil {
		t.Fatal("the renewal recorded no IPv6 address")
	}
	if got.ValidLft != infiniteLft || got.PreferedLft != 0 {
		t.Errorf("the deprecated address was applied as ValidLft=%d PreferedLft=%d, want %d "+
			"and 0: both lifetimes zero sends no IFA_CACHEINFO and the kernel makes the "+
			"address permanent and preferred", got.ValidLft, got.PreferedLft, infiniteLft)
	}

	keep := &dhcpManager{plugin: &Plugin{}}
	if err := keep.renew(true, dhcp.Info{IP: "2001:db8:1::a/64"}); err != nil {
		t.Fatalf("renew: %v", err)
	}
	_, kept := keep.lastIPs()
	if kept == nil {
		t.Fatal("the control renewal recorded no IPv6 address")
	}
	if kept.ValidLft != 0 || kept.PreferedLft != 0 {
		t.Errorf("an infinite lease gave ValidLft=%d PreferedLft=%d, want both zero",
			kept.ValidLft, kept.PreferedLft)
	}

	finite := &dhcpManager{plugin: &Plugin{}}
	if err := finite.renew(true, dhcp.Info{
		IP:               "2001:db8:1::a/64",
		LeaseSeconds:     7200,
		PreferredSeconds: 3600,
	}); err != nil {
		t.Fatalf("renew: %v", err)
	}
	_, timed := finite.lastIPs()
	if timed == nil {
		t.Fatal("the timed renewal recorded no IPv6 address")
	}
	if timed.ValidLft != 7200 || timed.PreferedLft != 3600 {
		t.Errorf("a lease of 7200s preferred 3600s was applied as ValidLft=%d "+
			"PreferedLft=%d: the kernel counts down what it was given, so a number this "+
			"path invents is the deadline the address really has",
			timed.ValidLft, timed.PreferedLft)
	}
}
