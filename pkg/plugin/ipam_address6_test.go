// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/runtime"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	a6Leased   = "fd00:6470:6865::20/128"
	a6Other    = "fd00:6470:6865::21/128"
	a6Outside  = "fd00:9999::20/128"
	a6LinkLoc  = "fe80::42"
	a6Leased4  = "192.168.99.61/24"
	a6Endpoint = "ep-a6-0000000000"
)

// a6Server is the DHCPv6 server the seam stands for; it writes the lease onto the record as the library does (#1132).
type a6Server struct {
	answer string
	err    error
	calls  int
	iface  string
	opts   dhcp.DHCPClientOptions
}

type a6Sent struct {
	recs []lease.Record
	cfgs []runtime.ReleaseConfig
}

func a6Fixture(t *testing.T) (*Plugin, *ipamBinding, *a6Server, *a6Sent) {
	t.Helper()
	p, b := v6BoundFixture(t)
	if err := saveNetwork(ipamTestNetwork, a6Options(), b); err != nil {
		t.Fatal(err)
	}
	return p, b, a6Seams(t, p), a6Releases(t)
}

func a6Options() DHCPNetworkOptions {
	return DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br-test", IPv6Mode: "dhcp"}
}

func a6Seams(t *testing.T, p *Plugin) *a6Server {
	t.Helper()
	s := &a6Server{answer: a6Leased}
	prevLink, prevLink6, prevGet := ipamAddReserveLink, ipamAddReserveLink6, dhcpGetIP
	prevByName, prevList := nlLinkByName, nlAddrList
	noLink := func(_ *Plugin, _ context.Context, _, _, _ string, _ DHCPNetworkOptions, _ net.HardwareAddr) (func(), error) {
		return func() {}, nil
	}
	ipamAddReserveLink, ipamAddReserveLink6 = noLink, noLink
	nlLinkByName = func(n string) (netlink.Link, error) {
		if n != a6Options().Bridge {
			return nil, netlink.LinkNotFoundError{}
		}
		return &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: n, Index: 7}}, nil
	}
	nlAddrList = func(_ netlink.Link, family int) ([]netlink.Addr, error) {
		if family != unix.AF_INET6 {
			return nil, nil
		}
		return []netlink.Addr{{IPNet: &net.IPNet{IP: net.ParseIP(a6LinkLoc), Mask: net.CIDRMask(64, 128)}}}, nil
	}
	dhcpGetIP = func(_ context.Context, iface string, o *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		if !o.V6 {
			if o.RecordID != "" {
				if err := p.records.Observed(o.RecordID, acquired(a6Leased4, time.Hour), nil); err != nil {
					t.Errorf("Observed: %v", err)
				}
			}
			return dhcp.Info{IP: a6Leased4, Gateway: "192.168.99.1"}, dhcp.RAObservation{}, nil
		}
		s.calls, s.iface, s.opts = s.calls+1, iface, *o
		if s.err != nil {
			return dhcp.Info{}, dhcp.RAObservation{}, s.err
		}
		if o.RecordID != "" {
			if err := p.records.Observed(o.RecordID, acquired6(s.answer, time.Hour), nil); err != nil {
				t.Errorf("Observed: %v", err)
			}
		}
		return dhcp.Info{IP: s.answer}, dhcp.RAObservation{Seen: true, Managed: true}, nil
	}
	t.Cleanup(func() {
		ipamAddReserveLink, ipamAddReserveLink6, dhcpGetIP = prevLink, prevLink6, prevGet
		nlLinkByName, nlAddrList = prevByName, prevList
	})
	return s
}

func a6Releases(t *testing.T) *a6Sent {
	t.Helper()
	sent := &a6Sent{}
	prev := rtSendRelease
	rtSendRelease = func(rec lease.Record, cfg runtime.ReleaseConfig) error {
		sent.recs, sent.cfgs = append(sent.recs, rec), append(sent.cfgs, cfg)
		return nil
	}
	t.Cleanup(func() { rtSendRelease = prev })
	return sent
}

func a6Request(p *Plugin, poolID, addr string, mac net.HardwareAddr) (RequestAddressResponse, error) {
	opts := map[string]string{}
	if mac != nil {
		opts[ipamOptMacAddress] = mac.String()
	}
	return p.RequestAddress(context.Background(), RequestAddressRequest{PoolID: poolID, Address: addr, Options: opts})
}

