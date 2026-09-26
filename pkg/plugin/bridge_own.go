// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	dNetwork "github.com/docker/docker/api/types/network"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	// bridgeNFCallIPTablesSysctl is per network namespace since Linux 5.3 and absent while br_netfilter is not loaded.
	bridgeNFCallIPTablesSysctl = "/proc/sys/net/bridge/bridge-nf-call-iptables"
	sysClassNetDir             = "/sys/class/net"
	// nfDrop is NF_DROP, the verdict nf_tables reports as a base chain's policy (include/uapi/linux/netfilter.h).
	nfDrop = 0
	// dockerBridgeNameOption is the option Docker's own bridge driver names its bridge by.
	dockerBridgeNameOption = "com.docker.network.bridge.name"
)

// ownsBridge reports a bridge-mode network with parent, whose bridge this plugin makes, marks and removes (#903).
func (o DHCPNetworkOptions) ownsBridge() bool {
	return o.effectiveMode() == ModeBridge && o.Parent != ""
}

// validateBridgeOwnOptions refuses force_create without a bridge made from parent and, with parent, a kernel-illegal
// parent, Docker's own bridge names and release_lease; stored options pass the same function (#903).
func validateBridgeOwnOptions(opts DHCPNetworkOptions) error {
	if opts.ForceCreate && !opts.ownsBridge() {
		return fmt.Errorf("%w: force_create applies only with mode=bridge and parent set: it overrides the firewall check for a bridge this plugin makes",
			util.ErrModeMismatch)
	}
	if !opts.ownsBridge() {
		return nil
	}
	if !dhcp.ValidIfaceName(opts.Parent) {
		return fmt.Errorf("%w: invalid parent %q: not a kernel-legal interface name", util.ErrIPAM, opts.Parent)
	}
	if opts.Parent == opts.Bridge {
		return fmt.Errorf("%w: parent and bridge both name %v; parent is the NIC this plugin enslaves into the bridge it makes",
			util.ErrIPAM, opts.Bridge)
	}
	// A later Docker network of that name would take a bridge this plugin made (#903).
	if opts.Bridge == "docker0" || strings.HasPrefix(opts.Bridge, "br-") {
		return fmt.Errorf("%w: bridge %q cannot be made from parent: docker0 and br-* are the names Docker gives its own bridges; choose another bridge name",
			util.ErrIPAM, opts.Bridge)
	}
	return bridgeReleaseRefusal(opts)
}

// readFlagFile reads a 0 or 1 sysctl; a missing file reads as 0.
func readFlagFile(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(b)) != "0", nil
}

// nfCallIPTablesUnder reads the host sysctl, then the bridge's own nf_call_iptables, which the kernel ORs with it,
// measured on Linux 6.12 (#903).
func nfCallIPTablesUnder(sysctl, classDir, bridge string) (bool, error) {
	on, err := readFlagFile(sysctl)
	if err != nil || on {
		return on, err
	}
	return readFlagFile(filepath.Join(classDir, bridge, "bridge", "nf_call_iptables"))
}

// bridgeNFCallIPTables and readForwardPolicy are seams for the unit tests (#903).
var (
	bridgeNFCallIPTables = func(bridge string) (bool, error) {
		return nfCallIPTablesUnder(bridgeNFCallIPTablesSysctl, sysClassNetDir, bridge)
	}
	readForwardPolicy = nftForwardPolicy
)

// nftForwardPolicy asks nf_tables for the policy of chain FORWARD in table ip filter, which Docker sets to DROP on an
// iptables-nft host, with NFT_MSG_GETCHAIN; no library in go.mod reads it, and the reply is ENOENT where the chain is
// absent, as on an iptables-legacy host (#903).
func nftForwardPolicy() (uint32, error) {
	req := nl.NewNetlinkRequest(unix.NFNL_SUBSYS_NFTABLES<<8|unix.NFT_MSG_GETCHAIN, 0)
	req.AddData(&nl.Nfgenmsg{NfgenFamily: unix.NFPROTO_IPV4, Version: nl.NFNETLINK_V0})
	req.AddData(nl.NewRtAttr(unix.NFTA_CHAIN_TABLE, nl.ZeroTerminated("filter")))
	req.AddData(nl.NewRtAttr(unix.NFTA_CHAIN_NAME, nl.ZeroTerminated("FORWARD")))
	msgs, err := req.Execute(unix.NETLINK_NETFILTER, 0)
	if err != nil {
		return 0, err
	}
	for _, m := range msgs {
		if len(m) < nl.SizeofNfgenmsg {
			continue
		}
		attrs, err := nl.ParseRouteAttr(m[nl.SizeofNfgenmsg:])
		if err != nil {
			return 0, err
		}
		for _, a := range attrs {
			if a.Attr.Type == unix.NFTA_CHAIN_POLICY && len(a.Value) == 4 {
				return binary.BigEndian.Uint32(a.Value), nil
			}
		}
	}
	return 0, errors.New("the chain carries no policy")
}

