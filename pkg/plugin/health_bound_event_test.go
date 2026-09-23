// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const (
	leaseA = "192.0.2.17/24"
	leaseB = "192.0.2.42/24"
)

func testLease(t *testing.T, addr, renew string) lease.Lease {
	t.Helper()
	return lease.Lease{
		Addr:     netip.MustParsePrefix(addr),
		ServerID: netip.MustParseAddr("192.0.2.1"),
		Renew:    mustTime(t, renew),
		Rebind:   mustTime(t, "2026-09-05T10:30:00Z"),
		Expire:   mustTime(t, "2026-09-05T11:00:00Z"),
	}
}

func v4Event(kind, ip string) dhcp.Event { return dhcp.Event{Type: kind, Data: dhcp.Info{IP: ip}} }

func newWindowManager(c *fakeJoinClient) *dhcpManager {
	m := newDHCPManager(nil, JoinRequest{EndpointID: "aaaaaaaaaaaabbbbbbbbbbbb", NetworkID: "n"},
		DHCPNetworkOptions{Mode: ModeMacvlan})
	m.setHealthClient(c)
	return m
}

func assertAcquiring(t *testing.T, e EndpointHealth, window string) {
	t.Helper()
	if e.LeaseState != "acquiring" {
		t.Errorf("%s: lease_state=%q last_event=%q address=%q; want acquiring", window, e.LeaseState, e.LastEvent,
			e.Address)
	}
	for _, f := range []struct{ name, got string }{
		{"address", e.Address}, {"server", e.Server}, {"renew_at", e.RenewAt},
		{"rebind_at", e.RebindAt}, {"expires_at", e.ExpiresAt},
	} {
		if f.got != "" {
			t.Errorf("%s: acquiring entry carries %s=%q", window, f.name, f.got)
		}
	}
	if (e.LastEvent == "") != (e.LastEventAt == "") {
		t.Errorf("%s: last_event=%q beside last_event_at=%q; the event and its time come together", window,
			e.LastEvent, e.LastEventAt)
	}
}

func assertBoundBy(t *testing.T, e EndpointHealth, event, addr, renew string) {
	t.Helper()
	for _, f := range []struct{ name, got, want string }{
		{"lease_state", e.LeaseState, "bound"},
		{"last_event", e.LastEvent, event},
		{"address", e.Address, addr},
		{"renew_at", e.RenewAt, renew},
		{"server", e.Server, "192.0.2.1"},
	} {
		if f.got != f.want {
			t.Errorf("%s=%q, want %q (entry %+v)", f.name, f.got, f.want, e)
		}
	}
	if e.LastEventAt == "" {
		t.Errorf("bound entry has no last_event_at: %+v", e)
	}
}

func TestHealthView_LeaseHeldBeforeItsBoundEventIsAcquiringNeverBoundWithoutTheEvent(t *testing.T) {
	c := &fakeJoinClient{mode: proto.ConflictWait, bound: true, l: testLease(t, leaseA, "2026-09-05T10:00:00Z")}
	m := newWindowManager(c)

	assertAcquiring(t, m.healthView(), "client holds the lease, no event recorded")

	m.handleEvent(dhcp.Event{Type: "bound", Data: dhcp.Info{IP: "2001:db8::17/128"}}, true)
	e := m.healthView()
	assertAcquiring(t, e, "v4 lease held, only a v6 event recorded")
	if e.LastEvent != "" {
		t.Errorf("last_event=%q from the v6 client; every field describes the v4 client", e.LastEvent)
	}

	m.handleEvent(v4Event("bound", leaseA), false)
	assertBoundBy(t, m.healthView(), "bound", leaseA, "2026-09-05T10:00:00Z")

	m.handleEvent(dhcp.Event{Type: "renew", Data: dhcp.Info{IP: "2001:db8::17/128"}}, true)
	assertBoundBy(t, m.healthView(), "bound", leaseA, "2026-09-05T10:00:00Z")
}

func TestHealthView_RenewalBeforeItsEventKeepsTheRecordedTimes(t *testing.T) {
	c := &fakeJoinClient{bound: true, l: testLease(t, leaseA, "2026-09-05T10:00:00Z")}
	m := newWindowManager(c)
	m.handleEvent(v4Event("bound", leaseA), false)

	c.l = testLease(t, leaseA, "2026-09-05T12:00:00Z")
	assertBoundBy(t, m.healthView(), "bound", leaseA, "2026-09-05T10:00:00Z")

	m.handleEvent(v4Event("renew", leaseA), false)
	assertBoundBy(t, m.healthView(), "renew", leaseA, "2026-09-05T12:00:00Z")
}

func TestHealthView_LeaseGoneBeforeItsEventIsAcquiringWithTheLastRecordedEvent(t *testing.T) {
	c := &fakeJoinClient{bound: true, l: testLease(t, leaseA, "2026-09-05T10:00:00Z")}
	m := newWindowManager(c)
	m.handleEvent(v4Event("bound", leaseA), false)

	c.bound, c.l = false, lease.Lease{}
	e := m.healthView()
	assertAcquiring(t, e, "lease gone, nak not yet recorded")
	if e.LastEvent != "bound" {
		t.Errorf("last_event=%q; the recorded event stays until the next one", e.LastEvent)
	}

	m.handleEvent(v4Event("nak", ""), false)
	e = m.healthView()
	assertAcquiring(t, e, "nak recorded")
	if e.LastEvent != "nak" {
		t.Errorf("last_event=%q, want nak", e.LastEvent)
	}
}

func TestHealthView_LeaseMovedBeforeItsEventIsAcquiringNotTheOtherAddress(t *testing.T) {
	c := &fakeJoinClient{bound: true, l: testLease(t, leaseA, "2026-09-05T10:00:00Z")}
	m := newWindowManager(c)
	m.handleEvent(v4Event("bound", leaseA), false)

	c.l = testLease(t, leaseB, "2026-09-05T10:05:00Z")
	assertAcquiring(t, m.healthView(), "client moved to another address, its event not yet recorded")

	m.handleEvent(v4Event("nak", ""), false)
	assertAcquiring(t, m.healthView(), "nak recorded while the client holds B")

	m.handleEvent(v4Event("bound", leaseA), false)
	assertAcquiring(t, m.healthView(), "bound for A recorded while the client holds B")

	m.handleEvent(v4Event("bound", leaseB), false)
	assertBoundBy(t, m.healthView(), "bound", leaseB, "2026-09-05T10:05:00Z")
}