func a6Records(t *testing.T, p *Plugin) []lease.Record {
	t.Helper()
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	if len(rb.Rejects) != 0 {
		t.Fatalf("the journal folds with rejects %+v", rb.Rejects)
	}
	var out []lease.Record
	for _, rec := range rb.Records {
		if rec.Scope == dhcp.Scope6(ipamTestNetwork) {
			out = append(out, rec)
		}
	}
	return out
}

func a6Live(t *testing.T, p *Plugin) []lease.Record {
	t.Helper()
	var out []lease.Record
	for _, rec := range a6Records(t, p) {
		if rec.Phase == lease.PhaseReserved || rec.Phase == lease.PhaseCreated {
			out = append(out, rec)
		}
	}
	return out
}

func a6Addr(s string) netip.Addr { return netip.MustParsePrefix(s).Addr() }

func TestIpamAcceptedReservation6(t *testing.T) {
	for _, tc := range []struct {
		name, got, demanded, want string
	}{
		{"inside, no --ip6", a6Leased, "", ""},
		{"the address --ip6 asked for", a6Leased, a6Leased, ""},
		{"another address than --ip6 (D3)", a6Other, a6Leased, "--ip6 asked for fd00:6470:6865::20 and the DHCPv6 server answered fd00:6470:6865::21 instead"},
		{"outside the prefix (D4)", a6Outside, "", "answered fd00:9999::20, which is outside this network's IPv6 subnet fd00:6470:6865::/64"},
		{"outside and not the hint: the prefix is named first", a6Outside, a6Leased, "outside this network's IPv6 subnet"},
	} {
		var demanded netip.Addr
		if tc.demanded != "" {
			demanded = a6Addr(tc.demanded)
		}
		err := ipamAccept6(a6Addr(tc.got), ipamTestPool6, demanded)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
	if err := ipamAccept6(a6Addr(a6Leased), "not-a-prefix", netip.Addr{}); err == nil {
		t.Error("an unreadable pool accepted the reply")
	}
}

// The re-pointed row 22: the v6 PoolID runs the DHCPv6 arm, never the v4 lease path (#1132).
func TestRequestAddress_V6PoolIsAnsweredByDHCPv6NeverByAV4Lease(t *testing.T) {
	p, b, srv, _ := a6Fixture(t)
	res, err := a6Request(p, b.PoolID6, "", net.HardwareAddr(f0MAC(0x01)))
	if err != nil {
		t.Fatalf("RequestAddress: %v", err)
	}
	if !srv.opts.V6 || srv.calls != 1 || res.Address != a6Leased {
		t.Fatalf("answered %q after %d DHCPv6 calls (V6=%v), want %s from one DHCPv6 exchange", res.Address, srv.calls, srv.opts.V6, a6Leased)
	}
}

func TestRequestAddress_V6RefusedReplyIsReleasedAndLeavesNoReservedRecord(t *testing.T) {
	for _, tc := range []struct {
		name, ip6, answer string
		want              []string
	}{
		{"hint", "fd00:6470:6865::20", a6Other, []string{"fd00:6470:6865::20", "fd00:6470:6865::21", "That lease was released"}},
		{"outside", "", a6Outside, []string{"fd00:9999::20", "fd00:6470:6865::/64", "That lease was released"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, srv, sent := a6Fixture(t)
			srv.answer = tc.answer
			_, err := a6Request(p, b.PoolID6, tc.ip6, f0MAC(0x01))
			if err == nil || !errors.Is(err, util.ErrIPAM) {
				t.Fatalf("got %v, want the refusal", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the refusal %q does not name %q", err, w)
				}
			}
			if len(sent.recs) != 1 || sent.recs[0].Lease.Addr.String() != tc.answer ||
				sent.cfgs[0].Interface != a6Options().Bridge || sent.cfgs[0].Source != netip.MustParseAddr(a6LinkLoc) {
				t.Fatalf("released %+v via %+v, want one Release of %s on %s from its link-local (RFC 9915 section 18.2.7)",
					sent.recs, sent.cfgs, tc.answer, a6Options().Bridge)
			}
			if live := a6Live(t, p); len(live) != 0 {
				t.Errorf("the refusal left %+v, want no RESERVED v6 record", live)
			}
			if n := p.ipamReserves.len(); n != 0 {
				t.Errorf("%d reservations held after a refusal", n)
			}
		})
	}
}

func TestRequestAddress_V6SilentServerLeavesNoReservedRecord(t *testing.T) {
	for _, fail := range []error{context.DeadlineExceeded, dhcp.ErrNoV6Address} {
		p, b, srv, sent := a6Fixture(t)
		srv.err = fail
		_, err := a6Request(p, b.PoolID6, "", f0MAC(0x01))
		if err == nil || !errors.Is(err, fail) || !strings.Contains(err.Error(), "failed to reserve an IPv6 address") {
			t.Errorf("%v: got %v, want the reserve failure wrapping it", fail, err)
		}
		if live := a6Live(t, p); len(live) != 0 {
			t.Errorf("%v: left %+v, want no RESERVED v6 record", fail, live)
		}
		if len(sent.recs) != 0 || p.ipamReserves.len() != 0 {
			t.Errorf("%v: %d releases and %d reservations after a lease-less exchange", fail, len(sent.recs), p.ipamReserves.len())
		}
	}
}

// Defeat row 8: the library's DAD verdict comes back as an acquisition error, and the refusal carries it (#1132).
func TestRequestAddress_V6DADConflictIsLoud(t *testing.T) {
	p, b, srv, _ := a6Fixture(t)
	srv.err = errors.New("dhcp: duplicate address detection found fd00:6470:6865::20 in use on the link; the DHCPv6 acquisition budget then ran out")
	_, err := a6Request(p, b.PoolID6, "", f0MAC(0x01))
	if err == nil || !strings.Contains(err.Error(), "duplicate address detection found fd00:6470:6865::20") {
		t.Fatalf("got %v, want the DAD verdict in the refusal", err)
	}
	recs := a6Records(t, p)
	if len(recs) != 1 || recs[0].Phase != lease.PhaseClosed {
		t.Fatalf("records %+v, want the one attempt closed", recs)
	}
}

func TestRequestAddress_V6TemporaryWithIP6Refused(t *testing.T) {
	p, b, srv, _ := a6Fixture(t)
	opts := a6Options()
	opts.IPv6Temporary = true
	if err := saveNetwork(ipamTestNetwork, opts, b); err != nil {
		t.Fatal(err)
	}
	_, err := a6Request(p, b.PoolID6, "fd00:6470:6865::20", f0MAC(0x01))
	if err == nil || !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), "ipv6_temporary") || srv.calls != 0 {
		t.Fatalf("got %v after %d exchanges, want the D10 refusal before any", err, srv.calls)
	}
	if _, err := a6Request(p, b.PoolID6, "", f0MAC(0x01)); err != nil {
		t.Fatalf("without --ip6 the temporary-address network refused: %v", err)
	}
}

