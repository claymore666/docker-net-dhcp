// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"net/netip"
	"syscall"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/runtime"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// The values of the `release_lease` network option (#962); off by default, as a host does not release at power-off
// (#800).
const (
	// ReleaseNever is the default: no path sends a DHCPRELEASE or a
	// DHCPv6 Release, which is exactly the v1.9.0 behaviour (#800).
	ReleaseNever = "never"
	// ReleaseOnStop releases when the endpoint leaves its sandbox: every `docker stop`, every `docker rm` of a
	// running container and every `docker network disconnect` (#962).
	ReleaseOnStop = "on_stop"
	// ReleaseOnRemove releases at the record's tombstone deadline, because libnetwork calls DeleteEndpoint at stop and
	// never at `docker rm` of a stopped container (#984).
	ReleaseOnRemove = "on_remove"
)

// parseReleaseLease normalises the option and refuses unknown values at CreateNetwork (#962).
func parseReleaseLease(v string) (string, error) {
	switch v {
	case "", ReleaseNever:
		return ReleaseNever, nil
	case ReleaseOnStop:
		return ReleaseOnStop, nil
	case ReleaseOnRemove:
		return ReleaseOnRemove, nil
	default:
		return "", fmt.Errorf("%w: release_lease %q is not one of %s, %s, %s",
			util.ErrIPAM, v, ReleaseNever, ReleaseOnStop, ReleaseOnRemove)
	}
}

func (o DHCPNetworkOptions) releasesOnStop() bool {
	return o.ReleaseLease == ReleaseOnStop
}

func (o DHCPNetworkOptions) releasesOnRemove() bool {
	return o.ReleaseLease == ReleaseOnRemove
}

func (m *dhcpManager) releasedAny() bool {
	return m.releasedV4.Load() || m.releasedV6.Load()
}

// withdrawV6Address removes the address before a Release, as RFC 9915 section 18.2.7 requires; a missing one is
// absent already.
func (m *dhcpManager) withdrawV6Address() error {
	_, last := m.lastIPs()
	if last == nil || last.IPNet == nil || last.IP == nil {
		return nil
	}
	if m.netHandle == nil || m.ctrLink == nil {
		return nil
	}
	if err := nlAddrDel(m.netHandle, m.ctrLink, last); err != nil {
		if errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.ENODEV) {
			return nil
		}
		return fmt.Errorf("failed to remove the DHCPv6 address %v before releasing it: %w", last, err)
	}
	return nil
}

// releaseOutcome names why a family's release did or did not leave the host; the counters do not split on it (#962).
type releaseOutcome string

const (
	releaseSent      releaseOutcome = "sent"
	releaseNoRecord  releaseOutcome = "no_record"
	releaseNoAddress releaseOutcome = "no_address"
	// releaseNoServer: no option 54 or server DUID, and RFC 2131 section 4.4.4 unicasts a DHCPRELEASE with no
	// broadcast fallback.
	releaseNoServer   releaseOutcome = "no_server"
	releaseBadRecord  releaseOutcome = "bad_record"
	releaseNoSource   releaseOutcome = "no_source"
	releaseSendFailed releaseOutcome = "send_failed"
	// releaseWithdrawFailed: the address stayed on the link, so RFC 9915 section 18.2.7 forbids the exchange.
	releaseWithdrawFailed releaseOutcome = "withdraw_failed"
)

// releaseHeldLease builds the release from the durable record, which holds the used address, identity, chaddr and
// server even when no client attached (#962). Measured against dnsmasq 2.91: it matches v4 on the client-identifier,
// else chaddr, and v6 on DUID and IAID, never the source address, so Identity is replayed as sent. The v6 address
// comes off the link first (RFC 9915 section 18.2.7); a DHCPRELEASE is unicast (RFC 2131 section 4.4.4) and names
// the lease by ciaddr (section 3.1), so the v4 address stays.
func (m *dhcpManager) releaseHeldLease(v6 bool) releaseOutcome {
	rec, ok := m.releaseRecord(v6)
	if !ok {
		return releaseNoRecord
	}

	if v6 {
		if err := m.withdrawV6Address(); err != nil {
			log.WithError(err).WithFields(m.logFields(true)).
				Warn("Not releasing the DHCPv6 lease: the address could not be taken off the link first, " +
					"and RFC 9915 section 18.2.7 requires that before the exchange begins")
			return releaseWithdrawFailed
		}
	}

	held, _ := m.releasedAddr(v6)
	return releaseFromRecord(rec, m.opts, v6, held, m.logFields(v6))
}

