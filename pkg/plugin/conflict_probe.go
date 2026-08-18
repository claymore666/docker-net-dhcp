// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// conflictProbeBudget caps how long we wait for the kernel to resolve
// the leased address on the parent link. Sized against what the
// resolution actually costs: one ARP request and its reply on the local
// segment, typically well under 100ms. 2s is generous enough for a
// loaded host and a retransmit, and it is never on CreateEndpoint's
// critical path — the probe runs asynchronously after the lease.
//
// The budget is a cap, not a wait. A held address answers immediately;
// only a free address pays the full duration, and nothing is waiting on
// that answer.
const conflictProbeBudget = 2 * time.Second

// conflictProbePoll is how often the neighbour table is re-read while
// waiting. The kernel populates the entry asynchronously once the reply
// arrives, so this is a poll rather than a subscription — a netlink
// neighbour subscription would be tidier but costs a goroutine per
// probe for an answer that arrives in one round trip.
const conflictProbePoll = 50 * time.Millisecond

// resolvedStates are the neighbour states in which HardwareAddr is a
// real answer from the wire. NUD_INCOMPLETE and NUD_FAILED carry no
// address (or a stale one) and mean "nobody replied".
const resolvedStates = netlink.NUD_REACHABLE | netlink.NUD_STALE |
	netlink.NUD_DELAY | netlink.NUD_PROBE | netlink.NUD_PERMANENT

