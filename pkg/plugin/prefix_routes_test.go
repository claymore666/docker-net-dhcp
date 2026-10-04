// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/runtime"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

func TestDecodeOpts_IPv6PDTakesAPrefixLength(t *testing.T) {
	opts, err := decodeOpts(map[string]interface{}{"ipv6_pd": "64"})
	if err != nil {
		t.Fatalf("ipv6_pd=64 was refused: %v", err)
	}
	if opts.IPv6PD != 64 {
		t.Errorf("ipv6_pd=64 decoded to %d", opts.IPv6PD)
	}
	if _, err := decodeOpts(map[string]interface{}{"ipv6_pd": "sixty-four"}); err == nil {
		t.Error("ipv6_pd=sixty-four was accepted; a value that is not a number must be refused at create")
	}
	opts, err = decodeOpts(map[string]interface{}{"vendor_class": "acme"})
	if err != nil {
		t.Fatalf("decodeOpts: %v", err)
	}
	if opts.IPv6PD != 0 {
		t.Errorf("a network stored before the option existed asks for a /%d, want none", opts.IPv6PD)
	}
}

// off and slaac send no Solicit and ipvlan shares the parent's link, so the key could only do nothing there (#214).
func TestValidateIPv6Options_IPv6PDNeedsASolicitAndItsOwnLink(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    DHCPNetworkOptions
		wantErr error
	}{
		{"dhcp", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6PD: 64}, nil},
		{"auto", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "auto", IPv6PD: 56}, nil},
		{"the old ipv6 switch, which is dhcp", DHCPNetworkOptions{Bridge: "br0", IPv6: true, IPv6PD: 64}, nil},
		{"the longest length", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6PD: 128}, nil},
		{"the shortest length", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6PD: 1}, nil},
		{"macvlan", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "eth0", IPv6Mode: "dhcp", IPv6PD: 64}, nil},
		{"slaac", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac", IPv6PD: 64}, util.ErrIPAM},
		{"off", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "off", IPv6PD: 64}, util.ErrIPAM},
		{"no IPv6 at all", DHCPNetworkOptions{Bridge: "br0", IPv6PD: 64}, util.ErrIPAM},
		{"past 128", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6PD: 129}, util.ErrIPAM},
		{"negative", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6PD: -1}, util.ErrIPAM},
		{"ipvlan", DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "eth0", IPv6Mode: "dhcp", IPv6PD: 64}, util.ErrModeMismatch},
		{"slaac without the key", DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "slaac"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIPv6PD(tc.opts, mustMode6(t, tc.opts))
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validateIPv6PD = %v, want accepted", err)
				}
				if err := validateIPv6Options(tc.opts, nil); err != nil {
					t.Fatalf("validateIPv6Options = %v, want accepted", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("validateIPv6PD = %v, want %v", err, tc.wantErr)
			}
			for _, need := range []string{"ipv6_pd", "issue #214"} {
				if !strings.Contains(err.Error(), need) {
					t.Errorf("the refusal %q does not say %q", err, need)
				}
			}
			if err := validateIPv6Options(tc.opts, nil); err == nil {
				t.Error("validateIPv6Options accepted it: the check is not on the create path")
			}
		})
	}
}

func mustMode6(t *testing.T, o DHCPNetworkOptions) proto.Mode6 {
	t.Helper()
	m, err := o.ipv6Mode()
	if err != nil {
		t.Fatalf("ipv6Mode: %v", err)
	}
	return m
}

func TestV6Wiring_CarriesTheDelegatedPrefixLength(t *testing.T) {
	id6 := dhcp.Identity6{DUID: []byte{0, 4, 1, 2, 3, 4}, IAID: 0x11223344}
	for _, want := range []int{0, 56, 64} {
		var base dhcp.DHCPClientOptions
		opts := DHCPNetworkOptions{Bridge: "br0", IPv6Mode: "dhcp", IPv6PD: want}
		if err := (&Plugin{}).v6Wiring(&base, opts, id6, "rec-1", "", "0123456789abcdef", "net-1"); err != nil {
			t.Fatalf("v6Wiring: %v", err)
		}
		if base.IPv6PD != want {
			t.Errorf("ipv6_pd=%d reached the client as %d", want, base.IPv6PD)
		}
	}
}

