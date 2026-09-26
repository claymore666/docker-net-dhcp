// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// vlanListBudget bounds the Docker network list a removal reads its users from (#902).
const vlanListBudget = 5 * time.Second

// vlanOwnerAlias marks a sub-interface this plugin created. The kernel drops an alias sent with the create, measured
// on Linux 6.12, so it is set by a second call (#902).
const vlanOwnerAlias = "docker-net-dhcp"

// parseVlanID reads the vlan option: empty for none, else a canonical decimal in 1..4094, since 0 and 4095 are
// reserved (IEEE 802.1Q 9.6) and "0100" would name a link other than the one the option reads as (#902).
func parseVlanID(v string) (int, error) {
	if v == "" {
		return 0, nil
	}
	id, err := strconv.Atoi(v)
	if err != nil || strconv.Itoa(id) != v || id < 1 || id > 4094 {
		return 0, fmt.Errorf("%w: vlan %q is not a VLAN ID from 1 to 4094", util.ErrIPAM, v)
	}
	return id, nil
}

// linkParent is the link the network's children attach to: the parent, or its 802.1Q sub-interface `<parent>.<id>`,
// the name Docker's own macvlan driver uses (#902).
func (o DHCPNetworkOptions) linkParent() string {
	if o.Vlan == "" {
		return o.Parent
	}
	return o.Parent + "." + o.Vlan
}

// validateVlanOption refuses vlan outside macvlan and ipvlan, a bad ID, and a sub-interface name over the kernel's
// 15 bytes, which the kernel would refuse only at the first create (#902).
func validateVlanOption(opts DHCPNetworkOptions) error {
	if opts.Vlan == "" {
		return nil
	}
	if m := opts.effectiveMode(); m != ModeMacvlan && m != ModeIPvlan {
		return fmt.Errorf("%w: vlan cannot be set in mode=%v", util.ErrModeMismatch, m)
	}
	if _, err := parseVlanID(opts.Vlan); err != nil {
		return err
	}
	if !dhcp.ValidIfaceName(opts.linkParent()) {
		return fmt.Errorf("%w: vlan sub-interface name %q is not a kernel-legal interface name (at most 15 bytes); use a shorter parent name",
			util.ErrIPAM, opts.linkParent())
	}
	return nil
}

// vlanVerdict decides what an existing link of the sub-interface's name is: nil to adopt it, an error when it is not
// an 802.1Q vlan of this ID on this parent, since endpoints on it would land on another segment (#902).
func vlanVerdict(existing netlink.Link, parentIndex, id int) error {
	name := existing.Attrs().Name
	v, ok := existing.(*netlink.Vlan)
	if !ok {
		return fmt.Errorf("%w: %v exists and is a %v link, not a vlan; remove it or choose another vlan", util.ErrParentInvalid, name, existing.Type())
	}
	if v.ParentIndex != parentIndex {
		return fmt.Errorf("%w: %v exists on another parent (index %d, want %d)", util.ErrParentInvalid, name, v.ParentIndex, parentIndex)
	}
	if v.VlanId != id {
		return fmt.Errorf("%w: %v exists with vlan id %d, want %d", util.ErrParentInvalid, name, v.VlanId, id)
	}
	if v.VlanProtocol != netlink.VLAN_PROTOCOL_8021Q {
		return fmt.Errorf("%w: %v exists with protocol %v, want 802.1Q", util.ErrParentInvalid, name, v.VlanProtocol)
	}
	return nil
}

// vlanOwned reports the mark, so a sub-interface the operator or Docker made is used and never removed (#902).
func vlanOwned(l netlink.Link) bool {
	return l.Attrs().Alias == vlanOwnerAlias
}

// vlanLinkAdd creates the sub-interface through the parent gate's funnel, and vlanTrialAdd adds the removal's trial
// children; both are seams for the unit tests (#571, #902).
var (
	vlanLinkAdd  = addChildLink
	vlanTrialAdd = addChildLink
)

