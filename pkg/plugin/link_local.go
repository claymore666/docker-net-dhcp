// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net"
	"net/netip"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/runtime"
	"github.com/claymore666/dhcp-golib/wire"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// RFC 3927 section 2.1 reserves the first and last 256 addresses of 169.254/16.
const (
	linkLocalFirst = 169<<24 | 254<<16 | 1<<8
	linkLocalCount = 254 * 256
)

// linkLocalStateName is the health document's lease_state for an endpoint on its fallback address (#904).
const linkLocalStateName = "link_local"

// isLinkLocalV4 is true for any address in 169.254/16, the RFC 3927 prefix.
func isLinkLocalV4(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 169 && ip4[1] == 254
}

// isLinkLocalV4String reads a bare address or a CIDR, as the fingerprint and Docker's view carry either.
func isLinkLocalV4String(s string) bool {
	if ip, _, err := net.ParseCIDR(s); err == nil {
		return isLinkLocalV4(ip)
	}
	return isLinkLocalV4(net.ParseIP(s))
}

// isLinkLocalAddr is isLinkLocalV4 on a possibly nil netlink address.
func isLinkLocalAddr(a *netlink.Addr) bool {
	return a != nil && isLinkLocalV4(a.IP)
}

// linkLocalWindow is one claim: RFC 5227 section 2.1.1's probe window plus RFC 3927 section 2.4's later announcements.
func linkLocalWindow(acd proto.ACDParams) time.Duration {
	gaps := acd.AnnounceNum - 1
	if gaps < 0 {
		gaps = 0
	}
	return dhcp.ConflictWindow(acd) + time.Duration(gaps)*time.Duration(acd.AnnounceInterval)
}

// linkLocalClaimDeadline keeps v6AcquisitionDeadline's margin before the engine's 30 s (#911, #904).
func linkLocalClaimDeadline(callStart time.Time) time.Time {
	return callStart.Add(pluginCallBudget - pluginCallMargin)
}

// linkLocalDrain is what the one-shot may take past its deadline to drain and write its record (#899, #904).
const linkLocalDrain = time.Second

// linkLocalLeaseTimeout is the most lease_timeout a link_local_fallback network can spend on DHCP: 30 - 4 - 9 - 1 s.
var linkLocalLeaseTimeout = pluginCallBudget - pluginCallMargin - linkLocalWindow(proto.DefaultACDParams()) - linkLocalDrain

// linkLocalDHCPDeadline leaves the drain and one whole claim window after the DHCP attempt (#904).
func linkLocalDHCPDeadline(callStart time.Time) time.Time {
	return callStart.Add(linkLocalLeaseTimeout)
}

// leaseTimeoutFor is the one-shot's DHCP budget: the operator's, else the derived one for the network's shape.
func leaseTimeoutFor(opts DHCPNetworkOptions) time.Duration {
	switch {
	case opts.LeaseTimeout != 0:
		return opts.LeaseTimeout
	case opts.LinkLocalFallback:
		return linkLocalLeaseTimeout
	default:
		return defaultLeaseTimeout
	}
}

