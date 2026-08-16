// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// preflightProbeBudget caps how long the validate_dhcp probe waits
// for an OFFER + ACK from the upstream server before declaring the
// parent NIC unreachable. Sized so that losing the FIRST discover
// still passes (#307) — the probe interface is a freshly-created
// macvlan child that solicits immediately, and that first broadcast
// is not reliably delivered:
//
//	dhcpcd startup (unshare/mounts/DUID/carrier)   ≤ ~2s on slow or
//	                                               virtualized hosts
//	initial DISCOVER lost + jittered retransmit    ~3–4s
//	server response + handler round-trip           < 0.5s
//
// 8s covers that worst case with margin. The old 5s value satisfied
// this same one-retry intent only with subsecond startup and failed
// against live servers on the CI runner class (#307 has two full
// timelines). Note the budget is a cap, not a wait: a successful
// probe returns as soon as the lease lands (typically < 2s), so the
// full duration is only ever paid when the parent is genuinely
// unreachable and the operator is about to get an error anyway.
const preflightProbeBudget = 8 * time.Second

// runDHCPProbe verifies that the parent NIC can reach a working DHCP
// server on the local segment. Implements the validate_dhcp=true
// driver-opt path:
//
//  1. Generate a random locally-administered MAC and a unique probe
//     link name so the DISCOVER doesn't collide with any stable
//     upstream reservation, and concurrent probes on the same host
//     don't fight for the link name.
//
//  2. Create a temporary child of the parent NIC in the host netns, of
//     the SAME KIND the network's endpoints will use — macvlan (mode
//     bridge) for a macvlan network, ipvlan (L2) for an ipvlan one.
//
//     This used to build a macvlan whatever the mode was, on the
//     reasoning that ipvlan slaves share the parent MAC (clashing with
//     the random probe MAC) and that reachability is mode-agnostic.
//     Reachability is; the parent is not. macvlan and ipvlan children
//     are mutually exclusive on one parent — see explainChildLinkAdd —
//     so a macvlan probe on an ipvlan network is refused outright the
//     moment any ipvlan container is running on that NIC, and while it
//     does run it blocks every ipvlan endpoint on the same parent.
//     `docker network create -o mode=ipvlan -o validate_dhcp=true`
//     could therefore fail for a reason that had nothing to do with
//     DHCP, which is the opposite of what the flag is for.
//
//     The shared MAC that motivated the old choice turns out not to
//     bite. An ipvlan probe link wears the parent's address because the
//     kernel permits nothing else, but the random probe MAC is still
//     what dhcpcd derives its DUID and IAID from, so the probe's
//     identity stays its own — the link's address and the DHCP identity
//     are separate things, exactly as they are on the release path. The
//     one thing the parent's address does reach is chaddr, which is why
//     Broadcast is already requested below (#243): an ipvlan-L2 segment
//     cannot demux a unicast OFFER to a shared MAC.
//
//  3. Bring it up and run dhcp.GetIP one-shot with the probe budget.
//     dhcpcd has no DISCOVER-only flag; we accept the full DORA and
//     let the upstream server briefly hold a lease that times out
//     naturally (no `release` directive sent). The cost is one
//     transient pool entry
//     per `docker network create -o validate_dhcp=true`.
//
//  4. Tear down the child unconditionally on return.
//
// On success returns nil. On failure wraps the underlying error
// (link-create failed, dhcpcd timeout, malformed lease, etc.) with
// a parent-aware prefix so the operator's docker CLI surfaces a
// clear "no DHCP OFFER on <parent> within 8s" message instead of
// the generic CreateNetwork failure shape.
func runDHCPProbe(ctx context.Context, parent, mode string) error {
	if parent == "" {
		return errors.New("validate_dhcp: parent NIC name is empty")
	}
	if _, err := netlink.LinkByName(parent); err != nil {
		return fmt.Errorf("validate_dhcp: parent %q not found: %w", parent, err)
	}

	probeName, err := newProbeLinkName()
	if err != nil {
		return fmt.Errorf("validate_dhcp: name generation: %w", err)
	}
	probeMAC, err := newProbeMAC()
	if err != nil {
		return fmt.Errorf("validate_dhcp: MAC generation: %w", err)
	}

	parentLink, err := netlink.LinkByName(parent)
	if err != nil {
		return fmt.Errorf("validate_dhcp: relookup parent: %w", err)
	}
	probeLink := newProbeLink(mode, probeName, parentLink.Attrs().Index, probeMAC)

	if err := netlink.LinkAdd(probeLink); err != nil {
		return fmt.Errorf("validate_dhcp: %w",
			explainChildLinkAdd(err, mode, parent, parentLink.Attrs().Index))
	}
	defer func() {
		// Best-effort: a failed Del here only leaves a temporary
		// link the operator can remove with `ip link del`. Logging
		// at warn so it doesn't silently leak names across runs.
		if err := netlink.LinkDel(probeLink); err != nil {
			log.WithError(err).WithField("link", probeName).Warn("validate_dhcp probe link cleanup failed")
		}
	}()

	if err := netlink.LinkSetUp(probeLink); err != nil {
		return fmt.Errorf("validate_dhcp: bring probe link up: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, preflightProbeBudget)
	defer cancel()

	info, err := dhcp.GetIP(probeCtx, probeName, &dhcp.DHCPClientOptions{
		// MAC is the probe link's (random) address; dhcpcd derives its
		// DUID-LL from it. Identity-neutral otherwise:
		// Hostname intentionally empty — the probe shouldn't
		// register any name in the upstream's lease table.
		// VendorClass / ClientID likewise omitted: the goal is
		// "is anyone listening?" not "would my real client get a
		// lease?" — keeping the probe identity-neutral avoids
		// false negatives when class-based policy denies the
		// probe but would accept the real container.
		MAC: probeMAC,
		// Request a broadcast reply (#243). The probe is a transient
		// reachability check from a brand-new random MAC; asking the
		// server to broadcast its OFFER makes the probe robust whether
		// or not the server unicasts back to an unconfigured client,
		// so a reachable server is never reported as unreachable.
		Broadcast: true,
	})
	if err != nil {
		if errors.Is(err, util.ErrNoLease) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("no DHCP OFFER on %q within %v — parent NIC may be isolated, firewalled (UDP/67-68), or VLAN-tagged wrong", parent, preflightProbeBudget)
		}
		return fmt.Errorf("validate_dhcp probe on %q: %w", parent, err)
	}

	log.
		WithField("parent", parent).
		WithField("probe_ip", info.IP).
		WithField("probe_gateway", info.Gateway).
		Info("validate_dhcp probe succeeded — DHCP server reachable")
	return nil
}

// newProbeLink builds the temporary child the probe runs on.
//
// Split out of runDHCPProbe so the one property that matters can be
// asserted without CAP_NET_ADMIN: the probe attaches to the parent as
// the SAME KIND the network's endpoints will, because the kernel will
// not let one parent carry both kinds (explainChildLinkAdd). Getting
// this wrong is invisible in a unit-testable seam otherwise, and it is
// what made `validate_dhcp=true` unusable on an ipvlan network.
//
// The MAC is applied only where the kernel accepts one. ipvlan children
// inherit the parent's address; the random probe MAC still reaches
// dhcpcd, where it becomes the probe's DUID and IAID, so identity is
// unaffected either way.
func newProbeLink(mode, name string, parentIndex int, mac net.HardwareAddr) netlink.Link {
	la := netlink.NewLinkAttrs()
	la.Name = name
	la.ParentIndex = parentIndex
	if mode != ModeIPvlan {
		la.HardwareAddr = mac
	}
	return newChildLink(mode, la)
}

// newProbeLinkName returns a per-probe link name unique enough to
// avoid collision with concurrent probes on the same host. 6 hex
// chars after the "dh-probe-" prefix == 3 random bytes (16M-space).
// Collision odds are negligible at the volume `docker network create`
// runs. Total length 15 == IFNAMSIZ-1 (Linux's max printable interface
// name); a longer suffix here would have the kernel refuse LinkAdd.
func newProbeLinkName() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "dh-probe-" + hex.EncodeToString(b[:]), nil
}

// newProbeMAC returns a random locally-administered unicast MAC
// (LAA bit set, multicast bit clear). Avoids collision with any
// stable upstream reservation: a real device's MAC almost certainly
// has the LAA bit clear (manufacturer-assigned), so anything in this
// space is recognisably "ephemeral / synthesised" to network admins
// who notice it in dnsmasq logs.
func newProbeMAC() (net.HardwareAddr, error) {
	mac := make(net.HardwareAddr, 6)
	if _, err := rand.Read(mac); err != nil {
		return nil, err
	}
	mac[0] = (mac[0] | 0x02) & 0xfe // set LAA bit, clear multicast bit
	return mac, nil
}