func isLinkNotFound(err error) bool {
	var lnf netlink.LinkNotFoundError
	return errors.As(err, &lnf)
}

// ensureVlanLink adopts or creates the network's sub-interface, and runs at CreateNetwork and before every child,
// since a host reboot loses the link while Docker keeps the network (#902). It reports whether it created one.
func (p *Plugin) ensureVlanLink(ctx context.Context, opts DHCPNetworkOptions, op string) (bool, error) {
	if opts.Vlan == "" {
		return false, nil
	}
	id, err := parseVlanID(opts.Vlan)
	if err != nil {
		return false, err
	}
	name := opts.linkParent()
	p.vlanMu.Lock()
	defer p.vlanMu.Unlock()

	parent, err := validateParentForChild(opts.Parent)
	if err != nil {
		return false, err
	}
	if existing, err := nlLinkByName(name); err == nil {
		return false, vlanVerdict(existing, parent.Attrs().Index, id)
	} else if !isLinkNotFound(err) {
		return false, fmt.Errorf("failed to look up vlan sub-interface %v: %w", name, err)
	}

	la := netlink.NewLinkAttrs()
	la.Name = name
	la.ParentIndex = parent.Attrs().Index
	// The physical parent's gate, the link this adds to; the kind coexists with every child kind (#902).
	guard := p.lockParent(ctx, opts.Parent, parentGateKindVlan, op)
	defer guard.Unlock()
	if err := vlanLinkAdd(guard, &netlink.Vlan{LinkAttrs: la, VlanId: id, VlanProtocol: netlink.VLAN_PROTOCOL_8021Q}); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return false, fmt.Errorf("failed to create vlan sub-interface %v: %w", name, err)
		}
		// Made outside the plugin since the lookup above, so judged like any existing link (#902).
		existing, lerr := nlLinkByName(name)
		if lerr != nil {
			if other := vlanNamedOtherwise(parent.Attrs().Index, id); other != "" {
				return false, fmt.Errorf("%w: vlan %d already exists on %v as %v, and the kernel takes one per parent and ID; use -o parent=%v without vlan, or remove %v",
					util.ErrParentInvalid, id, opts.Parent, other, other, other)
			}
			return false, fmt.Errorf("failed to create vlan sub-interface %v: %w", name, err)
		}
		return false, vlanVerdict(existing, parent.Attrs().Index, id)
	}
	created, err := nlLinkByName(name)
	if err == nil {
		err = nlLinkSetAlias(created, vlanOwnerAlias)
	}
	if err == nil {
		err = vlanHostIPv6Off(name)
	}
	if err == nil {
		err = nlLinkSetUp(created)
	}
	if err != nil {
		if created != nil {
			if derr := nlLinkDel(created); derr != nil {
				log.WithError(derr).WithField("link", name).Warn("Failed to remove a vlan sub-interface after its setup failed")
			}
		}
		return false, fmt.Errorf("failed to set up vlan sub-interface %v: %w", name, err)
	}
	log.WithFields(log.Fields{"link": name, "parent": opts.Parent, "vlan": id, "op": op}).Info("Created vlan sub-interface")
	return true, nil
}

// vlanNamedOtherwise names an 802.1Q vlan of this ID already on the parent under another name, which the kernel's
// EEXIST leaves out, so the refusal points at it (#902).
func vlanNamedOtherwise(parentIndex, id int) string {
	links, err := util.DumpResult(nlLinkList())
	if err != nil {
		return ""
	}
	for _, l := range links {
		if v, ok := l.(*netlink.Vlan); ok && v.ParentIndex == parentIndex && v.VlanId == id && v.VlanProtocol == netlink.VLAN_PROTOCOL_8021Q {
			return v.Attrs().Name
		}
	}
	return ""
}

// vlanHostIPv6Off turns IPv6 off on the host side of a created sub-interface before it comes up, or the host takes a
// link-local, a SLAAC address and an advertised default route on that vlan; the children keep theirs, measured on
// Linux 6.12 (#902). A seam for the unit tests.
var vlanHostIPv6Off = func(name string) error { return disableHostIPv6Under(ipv6DisableSysctlDir, name) }