// validateLinkLocalFallback refuses what the fallback cannot honour; IPAM mode is refused in CreateNetwork (#904).
func validateLinkLocalFallback(opts DHCPNetworkOptions) error {
	if !opts.LinkLocalFallback {
		return nil
	}
	// Measured 2026-09-25 on Linux 6.12: the reply to an ipvlan l2 child's ARP probe reached the parent only, never
	// the child, so a defended address would read as free (#904).
	if opts.effectiveMode() == ModeIPvlan {
		return fmt.Errorf("%w: link_local_fallback cannot be set in mode=ipvlan: an ipvlan child does not receive the ARP replies to its own probes, so the plugin could not tell a link-local address in use from a free one (RFC 5227 section 2.1.1). Use mode=macvlan or mode=bridge",
			util.ErrModeMismatch)
	}
	if m, err := opts.ipv6Mode(); err == nil && m != proto.Mode6Off {
		return fmt.Errorf("%w: link_local_fallback cannot be combined with ipv6_mode=%s: the fallback is IPv4 only, and it spends the part of the engine's %v per endpoint that the DHCPv6 acquisition runs in",
			util.ErrModeMismatch, m, pluginCallBudget)
	}
	if opts.LeaseTimeout > linkLocalLeaseTimeout {
		acd := proto.DefaultACDParams()
		return fmt.Errorf("%w: lease_timeout %v is longer than link_local_fallback allows: the engine gives an endpoint %v, the plugin keeps %v of it and %v for the DHCP attempt to stop, and claiming a link-local address takes up to %v (RFC 5227 section 2.1.1's probe window %v plus RFC 3927 section 2.4's second announcement %v later), which leaves %v for DHCP. Set lease_timeout to %v or less, or leave it unset",
			util.ErrIPAM, opts.LeaseTimeout, pluginCallBudget, pluginCallMargin, linkLocalDrain, linkLocalWindow(acd),
			dhcp.ConflictWindow(acd), time.Duration(acd.AnnounceInterval), linkLocalLeaseTimeout, linkLocalLeaseTimeout)
	}
	return nil
}

// ipamRefuseLinkLocal: in IPAM mode Docker holds the address from RequestAddress on and cannot follow a lease (#904).
func ipamRefuseLinkLocal(opts DHCPNetworkOptions) error {
	if !opts.LinkLocalFallback {
		return nil
	}
	return fmt.Errorf("%w: link_local_fallback cannot be combined with this plugin as the IPAM driver: in IPAM mode Docker assigns the address before the endpoint exists and never learns of a later change, so a container that fell back to 169.254/16 would keep that address in Docker's records after it moved to a lease. Create the network with --ipam-driver null instead",
		util.ErrIPAM)
}

// linkLocalEligible: only a DHCP timeout or a no-lease falls back, never an engine cancel or a socket error (#904).
func linkLocalEligible(reqCtx context.Context, err error) bool {
	if reqCtx.Err() != nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, dhcp.ErrNoLease)
}

// arpLink is the part of runtime.ARPSocket a claim uses, a seam for the tests.
type arpLink interface {
	Send(frame []byte) error
	Received() <-chan lease.ARPInbound
	HardwareAddr() net.HardwareAddr
	Close() error
}

var openARPLink = func(iface string) (arpLink, error) {
	return runtime.NewARPSocket(iface)
}

// linkLocalACD is the claim's timing, RFC 5227 section 1.1's constants, which RFC 3927 section 2.2 shares.
var linkLocalACD = proto.DefaultACDParams

var (
	// ErrLinkLocalNoTime is a claim with no whole window left before the engine's deadline.
	ErrLinkLocalNoTime = errors.New("no time left before the engine's deadline to probe another link-local address")
	// ErrLinkLocalTooManyConflicts is RFC 3927 section 2.2.1's MAX_CONFLICTS reached.
	ErrLinkLocalTooManyConflicts = errors.New("every link-local address tried was in use (RFC 3927 section 2.2.1's MAX_CONFLICTS)")
)

