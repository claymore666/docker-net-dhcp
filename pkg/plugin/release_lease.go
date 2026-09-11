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

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// The values of the `release_lease` network option (#962).
//
// RELEASE IS OFF BY DEFAULT AND THAT IS #800's RULE, NOT AN OVERSIGHT.
// A container is a host on this segment and a host does not hand its
// address back when it is switched off; the lease expires on the
// server's clock and a container that comes back before then re-claims
// it. Networks that would rather have the address back early say so.
const (
	// ReleaseNever is the default: no path sends a DHCPRELEASE or a
	// DHCPv6 Release, which is exactly the v1.9.0 behaviour (#800).
	ReleaseNever = "never"
	// ReleaseOnStop hands the lease back when the endpoint leaves its
	// sandbox, which is every `docker stop`, every `docker rm` of a
	// running container and every `docker network disconnect`. A
	// container that restarts asks for a fresh lease.
	ReleaseOnStop = "on_stop"
	// ReleaseOnRemove is #962's third value. It arrives in the next
	// change on this milestone and is refused until then. See
	// releaseOnRemoveRefusal.
	ReleaseOnRemove = "on_remove"
)

// releaseOnRemoveRefusal is what an operator gets back from `docker
// network create` until `on_remove` lands.
//
// MEASURED, and it is a fact about libnetwork rather than about this
// plugin: `DeleteEndpoint` runs when a container STOPS, not when it is
// removed. The tombstone that keeps a MAC stable across `docker
// restart` is written at `DeleteEndpoint` and consumed at the next
// `CreateEndpoint` inside a 60-second TTL (docs/reference.md, "Restart
// stability"), which is only possible if both run during the restart.
// So a release hung off `DeleteEndpoint` fires on every `docker stop` --
// that is this option's `on_stop` -- and `docker rm` of a container
// that is already stopped reaches this plugin not at all.
//
// WHICH IS WHY `on_remove` IS A DIFFERENT MECHANISM, NOT A THIRD CALL
// SITE. It is a TIMED release: the stop keeps the record, and if no
// container has claimed the address back within a TTL the plugin sends
// the release itself from the host, off the record's stored identity.
// The sender that does that is HERE, because `on_stop` needs it too:
// what `on_remove` still owes is the TTL and the timer that fires it.
// That is the next change on this milestone and not this one.
//
// Refusing it until then is the fail-closed answer. Accepting it now
// and releasing on every stop would give two names to one behaviour and
// would make the reference page false for whoever read it.
const releaseOnRemoveRefusal = "release_lease=on_remove is not available yet: " +
	"it arrives in the next change on this milestone. " +
	"Docker deletes an endpoint when its container STOPS, not when the container is removed, " +
	"so a release sent from that handler would fire on every `docker stop` (which is release_lease=on_stop) " +
	"and would never fire for `docker rm` of an already-stopped container. " +
	"Use release_lease=never or release_lease=on_stop for now. See issue #962."

// parseReleaseLease normalises and validates the `release_lease`
// option. Empty is ReleaseNever.
//
// The refusal happens once, at CreateNetwork, so that everything after
// it deals in a value with two inhabitants -- the same rule
// conflict_check follows. A typo that silently selected the default
// would be a network an operator believes releases and that never does.
func parseReleaseLease(v string) (string, error) {
	switch v {
	case "", ReleaseNever:
		return ReleaseNever, nil
	case ReleaseOnStop:
		return ReleaseOnStop, nil
	case ReleaseOnRemove:
		return "", fmt.Errorf("%w: %s", util.ErrIPAM, releaseOnRemoveRefusal)
	default:
		return "", fmt.Errorf("%w: release_lease %q is not one of %s, %s",
			util.ErrIPAM, v, ReleaseNever, ReleaseOnStop)
	}
}

// releasesOnStop reports whether this network hands its leases back
// when an endpoint leaves.
//
// A METHOD AND NOT A COMPARISON AT EACH SITE. The release decision, the
// record's phase and the tombstone skip are three consequences of one
// answer, and three separate string comparisons are three places for
// them to disagree about what the network asked for.
func (o DHCPNetworkOptions) releasesOnStop() bool {
	return o.ReleaseLease == ReleaseOnStop
}