// disableHostIPv6Under writes on a thread never unlocked, so it exits with its private mount namespace (#868, #902). A
// kernel booted with ipv6.disable=1 has no conf tree and nothing to turn off.
func disableHostIPv6Under(dir, name string) error {
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := makeProcSysWritable(); err != nil {
			log.WithError(err).Debug("Could not make /proc/sys writable; attempting the disable_ipv6 write anyway")
		}
		done <- os.WriteFile(ipv6DisablePath(dir, name), []byte("1\n"), 0o644)
	}()
	return <-done
}

// vlanReleaseRefusal refuses release_lease on a sub-interface the plugin makes or made: a release is sent from the
// host's address on it, and the host has none there, no IPv4 and no IPv6 since the create turns it off (#902, #962).
func vlanReleaseRefusal(opts DHCPNetworkOptions) error {
	if opts.Vlan == "" {
		return nil
	}
	if rl, err := parseReleaseLease(opts.ReleaseLease); err != nil || rl == ReleaseNever {
		return nil
	}
	existing, err := nlLinkByName(opts.linkParent())
	if (err == nil && !vlanOwned(existing)) || (err != nil && !isLinkNotFound(err)) {
		return nil
	}
	return fmt.Errorf("%w: release_lease=%s is refused on %v, a vlan sub-interface this plugin makes: the release is sent from the host's address on it, and the host has none there. Create %v yourself with a host address, or leave release_lease unset",
		util.ErrIPAM, opts.ReleaseLease, opts.linkParent(), opts.linkParent())
}

// vlanUsers names the networks other than self whose children sit on link: this plugin's stored records and every
// macvlan or ipvlan network Docker lists, Docker's own drivers included (#902).
func vlanUsers(link, self string, stored map[string]DHCPNetworkOptions, nets []dNetwork.Summary) []string {
	var users []string
	for id, o := range stored {
		if id != self && o.linkParent() == link {
			users = append(users, shortID(id))
		}
	}
	for _, n := range nets {
		if _, mine := stored[n.ID]; mine || n.ID == self {
			continue
		}
		if _, parent, _, ok := siblingSubMode(n); ok && parent == link {
			users = append(users, n.Name)
		}
	}
	return users
}

// storedNetworkOptions reads every stored record; one that fails to load is skipped, since the Docker list covers it.
func storedNetworkOptions() (map[string]DHCPNetworkOptions, error) {
	ids, err := listStateNetworks()
	if err != nil {
		return nil, err
	}
	out := make(map[string]DHCPNetworkOptions, len(ids))
	for _, id := range ids {
		if o, err := loadOptions(id); err == nil {
			out[id] = o
		}
	}
	return out, nil
}

// stackedOn names the host links whose lower is index; removing the sub-interface would delete them with it,
// measured on Linux 6.12 (#902).
func stackedOn(links []netlink.Link, index int) []string {
	var names []string
	for _, l := range links {
		if l.Attrs().ParentIndex == index && l.Attrs().Index != index {
			names = append(names, l.Attrs().Name)
		}
	}
	return names
}