// releaseFromRecord needs only the record, the options and the family, which is all the deferred release holds (#984).
func releaseFromRecord(rec lease.Record, opts DHCPNetworkOptions, v6 bool, held netip.Addr, fields log.Fields) releaseOutcome {
	src, iface, err := hostSourceFor(opts, v6, held)
	if err != nil {
		log.WithError(err).WithFields(fields).
			Warn("Not releasing: no address on the parent for this family to send the release from")
		return releaseNoSource
	}

	if err := rtSendRelease(rec, runtime.ReleaseConfig{Interface: iface, Source: src}); err != nil {
		log.WithError(err).WithFields(fields).
			WithField("source", src.String()).
			Debug("The release was refused or could not be sent")
		return classifyReleaseError(err)
	}
	return releaseSent
}

var rtSendRelease = runtime.SendRelease

// releaseRecord falls back to the (scope, MAC) index when an attach cancelled before setupClient left no id (#966),
// refusing a retained record, which the next CreateEndpoint inherits (#820), and a closed one.
func (m *dhcpManager) releaseRecord(v6 bool) (lease.Record, bool) {
	if m.plugin == nil || m.plugin.records == nil {
		return lease.Record{}, false
	}
	rb, err := m.plugin.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithFields(m.logFields(v6)).
			Warn("Could not read the lease records back, so this endpoint's lease cannot be released")
		return lease.Record{}, false
	}

	if id := m.recordID; !v6 && id != "" {
		return rb.ByID(id)
	}
	if id := m.recordID6; v6 && id != "" {
		return rb.ByID(id)
	}

	key := m.recordKey()
	if len(key) == 0 {
		return lease.Record{}, false
	}
	scope := m.joinReq.NetworkID
	if v6 {
		scope = dhcp.Scope6(scope)
	}
	matches := rb.ByScopeMAC(scope, key)
	if len(matches) == 0 {
		return lease.Record{}, false
	}
	rec := matches[len(matches)-1]
	if rec.Phase == lease.PhaseRetained || rec.Phase == lease.PhaseClosed {
		return lease.Record{}, false
	}
	m.adoptRecordID(v6, rec.ID)
	return rec, true
}

func (m *dhcpManager) adoptRecordID(v6 bool, id string) {
	if v6 {
		m.recordID6 = id
		return
	}
	m.recordID = id
}

func classifyReleaseError(err error) releaseOutcome {
	switch {
	case errors.Is(err, lease.ErrReleaseNoAddr):
		return releaseNoAddress
	case errors.Is(err, lease.ErrReleaseNoServer):
		return releaseNoServer
	case errors.Is(err, lease.ErrReleaseFamily),
		errors.Is(err, lease.ErrReleaseNoIdentity),
		errors.Is(err, lease.ErrReleaseIAIDMismatch):
		return releaseBadRecord
	case errors.Is(err, runtime.ErrReleaseNoSource),
		errors.Is(err, runtime.ErrReleaseSourceFamily),
		errors.Is(err, runtime.ErrReleaseNoInterface),
		errors.Is(err, runtime.ErrReleaseSourceIsReleased):
		return releaseNoSource
	default:
		return releaseSendFailed
	}
}

func (m *dhcpManager) announceRelease(v6 bool, out releaseOutcome) {
	announceReleaseOutcome(log.WithFields(m.logFields(v6)).WithField("outcome", string(out)), out)
}

// announceReleaseOutcome takes the entry, since the deferred release has no manager (#984).
func announceReleaseOutcome(entry *log.Entry, out releaseOutcome) {
	switch out {
	case releaseSent:
		entry.Debug("The lease was handed back before the client stopped")
	case releaseNoAddress:
		entry.Debug("No lease was held for this family, so there was nothing to hand back")
	case releaseNoRecord:
		entry.Warn("This endpoint has no lease record for this family, so no release could be built " +
			"and the address, if there is one, is left to expire on the server")
	case releaseNoServer:
		entry.Warn("The lease record names no server, so there is nowhere to unicast the release " +
			"and the address is left to expire on the server")
	case releaseBadRecord:
		entry.Warn("The lease record's own fields disagree, so no release was built. This is a " +
			"plugin defect: report it with the record id and the plugin version")
	case releaseNoSource:
		entry.Warn("No address on the parent to send the release from, so nothing was sent and the " +
			"address is left to expire on the server. A v6 release needs a link-local address on " +
			"the parent; a parent with IPv6 disabled has none")
	case releaseSendFailed:
		entry.Warn("The release was built and did not leave the host, so the address is left to " +
			"expire on the server")
	case releaseWithdrawFailed:
		entry.Warn("The DHCPv6 address could not be taken off the link, and RFC 9915 section " +
			"18.2.7 requires that before the exchange begins, so nothing was sent and the " +
			"address is left to expire on the server")
	}
}