// linkLocalFallback claims a 169.254/16 address after an eligible DHCPv4 failure, else returns the failure (#904).
func (p *Plugin) linkLocalFallback(ctx context.Context, opts DHCPNetworkOptions, callStart time.Time, iface, endpointID string, acqErr error) (dhcp.Info, error) {
	if !opts.LinkLocalFallback || !linkLocalEligible(ctx, acqErr) {
		return dhcp.Info{}, acqErr
	}
	fields := log.Fields{"endpoint": shortID(endpointID), "iface": iface}
	log.WithError(acqErr).WithFields(fields).
		Warn("No DHCPv4 lease in time; claiming an RFC 3927 link-local address and soliciting on")

	link, err := openARPLink(iface)
	if err != nil {
		return dhcp.Info{}, fmt.Errorf("no DHCPv4 lease (%v), and the link-local fallback could not open the link: %w", acqErr, err)
	}
	defer link.Close()

	claimCtx, cancel := context.WithDeadline(ctx, linkLocalClaimDeadline(callStart))
	defer cancel()
	addr, tried, err := claimLinkLocal(claimCtx, link, linkLocalACD(), newLinkLocalPicker(link.HardwareAddr()))
	if err != nil {
		return dhcp.Info{}, fmt.Errorf("no DHCPv4 lease (%v), and no link-local address after %d candidates: %w", acqErr, tried, err)
	}
	log.WithFields(fields).WithField("ip", addr.String()).WithField("candidates", tried).
		Warn("Endpoint is on an IPv4 link-local address with no gateway until a DHCP lease arrives")
	return dhcp.Info{IP: netip.PrefixFrom(addr, 16).String()}, nil
}

// acquireV4 ends DHCP a drain and a claim window early, so a claim on the caller's context meets the deadline (#904).
func (p *Plugin) acquireV4(ctx context.Context, opts DHCPNetworkOptions, callStart time.Time, iface string, pol serverPolicy, timeout time.Duration, endpointID string, base dhcp.DHCPClientOptions) (dhcp.Info, error) {
	if !opts.LinkLocalFallback {
		info, _, err := p.acquireWithPolicy(ctx, iface, pol, false, timeout, endpointID, base)
		return info, err
	}
	dhcpCtx, cancel := context.WithDeadline(ctx, linkLocalDHCPDeadline(callStart))
	defer cancel()
	info, _, err := p.acquireWithPolicy(dhcpCtx, iface, pol, false, timeout, endpointID, base)
	if err != nil {
		info, err = p.linkLocalFallback(ctx, opts, callStart, iface, endpointID, err)
	}
	return info, err
}

// newLinkLocalPicker seeds RFC 3927 section 2.1's generator from the MAC: one interface, one sequence.
func newLinkLocalPicker(mac net.HardwareAddr) func() netip.Addr {
	h := fnv.New64a()
	_, _ = h.Write(mac)
	rng := rand.New(rand.NewPCG(h.Sum64(), 0x169254))
	return func() netip.Addr {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(linkLocalFirst+rng.IntN(linkLocalCount)))
		return netip.AddrFrom4(b)
	}
}

// linkLocalConflict is RFC 5227 section 2.1.1: a packet sent from the candidate, or another host's Probe for it.
func linkLocalConflict(pkt *wire.ARPPacket, candidate netip.Addr, own net.HardwareAddr) bool {
	if bytes.Equal(pkt.SenderHW, own) {
		return false
	}
	if pkt.SenderIP == candidate {
		return true
	}
	return pkt.IsProbe() && pkt.TargetIP == candidate
}

// claimLinkLocal probes candidates until one is free and announced, while a whole window still fits the deadline.
// RFC 3927 section 2.2.1 slows to one attempt a minute after MAX_CONFLICTS, which no endpoint's budget holds, so
// the claim stops there instead.
func claimLinkLocal(ctx context.Context, link arpLink, acd proto.ACDParams, next func() netip.Addr) (netip.Addr, int, error) {
	window := linkLocalWindow(acd)
	tried := 0
	for {
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < window {
			return netip.Addr{}, tried, ErrLinkLocalNoTime
		}
		if acd.MaxConflicts > 0 && tried >= acd.MaxConflicts {
			return netip.Addr{}, tried, ErrLinkLocalTooManyConflicts
		}
		candidate := next()
		tried++
		free, err := probeLinkLocal(ctx, link, acd, candidate)
		if err != nil {
			return netip.Addr{}, tried, err
		}
		if !free {
			log.WithField("ip", candidate.String()).Info("Link-local candidate is in use on the link; trying another")
			continue
		}
		if err := announceLinkLocal(ctx, link, acd, candidate); err != nil {
			return netip.Addr{}, tried, err
		}
		return candidate, tried, nil
	}
}

