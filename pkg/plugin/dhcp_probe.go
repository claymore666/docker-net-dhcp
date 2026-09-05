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

	"github.com/claymore666/dhcp-golib/proto"
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
//	client startup (netns entry, socket, DUID, carrier) ≤ ~2s on slow
//	                                                    or virtualized hosts
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
//     what the DHCP client derives its DUID and IAID from, so the probe's
//     identity stays its own — the link's address and the DHCP identity
//     are separate things, the same way they are for a container's own
//     endpoint. (This used to say "exactly as they are on the release
//     path", an analogy to a path #800 deleted; a reader can check the
//     endpoint and cannot check a path that is gone.) The
//     one thing the parent's address does reach is chaddr, which is
//     why the OFFER has to come back broadcast (#243): an ipvlan-L2
//     segment cannot demux a unicast OFFER to a shared MAC. The probe
//     no longer asks for that explicitly — the library sets the
//     BROADCAST flag by default for every client on its raw transport,
//     and the chassis stopped overriding it (see pkg/dhcp/params.go).
//
//  3. Bring it up and run dhcp.GetIP one-shot with the probe budget.
//     The library has no DISCOVER-only mode, as the external client
//     it replaced had no such flag; we accept the full DORA and let the upstream
//     server briefly hold a lease that times out naturally. Since #800 that is true of every client this plugin
//     starts, not something the probe does differently — the probe's
//     lease is short-lived only because the probe is. The cost is one
//     transient pool entry
//     per `docker network create -o validate_dhcp=true`.
//
//  4. Tear down the child unconditionally on return.
//
// On success returns nil. On failure wraps the underlying error
// (link-create failed, acquisition timeout, malformed lease, etc.) with
// a parent-aware prefix so the operator's docker CLI surfaces a
// clear "no DHCP OFFER on <parent> within 8s" message instead of
// the generic CreateNetwork failure shape.
//
// # The parent gate
//
// The probe takes the gate for parent itself, as its first act, and
// holds it until the probe link is gone. It has to hold it for longer
// than the LinkAdd: the probe keeps a child on the parent for the whole
// DORA, so it is a holder as well as a waiter — no endpoint may start
// on top of it, and it must not start on top of a reclaim.
//
// The ordering of the two defers below is what gives that, and it is
// the only subtle thing in this function. Deferred calls run last-in
// first-out, so registering Unlock FIRST makes it run LAST — after the
// deferred LinkDel. The parent stays occupied until the child link is
// removed, not merely until the lease arrives. Register them the other
// way round and the gate opens while the probe's child is still
// attached, which is the EBUSY the gate exists to prevent.
//
// This is now the only path that holds a parent across a DHCP round
// trip: the orphaned-lease reclaim, which used to be the other one and
// the more demanding of the two, was removed in v1.9.0 (#800). Its gate
// was taken several frames up, deliberately, so the hold spanned its
// whole lifetime. Taking it at the top of THIS function is the same
// rule and not a shortcut — the function already spans the entire hold,
// DORA and teardown both, so the position of the lock changes and its
// duration does not (#577).
func (p *Plugin) runDHCPProbe(ctx context.Context, parent, mode string, pol serverPolicy) error {
	// First statement, before the parent is even looked up: the hold
	// must cover everything the caller used to wrap, or this is a
	// change to the gate's duration rather than to its location.
	guard := p.lockParent(ctx, parent, "preflight_probe")
	defer guard.Unlock()

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

	if err := addChildLink(guard, probeLink); err != nil {
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

	// The router-advertisement observation is #868's discriminator for a
	// container endpoint; the preflight probe asks a different question
	// ("is anyone listening?") and has no use for it.
	info, _, err := dhcp.GetIP(probeCtx, probeName, preflightProbeOptions(probeMAC, pol))
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

// preflightProbeOptions is the client configuration for validate_dhcp's
// throwaway lease.
//
// Split out of runDHCPProbe for the reason newProbeLink was: the
// properties that matter here cannot be reached through runDHCPProbe,
// which needs CAP_NET_ADMIN, a real parent and a DHCP server, so left
// inline they are asserted by nothing at all. ConflictMode in
// particular is a value whose wrong setting does not fail any test and
// does not fail on a quiet network -- it fails validate_dhcp against a
// working server, which is how it reached the lane.
func preflightProbeOptions(probeMAC net.HardwareAddr, pol serverPolicy) *dhcp.DHCPClientOptions {
	return &dhcp.DHCPClientOptions{
		// MAC is the probe link's (random) address, and the DHCP client
		// derives its DUID-LL from it. Identity-neutral otherwise:
		// Hostname intentionally empty — the probe shouldn't
		// register any name in the upstream's lease table.
		// VendorClass / ClientID likewise omitted: the goal is
		// "is anyone listening?" not "would my real client get a
		// lease?" — keeping the probe identity-neutral avoids
		// false negatives when class-based policy denies the
		// probe but would accept the real container.
		MAC: probeMAC,
		// Honour the network's server policy (#111, #669). The probe is
		// deliberately identity-neutral — it asks "is anyone listening?"
		// rather than "would my exact client be accepted?" — but a
		// server this network will never take a lease from is not an
		// answer to that question. Without this, validate_dhcp passes
		// on a segment where the only responder is denied, and every
		// container then fails at CreateEndpoint. Flat lists, not the
		// tier ladder: the probe asks whether ANY acceptable server is
		// there, not which one is preferred.
		AllowServers: pol.allowList(),
		DenyServers:  pol.denyList(),
		// RFC 5227 IS OFF HERE, WHATEVER THE NETWORK ASKED FOR, and it
		// is not the mode being ignored -- the mode is about an address
		// this plugin is going to USE, and this one is thrown away
		// milliseconds later on a link that is deleted with it.
		// Section 2.1 exists to answer "may I use this address"; the
		// probe never asks that question.
		//
		// It is also the difference between a working gate and a
		// broken one: preflightProbeBudget is 8s and conflict_check=wait
		// spends up to 7 of them waiting out a check whose answer
		// nothing reads, so validate_dhcp=true failed against a
		// perfectly good DHCP server. MEASURED on the 2.x lane
		// 2026-09-04 (TestPreflightProbe_PassesOnReachableServer, 8.1s).
		ConflictMode: proto.ConflictOff,
	}
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
// inherit the parent's address; the random probe MAC still reaches the
// DHCP client, where it becomes the probe's DUID and IAID, so identity
// is unaffected either way.
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