// probeAddressConflict asks whether some *other* device on the segment
// already holds ip, by resolving it on the parent link and comparing
// the answering MAC against ours.
//
// It returns the foreign MAC when one answers, nil when the address is
// unclaimed, and an error when the question could not be asked at all
// — which is not the same as "no conflict" and must not be reported as
// one. See #524: the whole failure being fixed here is a check that
// silently did not happen.
//
// # Why the address is resolved by sending, not by asking netlink
//
// The tempting implementation is pure netlink: insert the neighbour in
// NUD_INCOMPLETE and let the kernel resolve it. That call succeeds, the
// kernel does not probe, and the entry stays INCOMPLETE forever — so
// the probe reports "nobody holds it" while a squatter is sitting on
// the address. Measured on a veth pair against a live squatter: the
// netlink-only trigger returned unresolved, the datagram below returned
// the squatter's exact MAC in NUD_REACHABLE. A false negative here is
// indistinguishable from the bug we are fixing, so the datagram stays.
//
// The datagram also keeps the plugin inside its declared privileges. An
// RFC 5227 ARP probe would need AF_PACKET and therefore CAP_NET_RAW,
// which config.json does not grant — adding it would force every
// operator to re-approve the plugin's privileges on upgrade. Ordinary
// traffic makes the kernel do the ARP for us with CAP_NET_ADMIN alone.
//
// It is sent to the discard port and its delivery is irrelevant: an
// ICMP unreachable, a closed port, or nothing at all are equally fine.
// The packet exists to make the kernel resolve L2, not to be received.
func (p *Plugin) probeAddressConflict(ctx context.Context, parent string, ip net.IP, subnet *net.IPNet, ourMAC net.HardwareAddr) (net.HardwareAddr, error) {
	parentLink, err := nlLinkByName(parent)
	if err != nil {
		return nil, fmt.Errorf("parent link %q: %w", parent, err)
	}
	idx := parentLink.Attrs().Index

	// Probing from the PARENT is not a convenience — it is the only
	// vantage point where an answer means anything, and both
	// alternatives were tried and measured (#524, #528).
	//
	// Our own endpoint holds the leased address too; that is the
	// premise. macvlan isolates a parent from its own children, so from
	// here our endpoint cannot answer and any reply is somebody else's.
	// A throwaway bridge-mode child instead reaches its siblings — and
	// our endpoint answered first, MAC matched ours, verdict "not a
	// conflict" with a live squatter on the address.
	//
	// The cost of that isolation is that a same-host sibling squatter is
	// invisible. That is excluded by construction, not pending work; see
	// the closed #528.

	// Egress control, which the first version of this function lacked.
	// An ordinary datagram is routed by the HOST table, so without these
	// two the packet can leave by an entirely different interface and
	// the neighbour entry lands where we are not looking — measured as a
	// squatted address reported CLEAN on a parent that had no address of
	// its own.
	//
	// WHICH source address is not a detail: it decides whether the
	// question is answerable at all. See pickProbeSource.
	src, err := pickProbeSource(parentLink, ip, subnet)
	if err != nil {
		return nil, fmt.Errorf("probe source address: %w", err)
	}
	if src.borrowed {
		if err := netlink.AddrAdd(parentLink, src.addr); err != nil {
			return nil, fmt.Errorf("add probe source to %s: %w", parent, err)
		}
		defer func() {
			if err := netlink.AddrDel(parentLink, src.addr); err != nil {
				log.WithError(err).WithFields(log.Fields{
					"parent": parent, "addr": src.addr.IPNet.String(),
				}).Warn("[conflict-probe] could not remove the temporary probe address; remove it with `ip addr del`")
			}
		}()
	}

	route := &netlink.Route{
		LinkIndex: idx,
		Dst:       &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)},
		Scope:     netlink.SCOPE_LINK,
		Src:       src.addr.IP,
	}
	if err := p.addProbeRoute(route, parent, ip); err != nil {
		return nil, err
	}
	defer func() {
		err := netlink.RouteDel(route)
		switch {
		case err == nil:
		case errors.Is(err, unix.ESRCH):
			// Already gone. The postcondition — no probe route left on
			// the parent — is met, so this is not a cleanup failure and
			// must not read as one: #574 exists because a leftover route
			// silently disables detection for an address, and a warning
			// that is usually noise is a warning nobody reads when it
			// is not (#575). The one legitimate way here is two probes
			// for the same address overlapping — an endpoint restarting
			// onto the address it had — where addProbeRoute's reclaim
			// makes both own one route and the first to finish removes
			// it. Info, so the overlap is visible without alarming.
			log.WithFields(log.Fields{
				"parent": parent, "dst": ip.String(),
			}).Info("[conflict-probe] temporary probe route was already removed")
		default:
			log.WithError(err).WithFields(log.Fields{
				"parent": parent, "dst": ip.String(),
			}).Warn("[conflict-probe] could not remove the temporary probe route; remove it with `ip route del`")
		}
	}()

	// Drop any cached answer so the reply is from this probe.
	_ = netlink.NeighDel(&netlink.Neigh{LinkIndex: idx, IP: ip})

	// Trigger resolution with a datagram. netlink cannot do it:
	// inserting the neighbour in NUD_INCOMPLETE succeeds, the kernel
	// never probes, and the entry stays INCOMPLETE — a clean verdict
	// over a squatted address. The datagram goes to the discard port and
	// its delivery is irrelevant; it exists to make the kernel ARP.
	dialer := net.Dialer{Timeout: conflictProbeBudget, LocalAddr: &net.UDPAddr{IP: src.addr.IP}}
	conn, dialErr := dialer.DialContext(ctx, "udp", net.JoinHostPort(ip.String(), "9"))
	if conn != nil {
		_, _ = conn.Write([]byte{0})
		_ = conn.Close()
	}

	deadline := time.Now().Add(conflictProbeBudget)
	sawEntry := false
	for {
		neighs, err := netlink.NeighList(idx, unix.AF_INET)
		if err != nil {
			return nil, fmt.Errorf("read neighbour table for %s: %w", parent, err)
		}
		for _, n := range neighs {
			if !n.IP.Equal(ip) {
				continue
			}
			sawEntry = true
			if n.HardwareAddr != nil && n.State&resolvedStates != 0 {
				if macsEqual(n.HardwareAddr, ourMAC) {
					// Should be unreachable from the parent in
					// macvlan/ipvlan, and is the normal case over a
					// bridge, where the host can reach the container.
					return nil, nil
				}
				return n.HardwareAddr, nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(conflictProbePoll):
		}
	}

	if !sawEntry && dialErr != nil {
		return nil, fmt.Errorf("could not reach %s via %s to probe it: %w", ip, parent, dialErr)
	}

	// Silence. What that is worth depends entirely on the source address
	// we had to send from.
	//
	// From an on-subnet source, silence is an answer: every host that
	// holds the address would have replied, so nobody holds it.
	//
	// From the borrowed link-local source it is NOT an answer, and
	// reporting it as one is the bug this file exists to prevent. A
	// responder only replies to an ARP request whose sender it can route
	// back to, so a host with neither a default route nor a link-local
	// route stays silent while holding the address. Measured on 6.12,
	// two namespaces over a veth pair, squatter on 192.168.101.42:
	//
	//   responder routes      link-local sender   on-subnet sender
	//   none                  INCOMPLETE          answered
	//   link-local route      answered            -
	//   default route         answered            -
	//
	// So the fallback is a best-effort: a reply still proves a conflict,
	// but silence proves nothing, and it is counted as a probe that
	// could not run rather than as a clean segment (#524).
	if !src.onSubnet {
		return nil, fmt.Errorf(
			"nothing answered for %s, but the probe had to be sent from %s: %s has no address "+
				"on the leased subnet, and a host only answers an ARP request whose sender it can "+
				"route back to. A squatter with no default route is silent here, so this is "+
				"undetermined rather than clean. Give %s an address on the segment to enable "+
				"conflict detection",
			ip, src.addr.IP, parent, parent)
	}
	return nil, nil
}