// probeLinkLocal runs RFC 5227 section 2.1.1 once, reading every frame for a conflict.
func probeLinkLocal(ctx context.Context, link arpLink, acd proto.ACDParams, candidate netip.Addr) (bool, error) {
	own := link.HardwareAddr()
	probe, err := wire.EncodeARP(&wire.ARPPacket{
		Op:       wire.ARPRequest,
		SenderHW: own,
		TargetHW: make([]byte, 6),
		SenderIP: netip.IPv4Unspecified(),
		TargetIP: candidate,
	})
	if err != nil {
		return false, err
	}

	waits := make([]time.Duration, 0, acd.ProbeNum+1)
	waits = append(waits, randUpTo(0, time.Duration(acd.ProbeWait)))
	for i := 1; i < acd.ProbeNum; i++ {
		waits = append(waits, randUpTo(time.Duration(acd.ProbeMin), time.Duration(acd.ProbeMax)))
	}
	waits = append(waits, time.Duration(acd.AnnounceWait))

	for i, wait := range waits {
		conflict, err := watchARP(ctx, link, wait, candidate, own)
		if err != nil || conflict {
			return false, err
		}
		if i < len(waits)-1 {
			if err := link.Send(probe); err != nil {
				return false, fmt.Errorf("sending an ARP probe for %v: %w", candidate, err)
			}
		}
	}
	return true, nil
}

// announceLinkLocal sends RFC 5227 section 2.3's ANNOUNCE_NUM announcements, ANNOUNCE_INTERVAL apart.
func announceLinkLocal(ctx context.Context, link arpLink, acd proto.ACDParams, addr netip.Addr) error {
	frame, err := wire.EncodeARP(&wire.ARPPacket{
		Op:       wire.ARPRequest,
		SenderHW: link.HardwareAddr(),
		TargetHW: make([]byte, 6),
		SenderIP: addr,
		TargetIP: addr,
	})
	if err != nil {
		return err
	}
	for i := 0; i < acd.AnnounceNum; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(acd.AnnounceInterval)):
			}
		}
		if err := link.Send(frame); err != nil {
			return fmt.Errorf("sending an ARP announcement for %v: %w", addr, err)
		}
	}
	return nil
}

// watchARP reads frames for d and reports the first conflict for candidate.
func watchARP(ctx context.Context, link arpLink, d time.Duration, candidate netip.Addr, own net.HardwareAddr) (bool, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, nil
		case in, ok := <-link.Received():
			if !ok {
				return false, errors.New("the ARP socket closed during the probe")
			}
			if in.Err != nil {
				continue
			}
			pkt, err := wire.DecodeARP(in.Frame)
			if err != nil {
				continue
			}
			if linkLocalConflict(pkt, candidate, own) {
				return true, nil
			}
		}
	}
}

// randUpTo is uniform in [lo, hi], RFC 5227 section 2.1.1's "random time interval".
func randUpTo(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + rand.N(hi-lo+1)
}

// onLinkLocal is true while the endpoint's IPv4 address is its RFC 3927 fallback.
func (m *dhcpManager) onLinkLocal() bool {
	v4, _ := m.lastIPs()
	return isLinkLocalAddr(v4)
}

