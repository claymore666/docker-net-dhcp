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

	"github.com/devplayer0/docker-net-dhcp/pkg/udhcpc"
	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

// preflightProbeBudget caps how long the validate_dhcp probe waits
// for an OFFER + ACK from the upstream server before declaring the
// parent NIC unreachable. Five seconds is enough for a healthy LAN
// (Fritz.Box-class servers respond in under 100ms) plus retry once
// at udhcpc's default ~3s discover timeout. Tight on purpose: the
// operator is blocked in `docker network create` while it runs.
const preflightProbeBudget = 5 * time.Second

// runDHCPProbe verifies that the parent NIC can reach a working DHCP
// server on the local segment. Implements the validate_dhcp=true
// driver-opt path:
//
//  1. Generate a random locally-administered MAC and a unique probe
//     link name so the DISCOVER doesn't collide with any stable
//     upstream reservation, and concurrent probes on the same host
//     don't fight for the link name.
//  2. Create a temporary macvlan child of the parent NIC in the host
//     netns (mode bridge — same submode the production endpoint
//     uses, so the probe path matches the real path as closely as
//     possible). ipvlan callers also get macvlan here: ipvlan slaves
//     share the parent MAC, which would clash with the random probe
//     MAC, and the probe goal (verifying DHCP reachability of the
//     parent) is mode-agnostic.
//  3. Bring it up and run udhcpc.GetIP one-shot with the probe budget.
//     Busybox has no DISCOVER-only flag; we accept the full DORA and
//     let the upstream server briefly hold a lease that times out
//     naturally (no -R sent). The cost is one transient pool entry
//     per `docker network create -o validate_dhcp=true`.
//  4. Tear down the macvlan child unconditionally on return.
//
// On success returns nil. On failure wraps the underlying error
// (link-create failed, udhcpc timeout, malformed lease, etc.) with
// a parent-aware prefix so the operator's docker CLI surfaces a
// clear "no DHCP OFFER on <parent> within 5s" message instead of
// the generic CreateNetwork failure shape.
func runDHCPProbe(ctx context.Context, parent string) error {
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
	la := netlink.NewLinkAttrs()
	la.Name = probeName
	la.ParentIndex = parentLink.Attrs().Index
	la.HardwareAddr = probeMAC
	probeLink := &netlink.Macvlan{LinkAttrs: la, Mode: netlink.MACVLAN_MODE_BRIDGE}

	if err := netlink.LinkAdd(probeLink); err != nil {
		return fmt.Errorf("validate_dhcp: create probe macvlan on %q: %w", parent, err)
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

	info, err := udhcpc.GetIP(probeCtx, probeName, &udhcpc.DHCPClientOptions{
		// Hostname intentionally empty — the probe shouldn't
		// register any name in the upstream's lease table.
		// VendorClass / ClientID likewise omitted: the goal is
		// "is anyone listening?" not "would my real client get a
		// lease?" — keeping the probe identity-neutral avoids
		// false negatives when class-based policy denies the
		// probe but would accept the real container.
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
