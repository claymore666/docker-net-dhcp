// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

const s3Leased6 = "fd00:960::61/64"

// s3Fixture is f0Fixture on a real bridge, so createIPAMEndpoint makes and removes a real veth (#960).
func s3Fixture(t *testing.T, mode6 string) (*Plugin, *ipamBinding, DHCPNetworkOptions) {
	t.Helper()
	p, b, _, _ := f0Fixture(t)
	p.docker = f0Docker()
	h, err := netlink.NewHandle()
	if err != nil {
		t.Fatalf("NewHandle: %v", err)
	}
	addLink(t, h, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: f0Bridge}})
	opts := f0Options()
	opts.IPv6Mode = mode6
	if err := saveNetwork(ipamTestNetwork, opts, b); err != nil {
		t.Fatalf("saveNetwork: %v", err)
	}
	return p, b, opts
}

func s3Records6(t *testing.T, p *Plugin) []lease.Record {
	t.Helper()
	rb, err := p.records.Rebuilt()
	if err != nil {
		t.Fatalf("Rebuilt: %v", err)
	}
	var out []lease.Record
	for _, rec := range rb.Records {
		if rec.Scope == dhcp.Scope6(ipamTestNetwork) {
			out = append(out, rec)
		}
	}
	return out
}

func s3Identity(t *testing.T, rec lease.Record) dhcp.Identity6 {
	t.Helper()
	id6, err := dhcp.ParseIdentity6(rec.Identity)
	if err != nil {
		t.Fatalf("record %s: %v", rec.ID, err)
	}
	return id6
}

func s3SameIdentity(a, b dhcp.Identity6) bool {
	return bytes.Equal(a.DUID, b.DUID) && a.IAID == b.IAID
}

func s3LinksGone(t *testing.T, ep string) {
	t.Helper()
	host, ctr := vethPairNames(ep)
	for _, name := range []string{host, ctr} {
		if _, err := netlink.LinkByName(name); err == nil {
			t.Errorf("the link %s outlived a failed CreateEndpoint, so the next attempt meets a name in use", name)
		}
	}
}

func TestIPAMEndpointV6_AFatalVerdictGivesUpBothRecordsAndRemovesTheLink(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	restarted := f0MAC(0x02)
	p, b, opts := s3Fixture(t, "dhcp")
	id4, id6 := s2Rebound(t, p, b, restarted)
	_, v6 := stubV6OneShot(t, dhcp.Info{}, dhcp.RAObservation{Seen: true, Managed: true}, dhcp.ErrNoLease)
	req := f0CreateRequest(restarted, f0Addr)

	_, err := p.createIPAMEndpoint(context.Background(), time.Now(), req, opts, b)
	if err == nil || !errors.Is(err, dhcp.ErrNoLease) || !strings.HasPrefix(err.Error(), "failed to get initial IPv6 address via DHCPv6: ") {
		t.Fatalf("CreateEndpoint err = %v, want the managed segment's missing lease (#868)", err)
	}
	if v6.opts.RecordID != id6 {
		t.Fatalf("the one-shot ran on record %q, want the re-bound %q", v6.opts.RecordID, id6)
	}
	s3LinksGone(t, req.EndpointID)
	// The v4 record holds its lease, so the pair is retained on one deadline and a retry re-binds both (#960).
	s2Pair(t, p, id4, id6, restarted, lease.PhaseRetained)
	if n := len(s3Records6(t, p)); n != 1 {
		t.Errorf("the failed attempt left %d v6 records, want the one it was handed", n)
	}
	if fp, ok := p.endpointFingerprints[req.EndpointID]; ok {
		t.Errorf("a failed CreateEndpoint remembered %+v, which a later re-bind reads as a running endpoint", fp)
	}
}