// firewallDropReason says why bridged frames would be dropped, or "" when they pass: bridge-nf-call-iptables sends
// them through iptables, where Docker's FORWARD policy drops them, measured on Linux 6.12 (#903).
func firewallDropReason(bridge string) string {
	on, err := bridgeNFCallIPTables(bridge)
	if err != nil {
		return fmt.Sprintf("bridge-nf-call-iptables cannot be read (%v)", err)
	}
	if !on {
		return ""
	}
	policy, err := readForwardPolicy()
	if err != nil {
		return fmt.Sprintf("bridge-nf-call-iptables is 1 and the policy of the ip filter FORWARD chain cannot be read over nf_tables (%v); this plugin reads nf_tables only, so a host on iptables-legacy or without nf_tables lands here", err)
	}
	if policy == nfDrop {
		return "bridge-nf-call-iptables is 1 and the policy of the ip filter FORWARD chain is DROP"
	}
	return ""
}

// firewallRefusal is decision D0: a create that would succeed while no container on it leases is refused, and
// force_create logs the verdict at warning level and creates (#903).
func firewallRefusal(opts DHCPNetworkOptions) error {
	why := firewallDropReason(opts.Bridge)
	var err error
	if why != "" {
		err = fmt.Errorf("%s, so the host would drop the DHCP frames bridged between the ports of %v and no container on this network would get a lease. Add `iptables -I DOCKER-USER -i %v -o %v -j ACCEPT` on the host, then create the network with -o force_create=true: %w",
			why, opts.Bridge, opts.Bridge, opts.Bridge, util.ErrIPAM)
	}
	if !opts.ForceCreate {
		return err
	}
	entry := log.WithFields(log.Fields{"bridge": opts.Bridge, "parent": opts.Parent})
	if err != nil {
		entry.WithError(err).Warn("force_create=true: creating the network although the firewall check expects its bridged frames dropped")
	} else {
		entry.Warn("force_create=true: the firewall check found nothing that drops bridged frames")
	}
	return nil
}

// bridgeOwned reports the mark #902 gives a sub-interface, so a bridge the operator or Docker made is never enslaved
// into or removed (#903).
func bridgeOwned(l netlink.Link) bool {
	return l.Type() == "bridge" && l.Attrs().Alias == vlanOwnerAlias
}

// bridgeLinkAdd and bridgeHostIPv6Off are seams for the unit tests. A fresh bridge takes an fe80 unless disable_ipv6
// is written before it comes up, measured on Linux 6.12 (#903).
var (
	bridgeLinkAdd     = addChildLink
	bridgeHostIPv6Off = func(name string) error { return disableHostIPv6Under(ipv6DisableSysctlDir, name) }
)

// enslaveParent takes the parent gate's guard, since the port claims the parent's rx_handler (#903).
func enslaveParent(_ *parentGuard, parent, bridge netlink.Link) error {
	return nlLinkSetMaster(parent, bridge)
}

// routeLeaves reports a route out of the link at index, a multipath nexthop included.
func routeLeaves(r netlink.Route, index int) bool {
	if r.LinkIndex == index {
		return true
	}
	for _, nh := range r.MultiPath {
		if nh.LinkIndex == index {
			return true
		}
	}
	return false
}

// kernelLinkLocalRoute is the fe80::/64 route the kernel puts in main on every up link with IPv6 on (#903).
func kernelLinkLocalRoute(r netlink.Route) bool {
	return r.Protocol == unix.RTPROT_KERNEL && r.Dst != nil && r.Dst.String() == "fe80::/64"
}