// probeSource is the address a probe sends from, and whether an
// unanswered probe from it means anything.
type probeSource struct {
	addr *netlink.Addr
	// onSubnet is true when the source sits inside the leased subnet, so
	// any host holding the address can route a reply back to it and
	// silence is therefore a real verdict.
	onSubnet bool
	// borrowed is true when we added the address ourselves and must
	// remove it again.
	borrowed bool
}

// pickProbeSource chooses the address the conflict probe sends from.
//
// An address the parent ALREADY holds on the leased subnet is the right
// answer wherever one exists: it is on-subnet so every responder can
// reply to it, it is the address the host already announces for its own
// traffic, and using it mutates nothing.
//
// That covers the normal deployment — a macvlan or ipvlan parent is the
// host NIC and carries the host's LAN address, and a bridge parent
// always has one. A parent deliberately left bare (a NIC dedicated to
// being a macvlan parent) has no such address, and there is no address
// on the operator's subnet we may invent: the next one their DHCP
// server hands out is exactly the hazard this file detects. So that
// case falls back to a random link-local source, whose answers are
// trustworthy and whose silences are not — see the caller.
// addProbeRoute installs the probe's temporary /32, reclaiming a
// leftover one if the previous owner never got to remove it (#572).
//
// # Why a leftover is possible at all
//
// The probe runs in a detached goroutine with its own background
// context, so nothing waits for it. If the process goes away inside the
// probe's window — a daemon restart, a `docker plugin disable`, an
// upgrade — the deferred RouteDel never runs and a /32 for that address
// is left on the parent. The next probe for the same address then gets
// EEXIST from RouteAdd, reports "probe could not run", and the address
// is never checked. Intermittent, because it needs the process to die
// inside a roughly two-second window, and invisible in the ordinary
// case because the counter it increments is not healthy-affecting.
//
// # Why reclaiming is safe
//
// EEXIST is keyed on the destination, so the route in the way is for
// THIS address. Two probes for one address cannot both be legitimate:
// the address belongs to a single endpoint at a time, and a genuine
// overlap (an endpoint restarting onto the same address) wants exactly
// the route we are about to install anyway.
//
// So it is replaced rather than deleted-and-re-added. RouteReplace is
// atomic, which matters precisely in that overlap case: a del/add pair
// leaves a window with no route at all, and a concurrent probe's ARP
// would leave by whatever the host table says instead — which is the
// misrouting #524 was about. Replacing an identical route is a no-op
// for anyone else using it.
//
// The reclaim is counted, not silent. A rising count means the plugin
// is being stopped inside probe windows, which is worth seeing even
// though each individual instance is now recovered.
func (p *Plugin) addProbeRoute(route *netlink.Route, parent string, ip net.IP) error {
	err := netlink.RouteAdd(route)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("route %s via %s: %w", ip, parent, err)
	}

	p.conflictProbeStaleRoutes.Add(1)
	log.WithFields(log.Fields{
		"parent": parent, "dst": ip.String(),
	}).Info("[conflict-probe] reclaiming a leftover probe route; a previous probe was cut short before it could clean up")

	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("reclaim leftover route %s via %s: %w", ip, parent, err)
	}
	return nil
}

func pickProbeSource(parentLink netlink.Link, target net.IP, subnet *net.IPNet) (probeSource, error) {
	if subnet != nil {
		addrs, err := nlAddrList(parentLink, unix.AF_INET)
		if err != nil {
			return probeSource{}, fmt.Errorf("list addresses on %s: %w", parentLink.Attrs().Name, err)
		}
		for i := range addrs {
			if addrs[i].IP == nil || !subnet.Contains(addrs[i].IP) {
				continue
			}
			// The parent holding the address we are asking about is
			// itself the conflict — the host is the squatter. Sending
			// from it would ask the address about itself and resolve
			// locally, so skip it and let a different source, or the
			// fallback, put the question on the wire.
			if addrs[i].IP.Equal(target) {
				continue
			}
			return probeSource{addr: &addrs[i], onSubnet: true}, nil
		}
	}

	ll, err := newProbeLinkLocal()
	if err != nil {
		return probeSource{}, err
	}
	return probeSource{addr: ll, borrowed: true}, nil
}

