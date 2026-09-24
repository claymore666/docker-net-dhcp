// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// fakeJoinClient records every name call because the library renews early for each one (#961).
type fakeJoinClient struct {
	mode   proto.ConflictMode
	phase  proto.ACDPhase
	l      lease.Lease
	bound  bool
	names  []string
	setErr error
	onSet  func()
}

func (f *fakeJoinClient) ConflictMode() proto.ConflictMode { return f.mode }
func (f *fakeJoinClient) ACDPhase() proto.ACDPhase         { return f.phase }
func (f *fakeJoinClient) Lease() (lease.Lease, bool)       { return f.l, f.bound }

func (f *fakeJoinClient) SetHostname(name string) error {
	f.names = append(f.names, name)
	if f.onSet != nil {
		f.onSet()
	}
	return f.setErr
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("bad fixture time %q: %v", s, err)
	}
	return v
}

func TestEndpointViews_TwoEndpointsRenderTheirOwnFields(t *testing.T) {
	p := newHealthPlugin()

	bound := &fakeJoinClient{
		mode:  proto.ConflictWait,
		phase: proto.ACDDefending,
		bound: true,
		l: lease.Lease{
			Addr:     netip.MustParsePrefix("192.0.2.17/24"),
			ServerID: netip.MustParseAddr("192.0.2.1"),
			Renew:    mustTime(t, "2026-09-05T10:00:00Z"),
			Rebind:   mustTime(t, "2026-09-05T10:30:00Z"),
			Expire:   mustTime(t, "2026-09-05T11:00:00Z"),
		},
	}
	acquiring := &fakeJoinClient{mode: proto.ConflictAsync, phase: proto.ACDProbing}

	mBound := newDHCPManager(nil, JoinRequest{
		EndpointID: "aaaaaaaaaaaabbbbbbbbbbbb",
		NetworkID:  "111111111111222222222222",
	}, DHCPNetworkOptions{Mode: ModeMacvlan})
	mBound.setHealthClient(bound)
	mBound.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{IP: "192.0.2.17/24"}}, false)

	mAcquiring := newDHCPManager(nil, JoinRequest{
		EndpointID: "ccccccccccccdddddddddddd",
		NetworkID:  "333333333333444444444444",
	}, DHCPNetworkOptions{Mode: ModeIPvlan})
	mAcquiring.setHealthClient(acquiring)

	p.persistentDHCP["one"] = mBound
	p.persistentDHCP["two"] = mAcquiring

	views := p.endpointViews()
	byEndpoint := map[string]EndpointHealth{}
	for _, v := range views {
		byEndpoint[v.Endpoint] = v
	}

	got, ok := byEndpoint["aaaaaaaaaaaa"]
	if !ok {
		t.Fatalf("no entry for the bound endpoint; got %+v", views)
	}
	for _, c := range []struct{ name, got, want string }{
		{"network", got.Network, "111111111111"},
		{"mode", got.Mode, ModeMacvlan},
		{"address", got.Address, "192.0.2.17/24"},
		{"lease_state", got.LeaseState, "bound"},
		{"server", got.Server, "192.0.2.1"},
		{"renew_at", got.RenewAt, "2026-09-05T10:00:00Z"},
		{"rebind_at", got.RebindAt, "2026-09-05T10:30:00Z"},
		{"expires_at", got.ExpiresAt, "2026-09-05T11:00:00Z"},
		{"conflict_check", got.ConflictCheck, "wait"},
		{"acd_phase", got.ACDPhase, "defending"},
		{"last_event", got.LastEvent, "bound"},
	} {
		if c.got != c.want {
			t.Errorf("bound endpoint %s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if got.LastEventAt == "" {
		t.Error("bound endpoint has a last_event and no last_event_at")
	}

	got, ok = byEndpoint["cccccccccccc"]
	if !ok {
		t.Fatalf("no entry for the acquiring endpoint; got %+v", views)
	}
	for _, c := range []struct{ name, got, want string }{
		{"network", got.Network, "333333333333"},
		{"mode", got.Mode, ModeIPvlan},
		{"lease_state", got.LeaseState, "acquiring"},
		{"conflict_check", got.ConflictCheck, "async"},
		{"acd_phase", got.ACDPhase, "probing"},
	} {
		if c.got != c.want {
			t.Errorf("acquiring endpoint %s = %q, want %q", c.name, c.got, c.want)
		}
	}
	for _, c := range []struct{ name, got string }{
		{"address", got.Address},
		{"server", got.Server},
		{"renew_at", got.RenewAt},
		{"rebind_at", got.RebindAt},
		{"expires_at", got.ExpiresAt},
		{"last_event", got.LastEvent},
		{"last_event_at", got.LastEventAt},
	} {
		if c.got != "" {
			t.Errorf("acquiring endpoint %s = %q; it holds no lease and saw no event, "+
				"so this is the other endpoint's value or a stale one", c.name, c.got)
		}
	}
}

func TestEndpointViews_NoClientYetIsUnknownNotIdle(t *testing.T) {
	p := newHealthPlugin()
	p.persistentDHCP["e"] = newDHCPManager(nil, JoinRequest{EndpointID: "e1", NetworkID: "n1"}, DHCPNetworkOptions{})

	views := p.endpointViews()
	if len(views) != 1 {
		t.Fatalf("want one entry, got %d", len(views))
	}
	v := views[0]
	if v.ConflictCheck != "unknown" || v.ACDPhase != "unknown" {
		t.Errorf("conflict_check=%q acd_phase=%q for a manager with no client; want unknown/unknown",
			v.ConflictCheck, v.ACDPhase)
	}
	if v.LeaseState != "acquiring" {
		t.Errorf("lease_state=%q; want acquiring", v.LeaseState)
	}
	if v.Mode != ModeBridge {
		t.Errorf("mode=%q; want the default %q rather than an empty string", v.Mode, ModeBridge)
	}
}

func TestHealthDocument_EndpointsMatchActiveEndpoints(t *testing.T) {
	p := newHealthPlugin()

	h := p.healthSnapshot()
	if h.ActiveEndpoints != 0 || len(h.Endpoints) != 0 {
		t.Fatalf("empty plugin: active_endpoints=%d endpoints=%d", h.ActiveEndpoints, len(h.Endpoints))
	}
	if h.Endpoints == nil {
		t.Error("endpoints is null on a host with no containers; an empty array is the honest rendering")
	}

	for i, id := range []string{"e1", "e2", "e3"} {
		p.persistentDHCP[id] = newDHCPManager(nil, JoinRequest{EndpointID: id}, DHCPNetworkOptions{})
		h = p.healthSnapshot()
		if h.ActiveEndpoints != i+1 || len(h.Endpoints) != i+1 {
			t.Errorf("after %d joins: active_endpoints=%d, endpoints=%d", i+1, h.ActiveEndpoints, len(h.Endpoints))
		}
	}
}

func TestEndpointViews_AreOrderedByEndpoint(t *testing.T) {
	p := newHealthPlugin()
	for _, id := range []string{"ee", "aa", "mm", "bb"} {
		p.persistentDHCP[id] = newDHCPManager(nil, JoinRequest{EndpointID: id}, DHCPNetworkOptions{})
	}
	first := p.endpointViews()
	for i := 1; i < len(first); i++ {
		if first[i-1].Endpoint > first[i].Endpoint {
			t.Fatalf("entry %d (%s) sorts after %d (%s)", i-1, first[i-1].Endpoint, i, first[i].Endpoint)
		}
	}
	for n := 0; n < 8; n++ {
		again := p.endpointViews()
		for i := range first {
			if again[i].Endpoint != first[i].Endpoint {
				t.Fatalf("poll %d ordered %v, first poll ordered %v", n, again, first)
			}
		}
	}
}

func TestHealthDocument_CountAndArrayAgreeUnderChurn(t *testing.T) {
	p := newHealthPlugin()

	const reads = 2000

	var (
		stop      atomic.Bool
		mutations atomic.Int64
		wg        sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			id := fmt.Sprintf("e%d", i%64)
			m := newDHCPManager(nil, JoinRequest{EndpointID: id, NetworkID: "n"}, DHCPNetworkOptions{})
			p.mu.Lock()
			p.persistentDHCP[id] = m
			p.mu.Unlock()
			mutations.Add(1)

			p.mu.Lock()
			delete(p.persistentDHCP, id)
			p.mu.Unlock()
			mutations.Add(1)
		}
	}()

	before := mutations.Load()
	for i := 0; i < reads; i++ {
		h := p.healthSnapshot()
		if h.ActiveEndpoints != len(h.Endpoints) {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("document %d: active_endpoints=%d, endpoints has %d entries — "+
				"the count and the array were derived from two different reads of the manager map",
				i, h.ActiveEndpoints, len(h.Endpoints))
		}
	}
	during := mutations.Load() - before

	stop.Store(true)
	wg.Wait()

	if during == 0 {
		t.Fatalf("no Join/Leave committed while the %d documents were being taken; "+
			"this run measured a quiet plugin, not a concurrent one", reads)
	}
	t.Logf("%d documents read while %d map mutations committed", reads, during)
}
