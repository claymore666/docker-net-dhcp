// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// fakeEndpointClient is a DHCP client in a state this package cannot
// build a real one into. Its three methods are the whole of what the
// health document asks a client.
type fakeEndpointClient struct {
	mode  proto.ConflictMode
	phase proto.ACDPhase
	l     lease.Lease
	bound bool
}

func (f *fakeEndpointClient) ConflictMode() proto.ConflictMode { return f.mode }
func (f *fakeEndpointClient) ACDPhase() proto.ACDPhase         { return f.phase }
func (f *fakeEndpointClient) Lease() (lease.Lease, bool)       { return f.l, f.bound }

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("bad fixture time %q: %v", s, err)
	}
	return v
}

// Two endpoints in DIFFERENT states, asserted field by field.
//
// The states are chosen so that no single value is shared between them:
// different ids, networks, modes, addresses, servers, lease states,
// conflict modes and ACD phases. An entry rendered from the wrong
// manager, a field copied from the previous iteration, or a struct
// reused across the loop shows up as a value from the other row rather
// than as a missing one — and "the array has two entries" would hold
// under every one of those.
func TestEndpointViews_TwoEndpointsRenderTheirOwnFields(t *testing.T) {
	p := newHealthPlugin()

	bound := &fakeEndpointClient{
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
	acquiring := &fakeEndpointClient{mode: proto.ConflictAsync, phase: proto.ACDProbing}

	// Endpoint ids are longer than shortID's 12 so the trim is driven
	// too: a document that leaked the full id would differ here.
	mBound := newDHCPManager(nil, JoinRequest{
		EndpointID: "aaaaaaaaaaaabbbbbbbbbbbb",
		NetworkID:  "111111111111222222222222",
	}, DHCPNetworkOptions{Mode: ModeMacvlan})
	mBound.setHealthClient(bound)
	mBound.noteEvent("bound")

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

// A manager with no client at all -- the window between Join registering
// the manager and setupClient publishing the client. The phase and the
// mode are `unknown` there and not `idle`/`wait`: idle in wait mode is a
// statement about a running check, and this is the absence of one.
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

// The array is bounded by active_endpoints: the same map, so the two
// cannot disagree about how many endpoints this host has.
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

// Two consecutive polls of an unchanged host produce the same document.
// Map order would otherwise reorder the array on every read, which makes
// a diff of two polls unreadable and a golden of it impossible.
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