func TestRequestAddress_V6DuplicateMACRefused(t *testing.T) {
	t.Run("a live v6 record under the MAC", func(t *testing.T) {
		p, b, srv, _ := a6Fixture(t)
		s2Created6(t, p, f0MAC(0x01), a6Other, time.Hour)
		_, err := a6Request(p, b.PoolID6, "", f0MAC(0x01))
		if err == nil || !strings.Contains(err.Error(), "already leasing an address for the hardware address") || srv.calls != 0 {
			t.Fatalf("got %v after %d exchanges, want the duplicate-MAC refusal", err, srv.calls)
		}
	})
	t.Run("a v6 request under the MAC in flight", func(t *testing.T) {
		p, b, srv, _ := a6Fixture(t)
		p.ipamReserves.begin(ipamReserveKey(b.PoolID6, f0MAC(0x01)), time.Now())
		_, err := a6Request(p, b.PoolID6, "", f0MAC(0x01))
		if err == nil || !strings.Contains(err.Error(), "an IPv6 address request under it is still running") || srv.calls != 0 {
			t.Fatalf("got %v after %d exchanges, want the in-flight refusal", err, srv.calls)
		}
	})
	t.Run("--ip6 held by another MAC", func(t *testing.T) {
		p, b, srv, _ := a6Fixture(t)
		s2Created6(t, p, f0MAC(0x02), a6Leased, time.Hour)
		_, err := a6Request(p, b.PoolID6, "fd00:6470:6865::20", f0MAC(0x01))
		if err == nil || !strings.Contains(err.Error(), "is held by another endpoint") || srv.calls != 0 {
			t.Fatalf("got %v after %d exchanges, want the held-address refusal and no replay answer", err, srv.calls)
		}
	})
}

