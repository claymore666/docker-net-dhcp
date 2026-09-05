// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"sort"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// endpointClient is what the health document asks a running DHCP client.
// Three read-only methods, all of which *dhcp.DHCPClient already has.
//
// The interface exists so this document can be driven against states a
// unit test cannot otherwise build: a bound lease with T1, T2, an
// expiry and a server lives inside the library client and there is no
// exported way to put one there. Narrowing the field to what is read
// makes those states reachable, and the alternative -- asserting only
// on the two states an empty client can be in -- is how "the array has
// two entries" passes for a test of the fields.
type endpointClient interface {
	ConflictMode() proto.ConflictMode
	ACDPhase() proto.ACDPhase
	Lease() (lease.Lease, bool)
}

// EndpointHealth is one entry of /Plugin.Health's `endpoints` array:
// what the plugin-wide counters cannot say, which is WHICH container is
// in what state.
//
// Every field is READ, none is accumulated. The address, the lease
// times and the server come from the lease the renewal client holds;
// the RFC 5227 phase and the conflict mode come from the same client;
// the ids, the mode and the last event come from the manager that owns
// it. Nothing here is a counter, so nothing here needs a reset rule.
//
// The array is bounded by active_endpoints, which is the same map
// length that field already reports, so a host with no containers has
// an empty array rather than an absent one.
type EndpointHealth struct {
	Endpoint string `json:"endpoint"`
	Network  string `json:"network"`
	Mode     string `json:"mode"`
	// Address is the lease the renewal client currently holds, in CIDR
	// form. Empty means it holds none: the client is still acquiring,
	// or its lease was lost.
	Address string `json:"address,omitempty"`
	// LeaseState is `bound` when the client holds a lease and
	// `acquiring` when it does not. Deliberately two values and not the
	// protocol's state machine: RENEWING and REBINDING are visible in
	// the times below, and a state name the plugin cannot read from the
	// client would be a guess.
	LeaseState string `json:"lease_state"`
	// RenewAt / RebindAt / ExpiresAt are T1, T2 and the lease's end as
	// ABSOLUTE times (RFC 3339), not as remaining seconds. A remaining
	// second is only meaningful together with the instant it was read,
	// and this document is polled, cached and pasted into issues.
	//
	// An empty ExpiresAt on a bound endpoint is the protocol's infinite
	// lease (0xFFFFFFFF), which the library represents as a zero time
	// so that "no expiry" is a value rather than a threshold to guess.
	RenewAt   string `json:"renew_at,omitempty"`
	RebindAt  string `json:"rebind_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	// Server is the DHCP server that granted the lease (option 54).
	Server string `json:"server,omitempty"`
	// LastEvent is the most recent lifecycle event the manager saw --
	// `bound`, `renew`, `nak`, and so on -- with the time it saw it.
	// Empty on an endpoint whose client has not reported anything yet.
	LastEvent   string `json:"last_event,omitempty"`
	LastEventAt string `json:"last_event_at,omitempty"`
	// ConflictCheck is the RFC 5227 mode this client runs in (D23) and
	// ACDPhase is where that check has got to. Read the phase against
	// the mode and never alone: in `off` the phase is `idle` because
	// nothing runs, which is not the same fact as `idle` in `wait`.
	ConflictCheck string `json:"conflict_check"`
	ACDPhase      string `json:"acd_phase"`
}

// healthView describes this manager for the health document.
//
// It takes no lock of its own beyond the two the manager already has
// for these fields, and it holds neither across the client call: the
// walk below holds p.mu only long enough to copy the map's values, so
// a slow client cannot stall a Join.
func (m *dhcpManager) healthView() EndpointHealth {
	e := EndpointHealth{
		Endpoint: shortID(m.joinReq.EndpointID),
		Network:  shortID(m.joinReq.NetworkID),
		Mode:     m.opts.effectiveMode(),
	}

	kind, at := m.lastEventSeen()
	e.LastEvent = kind
	if !at.IsZero() {
		e.LastEventAt = at.Format(time.RFC3339Nano)
	}

	c := m.healthClient()
	if c == nil {
		e.LeaseState = "acquiring"
		e.ConflictCheck = "unknown"
		e.ACDPhase = "unknown"
		return e
	}

	e.ConflictCheck = c.ConflictMode().String()
	e.ACDPhase = c.ACDPhase().String()

	l, ok := c.Lease()
	if !ok {
		e.LeaseState = "acquiring"
		return e
	}
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

// endpointViewsOf is one entry per manager in the slice it is given,
// sorted by endpoint id so two consecutive polls of an unchanged host
// produce the same document rather than Go's map order.
//
// IT TAKES THE MANAGERS RATHER THAN READING THE MAP, and that is the
// whole design. `active_endpoints` and this array are one fact — how
// many endpoints this host manages — and until 2.0-alpha.1 each derived
// it from its own read of p.persistentDHCP under its own acquisition of
// p.mu. A Join or Leave committing between the two made the array
// longer or shorter than the count while the reference shipped the
// equality, a comment said the two could not disagree, and an
// integration cell asserted it. Measured under churn: they disagreed.
//
// So healthSnapshot takes p.mu ONCE, copies the map's values, and both
// fields come out of that one slice — the count as len() of what this
// function returns. There is no second read to be stale, and no
// assignment of one field from the other after the fact: the equality is
// what the code does, not something a test has to keep noticing.
//
// The lock is released before any client is asked anything: a slow
// client must not stall a Join.
func endpointViewsOf(managers []*dhcpManager) []EndpointHealth {
	out := make([]EndpointHealth, 0, len(managers))
	for _, m := range managers {
		out = append(out, m.healthView())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })
	return out
}

// managerSnapshot copies the registered managers under one acquisition
// of p.mu, with the join-hint count taken in the same window.
func (p *Plugin) managerSnapshot() ([]*dhcpManager, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	managers := make([]*dhcpManager, 0, len(p.persistentDHCP))
	for _, m := range p.persistentDHCP {
		managers = append(managers, m)
	}
	return managers, len(p.joinHints)
}

// endpointViews is the whole walk for a caller that has no manager slice
// of its own.
func (p *Plugin) endpointViews() []EndpointHealth {
	managers, _ := p.managerSnapshot()
	return endpointViewsOf(managers)
}