func TestIPAMEndpointV6_AReBoundRecordIsResumedNotMinted(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	first, restarted := f0MAC(0x01), f0MAC(0x02)
	p, b, opts := s3Fixture(t, "dhcp")
	_, id6 := s2Rebound(t, p, b, restarted)
	stored := s3Identity(t, f0Rec(t, p, id6))
	start := time.Now()
	_, v6 := stubV6OneShot(t, dhcp.Info{IP: s3Leased6, Gateway: "fe80::1"}, dhcp.RAObservation{Seen: true, Managed: true}, nil)
	req := f0CreateRequest(restarted, f0Addr)

	res, err := p.createIPAMEndpoint(context.Background(), start, req, opts, b)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	_, ctrName := vethPairNames(req.EndpointID)
	minted, _ := resolveIdentity6(opts, req.EndpointID, restarted)
	switch {
	case v6.opts.RecordID != id6:
		t.Errorf("the one-shot ran on record %q, want the re-bound %q", v6.opts.RecordID, id6)
	case !s3SameIdentity(v6.opts.Identity6, stored) || s3SameIdentity(v6.opts.Identity6, minted):
		t.Errorf("the one-shot carried %+v, want the stored %+v and not one minted on the new MAC (RFC 9915 section 11)",
			v6.opts.Identity6, stored)
	case !bytes.Equal(stored.DUID[4:], first):
		t.Errorf("the stored DUID %x is not the first MAC's DUID-LL, so this case cannot tell resume from mint", stored.DUID)
	case v6.opts.PreferredV6 != strings.Split(s2Addr6, "/")[0]:
		t.Errorf("the one-shot asked for %q, want the re-bound record's %q", v6.opts.PreferredV6, s2Addr6)
	case !bytes.Equal(v6.opts.MAC, restarted) || v6.iface != ctrName:
		t.Errorf("the one-shot ran on %q with MAC %v, want %q and %v", v6.iface, v6.opts.MAC, ctrName, restarted)
	case !v6.deadline.Equal(start.Add(pluginCallBudget - pluginCallMargin)):
		t.Errorf("the one-shot's deadline is %v after the call started, want the daemon's budget from the call's "+
			"own start (#911)", v6.deadline.Sub(start))
	}
	if res.Interface.AddressIPv6 != s3Leased6 || res.Interface.Address != "" || res.Interface.MacAddress != "" {
		t.Errorf("CreateEndpoint answered %+v, want only the Reply's %s (#110)", *res.Interface, s3Leased6)
	}
	if n := len(s3Records6(t, p)); n != 1 {
		t.Errorf("the endpoint has %d v6 records, want the re-bound one alone", n)
	}
	if fp := p.endpointFingerprints[req.EndpointID]; fp.IPv6 != "fd00:960::61" || fp.IPv4 != "192.168.99.10" {
		t.Errorf("the fingerprint is %+v, want both addresses bare", fp)
	}
}

func TestIPAMEndpointV6_WithNoReBoundRecordMintsOnTheCurrentMAC(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	mac := f0MAC(0x03)
	p, b, opts := s3Fixture(t, "dhcp")
	id4 := f0Created(t, p, mac, f0Addr)
	f0Reservation(p, b, mac, id4, f0Addr, nil)
	_, v6 := stubV6OneShot(t, dhcp.Info{IP: s3Leased6, Gateway: "fe80::1"}, dhcp.RAObservation{Seen: true, Managed: true}, nil)
	req := f0CreateRequest(mac, f0Addr)

	if _, err := p.createIPAMEndpoint(context.Background(), time.Now(), req, opts, b); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	recs := s3Records6(t, p)
	if len(recs) != 1 {
		t.Fatalf("the endpoint has %d v6 records, want one", len(recs))
	}
	want, _ := resolveIdentity6(opts, req.EndpointID, mac)
	rec := recs[0]
	switch {
	case rec.ID != v6.opts.RecordID || rec.ID == id4:
		t.Errorf("the one-shot ran on %q beside the v4 %q, want the new v6 record %q", v6.opts.RecordID, id4, rec.ID)
	case !bytes.Equal(rec.CHAddr, mac) || rec.Phase != lease.PhaseCreated:
		t.Errorf("the v6 record is on %v in %v, want %v in Created", net.HardwareAddr(rec.CHAddr), rec.Phase, mac)
	case !s3SameIdentity(s3Identity(t, rec), want) || !s3SameIdentity(v6.opts.Identity6, want):
		t.Errorf("the stored identity is %+v and the one-shot's %+v, want resolveIdentity6's %+v", s3Identity(t, rec),
			v6.opts.Identity6, want)
	case v6.opts.PreferredV6 != "":
		t.Errorf("a fresh identity asked for %q, want no preferred address", v6.opts.PreferredV6)
	}
}