func (m *dhcpManager) releaseFamily(v6 bool) bool {
	out := m.releaseHeldLease(v6)
	sent := out == releaseSent
	m.countRelease(v6, sent)
	m.announceRelease(v6, out)
	return sent
}

// releaseHeldLeases reports whether any family released, since one tombstone carries both addresses (#962).
func (m *dhcpManager) releaseHeldLeases() (releasedV4, releasedV6 bool) {
	releasedV4 = m.releaseFamily(false)
	if m.opts.ipv6Enabled() {
		releasedV6 = m.releaseFamily(true)
	}

	log.WithFields(m.logFields(false)).
		WithField("released_v4", releasedV4).
		WithField("released_v6", releasedV6).
		Info("release_lease=on_stop: handing this endpoint's lease back before the client stops")
	return releasedV4, releasedV6
}

// announceDeferredRelease logs at the stop on an `on_remove` network when the address will go back (#984).
func (m *dhcpManager) announceDeferredRelease() {
	entry := log.WithFields(m.logFields(false)).WithField("window", tombstoneTTL.String())
	if v4, v6 := m.lastIPs(); v4 != nil && v4.IP != nil {
		entry = entry.WithField("ip", v4.IP.String())
		if v6 != nil && v6.IP != nil {
			entry = entry.WithField("ipv6", v6.IP.String())
		}
	}
	entry.Info("release_lease=on_remove: keeping this endpoint's addresses for the restart window; " +
		"they go back to the server at the record's deadline unless a container claims them back first")
}

func (m *dhcpManager) countRelease(v6 bool, sent bool) {
	if m.plugin == nil {
		return
	}
	m.plugin.countRelease(v6, sent)
}

// countRelease sits on the plugin because the deferred release moves the same counters with no manager (#984).
func (p *Plugin) countRelease(v6 bool, sent bool) {
	if sent {
		bumpFamily(&p.releasesSentV4, &p.releasesSentV6, v6)
		return
	}
	bumpFamily(&p.releaseFailuresV4, &p.releaseFailuresV6, v6)
}

func (o DHCPNetworkOptions) hostLink() string {
	if o.effectiveMode() == ModeBridge {
		return o.Bridge
	}
	return o.linkParent()
}

var errNoHostSource = errors.New("no usable source address on the parent for this family")

func (m *dhcpManager) hostSourceFor(v6 bool) (netip.Addr, string, error) {
	held, _ := m.releasedAddr(v6)
	return hostSourceFor(m.opts, v6, held)
}

// hostSourceFor picks the release source (#962): for v4 any non-link-local parent address (RFC 3927) that is not the
// released one, which rides in ciaddr (RFC 2131 section 3.1); for v6 a parent link-local, since the destination
// ff02::1:2 is link-scoped (RFC 9915 section 7.2) and the released address may not be the source (section 18.2.7).
// Several candidates: the smallest in byte order, as netlink's order changes when one is added. None refuses,
// commonly a parent with disable_ipv6=1.
func hostSourceFor(opts DHCPNetworkOptions, v6 bool, held netip.Addr) (netip.Addr, string, error) {
	name := opts.hostLink()
	if name == "" {
		return netip.Addr{}, "", fmt.Errorf("%w: this network names no parent interface", errNoHostSource)
	}
	link, err := nlLinkByName(name)
	if err != nil {
		return netip.Addr{}, "", fmt.Errorf("%w: parent %q: %w", errNoHostSource, name, err)
	}
	family := unix.AF_INET
	if v6 {
		family = unix.AF_INET6
	}
	addrs, err := util.DumpResult(nlAddrList(link, family))
	if err != nil {
		return netip.Addr{}, "", fmt.Errorf("%w: parent %q: %w", errNoHostSource, name, err)
	}

	var best netip.Addr
	for _, a := range addrs {
		if a.IP == nil {
			continue
		}
		cand, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		cand = cand.Unmap()
		if cand.Is4() == v6 || !cand.IsValid() {
			continue
		}
		if v6 != cand.IsLinkLocalUnicast() {
			continue
		}
		if held.IsValid() && cand == held {
			continue
		}
		if !best.IsValid() || cand.Less(best) {
			best = cand
		}
	}
	if !best.IsValid() {
		return netip.Addr{}, "", fmt.Errorf("%w: parent %q carries none", errNoHostSource, name)
	}
	return best, name, nil
}

func (m *dhcpManager) releasedAddr(v6 bool) (netip.Addr, bool) {
	v4a, v6a := m.lastIPs()
	a := v4a
	if v6 {
		a = v6a
	}
	if a == nil || a.IP == nil {
		return netip.Addr{}, false
	}
	addr, ok := netip.AddrFromSlice(a.IP)
	if !ok {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}