func TestRequestAddress_V6ReplayMissRefusedAndCounted(t *testing.T) {
	p, b, _, _ := a6Fixture(t)
	_, err := a6Request(p, b.PoolID6, "fd00:6470:6865::20", nil)
	if err == nil || !errors.Is(err, util.ErrIPAM) || p.ipamReplayMiss.Load() != 1 {
		t.Fatalf("got %v, miss count %d; want a refusal counted once", err, p.ipamReplayMiss.Load())
	}
	s2Created6(t, p, f0MAC(0x01), a6Leased, time.Hour)
	res, err := a6Request(p, b.PoolID6, "fd00:6470:6865::20", nil)
	if err != nil || res.Address != a6Leased || p.ipamReplayHits.Load() != 1 {
		t.Fatalf("hit: got %q, %v (hits %d), want the record's /128, as RequestAddress answered it", res.Address, err, p.ipamReplayHits.Load())
	}
	if _, err := a6Request(p, b.PoolID6, "", nil); err == nil {
		t.Fatal("a request with neither a MAC nor an address was answered")
	}
}

// Rows 9 and 27: the v6 call runs on the v4 reservation's re-bound tombstone, folded Created once (#1132).
func TestRequestAddress_V6UsesTheV4ReservationsReboundRecord(t *testing.T) {
	p, b, srv, _ := a6Fixture(t)
	first, restarted := f0MAC(0x01), f0MAC(0x02)
	id4 := f0Tombstone(t, p, first, f0Addr)
	id6 := s2Tombstone6(t, p, first, a6Leased)
	stored := s3Identity(t, f0Rec(t, p, id6))
	if _, err := a6Request(p, b.PoolID, "", restarted); err != nil {
		t.Fatalf("v4 RequestAddress: %v", err)
	}
	if rsv, _ := p.ipamReserves.peek(ipamReserveKey(b.PoolID, restarted)); rsv == nil || rsv.record != id4 || rsv.record6 != id6 {
		t.Fatalf("the v4 reservation is %+v, want it on both tombstones", rsv)
	}
	res, err := a6Request(p, b.PoolID6, "", restarted)
	switch {
	case err != nil:
		t.Fatalf("v6 RequestAddress: %v", err)
	case srv.opts.RecordID != id6 || !s3SameIdentity(srv.opts.Identity6, stored):
		t.Fatalf("ran on record %q with %+v, want %q with the stored identity (RFC 9915 section 11)", srv.opts.RecordID, srv.opts.Identity6, id6)
	case srv.opts.PreferredV6 != "fd00:6470:6865::20" || res.Address != a6Leased:
		t.Fatalf("asked for %q and answered %q, want the tombstone's address both times", srv.opts.PreferredV6, res.Address)
	}
	rsv6, ok := p.takeIPAMReservation6(b, restarted)
	if err := checkIPAMReservation6(rsv6, ok, restarted, a6Leased); err != nil || !rsv6.rebound {
		t.Fatalf("check: %v (rebound %v)", err, rsv6 != nil && rsv6.rebound)
	}
	if _, err := p.consumeIPAMReservation6(ipamTestNetwork, a6Endpoint, restarted, rsv6); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if rec := f0Rec(t, p, id6); rec.Phase != lease.PhaseCreated {
		t.Fatalf("the re-bound record is %v, want Created", rec.Phase)
	}
	a6Records(t, p)
}

// Rows 23 and 24 without a link: the own v4 reservation refuses nothing; the consume folds RESERVED (#1132).
func TestRequestAddress_V6ReservesAndConsumes(t *testing.T) {
	p, b, _, _ := a6Fixture(t)
	mac := f0MAC(0x01)
	if _, err := a6Request(p, b.PoolID, "", mac); err != nil {
		t.Fatalf("v4: %v", err)
	}
	res, err := a6Request(p, b.PoolID6, "fd00:6470:6865::20", mac)
	if err != nil || res.Address != a6Leased {
		t.Fatalf("v6: %q, %v; want %s", res.Address, err, a6Leased)
	}
	live := a6Live(t, p)
	if len(live) != 1 || live[0].Phase != lease.PhaseReserved || len(live[0].Identity) == 0 {
		t.Fatalf("v6 records %+v, want one RESERVED with an identity", live)
	}
	rsv6, ok := p.takeIPAMReservation6(b, mac)
	if err := checkIPAMReservation6(rsv6, ok, mac, a6Leased); err != nil {
		t.Fatalf("check: %v", err)
	}
	p.joinHints[a6Endpoint] = joinHint{}
	got, err := p.consumeIPAMReservation6(ipamTestNetwork, a6Endpoint, mac, rsv6)
	if err != nil || got != "fd00:6470:6865::20" {
		t.Fatalf("consume: %q, %v", got, err)
	}
	if rec := f0Rec(t, p, live[0].ID); rec.Phase != lease.PhaseCreated {
		t.Fatalf("the v6 record is %v after the consume, want Created", rec.Phase)
	}
	if h := p.joinHints[a6Endpoint]; h.IPv6 == nil || h.IPv6.IP.String() != "fd00:6470:6865::20" {
		t.Fatalf("the join hint carries %v, want the reserved address", h.IPv6)
	}
	a6Records(t, p)
}