// releasedAny reports whether either family's lease was handed back.
func (m *dhcpManager) releasedAny() bool {
	return m.releasedV4.Load() || m.releasedV6.Load()
}

// withdrawV6Address takes this endpoint's DHCPv6 address off the
// container link, which RFC 9915 section 18.2.7 requires before a
// Release exchange may begin.
//
// A missing address is not an error: the link is being torn down around
// this call and the kernel may have removed it already, and an address
// that is not on the interface satisfies the MUST by being absent.
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

// releaseOutcome is WHY a family's release did or did not leave the
// host, and it is a named value rather than a bool because the answers
// below are different operator problems.
//
// A bare false read "the address was not handed back" and said nothing
// about which of them it was. The counters do NOT split along this
// type: a release that did not happen is a release failure whatever the
// reason, and an operator reading `release_failures_v4` is asking how
// often the address was not handed back. The reason is what the log
// line is for.
//
// THE SET CHANGED WITH THE SENDER. While the release was asked of a
// running DHCP client there was a `no_client` answer, and it was the
// commonest one: a container stopped before its persistent client
// attached kept its address and was charged a failure. The release is
// now built from the durable record and sent from the host's own
// address on the parent, so there is no client to be absent and that
// answer is gone. What can go wrong instead is the record, the host's
// addressing, or the socket, which is what the four refusals below are.
type releaseOutcome string

const (
	// releaseSent is the only outcome a datagram left the host for.
	releaseSent releaseOutcome = "sent"
	// releaseNoRecord is no durable record for this endpoint and
	// family, or one that cannot be read back. Nothing to build from.
	releaseNoRecord releaseOutcome = "no_record"
	// releaseNoAddress is a record that holds no leased address, so
	// there is no binding to give back. A record that never bound is
	// the ordinary case and it is not a fault.
	releaseNoAddress releaseOutcome = "no_address"
	// releaseNoServer is a record that names no server: no option 54
	// for v4, no server DUID for v6. RFC 2131 section 4.4.4 unicasts a
	// DHCPRELEASE to the server and offers no broadcast fallback, so
	// there is nowhere to send it.
	releaseNoServer releaseOutcome = "no_server"
	// releaseBadRecord is a record whose own fields disagree: an
	// unknown family, a v6 record with no DUID and IAID, or two IAIDs
	// that do not match. It is a plugin defect rather than an operator
	// problem and it is kept apart from the three above for that
	// reason.
	releaseBadRecord releaseOutcome = "bad_record"
	// releaseNoSource is no address on the parent this release could
	// come from, in this family. See hostSourceFor.
	releaseNoSource releaseOutcome = "no_source"
	// releaseSendFailed is a datagram that was built and did not leave
	// the host.
	releaseSendFailed releaseOutcome = "send_failed"
	// releaseWithdrawFailed is the v6-only arm: the address could not
	// be taken off the container link, so RFC 9915 section 18.2.7
	// forbids beginning the exchange and nothing was built or sent.
	releaseWithdrawFailed releaseOutcome = "withdraw_failed"
)