func aggregate(t *testing.T, s string) netlink.Route {
	t.Helper()
	return *prefixAggregate(cidr(t, s))
}

func pdInfo(prefixes ...string) dhcp.Info {
	info := dhcp.Info{IP: "fd00:6470::1000/128"}
	for _, p := range prefixes {
		info.DelegatedPrefixes = append(info.DelegatedPrefixes, dhcp.V6Addr{IP: p, ValidSeconds: 3600, PreferredSeconds: 1800})
	}
	return info
}

// A RIO, connected or operator's route is left alone, the first two sharing a destination (RFC 3633 section 12.1, #214).
func TestReconcilePrefixRoutes_MakesTheAggregatesMatchTheLease(t *testing.T) {
	m, p, f := v6Manager(t)
	rio := netlink.Route{Dst: cidr(t, "fd00:98:0:2::/64"), Protocol: unix.RTPROT_RA, Type: unix.RTN_UNICAST, LinkIndex: 3}
	kernel := netlink.Route{Dst: cidr(t, "fd00:98:0:3::/64"), Protocol: unix.RTPROT_KERNEL, Type: unix.RTN_UNICAST, LinkIndex: 3}
	operator := netlink.Route{Dst: cidr(t, "fd00:98:0:4::/64"), Protocol: unix.RTPROT_STATIC, Type: unix.RTN_UNREACHABLE}
	f.routes = []netlink.Route{aggregate(t, "fd00:98:0:1::/64"), aggregate(t, "fd00:98:0:2::/64"), rio, kernel, operator}

	info := pdInfo("fd00:98:0:2::/64", "fd00:98:0:3::/64")
	if err := m.applyPrefixes(info.DelegatedPrefixes); err != nil {
		t.Fatalf("applyPrefixes: %v", err)
	}
	if got := destinations(f.deleted); len(got) != 1 || got[0] != "fd00:98:0:1::/64" {
		t.Fatalf("deleted %v, want only the aggregate of the prefix the lease dropped", got)
	}
	d := f.deleted[0]
	if d.Type != unix.RTN_UNREACHABLE || d.Protocol != unix.RTPROT_DHCP || d.LinkIndex != 0 || d.Gw != nil {
		t.Errorf("the delete key is %+v; a key without type and protocol, or with a link, takes a RIO or connected route", d)
	}
	if got := destinations(f.replace); len(got) != 1 || got[0] != "fd00:98:0:3::/64" {
		t.Fatalf("installed %v, want the new prefix only; the held one is already routed", got)
	}
	r := f.replace[0]
	if r.Type != unix.RTN_UNREACHABLE || r.Protocol != unix.RTPROT_DHCP || r.LinkIndex != 0 || r.Family != netlink.FAMILY_V6 {
		t.Errorf("installed %+v, want an unreachable proto dhcp route with no link (the kernel puts it on lo)", r)
	}
	if got := p.ipv6PrefixRoutesInstalled.Load(); got != 1 {
		t.Errorf("ipv6_prefix_routes_installed = %d, want 1", got)
	}
	if got := p.ipv6PrefixRoutesWithdrawn.Load(); got != 1 {
		t.Errorf("ipv6_prefix_routes_withdrawn = %d, want 1", got)
	}
}

