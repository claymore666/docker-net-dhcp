// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"crypto/rand"
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
func (p *Plugin) probeAddressConflict(ctx context.Context, parent string, ip net.IP, ourMAC net.HardwareAddr) (net.HardwareAddr, error) {
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
	// Link-local source on purpose: any address borrowed from the
	// operator's subnet might be the next one their server hands out,
	// which is the hazard this function exists to detect.
	src, err := newProbeLinkLocal()
	if err != nil {
		return nil, fmt.Errorf("probe source address: %w", err)
	}
	if err := netlink.AddrAdd(parentLink, src); err != nil {
		return nil, fmt.Errorf("add probe source to %s: %w", parent, err)
	}
	defer func() {
		if err := netlink.AddrDel(parentLink, src); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"parent": parent, "addr": src.IPNet.String(),
			}).Warn("[conflict-probe] could not remove the temporary probe address; remove it with `ip addr del`")
		}
	}()

	route := &netlink.Route{
		LinkIndex: idx,
		Dst:       &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)},
		Scope:     netlink.SCOPE_LINK,
		Src:       src.IP,
	}
	if err := netlink.RouteAdd(route); err != nil {
		return nil, fmt.Errorf("route %s via %s: %w", ip, parent, err)
	}
	defer func() {
		if err := netlink.RouteDel(route); err != nil {
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
	dialer := net.Dialer{Timeout: conflictProbeBudget, LocalAddr: &net.UDPAddr{IP: src.IP}}
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
	return nil, nil
}

// newProbeLinkLocal returns a random 169.254.0.0/16 address for the
// probe link. Random because two probes can run concurrently on one
// host and must not collide; link-local because any address from the
// operator's own subnet might be the next one their DHCP server hands
// out, which is precisely the fault this file detects.
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
		Mask: net.CIDRMask(16, 32),
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
	ip, _, err := net.ParseCIDR(cidr)
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

	foreign, err := p.probeAddressConflict(ctx, parent, ip, ourMAC)
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