// releaseHeldLease hands one family's lease back, and reports whether a
// datagram left the host and, when it did not, why.
//
// IT IS BUILT FROM THE RECORD AND NOT FROM A RUNNING CLIENT. The record
// is written at CreateEndpoint and the one-shot exchange writes its
// lease into it, so the record holds the address the container actually
// used, the identity as sent, the chaddr and the server. A client is a
// worse source for all four: it may never have attached, it may never
// have bound, and at teardown its namespace may already be gone. The
// address a container used is in the record either way.
//
// MEASURED against dnsmasq 2.91 before this was built, both families, on
// a veth fixture: a release sent from an address that never held the
// lease closes it, and one carrying the wrong identity does not. dnsmasq
// matches v4 on the client-identifier when the binding has one and on
// chaddr when it does not, and v6 on the DUID and IAID; the source
// address is not consulted in either family. That is a fact about
// dnsmasq and not about every server, which is why the record's
// `Identity` is replayed exactly as sent, including its absence.
//
// RFC 9915 section 18.2.7 is why the v6 arm removes the address first:
//
//	The client MUST stop using all of the leases being released
//	before the client begins the Release message exchange process.
//	For an address, this means the address MUST have been removed
//	from the interface.
//
// The second MUST in that section -- "The client MUST NOT use any of
// the addresses it is releasing as the source address in the Release
// message" -- is satisfied by construction, because the source is the
// parent's link-local and the released address is a global one on the
// container's link. The library refuses the violation anyway rather
// than trusting this comment.
//
// v4 has no such rule. RFC 2131 section 4.4.6 is the whole of what a
// DHCPRELEASE is:
//
//	If the client no longer requires use of its assigned network
//	address (e.g., the client is gracefully shut down), the client
//	sends a DHCPRELEASE message to the server.  Note that the
//	correct operation of DHCP does not depend on the transmission of
//	DHCPRELEASE messages.
//
// and section 4.4.4 is where the transmission rule lives:
//
//	The DHCP client broadcasts DHCPDISCOVER, DHCPREQUEST and
//	DHCPINFORM messages, unless the client knows the address of a
//	DHCP server.  The client unicasts DHCPRELEASE messages to the
//	server.
//
// Both are the library's to obey; the v4 address stays on the link
// because section 3.1(6) identifies the lease by 'ciaddr'.
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

	src, iface, err := m.hostSourceFor(v6)
	if err != nil {
		log.WithError(err).WithFields(m.logFields(v6)).
			Warn("Not releasing: no address on the parent for this family to send the release from")
		return releaseNoSource
	}

	if err := rtSendRelease(rec, runtime.ReleaseConfig{Interface: iface, Source: src}); err != nil {
		log.WithError(err).WithFields(m.logFields(v6)).
			WithField("source", src.String()).
			Debug("The release was refused or could not be sent")
		return classifyReleaseError(err)
	}
	return releaseSent
}

// rtSendRelease is the one call that puts a datagram on the wire, as a
// seam. Every refusal below it is reachable in a unit test only because
// this is a var: the real one opens a socket on the parent.
var rtSendRelease = runtime.SendRelease

// releaseRecord reads back the durable record this family's release is
// built from.
func (m *dhcpManager) releaseRecord(v6 bool) (lease.Record, bool) {
	id := m.recordID
	if v6 {
		id = m.recordID6
	}
	if id == "" || m.plugin == nil || m.plugin.records == nil {
		return lease.Record{}, false
	}
	rb, err := m.plugin.records.Rebuilt()
	if err != nil {
		log.WithError(err).WithFields(m.logFields(v6)).
			Warn("Could not read the lease records back, so this endpoint's lease cannot be released")
		return lease.Record{}, false
	}
	rec, found := rb.ByID(id)
	if !found {
		return lease.Record{}, false
	}
	return rec, true
}

// classifyReleaseError maps the library's typed refusals onto the
// reasons above.
//
// EVERY ONE OF THEM IS NAMED, and the default is the socket. A refusal
// folded into "send failed" would tell an operator to look at the
// network for a record that was never going to work.
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

