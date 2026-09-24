// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"sort"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

type endpointClient interface {
	ConflictMode() proto.ConflictMode
	ACDPhase() proto.ACDPhase
	Lease() (lease.Lease, bool)
}

// EndpointHealth is one entry of /Plugin.Health's `endpoints` array, the per-container state the counters cannot show.
type EndpointHealth struct {
	Endpoint string `json:"endpoint"`
	Network  string `json:"network"`
	Mode     string `json:"mode"`
	// Address is the lease the renewal client currently holds, in CIDR form.
	Address string `json:"address,omitempty"`
	// LeaseState is `bound` when the client holds the lease its last recorded event bound, `acquiring` otherwise.
	LeaseState string `json:"lease_state"`
	// RenewAt, RebindAt and ExpiresAt are T1, T2 and the lease end as RFC 3339 times; an empty
	// ExpiresAt on a bound endpoint is an infinite lease (RFC 2131 section 3.3).
	RenewAt   string `json:"renew_at,omitempty"`
	RebindAt  string `json:"rebind_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	// Server is the DHCP server that granted the lease (option 54).
	Server string `json:"server,omitempty"`
	// LastEvent is the v4 client's most recent lifecycle event, such as `bound`, `renew` or `nak`, with its time.
	LastEvent   string `json:"last_event,omitempty"`
	LastEventAt string `json:"last_event_at,omitempty"`
	// ConflictCheck is the RFC 5227 mode this client runs in and ACDPhase is where that check has got to.
	ConflictCheck string `json:"conflict_check"`
	ACDPhase      string `json:"acd_phase"`
}

func (m *dhcpManager) healthView() EndpointHealth {
	e := EndpointHealth{
		Endpoint: shortID(m.joinReq.EndpointID),
		Network:  shortID(m.joinReq.NetworkID),
		Mode:     m.opts.effectiveMode(),
	}

	rec, c := m.healthSnapshot()
	e.LastEvent = rec.event
	if !rec.at.IsZero() {
		e.LastEventAt = rec.at.Format(time.RFC3339Nano)
	}

	if c == nil {
		e.LeaseState = "acquiring"
		e.ConflictCheck = "unknown"
		e.ACDPhase = "unknown"
		return e
	}

	e.ConflictCheck = c.ConflictMode().String()
	e.ACDPhase = c.ACDPhase().String()

	// A conflict drops the lease with no event (dhcp.translateOne), so the record alone would stay bound; the live
	// read only ever demotes it, and every rendered field still comes from the record (#1044).
	live, ok := c.Lease()
	if !ok || live.Addr != rec.lease.Addr {
		e.LeaseState = "acquiring"
		return e
	}
	l := rec.lease
	e.LeaseState = "bound"
	e.Address = l.Addr.String()
	if l.ServerID.IsValid() {
		e.Server = l.ServerID.String()
	}
	if !l.Renew.IsZero() {
		e.RenewAt = l.Renew.Format(time.RFC3339Nano)
	}
	if !l.Rebind.IsZero() {
		e.RebindAt = l.Rebind.Format(time.RFC3339Nano)
	}
	if !l.Expire.IsZero() {
		e.ExpiresAt = l.Expire.Format(time.RFC3339Nano)
	}
	return e
}

// endpointViewsOf builds the array from the same snapshot active_endpoints counts, since two reads
// of the map disagreed under churn; it is sorted by endpoint id (#910).
func endpointViewsOf(managers []*dhcpManager) []EndpointHealth {
	out := make([]EndpointHealth, 0, len(managers))
	for _, m := range managers {
		out = append(out, m.healthView())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })
	return out
}

func (p *Plugin) managerSnapshot() ([]*dhcpManager, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	managers := make([]*dhcpManager, 0, len(p.persistentDHCP))
	for _, m := range p.persistentDHCP {
		managers = append(managers, m)
	}
	return managers, len(p.joinHints)
}

func (p *Plugin) endpointViews() []EndpointHealth {
	managers, _ := p.managerSnapshot()
	return endpointViewsOf(managers)
}