// retireVlanLink removes the sub-interface when network self was its last user. It keeps the link when it is not
// marked, when a create is in flight, when either user source cannot be read, or when anything sits on it, in this
// namespace or another (#902).
func (p *Plugin) retireVlanLink(ctx context.Context, self string, opts DHCPNetworkOptions, op string) {
	if opts.Vlan == "" {
		return
	}
	name := opts.linkParent()
	p.vlanMu.Lock()
	defer p.vlanMu.Unlock()
	fields := log.Fields{"link": name, "network": shortID(self), "op": op}
	keep := func(why string, extra ...any) {
		log.WithFields(fields).Info(fmt.Sprintf("Keeping vlan sub-interface: "+why, extra...))
	}

	if n := p.vlanPending[name]; n > 0 {
		keep("%d network create(s) in flight on it", n)
		return
	}
	stored, err := storedNetworkOptions()
	if err != nil {
		keep("cannot read the stored networks: %v", err)
		return
	}
	// Bounded, since this runs inside the daemon's own DeleteNetwork call; a list that does not come back keeps the link (#902).
	listCtx, cancel := context.WithTimeout(ctx, vlanListBudget)
	nets, err := p.docker.NetworkList(listCtx, dNetwork.ListOptions{})
	cancel()
	if err != nil {
		keep("cannot list Docker networks: %v", err)
		return
	}
	if users := vlanUsers(name, self, stored, nets); len(users) > 0 {
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
	if !vlanOwned(link) {
		keep("not created by this plugin (alias %q)", link.Attrs().Alias)
		return
	}
	links, err := util.DumpResult(nlLinkList())
	if err != nil {
		keep("cannot list host links: %v", err)
		return
	}
	if names := stackedOn(links, link.Attrs().Index); len(names) > 0 {
		keep("links still sit on it: %v", names)
		return
	}
	// The sub-interface's own gate, the one its children take, held from the trial to the removal (#902).
	guard := p.lockParent(ctx, name, parentGateKindVlan, op)
	defer guard.Unlock()
	if holder, err := vlanOccupied(guard, link); err != nil {
		keep("%s holds it, or the trial child failed: %v", holder, err)
		return
	}
	if err := nlLinkDel(link); err != nil {
		log.WithError(err).WithFields(fields).Warn("Failed to remove vlan sub-interface")
		return
	}
	log.WithFields(fields).Info("Removed vlan sub-interface; its last network is gone")
}

// vlanOccupied adds and removes one macvlan and one ipvlan child on the sub-interface. The kernel refuses one of the two
// while anything holds its rx_handler in any namespace, a container's child included, which no host link list shows,
// measured on Linux 6.12. A vlan stacked on it and moved to another namespace holds none and is not seen (#902).
func vlanOccupied(guard *parentGuard, link netlink.Link) (string, error) {
	for _, trial := range []struct {
		holder string
		build  func(netlink.LinkAttrs) netlink.Link
	}{
		{"an ipvlan child, a macvlan passthru child or a bridge", func(la netlink.LinkAttrs) netlink.Link {
			return &netlink.Macvlan{LinkAttrs: la, Mode: netlink.MACVLAN_MODE_BRIDGE}
		}},
		{"a macvlan or macvtap child", func(la netlink.LinkAttrs) netlink.Link {
			return &netlink.IPVlan{LinkAttrs: la, Mode: netlink.IPVLAN_MODE_L2}
		}},
	} {
		name, err := newProbeLinkName()
		if err != nil {
			return "an unnamed trial", err
		}
		la := netlink.NewLinkAttrs()
		la.Name = name
		la.ParentIndex = link.Attrs().Index
		child := trial.build(la)
		if err := vlanTrialAdd(guard, child); err != nil {
			return trial.holder, err
		}
		if err := nlLinkDel(child); err != nil {
			log.WithError(err).WithField("link", name).Warn("Failed to remove a vlan removal's trial child; the sub-interface's removal takes it along")
		}
	}
	return "", nil
}

// beginVlanCreate counts a CreateNetwork between its ensure and its save, so a failed create's rollback cannot remove
// a link another create has adopted but not stored yet (#902). The returned func ends the count.
func (p *Plugin) beginVlanCreate(opts DHCPNetworkOptions) func() {
	if opts.Vlan == "" {
		return func() {}
	}
	name := opts.linkParent()
	p.vlanMu.Lock()
	if p.vlanPending == nil {
		p.vlanPending = map[string]int{}
	}
	p.vlanPending[name]++
	p.vlanMu.Unlock()
	return func() {
		p.vlanMu.Lock()
		p.vlanPending[name]--
		if p.vlanPending[name] <= 0 {
			delete(p.vlanPending, name)
		}
		p.vlanMu.Unlock()
	}
}
