// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// A delegated prefix is never an address of the link it arrived on (RFC 3633 section 12.1), so the container gets one
// `unreachable <prefix> proto dhcp` route: router software inside adds longer routes that win, and an unused part of
// the prefix answers unreachable instead of looping to the LAN router. RTPROT_DHCP keeps it apart from the RIO routes
// and the kernel's connected routes, which a delete by destination alone would take (measured 2026-09-30, #214).

// prefixAggregate is the route and, unchanged, its delete key; no link index, so the kernel puts it on lo.
func prefixAggregate(dst *net.IPNet) *netlink.Route {
	return &netlink.Route{Dst: dst, Type: unix.RTN_UNREACHABLE, Protocol: unix.RTPROT_DHCP, Family: netlink.FAMILY_V6}
}

func (m *dhcpManager) installedPrefixRoutes() (map[string]*net.IPNet, error) {
	filter := &netlink.Route{Type: unix.RTN_UNREACHABLE, Protocol: unix.RTPROT_DHCP}
	routes, err := nlHandleRouteListFiltered(m.netHandle, netlink.FAMILY_V6, filter,
		netlink.RT_FILTER_TYPE|netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return nil, fmt.Errorf("failed to list the delegated prefix routes: %w", err)
	}
	out := make(map[string]*net.IPNet, len(routes))
	for _, r := range routes {
		if r.Dst != nil && r.Type == unix.RTN_UNREACHABLE && r.Protocol == unix.RTPROT_DHCP {
			out[r.Dst.String()] = r.Dst
		}
	}
	return out, nil
}

// reconcilePrefixRoutes makes this endpoint's own aggregates exactly prefixes; skip_routes does not govern them (#214).
func (m *dhcpManager) reconcilePrefixRoutes(prefixes []dhcp.V6Addr) error {
	if m.netHandle == nil {
		return nil
	}
	want := make(map[string]*net.IPNet, len(prefixes))
	for _, p := range prefixes {
		_, dst, err := net.ParseCIDR(p.IP)
		if err != nil || dst.IP.To4() != nil {
			log.WithFields(m.logFields(true)).WithField("prefix", p.IP).
				Warn("The delegated prefix is not an IPv6 prefix; no route installed for it")
			continue
		}
		want[dst.String()] = dst
	}
	installed, err := m.installedPrefixRoutes()
	if err != nil {
		return err
	}
	var firstErr error
	for key, dst := range m.ownedPrefixRoutes() {
		if _, keep := want[key]; keep {
			continue
		}
		if _, present := installed[key]; !present {
			m.disownPrefixRoute(key)
			log.WithFields(m.logFields(true)).WithField("prefix", key).
				Info("The delegated prefix is no longer held; its route was already gone from the container")
			continue
		}
		if err := nlHandleRouteDel(m.netHandle, prefixAggregate(dst)); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to remove the delegated prefix route %v: %w", key, err)
			}
			continue
		}
		m.disownPrefixRoute(key)
		m.countPrefixRoute(false)
		log.WithFields(m.logFields(true)).WithField("prefix", key).
			Info("The delegated prefix is no longer held; removed its route from the container")
	}
	owned := m.ownedPrefixRoutes()
	for key, dst := range want {
		if _, present := installed[key]; present {
			if _, ours := owned[key]; !ours {
				log.WithFields(m.logFields(true)).WithField("prefix", key).
					Warn("The container already has an unreachable route for the delegated prefix that this " +
						"endpoint did not install; leaving it to whoever did")
			}
			continue
		}
		if err := nlHandleRouteReplace(m.netHandle, prefixAggregate(dst)); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to install the delegated prefix route %v: %w", key, err)
			}
			continue
		}
		m.ownPrefixRoute(key, dst)
		m.countPrefixRoute(true)
		log.WithFields(m.logFields(true)).WithField("prefix", key).
			Info("Installed the delegated prefix as an unreachable route in the container")
	}
	return firstErr
}

func (m *dhcpManager) ownedPrefixRoutes() map[string]*net.IPNet {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	out := make(map[string]*net.IPNet, len(m.prefixRoutes))
	for k, v := range m.prefixRoutes {
		out[k] = v
	}
	return out
}

func (m *dhcpManager) ownPrefixRoute(key string, dst *net.IPNet) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	if m.prefixRoutes == nil {
		m.prefixRoutes = make(map[string]*net.IPNet, 1)
	}
	m.prefixRoutes[key] = dst
}

func (m *dhcpManager) disownPrefixRoute(key string) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	delete(m.prefixRoutes, key)
}

// seedPrefixRoutes claims the prefixes the endpoint's own record held, which a previous plugin process routed.
func (m *dhcpManager) seedPrefixRoutes(l *lease.Lease) {
	if l == nil {
		return
	}
	for _, p := range l.Prefixes {
		pfx := p.Addr.Masked()
		if !pfx.IsValid() || !pfx.Addr().Is6() || pfx.Addr().Is4In6() {
			continue
		}
		dst := &net.IPNet{IP: pfx.Addr().AsSlice(), Mask: net.CIDRMask(pfx.Bits(), 128)}
		m.ownPrefixRoute(dst.String(), dst)
	}
}

func (m *dhcpManager) countPrefixRoute(installed bool) {
	if m.plugin == nil {
		return
	}
	if installed {
		m.plugin.ipv6PrefixRoutesInstalled.Add(1)
		return
	}
	m.plugin.ipv6PrefixRoutesWithdrawn.Add(1)
}