// parentAddressItem names what makes enslaving the parent unsafe: any IPv4 address, an IPv6 address that is not
// link-local, or a main-table route out of it other than the kernel's fe80::/64. Enslaving leaves them on the port,
// where the host stops answering, measured on Linux 6.12. Table local holds the kernel's own entries and is not read
// (#903).
func parentAddressItem(parent netlink.Link) (string, error) {
	name, index := parent.Attrs().Name, parent.Attrs().Index
	addrs, err := util.DumpResult(nlAddrList(parent, netlink.FAMILY_ALL))
	if err != nil {
		return "", fmt.Errorf("failed to read the addresses of parent %v: %w", name, err)
	}
	for _, a := range addrs {
		switch {
		case a.IP.To4() != nil:
			return "the IPv4 address " + a.IPNet.String(), nil
		case !a.IP.IsLinkLocalUnicast():
			return "the IPv6 address " + a.IPNet.String(), nil
		}
	}
	routes, err := util.DumpResult(nlRouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE))
	if err != nil {
		return "", fmt.Errorf("failed to read the routes of parent %v: %w", name, err)
	}
	for _, r := range routes {
		if routeLeaves(r, index) && !kernelLinkLocalRoute(r) {
			return fmt.Sprintf("the route %s proto %v", describeRoute(r), r.Protocol), nil
		}
	}
	return "", nil
}

// refuseUnsafeParent runs before every enslave; own is the bridge the parent may already be a port of, 0 for none.
func refuseUnsafeParent(opts DHCPNetworkOptions, parent netlink.Link, own int) error {
	if err := refuseEnslavedParent(parent, own); err != nil {
		return err
	}
	item, err := parentAddressItem(parent)
	if err != nil || item == "" {
		return err
	}
	return fmt.Errorf("parent %v carries %s, and enslaving it into bridge %v leaves that on the port, where the host stops answering on it. Choose a NIC the host does not address, or put the host's address on a bridge you create yourself and drop parent (docs/bridge-mode.md): %w",
		opts.Parent, item, opts.Bridge, util.ErrIPAM)
}

func bridgeNotOursRefusal(opts DHCPNetworkOptions, existing netlink.Link) error {
	return fmt.Errorf("%v exists (a %v link) and this plugin did not make it, so it will not enslave %v into it; drop parent to use it as found, and for an existing network delete and re-create the network without parent: %w",
		opts.Bridge, existing.Type(), opts.Parent, util.ErrIPAM)
}

// bridgeParentRefusal keeps a bridge to one parent: it refuses when a stored network made it from another, and, for a
// bridge that exists, when no stored network made it from this one, as while another create is in flight on it (#903).
func bridgeParentRefusal(opts DHCPNetworkOptions, exists bool) error {
	stored, err := storedNetworkOptions()
	if err != nil {
		return fmt.Errorf("cannot read the stored networks to learn which parent bridge %v is made from, so %v is not enslaved: %w",
			opts.Bridge, opts.Parent, err)
	}
	mine := false
	for _, id := range slices.Sorted(maps.Keys(stored)) {
		o := stored[id]
		if !o.ownsBridge() || o.Bridge != opts.Bridge {
			continue
		}
		if o.Parent != opts.Parent {
			return fmt.Errorf("bridge %v is made from parent %v for network %v, and a bridge this plugin makes takes one parent, so %v is not enslaved into it; create this network with parent=%v: %w",
				opts.Bridge, o.Parent, shortID(id), opts.Parent, o.Parent, util.ErrIPAM)
		}
		mine = true
	}
	if exists && !mine {
		return fmt.Errorf("bridge %v exists and no network of this plugin was made on it from parent %v, so %v is not enslaved into it; another create may be making it from another parent. Delete the bridge when nothing uses it, then create the network again: %w",
			opts.Bridge, opts.Parent, opts.Parent, util.ErrIPAM)
	}
	return nil
}