func TestInstalledPrefixRoutes_ListsByTypeAndProtocol(t *testing.T) {
	m, _, _ := v6Manager(t)
	var gotFilter *netlink.Route
	var gotMask uint64
	nlHandleRouteListFiltered = func(_ *netlink.Handle, _ int, filter *netlink.Route, mask uint64) ([]netlink.Route, error) {
		gotFilter, gotMask = filter, mask
		return nil, nil
	}
	if _, err := m.installedPrefixRoutes(); err != nil {
		t.Fatalf("installedPrefixRoutes: %v", err)
	}
	if gotFilter == nil || gotFilter.Type != unix.RTN_UNREACHABLE || gotFilter.Protocol != unix.RTPROT_DHCP {
		t.Fatalf("filter = %+v, want type unreachable and protocol dhcp", gotFilter)
	}
	if want := uint64(netlink.RT_FILTER_TYPE | netlink.RT_FILTER_PROTOCOL); gotMask != want {
		t.Errorf("mask = %#x, want %#x: a field the mask does not name is not filtered on", gotMask, want)
	}
}

func TestReconcilePrefixRoutes_AFailedDeleteIsReportedAndTheRestStillApplied(t *testing.T) {
	m, _, f := v6Manager(t)
	f.routes = []netlink.Route{aggregate(t, "fd00:98:0:1::/64")}
	f.delErr = errors.New("netlink: operation not permitted")
	err := m.reconcilePrefixRoutes(pdInfo("fd00:98:0:2::/64").DelegatedPrefixes)
	if err == nil || !strings.Contains(err.Error(), "fd00:98:0:1::/64") {
		t.Errorf("reconcilePrefixRoutes = %v, want the failed delete named", err)
	}
	if got := destinations(f.replace); len(got) != 1 {
		t.Errorf("installed %v; one failed delete must not stop the new prefix being routed", got)
	}
	f.listErr = errors.New("netlink: dump interrupted")
	if err := m.reconcilePrefixRoutes(nil); err == nil {
		t.Error("a failed list was swallowed: nothing would be withdrawn and nothing said")
	}
}

func TestRenew_RoutesTheDelegatedPrefixesEvenOnSkipRoutes(t *testing.T) {
	for _, skip := range []bool{false, true} {
		m, _, f := v6Manager(t)
		m.opts.SkipRoutes = skip
		prevMTU, prevAddr := nlHandleLinkSetMTU, nlHandleAddrReplace
		nlHandleLinkSetMTU = func(*netlink.Handle, netlink.Link, int) error { return nil }
		nlHandleAddrReplace = func(*netlink.Handle, netlink.Link, *netlink.Addr) error { return nil }
		t.Cleanup(func() { nlHandleLinkSetMTU, nlHandleAddrReplace = prevMTU, prevAddr })

		if err := m.renew(true, pdInfo("fd00:98:0:4::/64")); err != nil {
			t.Fatalf("renew: %v", err)
		}
		if !hasAggregate(f.replace, "fd00:98:0:4::/64") {
			t.Errorf("skip_routes=%v: installed %v, want the delegated prefix's aggregate", skip, destinations(f.replace))
		}
		if got, _ := m.prefixHealth(time.Now()); len(got) != 1 || got[0].Prefix != "fd00:98:0:4::/64" {
			t.Errorf("skip_routes=%v: health shows %+v, want the renewed prefix", skip, got)
		}
	}
}

func hasAggregate(routes []netlink.Route, dst string) bool {
	for _, r := range routes {
		if r.Dst != nil && r.Dst.String() == dst && r.Type == unix.RTN_UNREACHABLE && r.Protocol == unix.RTPROT_DHCP {
			return true
		}
	}
	return false
}

func TestHandleEvent_AV6LossWithdrawsTheAggregates(t *testing.T) {
	for _, ev := range []string{"leasefail", "nak"} {
		t.Run(ev, func(t *testing.T) {
			m, _, f := v6Manager(t)
			m.notePrefixes(pdInfo("fd00:98:0:5::/64").DelegatedPrefixes, time.Now())
			f.routes = []netlink.Route{aggregate(t, "fd00:98:0:5::/64")}
			m.handleEvent(dhcp.Event{Type: ev}, true)
			if got := destinations(f.deleted); len(got) != 1 || got[0] != "fd00:98:0:5::/64" {
				t.Errorf("deleted %v after a v6 %s, want the aggregate", got, ev)
			}
			if got, _ := m.prefixHealth(time.Now()); len(got) != 0 {
				t.Errorf("health still shows %+v after the lease was lost", got)
			}
		})
	}
	t.Run("a v4 loss leaves the v6 prefix alone", func(t *testing.T) {
		m, _, f := v6Manager(t)
		f.routes = []netlink.Route{aggregate(t, "fd00:98:0:5::/64")}
		m.handleEvent(dhcp.Event{Type: "leasefail"}, false)
		if len(f.deleted) != 0 {
			t.Errorf("a v4 leasefail deleted %v", destinations(f.deleted))
		}
	})
}