// withdrawPrefixRoutes removes every aggregate this endpoint installed; a release calls it before the Release is sent (RFC 8415 section 18.2.7).
func (m *dhcpManager) withdrawPrefixRoutes() error {
	return m.reconcilePrefixRoutes(nil)
}

// applyPrefixes records a v6 lease event's prefixes and makes the container's aggregates match them (#214).
func (m *dhcpManager) applyPrefixes(prefixes []dhcp.V6Addr) error {
	m.notePrefixes(prefixes, time.Now())
	m.notePrefixAbsence(len(prefixes) == 0)
	return m.reconcilePrefixRoutes(prefixes)
}

// notePrefixAbsence logs, once per absence, a v6 lease that carries no prefix although ipv6_pd asked for one (#214).
func (m *dhcpManager) notePrefixAbsence(none bool) {
	if m.opts.IPv6PD == 0 {
		return
	}
	m.ipMu.Lock()
	was := m.prefixNoneLogged
	m.prefixNoneLogged = none
	m.ipMu.Unlock()
	if none && !was {
		log.WithFields(m.logFields(true)).WithField("ipv6_pd", m.opts.IPv6PD).
			Info("The endpoint carries no delegated prefix although ipv6_pd asks for one; no aggregate route is installed")
	}
}

type v6PrefixRecord struct {
	prefix    netip.Prefix
	preferred time.Time
	valid     time.Time
}

// notePrefixes records the held prefixes for the health entry and flags one another endpoint of the network holds.
func (m *dhcpManager) notePrefixes(prefixes []dhcp.V6Addr, now time.Time) {
	recs := make([]v6PrefixRecord, 0, len(prefixes))
	for _, p := range prefixes {
		pfx, err := netip.ParsePrefix(p.IP)
		if err != nil {
			continue
		}
		r := v6PrefixRecord{prefix: pfx.Masked()}
		if p.ValidSeconds > 0 {
			r.valid = now.Add(time.Duration(p.ValidSeconds) * time.Second)
		}
		if p.PreferredSeconds > 0 {
			r.preferred = now.Add(time.Duration(p.PreferredSeconds) * time.Second)
		} else if p.Deprecated {
			r.preferred = now
		}
		recs = append(recs, r)
	}
	other := m.overlappingEndpoint(recs)
	if m.plugin != nil {
		names := make([]string, 0, len(recs))
		for _, r := range recs {
			names = append(names, r.prefix.String())
		}
		m.plugin.updateEndpointPrefixes(m.joinReq.EndpointID, names)
	}
	m.ipMu.Lock()
	m.delegated = recs
	was := m.prefixOverlap
	m.prefixOverlap = other != ""
	m.ipMu.Unlock()
	if other != "" && !was {
		// Kept, never declined: RFC 8415 section 18.2.8 declines addresses, and a prefix is not one (#214).
		if m.plugin != nil {
			m.plugin.ipv6PrefixOverlaps.Add(1)
		}
		log.WithFields(m.logFields(true)).WithField("other_endpoint", other).
			Warn("The delegated prefix overlaps one another endpoint of this network holds; both keep it, " +
				"and the DHCPv6 server's pool configuration is the place to look")
	}
}

// overlappingEndpoint names another endpoint of the same network whose held prefix overlaps one of recs.
func (m *dhcpManager) overlappingEndpoint(recs []v6PrefixRecord) string {
	if m.plugin == nil || len(recs) == 0 {
		return ""
	}
	managers, _ := m.plugin.managerSnapshot()
	for _, o := range managers {
		if o == m || o.joinReq.NetworkID != m.joinReq.NetworkID {
			continue
		}
		for _, theirs := range o.heldPrefixes() {
			for _, mine := range recs {
				if mine.prefix.Overlaps(theirs.prefix) {
					return shortID(o.joinReq.EndpointID)
				}
			}
		}
	}
	return ""
}

func (m *dhcpManager) heldPrefixes() []v6PrefixRecord {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	return append([]v6PrefixRecord(nil), m.delegated...)
}

// DelegatedPrefixHealth is one IA_PD prefix of an endpoint's DHCPv6 lease on /Plugin.Health (#214).
type DelegatedPrefixHealth struct {
	Prefix         string `json:"prefix"`
	PreferredUntil string `json:"preferred_until,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
}

// prefixHealth is the held prefixes still valid at now, and whether one overlaps another endpoint's.
func (m *dhcpManager) prefixHealth(now time.Time) ([]DelegatedPrefixHealth, bool) {
	m.ipMu.Lock()
	defer m.ipMu.Unlock()
	var out []DelegatedPrefixHealth
	for _, r := range m.delegated {
		if !r.valid.IsZero() && !r.valid.After(now) {
			continue
		}
		h := DelegatedPrefixHealth{Prefix: r.prefix.String()}
		if !r.preferred.IsZero() {
			h.PreferredUntil = r.preferred.UTC().Format(time.RFC3339)
		}
		if !r.valid.IsZero() {
			h.ExpiresAt = r.valid.UTC().Format(time.RFC3339)
		}
		out = append(out, h)
	}
	return out, m.prefixOverlap
}

// dropPrefixRoutes empties the aggregates; only a loss forgets the prefixes, a Leave keeps them for the tombstone (#214).
func (m *dhcpManager) dropPrefixRoutes(v6 bool, why string) {
	if !v6 {
		return
	}
	if why != "leave" {
		m.notePrefixes(nil, time.Now())
	}
	if err := m.withdrawPrefixRoutes(); err != nil {
		log.WithError(err).WithFields(m.logFields(true)).WithField("cause", why).
			Warn("Failed to remove the delegated prefix routes from the container")
	}
}
