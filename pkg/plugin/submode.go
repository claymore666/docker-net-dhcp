// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// Sub-mode values of macvlan_mode and ipvlan_mode, spelled as `ip -d link` prints them (#905).
const (
	MacvlanModeBridge   = "bridge"
	MacvlanModeVEPA     = "vepa"
	MacvlanModePrivate  = "private"
	MacvlanModePassthru = "passthru"
	IPvlanModeL2        = "l2"
	IPvlanModeL3        = "l3"
	ipvlanModeL3S       = "l3s"
)

// parseMacvlanMode maps an unset value to bridge: netlink's MACVLAN_MODE_DEFAULT sends no mode and the kernel then
// builds vepa, so the default never reaches netlink as DEFAULT (#905).
func parseMacvlanMode(v string) (netlink.MacvlanMode, error) {
	switch v {
	case "", MacvlanModeBridge:
		return netlink.MACVLAN_MODE_BRIDGE, nil
	case MacvlanModeVEPA:
		return netlink.MACVLAN_MODE_VEPA, nil
	case MacvlanModePrivate:
		return netlink.MACVLAN_MODE_PRIVATE, nil
	case MacvlanModePassthru:
		return netlink.MACVLAN_MODE_PASSTHRU, nil
	}
	return 0, fmt.Errorf("%w: macvlan_mode %q is not one of %s, %s, %s, %s",
		util.ErrIPAM, v, MacvlanModeBridge, MacvlanModeVEPA, MacvlanModePrivate, MacvlanModePassthru)
}

// parseIPvlanMode accepts l2 only. An l3 or l3s child sent no DHCPDISCOVER out of its parent, with the server on the
// segment and with the server bound to the parent itself, measured on Linux 6.12 on 2026-09-24 (#905).
func parseIPvlanMode(v string) (netlink.IPVlanMode, error) {
	switch v {
	case "", IPvlanModeL2:
		return netlink.IPVLAN_MODE_L2, nil
	case IPvlanModeL3, ipvlanModeL3S:
		return 0, fmt.Errorf("%w: ipvlan_mode=%s is refused: an ipvlan %s child sends no broadcast out of its parent, so its DHCPDISCOVER reaches no DHCP server and no relay, not even a server on the parent itself. The accepted value is %s, the default",
			util.ErrIPAM, v, v, IPvlanModeL2)
	}
	return 0, fmt.Errorf("%w: ipvlan_mode %q is not accepted; the accepted value is %s (%s is refused, see the reference)",
		util.ErrIPAM, v, IPvlanModeL2, IPvlanModeL3)
}

func (o DHCPNetworkOptions) macvlanPassthru() bool {
	return o.effectiveMode() == ModeMacvlan && o.MacvlanMode == MacvlanModePassthru
}

// childWearsParentMAC: an ipvlan child and a macvlan passthru child carry the parent's MAC, and a MAC set on a
// passthru child changes the parent's own, measured on Linux 6.12 (#905).
func (o DHCPNetworkOptions) childWearsParentMAC() bool {
	return o.effectiveMode() == ModeIPvlan || o.macvlanPassthru()
}

// validateSubModes refuses a sub-mode given for the other mode, an unknown value, and require_mac beside passthru,
// whose child cannot take a user MAC (#905, #1036).
func validateSubModes(opts DHCPNetworkOptions) error {
	mode := opts.effectiveMode()
	if opts.MacvlanMode != "" && mode != ModeMacvlan {
		return fmt.Errorf("%w: macvlan_mode cannot be set in mode=%v", util.ErrModeMismatch, mode)
	}
	if opts.IPvlanMode != "" && mode != ModeIPvlan {
		return fmt.Errorf("%w: ipvlan_mode cannot be set in mode=%v", util.ErrModeMismatch, mode)
	}
	if _, err := parseMacvlanMode(opts.MacvlanMode); err != nil {
		return err
	}
	if _, err := parseIPvlanMode(opts.IPvlanMode); err != nil {
		return err
	}
	if opts.RequireMAC && opts.macvlanPassthru() {
		return fmt.Errorf("%w: require_mac cannot be set with macvlan_mode=passthru: the child wears the parent's MAC and a MAC set on it changes the parent's, so every container on the network would be refused",
			util.ErrModeMismatch)
	}
	return nil
}