// newProbeLinkLocal returns a random 169.254.x.y/32 address for the
// probe link. Random because two probes can run concurrently on one
// host and must not collide; link-local because any address from the
// operator's own subnet might be the next one their DHCP server hands
// out, which is precisely the fault this file detects.
//
// The prefix is /32, and that is load-bearing (#575). With a /16 every
// borrowed source on one parent shares a subnet, so the kernel makes
// the second one a SECONDARY of the first — and unless
// promote_secondaries is on, deleting the primary deletes every
// secondary with it and flushes the routes sourced from them. Two
// probes overlapping on an address-less parent then did exactly that
// to each other: the first to finish took the second's source address
// and its /32 route away mid-probe, and the second's cleanup found
// nothing to delete (ESRCH on the route, EADDRNOTAVAIL on the
// address). Reproduced in a fresh network namespace, where the kernel
// default is promote_secondaries=0 — which is what a nested-Docker
// runner is, and why it was only ever seen there. A /32 has no subnet
// to be secondary in, so no probe's cleanup can reach another's; the
// probe's own explicit /32 route to the target supplies the egress the
// connected /16 used to.
//
// .0 and .255 in the last octet are avoided, as are the first and last
// /24 of the range, which RFC 3927 reserves.
func newProbeLinkLocal() (*netlink.Addr, error) {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	third := 1 + int(b[0])%254  // 1..254
	fourth := 1 + int(b[1])%254 // 1..254
	return &netlink.Addr{IPNet: &net.IPNet{
		IP:   net.IPv4(169, 254, byte(third), byte(fourth)),
		Mask: net.CIDRMask(32, 32),
	}}, nil
}

// macsEqual compares two hardware addresses. net.HardwareAddr has no
// Equal method and a bytes.Equal on a nil operand would silently call
// two unknown MACs identical, which in this file would mean reporting
// a conflicting device as our own.
func macsEqual(a, b net.HardwareAddr) bool {
	if a == nil || b == nil {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkAddressConflict runs the probe for a freshly-leased endpoint and
// records the outcome. It is called asynchronously: nothing about
// CreateEndpoint's result depends on it, which is what lets the `-A`
// acquisition-latency argument stand unchanged (#524).
//
// The probe runs on the parent link, which lives in the host namespace
// and outlives the endpoint being moved into the container's netns, so
// running late cannot race Join.
func (p *Plugin) checkAddressConflict(parent, cidr, mac, endpointID, networkID string) {
	if parent == "" || cidr == "" {
		return
	}
	// The subnet is load-bearing, not decoration: it is what lets the
	// probe find an on-subnet source address on the parent, and an
	// on-subnet source is what makes silence mean something. A bare
	// address with no mask therefore probes in the degraded mode.
	ip, subnet, err := net.ParseCIDR(cidr)
	if err != nil {
		if ip = net.ParseIP(cidr); ip == nil {
			log.WithFields(log.Fields{
				"endpoint": shortID(endpointID),
				"address":  cidr,
			}).Warn("[conflict-probe] cannot parse leased address; address conflict not checked")
			p.conflictProbeFailures.Add(1)
			return
		}
	}
	ourMAC, err := net.ParseMAC(mac)
	if err != nil {
		// Without our own MAC there is nothing to compare against, and
		// a probe that cannot tell us from a squatter is worse than no
		// probe: in bridge mode it would report every endpoint as a
		// conflict.
		log.WithFields(log.Fields{
			"endpoint":    shortID(endpointID),
			"mac_address": mac,
		}).Warn("[conflict-probe] cannot parse endpoint MAC; address conflict not checked")
		p.conflictProbeFailures.Add(1)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), conflictProbeBudget+time.Second)
	defer cancel()

	foreign, err := p.probeAddressConflict(ctx, parent, ip, subnet, ourMAC)
	if err != nil {
		// A probe that could not run is counted separately and does not
		// mark the plugin unhealthy — it is an unanswered question, not
		// a known-broken address. It is counted at all so that the
		// detector cannot quietly stop working, which is precisely how
		// #524 stayed invisible.
		log.WithFields(log.Fields{
			"network":  shortID(networkID),
			"endpoint": shortID(endpointID),
			"parent":   parent,
			"address":  ip,
			"error":    err,
		}).Warn("[conflict-probe] address-conflict probe could not run")
		p.conflictProbeFailures.Add(1)
		return
	}
	// A verdict was reached — clean or not. Counted before the branch
	// so both outcomes land in it.
	p.addressConflictProbes.Add(1)

	if foreign == nil {
		log.WithFields(log.Fields{
			"endpoint": shortID(endpointID),
			"address":  ip,
		}).Debug("[conflict-probe] leased address is not held by another device")
		return
	}

	p.addressConflicts.Add(1)
	// Every fact needed to find the other device, in one line: the
	// production incident this comes from was diagnosed from exactly
	// this triple, gathered by hand.
	log.WithFields(log.Fields{
		"network":      shortID(networkID),
		"endpoint":     shortID(endpointID),
		"parent":       parent,
		"address":      ip,
		"other_mac":    foreign.String(),
		"endpoint_mac": ourMAC.String(),
	}).Error("[conflict-probe] leased address is already in use by another device on the segment; " +
		"traffic for this address will be wrong for both hosts. The DHCP server cannot see statically " +
		"configured hosts, so a static device inside the pool range will be handed out again.")
}
