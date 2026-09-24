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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// preflightProbeBudget lets the probe survive losing the first DISCOVER on a fresh macvlan child (#307):
//
//	client startup (netns, socket, DUID, carrier)   up to ~2 s on slow or virtual hosts
//	first DISCOVER lost, jittered retransmit         ~3-4 s
//	server response and handler round-trip          < 0.5 s
//
// The old 5 s failed against live servers on the CI runner class. A successful probe returns as soon as the lease lands.
const preflightProbeBudget = 8 * time.Second

// runDHCPProbe runs one DORA from a throwaway child of parent, for validate_dhcp=true. The child is the network's own
// kind, because macvlan and ipvlan children are mutually exclusive on one parent (explainChildLinkAdd). An ipvlan
// child wears the parent's MAC, but the random probe MAC still gives the DUID and IAID; the OFFER comes back
// broadcast because the library sets the flag by default (#243). The lease is left to expire (#800). The child takes
// the network's sub-mode (#905).
//
// Unlock is deferred before LinkDel so it runs after it: the parent gate is held until the child is gone (#577).
func (p *Plugin) runDHCPProbe(ctx context.Context, opts DHCPNetworkOptions, pol serverPolicy) error {
	parent, mode := opts.Parent, opts.effectiveMode()
	guard := p.lockParent(ctx, parent, mode, "preflight_probe")
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
	probeLink, err := newProbeLink(opts, probeName, parentLink.Attrs().Index, probeMAC)
	if err != nil {
		return fmt.Errorf("validate_dhcp: %w", err)
	}

	if err := addChildLink(guard, probeLink); err != nil {
		return fmt.Errorf("validate_dhcp: %w",
			explainChildLinkAdd(err, mode, parent, parentLink.Attrs().Index))
	}
	defer func() {
		if err := netlink.LinkDel(probeLink); err != nil {
			log.WithError(err).WithField("link", probeName).Warn("validate_dhcp probe link cleanup failed")
		}
	}()

	if err := pinPassthruProbe(opts, probeName); err != nil {
		return fmt.Errorf("validate_dhcp: %w", err)
	}
	if err := netlink.LinkSetUp(probeLink); err != nil {
		return fmt.Errorf("validate_dhcp: bring probe link up: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, preflightProbeBudget)
	defer cancel()

	// The router-advertisement observation is #868's discriminator for container endpoints; the probe has no use for
	// it.
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

func preflightProbeOptions(probeMAC net.HardwareAddr, pol serverPolicy) *dhcp.DHCPClientOptions {
	return &dhcp.DHCPClientOptions{
		// Identity-neutral: no hostname, vendor class or client id, so class-based policy cannot deny the probe alone
		// (#307).
		MAC: probeMAC,
		// The network's server policy applies (#111, #669), as flat lists: any acceptable server answers the question.
		AllowServers: pol.allowList(),
		DenyServers:  pol.denyList(),
		// RFC 5227 is off for the probe's throwaway address: conflict_check=wait spends up to 7 of the 8 s budget, and
		// validate_dhcp failed against a good server. Measured on the 2.x lane 2026-09-04,
		// TestPreflightProbe_PassesOnReachableServer at 8.1 s (#901).
		ConflictMode: proto.ConflictOff,
	}
}

// pinPassthruProbe sets a passthru probe child's MAC to the one it wears, the parent's, as CreateEndpoint pins an
// endpoint's: a udev rewrite of an unpinned passthru child would change the parent's MAC (#103, #905).
func pinPassthruProbe(opts DHCPNetworkOptions, name string) error {
	if !opts.macvlanPassthru() {
		return nil
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("re-fetch probe link: %w", err)
	}
	if err := netlink.LinkSetHardwareAddr(link, link.Attrs().HardwareAddr); err != nil {
		return fmt.Errorf("pin passthru probe link MAC: %w", err)
	}
	return nil
}

// newProbeLink builds the probe child as the network's own kind and sub-mode, and sets the MAC only where the child
// does not wear the parent's (#905).
func newProbeLink(opts DHCPNetworkOptions, name string, parentIndex int, mac net.HardwareAddr) (netlink.Link, error) {
	la := netlink.NewLinkAttrs()
	la.Name = name
	la.ParentIndex = parentIndex
	if !opts.childWearsParentMAC() {
		la.HardwareAddr = mac
	}
	return newChildLink(opts, la)
}

// "dh-probe-" plus 6 hex is 15 bytes, IFNAMSIZ-1: a longer name is refused by LinkAdd.
func newProbeLinkName() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "dh-probe-" + hex.EncodeToString(b[:]), nil
}

// newProbeMAC returns a locally administered unicast MAC, which no manufacturer-assigned reservation can match.
func newProbeMAC() (net.HardwareAddr, error) {
	mac := make(net.HardwareAddr, 6)
	if _, err := rand.Read(mac); err != nil {
		return nil, err
	}
	mac[0] = (mac[0] | 0x02) & 0xfe // set LAA bit, clear multicast bit
	return mac, nil
}