// ipamRefusePassthru refuses passthru in IPAM mode: libnetwork generates a MAC for every endpoint, and setting it on
// a passthru child changes the parent's MAC (#110, #905).
func ipamRefusePassthru(opts DHCPNetworkOptions) error {
	if !opts.macvlanPassthru() {
		return nil
	}
	return fmt.Errorf("%w: macvlan_mode=passthru networks cannot use this plugin as an IPAM driver, because the passthru child wears the parent's MAC and Docker's IPAM contract sets a per-endpoint one, which would change the parent's. Create the network with --ipam-driver null instead", util.ErrIPAM)
}

// siblingSubMode reads the parent and sub-mode of a macvlan or ipvlan network, this plugin's or Docker's own driver,
// with the kernel defaults for an unset mode; Docker's drivers take a dummy parent when none is given (#905).
func siblingSubMode(n dNetwork.Summary) (kind, parent, sub string, ok bool) {
	if IsDHCPPlugin(n.Driver) {
		o, err := decodeOpts(n.Options)
		if err != nil {
			return "", "", "", false
		}
		kind, parent, sub = o.effectiveMode(), o.Parent, o.MacvlanMode
		if kind == ModeIPvlan {
			sub = o.IPvlanMode
		}
	} else {
		kind, parent = n.Driver, n.Options["parent"]
		sub = n.Options[n.Driver+"_mode"]
	}
	switch {
	case parent == "":
		return "", "", "", false
	case kind == ModeMacvlan && sub == "":
		sub = MacvlanModeBridge
	case kind == ModeIPvlan && sub == "":
		sub = IPvlanModeL2
	case kind != ModeMacvlan && kind != ModeIPvlan:
		return "", "", "", false
	}
	return kind, kernelIfaceName(parent), sub, true
}

// refuseSiblingSubMode refuses a network the kernel would break against another on the same parent: a passthru child
// takes the parent alone, and the ipvlan mode is one per parent, so a new child switches every existing child to its
// mode (both measured on Linux 6.12, 2026-09-24). A failed network list is logged, and the kernel's refusal at the
// endpoint remains (#905).
func (p *Plugin) refuseSiblingSubMode(networkID string, opts DHCPNetworkOptions) error {
	nets, err := p.docker.NetworkList(context.Background(), dNetwork.ListOptions{})
	if err != nil {
		log.WithError(err).WithField("network", shortID(networkID)).
			Warn("Could not list Docker networks to compare sub-modes on the parent; creating the network unchecked")
		return nil
	}
	mode, parent := opts.effectiveMode(), kernelIfaceName(opts.Parent)
	for _, n := range nets {
		kind, other, sub, ok := siblingSubMode(n)
		if !ok || n.ID == networkID || kind != mode || other != parent {
			continue
		}
		if mode == ModeMacvlan && (sub == MacvlanModePassthru || opts.macvlanPassthru()) {
			return fmt.Errorf("%w: network %q already uses parent %q with macvlan_mode=%s. A macvlan_mode=passthru network takes its parent alone, so it cannot share the parent with another macvlan network; use another parent",
				util.ErrModeMismatch, n.Name, parent, sub)
		}
		if mode == ModeIPvlan && sub != IPvlanModeL2 {
			return fmt.Errorf("%w: network %q already uses parent %q with ipvlan_mode=%s. The kernel keeps one ipvlan mode per parent, and a child of this l2 network would switch that network's containers to l2; use another parent",
				util.ErrModeMismatch, n.Name, parent, sub)
		}
	}
	return nil
}

// retryPassthruAdd retries a passthru LinkAdd the kernel refused EINVAL: on `docker restart` the old child holds the
// parent from its dying namespace, where the host link list cannot see it, the #408 window. It reports whether it
// waited (#905).
func retryPassthruAdd(ctx context.Context, budget, interval time.Duration, add func() error) (bool, error) {
	err := add()
	if !errors.Is(err, unix.EINVAL) {
		return false, err
	}
	deadline := time.Now().Add(budget)
	for errors.Is(err, unix.EINVAL) && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return true, err
		case <-time.After(interval):
		}
		err = add()
	}
	return true, err
}