func TestPrefixHealth_HidesAPrefixPastItsValidLifetime(t *testing.T) {
	m := newDHCPManager(nil, JoinRequest{}, DHCPNetworkOptions{})
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	m.notePrefixes([]dhcp.V6Addr{
		{IP: "fd00:98:0:6::/64", ValidSeconds: 900, PreferredSeconds: 600},
		{IP: "fd00:98:0:7::/64", ValidSeconds: 3600, PreferredSeconds: 1800},
	}, now)
	got, _ := m.prefixHealth(now.Add(900 * time.Second))
	if len(got) != 1 || got[0].Prefix != "fd00:98:0:7::/64" {
		t.Fatalf("health at the first prefix's end = %+v, want only the second", got)
	}
	if got[0].ExpiresAt != "2026-10-04T13:00:00Z" || got[0].PreferredUntil != "2026-10-04T12:30:00Z" {
		t.Errorf("times = %+v, want the lease's preferred and valid ends", got[0])
	}
}

// An overlap is a server's pool mistake: flagged, counted and logged, and both endpoints keep the prefix (#214).
func TestNotePrefixes_AnOverlapIsFlaggedNotRefused(t *testing.T) {
	p := &Plugin{persistentDHCP: map[string]*dhcpManager{}}
	a := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "ep-a"}, DHCPNetworkOptions{}).withPlugin(p)
	b := newDHCPManager(nil, JoinRequest{NetworkID: "net-1", EndpointID: "ep-b"}, DHCPNetworkOptions{}).withPlugin(p)
	c := newDHCPManager(nil, JoinRequest{NetworkID: "net-2", EndpointID: "ep-c"}, DHCPNetworkOptions{}).withPlugin(p)
	p.persistentDHCP["ep-a"], p.persistentDHCP["ep-b"], p.persistentDHCP["ep-c"] = a, b, c
	now := time.Now()

	a.notePrefixes(pdInfo("fd00:98::/56").DelegatedPrefixes, now)
	c.notePrefixes(pdInfo("fd00:98:0:1::/64").DelegatedPrefixes, now)
	if _, flagged := c.prefixHealth(now); flagged {
		t.Error("an overlap with another network's endpoint was flagged; prefixes are judged per network")
	}
	b.notePrefixes(pdInfo("fd00:98:0:1::/64").DelegatedPrefixes, now)
	got, flagged := b.prefixHealth(now)
	if !flagged {
		t.Error("an overlap with another endpoint of the network was not flagged")
	}
	if len(got) != 1 {
		t.Errorf("health shows %+v; an overlapping prefix is kept, never dropped", got)
	}
	if n := p.ipv6PrefixOverlaps.Load(); n != 1 {
		t.Errorf("ipv6_prefix_overlaps = %d, want 1", n)
	}
	b.notePrefixes(pdInfo("fd00:98:0:1::/64").DelegatedPrefixes, now)
	if n := p.ipv6PrefixOverlaps.Load(); n != 1 {
		t.Errorf("ipv6_prefix_overlaps = %d after a renewal of the same overlap, want 1: it counts endpoints, not events", n)
	}
}