// leaveLinkLocal installs what Join withheld from a link-local endpoint, the gateway and routes Join would have
// returned for this lease, computed by the same functions; RFC 3927 section 1.9's removal of the old address is
// applyAddressChange's (#904).
func (m *dhcpManager) leaveLinkLocal(ip *netlink.Addr, info dhcp.Info) error {
	if m.netHandle == nil || m.ctrLink == nil || m.plugin == nil {
		return nil
	}
	hint := joinHint{IPv4: ip, Gateway: info.Gateway, Routes: dhcpStaticRoutes(info.Routes)}
	if m.opts.Gateway != "" {
		hint.Gateway = m.opts.Gateway
	}
	res := JoinResponse{Gateway: hint.Gateway}
	src, err := joinRouteSource(m.opts)
	if err != nil {
		return err
	}
	opts := m.opts
	if err := m.plugin.addRoutes(&opts, false, src, m.joinReq, hint, &res); err != nil {
		return err
	}
	m.plugin.appendDHCPStaticRoutes(opts, m.joinReq, hint, &res)

	var errs []error
	// On-link routes first, since a next hop, the gateway included, may be reachable only through one.
	for _, want := range []int{RouteTypeOnLink, RouteTypeNextHop} {
		for _, sr := range res.StaticRoutes {
			if sr == nil || sr.RouteType != want {
				continue
			}
			if err := m.installStaticRoute(sr); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if gw := net.ParseIP(res.Gateway); gw != nil {
		if err := m.netHandle.RouteReplace(&netlink.Route{LinkIndex: m.ctrLink.Attrs().Index, Gw: gw}); err != nil {
			errs = append(errs, fmt.Errorf("default route via %v: %w", gw, err))
		}
	}
	log.WithFields(m.logFields(false)).
		WithField("ip", ip.String()).
		WithField("gateway", res.Gateway).
		WithField("routes", describeStaticRoutes(res.StaticRoutes)).
		Info("Endpoint left its link-local address for a DHCP lease; installed the gateway and routes Join withheld")
	return errors.Join(errs...)
}

func (m *dhcpManager) installStaticRoute(sr *StaticRoute) error {
	_, dst, err := net.ParseCIDR(sr.Destination)
	if err != nil {
		return fmt.Errorf("route %s: %w", sr.Destination, err)
	}
	route := &netlink.Route{LinkIndex: m.ctrLink.Attrs().Index, Dst: dst, Scope: netlink.SCOPE_LINK}
	if sr.RouteType == RouteTypeNextHop {
		route.Gw = net.ParseIP(sr.NextHop)
		route.Scope = netlink.SCOPE_UNIVERSE
	}
	if err := m.netHandle.RouteReplace(route); err != nil {
		return fmt.Errorf("route %s: %w", sr.Destination, err)
	}
	return nil
}

func countLinkLocal(endpoints []EndpointHealth) int {
	n := 0
	for _, e := range endpoints {
		if e.LeaseState == linkLocalStateName {
			n++
		}
	}
	return n
}

// recoveredV4 is a recovered endpoint's v4 address. Docker keeps the 169.254/16 address CreateEndpoint answered after
// the move to a lease (#104), so a lease the endpoint's record would resume wins over it (#904).
func (p *Plugin) recoveredV4(networkID string, key net.HardwareAddr, docker *netlink.Addr) *netlink.Addr {
	if !isLinkLocalAddr(docker) {
		return docker
	}
	_, res := p.recordResume(networkID, key)
	if res.Lease == nil || !res.Lease.Addr.Addr().Is4() || isLinkLocalV4(res.Lease.Addr.Addr().AsSlice()) {
		return docker
	}
	a := res.Lease.Addr
	return &netlink.Addr{IPNet: &net.IPNet{IP: a.Addr().AsSlice(), Mask: net.CIDRMask(a.Bits(), 32)}}
}

// closeLinkLocalRecord closes an addressless v4 record, which on_remove would count as a failed release (#904).
func (p *Plugin) closeLinkLocalRecord(networkID string, key net.HardwareAddr) {
	if p.records == nil || len(key) == 0 {
		return
	}
	rb, err := p.records.Rebuilt()
	if err != nil {
		return
	}
	matches := rb.ByScopeMAC(networkID, key)
	if len(matches) == 0 {
		return
	}
	last := matches[len(matches)-1]
	if _, held := last.Addr(); held || last.Phase == lease.PhaseClosed {
		return
	}
	p.closeRecord(last.ID)
}