func TestCreateEndpoint_V6PoolRefusesAMissingOrMismatchedReservation(t *testing.T) {
	for _, tc := range []struct {
		name, engine6, want string
		reserve6            bool
	}{
		{"missing", a6Leased, "no IPv6 reservation is held", false},
		{"mismatched", a6Other, "while the IPv6 lease reserved for", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, b, _, _ := a6Fixture(t)
			mac := f0MAC(0x01)
			if _, err := a6Request(p, b.PoolID, "", mac); err != nil {
				t.Fatalf("v4: %v", err)
			}
			if tc.reserve6 {
				if _, err := a6Request(p, b.PoolID6, "", mac); err != nil {
					t.Fatalf("v6: %v", err)
				}
			}
			req := f0CreateRequest(mac, a6Leased4)
			req.Interface.AddressIPv6 = tc.engine6
			_, err := p.createIPAMEndpoint(context.Background(), time.Now(), req, a6Options(), b)
			if err == nil || !errors.Is(err, util.ErrIPAM) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if live := a6Live(t, p); len(live) != 0 {
				t.Errorf("the refusal left %+v live in the v6 scope", live)
			}
			if n := p.ipamReserves.len(); n != 0 {
				t.Errorf("%d reservations held after the refusal", n)
			}
		})
	}
}

// Row 16: an untaken v6 reservation is retained once, whichever of the pair the sweep meets first (#962, #1132).
func TestSweepIPAMReservations_RetainsAV6Reservation(t *testing.T) {
	p, b, _, _ := a6Fixture(t)
	mac := f0MAC(0x01)
	if _, err := a6Request(p, b.PoolID, "", mac); err != nil {
		t.Fatalf("v4: %v", err)
	}
	if _, err := a6Request(p, b.PoolID6, "", mac); err != nil {
		t.Fatalf("v6: %v", err)
	}
	id6 := a6Live(t, p)[0].ID
	if n := p.sweepIPAMReservations(time.Now().Add(tombstoneTTL + time.Second)); n < 1 {
		t.Fatalf("the sweep took %d reservations", n)
	}
	if rec := f0Rec(t, p, id6); rec.Phase != lease.PhaseRetained {
		t.Fatalf("the v6 record is %v, want Retained", rec.Phase)
	}
	if n := p.ipamReserves.len(); n != 0 {
		t.Fatalf("%d reservations left after the sweep", n)
	}
	a6Records(t, p)
}

// ReleaseAddress v4 then v6, as the engine sends it: the v6 record ends with its sibling, the reservation drops (#1132).
func TestReleaseAddress_V6PoolGivesTheRecordUp(t *testing.T) {
	p, b, _, _ := a6Fixture(t)
	mac := f0MAC(0x01)
	if _, err := a6Request(p, b.PoolID, "", mac); err != nil {
		t.Fatalf("v4: %v", err)
	}
	if _, err := a6Request(p, b.PoolID6, "", mac); err != nil {
		t.Fatalf("v6: %v", err)
	}
	id6 := a6Live(t, p)[0].ID
	for _, r := range []ReleaseAddressRequest{{PoolID: b.PoolID, Address: "192.168.99.61"}, {PoolID: b.PoolID6, Address: "fd00:6470:6865::20"}} {
		if err := p.ReleaseAddress(r); err != nil {
			t.Fatalf("ReleaseAddress %+v: %v", r, err)
		}
	}
	if rec := f0Rec(t, p, id6); rec.Phase != lease.PhaseRetained {
		t.Fatalf("the v6 record is %v, want Retained", rec.Phase)
	}
	if n := p.ipamReserves.len(); n != 0 {
		t.Fatalf("%d reservations left after both releases", n)
	}
	a6Records(t, p)

	// The v6 release alone ends the v6 record too.
	p2, b2, _, _ := a6Fixture(t)
	if _, err := a6Request(p2, b2.PoolID6, "", mac); err != nil {
		t.Fatalf("v6: %v", err)
	}
	id := a6Live(t, p2)[0].ID
	if err := p2.ReleaseAddress(ReleaseAddressRequest{PoolID: b2.PoolID6, Address: "fd00:6470:6865::20"}); err != nil {
		t.Fatal(err)
	}
	if rec := f0Rec(t, p2, id); rec.Phase != lease.PhaseRetained || p2.ipamReserves.len() != 0 {
		t.Fatalf("the v6 record is %v with %d reservations, want Retained and none", rec.Phase, p2.ipamReserves.len())
	}
}