// announceRelease is the line an operator reads when a stop did not
// hand the address back.
//
// IT IS A WARNING FOR EVERY OUTCOME BUT TWO, because on a network
// configured `release_lease=on_stop` a stop that releases nothing is
// the configuration not doing its job, and the address then sits in the
// server's pool until its own clock frees it. Each reason gets its own
// sentence: they are not variations on one problem and the fix for each
// is different.
//
// The two exceptions are the send itself and a record that holds no
// address. A record with no address is an endpoint that never got a
// lease, which is not a failure to release; there was nothing there.
func (m *dhcpManager) announceRelease(v6 bool, out releaseOutcome) {
	entry := log.WithFields(m.logFields(v6)).WithField("outcome", string(out))
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

// releaseFamily is one family's whole teardown step: ask, count, say.
//
// The three are together because they are one event seen three ways,
// and a caller that did two of them is the defect this collapses.
func (m *dhcpManager) releaseFamily(v6 bool) bool {
	out := m.releaseHeldLease(v6)
	sent := out == releaseSent
	m.countRelease(v6, sent)
	m.announceRelease(v6, out)
	return sent
}

// releaseHeldLeases is the whole of what `release_lease=on_stop` does
// at teardown, for both families, and it reports whether ANY family's
// address was handed back.
//
// THE ANY IS DELIBERATE AND IT IS WHAT THE TOMBSTONE READS. The
// tombstone is one object carrying the MAC, the v4 address and the v6
// address together; a dual-stack endpoint that released only its v4
// lease must not leave behind a tombstone whose next consumer asks for
// the address just handed back. Each family's RECORD is still settled
// on its own outcome, because a v6 lease that was not released is a v6
// lease this endpoint may still resume.
func (m *dhcpManager) releaseHeldLeases() (releasedV4, releasedV6 bool) {
	releasedV4 = m.releaseFamily(false)
	if m.opts.IPv6 {
		releasedV6 = m.releaseFamily(true)
	}

	log.WithFields(m.logFields(false)).
		WithField("released_v4", releasedV4).
		WithField("released_v6", releasedV6).
		Info("release_lease=on_stop: handing this endpoint's lease back before the client stops")
	return releasedV4, releasedV6
}

// countRelease moves the per-family pair. Sent is a packet the library
// saw leave; failed is an attempt that produced none, whatever the
// reason -- a client that held no binding, a send that failed, a
// request dropped from a full queue, or a v6 address that could not be
// taken off the link.
func (m *dhcpManager) countRelease(v6 bool, sent bool) {
	if m.plugin == nil {
		return
	}
	if sent {
		bumpFamily(&m.plugin.releasesSentV4, &m.plugin.releasesSentV6, v6)
		return
	}
	bumpFamily(&m.plugin.releaseFailuresV4, &m.plugin.releaseFailuresV6, v6)
}

// hostLink is the interface a release leaves the host by: the bridge in
// bridge mode, the parent NIC in macvlan and ipvlan mode.
//
// It is the PARENT and never the container's veth, which is the whole
// point of a release built from a record: the container's link may be
// gone, and the datagram still has to reach the segment the lease was
// granted on.
func (o DHCPNetworkOptions) hostLink() string {
	if o.effectiveMode() == ModeBridge {
		return o.Bridge
	}
	return o.Parent
}

// errNoHostSource is "this host has no address on the parent that a
// release could come from, in this family".
var errNoHostSource = errors.New("no usable source address on the parent for this family")

// hostSourceFor picks the address the release is sent FROM.
//
// THE CALLER PICKS IT AND THE LIBRARY REFUSES TO. runtime.ReleaseConfig
// has no default source, because the one default that looks obvious --
// the first address on the parent -- is the released address itself on
// a host that still carries it, which RFC 9915 section 18.2.7 forbids.
// So the choice is made here, where the mode and the released address
// are both known.
//
// THE RULE, per family:
//
//   - v4: any address on the parent that is not link-local (169.254/16,
//     RFC 3927) and not the address being released. A DHCPRELEASE
//     carries the released address in 'ciaddr' (RFC 2131 section 3.1(6))
//     and the source only has to be a return path the server could use,
//     which nothing reads because nothing answers a DHCPRELEASE.
//   - v6: a LINK-LOCAL address on the parent, and nothing else. The
//     destination is [ff02::1:2]:547, which is link-scoped multicast,
//     and RFC 9915 section 7.2 has a client send from its link-local
//     address. A global source would be refused by the library when it
//     is the released address and would be wrong even when it is not.
//
// ON A PARENT WITH SEVERAL, the smallest address in byte order wins.
// The rule is arbitrary and it is written down because the alternative
// is worse: netlink's order is the order the addresses were added, so
// "the first one" changes when an operator adds an address, and two
// releases on one parent would go out from two different sources for no
// reason a log could explain. Sorting makes the choice reproducible,
// which is what a support thread needs.
//
// ON A PARENT WITH NONE IN THAT FAMILY the release is refused and
// nothing is sent: errNoHostSource, counted as a release failure and
// warned with the parent's name. The v6 case is the one that happens in
// practice, on a parent whose IPv6 is disabled (`disable_ipv6=1`), and
// the answer is honest -- a host with no IPv6 on the segment has no way
// to tell a DHCPv6 server anything.
func (m *dhcpManager) hostSourceFor(v6 bool) (netip.Addr, string, error) {
	name := m.opts.hostLink()
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

	released, _ := m.releasedAddr(v6)
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
			// v6 takes link-local only; v4 takes anything but.
			continue
		}
		if released.IsValid() && cand == released {
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

// releasedAddr is the address this family is giving back, as the plugin
// last saw it on the link. It is used to keep the source off it.
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