func TestIPAMEndpointV6_AReBoundRecordNoLongerCreatedIsNotReused(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	mac := f0MAC(0x03)
	p, b, opts := s3Fixture(t, "dhcp")
	id4 := f0Created(t, p, mac, f0Addr)
	gone := s2Tombstone6(t, p, f0MAC(0x01), s2Addr6)
	key := ipamReserveKey(b.PoolID, mac)
	r, _ := p.ipamReserves.begin(key, time.Now())
	p.ipamReserves.finish(key, r, ipamReservation{addr: netip.MustParsePrefix(f0Addr), info: dhcp.Info{IP: f0Addr, Gateway: "192.168.99.1"},
		record: id4, record6: gone}, nil)
	_, v6 := stubV6OneShot(t, dhcp.Info{IP: s3Leased6}, dhcp.RAObservation{}, nil)

	if _, err := p.createIPAMEndpoint(context.Background(), time.Now(), f0CreateRequest(mac, f0Addr), opts, b); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if v6.opts.RecordID == gone || v6.opts.RecordID == "" {
		t.Errorf("the one-shot ran on %q, want a fresh record: %q is a tombstone and OpRebind needs Retained", v6.opts.RecordID, gone)
	}
	if got := f0Rec(t, p, gone); got.Phase != lease.PhaseRetained {
		t.Errorf("the tombstone %s moved to %v", gone, got.Phase)
	}
}

func TestIPAMEndpointV6_OffRunsNoV6AndWritesNoV6Record(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	mac := f0MAC(0x03)
	p, b, opts := s3Fixture(t, "")
	id4 := f0Created(t, p, mac, f0Addr)
	f0Reservation(p, b, mac, id4, f0Addr, nil)
	v4, v6 := stubV6OneShot(t, dhcp.Info{IP: s3Leased6}, dhcp.RAObservation{}, nil)
	req := f0CreateRequest(mac, f0Addr)

	res, err := p.createIPAMEndpoint(context.Background(), time.Now(), req, opts, b)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if v4.iface != "" || v6.iface != "" || res.Interface.AddressIPv6 != "" || len(s3Records6(t, p)) != 0 {
		t.Errorf("an ipv6-off network ran v4 %q and v6 %q, answered %q and wrote %d v6 records, want none of it",
			v4.iface, v6.iface, res.Interface.AddressIPv6, len(s3Records6(t, p)))
	}
	if fp := p.endpointFingerprints[req.EndpointID]; fp.IPv6 != "" {
		t.Errorf("the fingerprint carries %q on a v4-only endpoint", fp.IPv6)
	}
}

func TestIPAMEndpointV6_ATolerableAbsenceStartsTheEndpointWithNoV6(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	mac := f0MAC(0x03)
	p, b, opts := s3Fixture(t, "dhcp")
	id4 := f0Created(t, p, mac, f0Addr)
	f0Reservation(p, b, mac, id4, f0Addr, nil)
	stubV6OneShot(t, dhcp.Info{}, dhcp.RAObservation{}, dhcp.ErrNoLease)
	req := f0CreateRequest(mac, f0Addr)

	res, err := p.createIPAMEndpoint(context.Background(), time.Now(), req, opts, b)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v; a segment with no router is not fatal in dhcp mode (#868)", err)
	}
	if res.Interface.AddressIPv6 != "" || p.endpointFingerprints[req.EndpointID].IPv6 != "" {
		t.Errorf("a tolerated absence answered %q, want no IPv6", res.Interface.AddressIPv6)
	}
	if f0Rec(t, p, id4).Phase != lease.PhaseCreated {
		t.Errorf("the v4 record left Created on a tolerated v6 absence")
	}
}

func TestIPAMRebind6_ARunningEndpointsV6AddressIsNotOffered(t *testing.T) {
	first, restarted := f0MAC(0x01), f0MAC(0x02)
	p, _, _, _ := f0Fixture(t)
	id4 := f0Tombstone(t, p, first, f0Addr)
	s2Tombstone6(t, p, first, s2Addr6)
	p.endpointFingerprints["ep-running"] = endpointFingerprint{MAC: first.String(), IPv4: "192.168.99.99",
		IPv6: strings.Split(s2Addr6, "/")[0]}

	got4, _, _, got6 := p.ipamRebindCandidate(ipamTestNetwork, restarted)
	if got4 != id4 || got6 != "" {
		t.Errorf("the re-bind took (%q, %q), want (%q, \"\"): the v6 address is held by a running endpoint", got4, got6, id4)
	}
}
