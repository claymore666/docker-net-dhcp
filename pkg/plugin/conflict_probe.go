// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
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
	link, err := nlLinkByName(parent)
	if err != nil {
		return nil, fmt.Errorf("parent link %q: %w", parent, err)
	}
	idx := link.Attrs().Index

	// Drop any cached answer so the reply is from this probe rather
	// than from whatever the host learned earlier. Best-effort: no
	// entry is the common case and not an error.
	_ = netlink.NeighDel(&netlink.Neigh{LinkIndex: idx, IP: ip})

	// Trigger resolution. Every outcome of the dial is acceptable; only
	// a total inability to route to the address tells us anything, and
	// that is reported as a probe failure rather than as a clean bill.
	dialer := net.Dialer{Timeout: conflictProbeBudget}
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
					// Our own endpoint answered. Expected in bridge
					// mode, where the host can reach the container;
					// impossible in macvlan/ipvlan, where the parent
					// cannot reach its own child. Either way it is not
					// a conflict, and this comparison is the only
					// reason the check is safe to run in all modes.
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

	// Nothing answered. If we could not even form a route to the
	// address and the kernel never created an entry, we did not ask the
	// question — say so instead of reporting the address as free.
	if !sawEntry && dialErr != nil {
		return nil, fmt.Errorf("could not reach %s via %s to probe it: %w", ip, parent, dialErr)
	}
	return nil, nil
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