// ensureBridge adopts the bridge this plugin marked, enslaving the parent again when a reboot released it, or
// creates, marks and enslaves it. It runs at CreateNetwork and before every child, since a host reboot loses the bridge
// while Docker keeps the network (#903). It reports whether it created one.
func (p *Plugin) ensureBridge(ctx context.Context, opts DHCPNetworkOptions, op string) (bool, error) {
	if !opts.ownsBridge() {
		return false, nil
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()

	parent, err := validateParentForChild(opts.Parent)
	if err != nil {
		return false, err
	}
	if existing, err := nlLinkByName(opts.Bridge); err == nil {
		if !bridgeOwned(existing) {
			return false, bridgeNotOursRefusal(opts, existing)
		}
		if parent.Attrs().MasterIndex == existing.Attrs().Index {
			return false, nil
		}
		return false, p.enslaveAgain(ctx, opts, parent, existing, op)
	} else if !isLinkNotFound(err) {
		return false, fmt.Errorf("failed to look up bridge %v: %w", opts.Bridge, err)
	}

	if err := bridgeParentRefusal(opts, false); err != nil {
		return false, err
	}
	if err := refuseUnsafeParent(opts, parent, 0); err != nil {
		return false, err
	}
	guard := p.lockParent(ctx, opts.Parent, parentGateKindBridge, op)
	defer guard.Unlock()
	la := netlink.NewLinkAttrs()
	la.Name = opts.Bridge
	if err := bridgeLinkAdd(guard, &netlink.Bridge{LinkAttrs: la}); err != nil {
		// Made outside the plugin since the lookup above, so it carries no mark (#903).
		if existing, lerr := nlLinkByName(opts.Bridge); errors.Is(err, unix.EEXIST) && lerr == nil {
			return false, bridgeNotOursRefusal(opts, existing)
		}
		return false, fmt.Errorf("failed to create bridge %v: %w", opts.Bridge, err)
	}
	created, err := nlLinkByName(opts.Bridge)
	if err == nil {
		err = nlLinkSetAlias(created, vlanOwnerAlias)
	}
	if err == nil {
		err = bridgeHostIPv6Off(opts.Bridge)
	}
	if err == nil {
		err = nlLinkSetUp(created)
	}
	if err == nil {
		err = enslaveParent(guard, parent, created)
	}
	if err != nil {
		if created != nil {
			if derr := nlLinkDel(created); derr != nil {
				log.WithError(derr).WithField("bridge", opts.Bridge).Warn("Failed to remove a bridge after its setup failed")
			}
		}
		return false, fmt.Errorf("failed to set up bridge %v on parent %v: %w", opts.Bridge, opts.Parent, err)
	}
	log.WithFields(log.Fields{"bridge": opts.Bridge, "parent": opts.Parent, "op": op}).Info("Created bridge and enslaved the parent")
	return true, nil
}

// enslaveAgain makes the parent a port of the plugin's bridge again, after the same checks a create runs (#903).
func (p *Plugin) enslaveAgain(ctx context.Context, opts DHCPNetworkOptions, parent, bridge netlink.Link, op string) error {
	if err := bridgeParentRefusal(opts, true); err != nil {
		return err
	}
	if err := refuseUnsafeParent(opts, parent, bridge.Attrs().Index); err != nil {
		return err
	}
	guard := p.lockParent(ctx, opts.Parent, parentGateKindBridge, op)
	defer guard.Unlock()
	if err := enslaveParent(guard, parent, bridge); err != nil {
		return fmt.Errorf("failed to enslave parent %v into bridge %v: %w", opts.Parent, opts.Bridge, err)
	}
	log.WithFields(log.Fields{"bridge": opts.Bridge, "parent": opts.Parent, "op": op}).Info("Enslaved the parent into the bridge this plugin made")
	return nil
}

// bridgeUsers names the networks other than self on bridge: this plugin's stored records, and every network Docker
// lists that names it, through this plugin's bridge option or Docker's own bridge driver's (#903).
func bridgeUsers(bridge, self string, stored map[string]DHCPNetworkOptions, nets []dNetwork.Summary) []string {
	var users []string
	for id, o := range stored {
		if id != self && o.effectiveMode() == ModeBridge && o.Bridge == bridge {
			users = append(users, shortID(id))
		}
	}
	for _, n := range nets {
		if _, mine := stored[n.ID]; mine || n.ID == self {
			continue
		}
		if n.Options[dockerBridgeNameOption] == bridge {
			users = append(users, n.Name)
			continue
		}
		if !IsDHCPPlugin(n.Driver) {
			continue
		}
		// One that does not decode keeps the bridge: which bridge it sits on is unknown.
		if o, err := decodeOpts(n.Options); err != nil || (o.effectiveMode() == ModeBridge && o.Bridge == bridge) {
			users = append(users, n.Name)
		}
	}
	return users
}

// otherPorts names the ports of the bridge at index other than the parent.
func otherPorts(links []netlink.Link, index int, parent string) []string {
	var names []string
	for _, l := range links {
		if l.Attrs().MasterIndex == index && l.Attrs().Name != parent {
			names = append(names, l.Attrs().Name)
		}
	}
	return names
}

// retireBridge removes the bridge network self made from parent when self was its last user; the kernel releases the
// parent with its state kept, measured on Linux 6.12. It keeps a bridge that is not marked, one a create is in flight
// on, one whose users cannot be read, and one with a port other than the parent (#903).
func (p *Plugin) retireBridge(ctx context.Context, self string, opts DHCPNetworkOptions, op string) {
	if !opts.ownsBridge() {
		return
	}
	name := opts.Bridge
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	fields := log.Fields{"bridge": name, "parent": opts.Parent, "network": shortID(self), "op": op}
	keep := func(why string, extra ...any) {
		log.WithFields(fields).Info(fmt.Sprintf("Keeping bridge: "+why, extra...))
	}

	if n := p.bridgePending[name]; n > 0 {
		keep("%d network create(s) in flight on it", n)
		return
	}
	stored, err := storedNetworkOptions()
	if err != nil {
		keep("cannot read the stored networks: %v", err)
		return
	}
	listCtx, cancel := context.WithTimeout(ctx, vlanListBudget)
	nets, err := p.docker.NetworkList(listCtx, dNetwork.ListOptions{})
	cancel()
	if err != nil {
		keep("cannot list Docker networks: %v", err)
		return
	}
	if users := bridgeUsers(name, self, stored, nets); len(users) > 0 {
		keep("still used by %v", users)
		return
	}
	link, err := nlLinkByName(name)
	if err != nil {
		if !isLinkNotFound(err) {
			keep("lookup failed: %v", err)
		}
		return
	}
	if !bridgeOwned(link) {
		keep("not created by this plugin (alias %q)", link.Attrs().Alias)
		return
	}
	links, err := util.DumpResult(nlLinkList())
	if err != nil {
		keep("cannot list host links: %v", err)
		return
	}
	if ports := otherPorts(links, link.Attrs().Index, opts.Parent); len(ports) > 0 {
		keep("ports other than the parent sit on it: %v", ports)
		return
	}
	guard := p.lockParent(ctx, opts.Parent, parentGateKindBridge, op)
	defer guard.Unlock()
	if err := nlLinkDel(link); err != nil {
		log.WithError(err).WithFields(fields).Warn("Failed to remove bridge")
		return
	}
	log.WithFields(fields).Info("Removed bridge; its last network is gone and the parent is released")
	if parent, err := nlLinkByName(opts.Parent); err == nil && (parent.Attrs().MasterIndex != 0 || parent.Attrs().Promisc != 0) {
		log.WithFields(fields).WithFields(log.Fields{"master": parent.Attrs().MasterIndex, "promiscuity": parent.Attrs().Promisc}).
			Warn("The parent is not as it was before the create")
	}
}

// beginBridgeCreate counts a CreateNetwork between its ensure and its save, so a failed create's rollback cannot
// remove a bridge another create has adopted but not stored yet (#903). The returned func ends the count.
func (p *Plugin) beginBridgeCreate(opts DHCPNetworkOptions) func() {
	if !opts.ownsBridge() {
		return func() {}
	}
	name := opts.Bridge
	p.bridgeMu.Lock()
	if p.bridgePending == nil {
		p.bridgePending = map[string]int{}
	}
	p.bridgePending[name]++
	p.bridgeMu.Unlock()
	return func() {
		p.bridgeMu.Lock()
		p.bridgePending[name]--
		if p.bridgePending[name] <= 0 {
			delete(p.bridgePending, name)
		}
		p.bridgeMu.Unlock()
	}
}
