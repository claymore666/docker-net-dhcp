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

	// A throwaway macvlan child of the parent, not the parent itself.
	//
	// The parent looked like the obvious place to probe from and is the
	// wrong one, for two independent reasons — both measured, not
	// argued:
	//
	//  1. The parent cannot reach its own macvlan children, so a
	//     conflicting sibling on this host is invisible to it (#528).
	//  2. An ordinary datagram is routed by the HOST's routing table,
	//     not by the link we then read the neighbour table on. Those
	//     agree only when the parent happens to hold an address on the
	//     leased subnet. A parent NIC with no IP of its own is an
	//     ordinary deployment, and there the probe reported a squatted
	//     address as CLEAN — a live squatter, a live endpoint on its
	//     address, and probes=1 failures=0.
	//
	// A child in MACVLAN_MODE_BRIDGE can reach its siblings, and giving
	// it its own address plus a scope-link route to the target makes the
	// egress interface a property of this function rather than of how
	// the operator addressed the parent. pkg/plugin/dhcp_probe.go has
	// created and torn down exactly this kind of link for validate_dhcp
	// since v1.3.0.
	probeName, err := newProbeLinkName()
	if err != nil {
		return nil, fmt.Errorf("probe link name: %w", err)
	}
	probeMAC, err := newProbeMAC()
	if err != nil {
		return nil, fmt.Errorf("probe MAC: %w", err)
	}
	la := netlink.NewLinkAttrs()
	la.Name = probeName
	la.ParentIndex = parentLink.Attrs().Index
	la.HardwareAddr = probeMAC
	probeLink := &netlink.Macvlan{LinkAttrs: la, Mode: netlink.MACVLAN_MODE_BRIDGE}

	if err := netlink.LinkAdd(probeLink); err != nil {
		return nil, fmt.Errorf("create probe macvlan on %q: %w", parent, err)
	}
	defer func() {
		// Deleting the link takes its addresses and routes with it, so
		// this is the only cleanup needed. Best-effort: a failure here
		// leaves a link the operator can remove with `ip link del`,
		// logged rather than swallowed so names cannot leak silently.
		if err := netlink.LinkDel(probeLink); err != nil {
			log.WithError(err).WithField("link", probeName).Warn("[conflict-probe] probe link cleanup failed")
		}
	}()
	if err := netlink.LinkSetUp(probeLink); err != nil {
		return nil, fmt.Errorf("bring probe link up: %w", err)
	}

	// Re-look up to get the kernel-assigned index; LinkAdd does not fill
	// it in on the struct we handed it.
	live, err := nlLinkByName(probeName)
	if err != nil {
		return nil, fmt.Errorf("relookup probe link: %w", err)
	}
	idx := live.Attrs().Index

	// A link-local source address. ARP does not care that it is off the
	// target's subnet — the request carries it as the sender protocol
	// address and the holder replies regardless — and using 169.254/16
	// means we never have to pick an address out of the operator's pool,
	// which would be the very hazard this function exists to detect.
	src, err := newProbeLinkLocal()
	if err != nil {
		return nil, fmt.Errorf("probe source address: %w", err)
	}
	if err := netlink.AddrAdd(probeLink, src); err != nil {
		return nil, fmt.Errorf("address probe link: %w", err)
	}

	// A /32 scope-link route pins the egress to this link, which is the
	// whole point: without it the kernel picks an interface from the
	// host table and the neighbour entry lands somewhere we are not
	// looking.
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: idx,
		Dst:       &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)},
		Scope:     netlink.SCOPE_LINK,
		Src:       src.IP,
	}); err != nil {
		return nil, fmt.Errorf("route %s via probe link: %w", ip, err)
	}

	// Trigger resolution. The datagram goes to the discard port and its
	// delivery is irrelevant — an ICMP unreachable, a closed port or
	// nothing at all are equally fine. It exists to make the kernel ARP,
	// not to be received.
	//
	// It is a datagram rather than a netlink request because netlink
	// does not work: inserting the neighbour in NUD_INCOMPLETE succeeds,
	// the kernel never probes, and the entry stays INCOMPLETE — a clean
	// verdict over a squatted address. Measured on a veth pair.
	dialer := net.Dialer{
		Timeout:   conflictProbeBudget,
		LocalAddr: &net.UDPAddr{IP: src.IP},
	}
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
			return nil, fmt.Errorf("read neighbour table for %s: %w", probeName, err)
		}
		for _, n := range neighs {
			if !n.IP.Equal(ip) {
				continue
			}
			sawEntry = true
			if n.HardwareAddr != nil && n.State&resolvedStates != 0 {
				if macsEqual(n.HardwareAddr, ourMAC) {
					// Our own endpoint answered. Expected: the probe
					// link is a bridge-mode sibling, so it CAN reach the
					// endpoint — in every mode, not only over a bridge.
					// This comparison is what separates "somebody holds
					// the address" from "we do", and it is now the only
					// thing that does.
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

	// Nothing answered. If the datagram could not even be sent and the
	// kernel never created an entry, we did not ask the question — say
	// so rather than reporting the address as free.
	if !sawEntry && dialErr != nil {
		return nil, fmt.Errorf("could not reach %s via %s to probe it: %w", ip, probeName, dialErr)
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