// RFC 8415 section 18.2.7: the prefix is out of use before the Release leaves, and a failed withdrawal sends nothing.
func TestReleaseLease_ThePrefixRouteGoesBeforeTheRelease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		delErr    error
		wantCalls int
		want      releaseOutcome
	}{
		{"the route came out", nil, 1, releaseSent},
		{"the route could not be taken out", errors.New("netlink: operation not permitted"), 0, releaseWithdrawFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := nlAddrDel
			nlAddrDel = func(*netlink.Handle, netlink.Link, *netlink.Addr) error { return nil }
			t.Cleanup(func() { nlAddrDel = prev })

			p := &Plugin{}
			sender := installSender(t, nil)
			m := releasingManager(t, p, ReleaseOnStop, true)
			f := &fakeRouteTable{routes: []netlink.Route{aggregate(t, "fd00:98:0:8::/64")}, delErr: tc.delErr}
			f.install(t, m)
			var order []string
			nlHandleRouteDel = func(_ *netlink.Handle, r *netlink.Route) error {
				order = append(order, "route")
				return tc.delErr
			}
			sendPrev := rtSendRelease
			rtSendRelease = func(rec lease.Record, cfg runtime.ReleaseConfig) error {
				order = append(order, "release")
				return sendPrev(rec, cfg)
			}

			if got := m.releaseHeldLease(true); got != tc.want {
				t.Errorf("releaseHeldLease(v6) = %q, want %q", got, tc.want)
			}
			if got := sender.callCount(); got != tc.wantCalls {
				t.Fatalf("the wire saw %d release(s), want %d", got, tc.wantCalls)
			}
			if tc.wantCalls == 1 && strings.Join(order, ",") != "route,release" {
				t.Errorf("order %v, want the route out before the release", order)
			}
		})
	}
}

// Nothing was released on a Leave, so the prefixes stay on the fingerprint for the tombstone (#214).
func TestStopLeaving_WithdrawsTheAggregatesAndKeepsThePrefixesForTheTombstone(t *testing.T) {
	p := &Plugin{endpointFingerprints: map[string]endpointFingerprint{"ep1": {MAC: "02:42:ac:11:00:02"}}}
	m := stoppingManager(t, p, DHCPNetworkOptions{IPv6: true, Bridge: "br0"}, nil, nil)
	f := &fakeRouteTable{routes: []netlink.Route{aggregate(t, "fd00:98:0:9::/64")}}
	f.install(t, m)
	m.notePrefixes(pdInfo("fd00:98:0:9::/64").DelegatedPrefixes, time.Now())

	if err := m.StopForLeave(); err != nil {
		t.Fatalf("StopForLeave: %v", err)
	}
	if got := destinations(f.deleted); len(got) != 1 || got[0] != "fd00:98:0:9::/64" {
		t.Errorf("deleted %v at Leave, want the aggregate", got)
	}
	if got := p.endpointFingerprints["ep1"].Prefixes; len(got) != 1 || got[0] != "fd00:98:0:9::/64" {
		t.Errorf("fingerprint prefixes after Leave = %v, want the held prefix for the tombstone", got)
	}
}

func TestStop_AClosingPluginLeavesTheAggregates(t *testing.T) {
	p := &Plugin{}
	m := stoppingManager(t, p, DHCPNetworkOptions{IPv6: true, Bridge: "br0"}, nil, nil)
	f := &fakeRouteTable{routes: []netlink.Route{aggregate(t, "fd00:98:0:9::/64")}}
	f.install(t, m)
	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Errorf("a Stop that is not a Leave deleted %v; the container keeps running and keeps its prefix", destinations(f.deleted))
	}
}

func TestTombstone_CarriesTheDelegatedPrefixes(t *testing.T) {
	withStateDir(t, t.TempDir())
	p := newPluginForTest()
	p.addTombstone("net-A", "alpha", "aa:aa:aa:aa:aa:aa", "10.0.0.1", "fd00::1", "fd00:98:0:a::/64")
	ts, err := loadTombstones()
	if err != nil {
		t.Fatalf("loadTombstones: %v", err)
	}
	if len(ts) != 1 || len(ts[0].DelegatedPrefixes) != 1 || ts[0].DelegatedPrefixes[0] != "fd00:98:0:a::/64" {
		t.Errorf("tombstones = %+v, want the delegated prefix carried", ts)
	}
}