// Rows 23 and 24 on a real link: both families reserve under one MAC; CreateEndpoint answers no AddressIPv6 (#1132).
func TestRequestAddress_V6ReservesAndCreateEndpointConsumes(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	p, b, opts := s3Fixture(t, "dhcp")
	b.PoolID6, _ = ipamPoolID6(ipamLocalAddressSpace, ipamTestPool6, nil)
	b.Pool6 = ipamTestPool6
	if err := saveNetwork(ipamTestNetwork, opts, b); err != nil {
		t.Fatal(err)
	}
	p.ipamIndex.bind(b.PoolID6, ipamTestNetwork)
	a6Seams(t, p)
	mac := f0MAC(0x01)
	if _, err := a6Request(p, b.PoolID, "", mac); err != nil {
		t.Fatalf("v4: %v", err)
	}
	if _, err := a6Request(p, b.PoolID6, "", mac); err != nil {
		t.Fatalf("v6 refused beside its own v4 reservation: %v", err)
	}
	req := f0CreateRequest(mac, a6Leased4)
	req.Interface.AddressIPv6 = a6Leased
	res, err := p.createIPAMEndpoint(context.Background(), time.Now(), req, opts, b)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if res.Interface.AddressIPv6 != "" || res.Interface.Address != "" {
		t.Fatalf("CreateEndpoint answered %+v, want no address: the engine refuses one it did not ask for", res.Interface)
	}
	if fp := p.endpointFingerprints[req.EndpointID]; fp.IPv6 != "fd00:6470:6865::20" {
		t.Fatalf("the fingerprint holds %q, want the reserved v6 address", fp.IPv6)
	}
	if live := a6Live(t, p); len(live) != 1 || live[0].Phase != lease.PhaseCreated {
		t.Fatalf("v6 records %+v, want one Created", live)
	}
}

// Design D2: `--ip6` is the IA Address hint the one-shot sends, and without it a fresh MAC sends none (#1132).
func TestRequestAddress_V6SendsIP6AsTheHint(t *testing.T) {
	for _, tc := range []struct{ ip6, want string }{{"fd00:6470:6865::20", "fd00:6470:6865::20"}, {"", ""}} {
		p, b, srv, _ := a6Fixture(t)
		if _, err := a6Request(p, b.PoolID6, tc.ip6, f0MAC(0x01)); err != nil {
			t.Fatalf("--ip6 %q: %v", tc.ip6, err)
		}
		if srv.opts.PreferredV6 != tc.want {
			t.Errorf("--ip6 %q: the one-shot hinted %q, want %q", tc.ip6, srv.opts.PreferredV6, tc.want)
		}
	}
}

// Row 26: a v6 reservation link keeps IPv6 and gets the RA guard; a v4 one has IPv6 turned off (#1132, #1247).
func TestIPAMReserveLinkIPv6_V6KeepsTheLinkLocal(t *testing.T) {
	prevOff, prevGuard := childHostIPv6Off, ipamReserveLinkRAGuard
	t.Cleanup(func() { childHostIPv6Off, ipamReserveLinkRAGuard = prevOff, prevGuard })
	var off, guard []string
	childHostIPv6Off = func(n string) error { off = append(off, n); return nil }
	ipamReserveLinkRAGuard = func(n string) error { guard = append(guard, n); return nil }
	ipamReserveLinkIPv6("rsv6", true)
	ipamReserveLinkIPv6("rsv4", false)
	if len(off) != 1 || off[0] != "rsv4" || len(guard) != 1 || guard[0] != "rsv6" {
		t.Fatalf("IPv6 off on %v and the RA guard on %v, want off on [rsv4] only and the guard on [rsv6] only", off, guard)
	}
}
