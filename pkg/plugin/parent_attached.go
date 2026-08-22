// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

// This file implements the macvlan and ipvlan attachment modes. Both
// share the same lifecycle: a child sub-interface is created on a host
// parent NIC, an initial DHCP lease is acquired in the host netns,
// libnetwork moves the link into the container netns. The only
// per-mode difference is the netlink link type and whether the child
// can carry a distinct MAC.
//
// ipvlan support inspired by @LANCommander's fork
// (LANCommander/docker-net-dhcp), which independently added both
// modes side-by-side. Our implementation differs in keeping a separate
// `parent` driver option (instead of overloading `bridge`) and in
// using MAC-based link rediscovery instead of ifindex-based.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// subLinkName returns the host-side child link name for an endpoint.
// Mirrors the prefix used for the bridge-mode veth so existing log/diag
// patterns still apply. Used for both macvlan and ipvlan children.
// Defensive against short IDs (see vethPairNames) — never panics.
func subLinkName(endpointID string) string {
	prefix := endpointID
	if len(endpointID) > 12 {
		prefix = endpointID[:12]
	}
	return "dh-" + prefix
}

// validateParentForChild ensures the parent NIC exists, is up, and is
// itself a suitable parent for a macvlan/ipvlan child (i.e. not already
// a bridge or another macvlan/ipvlan). We do not change the parent's
// state — the host's NIC config is off-limits.
func validateParentForChild(name string) (netlink.Link, error) {
	link, err := nlLinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup parent interface %v: %w", name, err)
	}
	switch link.Type() {
	case "bridge", "macvlan", "macvtap", "ipvlan":
		return nil, fmt.Errorf("%w: %v is %v", util.ErrParentInvalid, name, link.Type())
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("%w: %v", util.ErrParentDown, name)
	}
	return link, nil
}

// newChildLink builds the right netlink.Link for the requested mode.
// macvlan submode is "bridge" so children on the same parent can talk
// to each other. ipvlan submode is L2 so it bridges (rather than
// L3-routes) packets — required for DHCP since DHCP needs L2 broadcast.
func newChildLink(mode string, la netlink.LinkAttrs) netlink.Link {
	if mode == ModeIPvlan {
		return &netlink.IPVlan{LinkAttrs: la, Mode: netlink.IPVLAN_MODE_L2}
	}
	return &netlink.Macvlan{LinkAttrs: la, Mode: netlink.MACVLAN_MODE_BRIDGE}
}

// explainChildLinkAdd turns the kernel's bare EBUSY on child-link
// creation into the sentence that answers it.
//
// macvlan and ipvlan children are MUTUALLY EXCLUSIVE on one parent: both
// claim the parent netdev's single receive handler, and the second kind
// to ask is refused with EBUSY. Many children of the SAME kind are fine,
// and removing the last child of one kind frees the parent for the
// other. Verified directly against the kernel, both directions.
//
// Left as a bare errno this is close to undiagnosable. "failed to create
// ipvlan link: device or resource busy" says nothing about the macvlan
// network next to it, and an operator reading it has no reason to
// suspect a different network is the cause. It cost this project a
// weekly cross-check failure that was first blamed on the runner image
// (#486), and both directions of it are in the CI record: an ipvlan
// endpoint refused while a macvlan child was live, and a macvlan
// validate_dhcp probe refused while an ipvlan child was live.
//
// Deliberately NOT a retry. Where this is teardown lag it would go away
// on its own, but where the operator really is running both kinds on one
// NIC it is permanent, and retrying a permanent condition only delays a
// confusing error — the same trade #486 called out. Naming the cause
// serves both cases: the transient one says what to wait for, the
// permanent one says what to change.
//
// The parent is inspected rather than guessed, so the message can name
// the kind actually in the way. Only reached on the error path.
func explainChildLinkAdd(err error, mode, parent string, parentIndex int) error {
	if !errors.Is(err, unix.EBUSY) {
		return fmt.Errorf("failed to create %v link: %w", mode, err)
	}

	occupant := childLinkKind(parentIndex)
	if occupant == "" || occupant == mode {
		// EBUSY with nothing of the other kind visible: the blocker has
		// already gone (a teardown that completed between the refusal
		// and this lookup) or lives somewhere this scan cannot see.
		// Say what is known and no more.
		return fmt.Errorf("failed to create %v link on %q: %w — the parent would not "+
			"accept another child; if a %v network is being torn down on the same "+
			"parent, retry once it has finished", mode, parent, err, otherChildMode(mode))
	}

	return fmt.Errorf("failed to create %v link on %q: %w — %q already carries %v "+
		"children, and a parent interface can be a macvlan port or an ipvlan port "+
		"but not both. Put the %v network on a different parent interface, or move "+
		"both networks to the same mode",
		mode, parent, err, parent, occupant, mode)
}

// childLinkKind reports the kind of parent-attached child already on
// this parent — "macvlan", "ipvlan", or "" if it carries neither.
func childLinkKind(parentIndex int) string {
	links, err := nlLinkList()
	if err != nil {
		return ""
	}
	for _, l := range links {
		if l.Attrs().ParentIndex != parentIndex {
			continue
		}
		switch l.Type() {
		case "macvlan":
			return ModeMacvlan
		case "ipvlan":
			return ModeIPvlan
		}
	}
	return ""
}

// otherChildMode names the kind that would conflict with this one.
func otherChildMode(mode string) string {
	if mode == ModeIPvlan {
		return ModeMacvlan
	}
	return ModeIPvlan
}

// childLinkUpBudget bounds the wait for a child link's hardware address
// to become free. See linkUpAwaitingAddress.
//
// The thing being waited for is Docker completing a DeleteEndpoint it
// has already begun, which is hundreds of milliseconds. This sits well
// inside the engine's own endpoint-creation patience, so a wait that
// does expire still surfaces as the plugin's error rather than as an
// engine timeout with no explanation.
const childLinkUpBudget = 3 * time.Second

// childLinkUpInterval paces the retries within that budget.
const childLinkUpInterval = 150 * time.Millisecond

// linkUpAwaitingAddress brings a child link up, waiting out the window
// where its hardware address is still held by the link it replaces.
//
// The kernel refuses to bring up a macvlan child whose address is
// already live on the parent — including the parent's own address. On
// restart the plugin deliberately re-applies the previous endpoint's MAC
// (that is how the lease comes back), so if DeleteEndpoint has not yet
// removed the old child, LinkSetUp returns EADDRINUSE and the whole
// restart fails:
//
//	Cannot restart container <id>: failed to set up container networking:
//	  ... failed to set macvlan link up: address already in use
//
// That is a user-visible restart failure, not a degradation (#408). It
// went unseen because the restart tests used containers that ignored
// SIGTERM and so took Docker's full 10s stop grace, by which time the
// old link was long gone. Containers that handle SIGTERM promptly —
// most well-behaved images — restart fast enough to hit it.
//
// The address frees itself once DeleteEndpoint lands, so waiting is the
// fix. There is deliberately NO fallback to a different address: the
// point of re-applying the tombstoned MAC is that this exact address is
// what brings the lease back, and coming up on a different one is the
// failure the feature exists to prevent. If the budget expires, failing
// is correct.
//
// Retrying on the kernel's answer rather than scanning for the old link
// is also deliberate. A child Docker has already moved into a netns that
// is being destroyed still holds the address on the parent's port and
// does not appear in a host-side link list, so a scan would report
// "free" and the LinkSetUp would still fail.
// Returns whether it had to wait — i.e. whether the window this
// function exists for actually arose. A caller with access to the
// health counters records that; the function itself stays free of the
// Plugin so the existing unit tests can call it directly.
//
// Reporting the wait rather than only its failure is the point. This is
// the fix for the release's headline defect and it did its whole job in
// silence: on success after retrying, nothing anywhere recorded that
// the window had been hit (#422). An operator could not tell whether
// their host meets it at all, how often, or whether the budget is close
// to expiring — and neither could we, which is the position #403
// describes for the #406 grace.
func linkUpAwaitingAddress(ctx context.Context, link netlink.Link, budget time.Duration) (bool, error) {
	deadline := time.Now().Add(budget)
	waited := false
	for {
		err := nlLinkSetUp(link)
		if err == nil {
			return waited, nil
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			return waited, err
		}
		// From here on the address was held by the departing link, so
		// whatever happens next, this call met the #408 window.
		waited = true
		if !time.Now().Before(deadline) {
			return waited, fmt.Errorf("%w (the address is still held by the link this one replaces, "+
				"after waiting %v for it to be removed)", err, budget)
		}
		select {
		case <-ctx.Done():
			return waited, fmt.Errorf("%w (last attempt: %w)", ctx.Err(), err)
		case <-time.After(childLinkUpInterval):
		}
	}
}

// noteRestartLinkUpWait records the outcome of a child link-up that met
// the #408 window: the departing link still holding the address.
//
// Split from linkUpAwaitingAddress so that function stays callable
// without a Plugin, and so this decision is directly testable — the
// #431 lesson, where a counter shipped for a release with nothing
// asserting it could move.
//
// Neither counter is healthy-affecting. A successful wait is the fix
// working. A timeout IS a real failure, but it surfaces through
// CreateEndpoint to the operator as `address already in use`; `healthy`
// is for faults that nothing else reports (#422).
func (p *Plugin) noteRestartLinkUpWait(r CreateEndpointRequest, waited bool, err error) {
	if !waited {
		return
	}
	fields := log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"budget":   childLinkUpBudget.String(),
	}
	if err != nil {
		p.restartLinkUpTimeouts.Add(1)
		log.WithError(err).WithFields(fields).
			Warn("Child link never got the address back; the departing link still holds it")
		return
	}
	p.restartLinkUpWaited.Add(1)
	log.WithFields(fields).
		Info("Child link came up after waiting out the departing link's address (#408)")
}

// createParentAttachedEndpoint creates the per-endpoint child link on
// the host's parent NIC (macvlan or ipvlan depending on mode), runs
// dhcpcd on it (still in host netns) to acquire an initial lease, and
// stashes the result for Join. Docker will move the link into the
// container's netns when it acts on our Join response.
func (p *Plugin) createParentAttachedEndpoint(ctx context.Context, r CreateEndpointRequest, opts DHCPNetworkOptions) (CreateEndpointResponse, error) {
	res := CreateEndpointResponse{Interface: &EndpointInterface{}}
	mode := opts.effectiveMode()

	parent, err := validateParentForChild(opts.Parent)
	if err != nil {
		return res, err
	}

	// MAC/IP selection: explicit > tombstone > kernel-picked /
	// server-picked. ipvlan children share the parent's MAC and ignore
	// HardwareAddr, so the tombstone path doesn't apply there (and an
	// explicit MAC is rejected loudly to avoid silent misconfiguration).
	// Static IPs (`docker run --ip`) are accepted in both modes — they
	// pass through to dhcpcd as a `request`-directive (DHCP option 50)
	// hint.
	effectiveMAC := ""
	if r.Interface != nil {
		effectiveMAC = r.Interface.MacAddress
	}
	explicitV4, err := resolveExplicitV4(r)
	if err != nil {
		return res, err
	}
	explicitV6, err := resolveExplicitV6(r)
	if err != nil {
		return res, err
	}
	// Look up the hostname up front so we can scope tombstone matching
	// to the same container (prevents identity swap during sequential
	// `compose restart`). Best-effort: if the lookup misses or returns
	// empty, consumeTombstone falls back to network-only matching.
	hostname := p.initialDHCPHostname(ctx, r.NetworkID, r.EndpointID)

	requestedIP := explicitV4
	requestedV6 := explicitV6
	if mode == ModeMacvlan && effectiveMAC == "" {
		if tombMAC, tombIP, tombIPv6, ok := p.consumeTombstone(r.NetworkID, hostname); ok {
			effectiveMAC = tombMAC
			if requestedIP == "" {
				requestedIP = tombIP
			}
			// Inherit the prior v6 as the DHCPv6 preferred address too,
			// so a restarting macvlan container keeps its v6 lease the
			// same way it keeps v4 (#213).
			if requestedV6 == "" {
				requestedV6 = tombIPv6
			}
			log.WithFields(log.Fields{
				"network":  shortID(r.NetworkID),
				"endpoint": shortID(r.EndpointID),
				"hostname": hostname,
			}).Info("Inherited MAC/IP from recent endpoint on same network (likely container restart)")
			log.WithFields(log.Fields{
				"network":      shortID(r.NetworkID),
				"endpoint":     shortID(r.EndpointID),
				"mac_address":  tombMAC,
				"requested_ip": requestedIP,
				"prior_ipv6":   tombIPv6,
			}).Debug("Tombstone inheritance details")
		}
	}

	la := netlink.NewLinkAttrs()
	la.Name = subLinkName(r.EndpointID)
	la.ParentIndex = parent.Attrs().Index
	if effectiveMAC != "" {
		// ipvlan children share the parent's MAC by design; libnetwork
		// passing us a custom MAC would silently get ignored, so we
		// fail loudly instead. (Tombstones are filtered out above for
		// ipvlan, so reaching this branch in ipvlan mode means the
		// caller really did request a custom MAC.)
		if mode == ModeIPvlan {
			return res, fmt.Errorf("%w: ipvlan does not support a custom MAC address (children share the parent's MAC)", util.ErrMACAddress)
		}
		mac, err := net.ParseMAC(effectiveMAC)
		if err != nil {
			return res, util.ErrMACAddress
		}
		la.HardwareAddr = mac
	}
	link := newChildLink(mode, la)

	// Queue behind anything else holding this parent — in practice an
	// orphan-lease reclaim, whose temporary link is created in a
	// goroutine ordered against nothing (#549). Held across the LinkAdd
	// only: two endpoints on the same parent contend for microseconds,
	// and it is the reclaim's multi-second DORA this exists to wait out.
	guard := p.lockParent(ctx, opts.Parent, "create_endpoint")
	err = addChildLink(guard, link)
	guard.Unlock()
	if err != nil {
		return res, explainChildLinkAdd(err, mode, opts.Parent, parent.Attrs().Index)
	}

	if err := func() error {
		// Reload to pick up the kernel-assigned MAC (macvlan) or the
		// inherited parent MAC (ipvlan) if we didn't set one.
		fresh, err := netlink.LinkByName(la.Name)
		if err != nil {
			return fmt.Errorf("failed to re-fetch %v link: %w", mode, err)
		}
		mac := fresh.Attrs().HardwareAddr

		// Pin the kernel-assigned MAC (macvlan only — ipvlan rejects
		// any MAC set with EOPNOTSUPP, and its children share the
		// parent's MAC anyway). The bridge path has pinned its veth
		// MACs for ages; macvlan never did, and the gap finally
		// surfaced (#103): systemd-udevd's MACAddressPolicy=persistent
		// (Debian default) replaces a *randomly assigned* MAC moments
		// after link creation, so the initial DHCP exchange ran from
		// udev's MAC while the link-local kept deriving from ours —
		// and libnetwork re-applies our reported MAC at Join. v4
		// survives by client-id matching, but the one-shot DHCPv6
		// poisons the server's neighbor cache (link-local -> udev's
		// MAC), blackholing the container's persistent client for the
		// cache lifetime (~45s on the wire capture). Explicitly
		// setting the MAC — even to its current value — flips
		// addr_assign_type to "set", which the udev policy respects.
		if mode != ModeIPvlan && effectiveMAC == "" {
			if err := netlink.LinkSetHardwareAddr(fresh, mac); err != nil {
				return fmt.Errorf("failed to pin %v link MAC: %w", mode, err)
			}
		}

		waited, err := linkUpAwaitingAddress(ctx, fresh, childLinkUpBudget)
		p.noteRestartLinkUpWait(r, waited, err)
		if err != nil {
			return fmt.Errorf("failed to set %v link up: %w", mode, err)
		}

		// libnetwork applies res.Interface.MacAddress to the link
		// during Join via netlink LinkSetHardwareAddr. The ipvlan
		// driver rejects any MAC change (slaves share the parent's
		// MAC by kernel design), even setting to the current value,
		// with EOPNOTSUPP. So we skip the MAC response entirely for
		// ipvlan; libnetwork leaves the link's MAC as-is, and
		// docker inspect picks the inherited MAC up via netlink
		// after Join finishes.
		if mode != ModeIPvlan && (r.Interface == nil || r.Interface.MacAddress == "") {
			res.Interface.MacAddress = mac.String()
		}

		timeout := defaultLeaseTimeout
		if opts.LeaseTimeout != 0 {
			timeout = opts.LeaseTimeout
		}
		// Client-id from the MAC for macvlan (tombstone-preserved, so the
		// IPv4 lease survives a restart) and from the endpoint ID for
		// ipvlan, whose slaves all share the parent MAC and so need
		// something that tells them apart — see resolveClientID (#371).
		// hostname was resolved earlier for tombstone matching and is
		// reused for the DHCP option 12 hint here. Operator-supplied
		// client_id overrides the derived value.
		clientID := resolveClientID(opts, r.EndpointID, mac)

		runDHCP := func(v6 bool) error {
			v6str := ""
			if v6 {
				v6str = "v6"
			}

			// Server preference ladder (#111) / deny-list (#669),
			// through the same helper the bridge path uses so the two
			// cannot drift.
			pol, err := resolveServerPolicy(opts)
			if err != nil {
				return err
			}

			base := dhcp.DHCPClientOptions{
				// .name, not the whole value: see the sibling in
				// network.go. Config, not identity.
				Hostname:    hostname.name,
				FQDN:        opts.fqdnMode(),
				ClientID:    clientID,
				VendorClass: opts.VendorClass,
				Broadcast:   mode == ModeIPvlan,
				// MAC pins the dhcpcd DUID-LL/IAID so the one-shot and
				// persistent clients share one identity (#152). NOTE:
				// ipvlan-L2 slaves share the parent MAC, so v6 identity
				// is not unique per endpoint in that mode — a known
				// limitation for ipvlan+ipv6 (bridge/macvlan have unique,
				// tombstone-preserved MACs).
				MAC: mac,
			}
			if v6 {
				base.PreferredV6 = requestedV6
			} else {
				base.RequestedIP = requestedIP
			}

			info, err := p.acquireWithPolicy(ctx, la.Name, pol, v6, timeout, r.EndpointID, base)
			if err != nil {
				return fmt.Errorf("failed to get initial IP%v address via DHCP%v: %w", v6str, v6str, err)
			}
			addr, err := netlink.ParseAddr(info.IP)
			if err != nil {
				return fmt.Errorf("failed to parse initial IP%v address: %w", v6str, err)
			}

			p.updateJoinHint(r.EndpointID, func(hint *joinHint) {
				hint.MacAddress = mac
				if v6 {
					res.Interface.AddressIPv6 = info.IP
					hint.IPv6 = addr
				} else {
					res.Interface.Address = info.IP
					hint.IPv4 = addr
					hint.Gateway = info.Gateway
					if opts.Gateway != "" {
						hint.Gateway = opts.Gateway
					}
					// DHCP option-121 classless static routes (RFC 3442);
					// any default route was already folded into
					// info.Gateway by the parser.
					hint.Routes = dhcpStaticRoutes(info.Routes)
				}
			})
			return nil
		}

		if err := runDHCP(false); err != nil {
			return err
		}
		if opts.IPv6 {
			if err := runDHCP(true); err != nil {
				return err
			}
		}
		return nil
	}(); err != nil {
		// Roll back the child link if anything after LinkAdd failed.
		// Best-effort: if LinkDel itself fails the kernel will reap the
		// link with the netns soon enough.
		_ = netlink.LinkDel(link)
		return res, err
	}

	var hintMAC, hintGW, hintIPv4, hintIPv6 string
	p.updateJoinHint(r.EndpointID, func(h *joinHint) {
		hintMAC = h.MacAddress.String()
		hintGW = h.Gateway
		if h.IPv4 != nil {
			hintIPv4 = h.IPv4.IP.String()
		}
		if h.IPv6 != nil {
			hintIPv6 = h.IPv6.IP.String()
		}
	})

	// Remember the chosen MAC and IPs so DeleteEndpoint can stash
	// them as a tombstone. macvlan only — for ipvlan the MAC is the
	// parent's and there's nothing to stabilize.
	if mode == ModeMacvlan {
		p.rememberEndpoint(r.EndpointID, endpointFingerprint{MAC: hintMAC, IPv4: hintIPv4, IPv6: hintIPv6, Ifname: p.hintIfname(r.EndpointID)}, hostname)
	}

	// Is anyone else already using the address we were just given
	// (#524)? Asynchronous on purpose: the answer changes no part of
	// this response, and keeping it off the critical path is what lets
	// dhcpcd keep `-A` and its acquisition deadline. The probe runs on
	// the parent link in the host namespace, so it cannot be raced by
	// Docker moving the child into the container's netns.
	if res.Interface.Address != "" {
		go p.checkAddressConflict(opts.Parent, res.Interface.Address, hintMAC, r.EndpointID, r.NetworkID)
	}

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"mode":     mode,
		"parent":   opts.Parent,
	}).Info("Endpoint created")
	log.WithFields(log.Fields{
		"network":     shortID(r.NetworkID),
		"endpoint":    shortID(r.EndpointID),
		"mac_address": hintMAC,
		"ip":          res.Interface.Address,
		"ipv6":        res.Interface.AddressIPv6,
		"gateway":     hintGW,
	}).Debug("Endpoint details")

	return res, nil
}

// deleteParentAttachedEndpoint best-effort cleans up the host-side
// child link. Once Docker has moved the link into the container netns
// the host can no longer see it, and the kernel removes it when the
// netns dies — so a "not found" here is the normal happy path. We
// only delete when the link is still in our netns (e.g. CreateEndpoint
// failed mid-way or Join was never called). Same code handles macvlan
// and ipvlan since they live under the same name.
func (p *Plugin) deleteParentAttachedEndpoint(r DeleteEndpointRequest) error {
	name := subLinkName(r.EndpointID)
	link, err := nlLinkByName(name)
	if err != nil {
		// Expected: the link is gone with the container netns.
		log.WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
		}).Debug("Child link already gone (expected)")
		return nil
	}
	if err := nlLinkDel(link); err != nil {
		return fmt.Errorf("failed to delete leftover child link %v: %w", name, err)
	}
	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
	}).Info("Cleaned up leftover child link in host netns")
	return nil
}

// findLinkByMAC walks the link table behind `handle` (typically the
// container's netns handle) and returns the link with the given hardware
// address. Used to re-discover a macvlan child after Docker has moved and
// renamed it inside the container.
func findLinkByMAC(handle linkLister, mac net.HardwareAddr) (netlink.Link, error) {
	links, err := handle.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list links: %w", err)
	}
	for _, l := range links {
		if bytes.Equal(l.Attrs().HardwareAddr, mac) {
			return l, nil
		}
	}
	return nil, fmt.Errorf("no link with MAC %v", mac)
}

// parentAttachedOperInfo is what we hand back to libnetwork in
// EndpointOperInfo for both macvlan and ipvlan endpoints.
type parentAttachedOperInfo struct {
	Mode     string `mapstructure:"mode"`
	Parent   string `mapstructure:"parent"`
	HostLink string `mapstructure:"sub_link_host"`
	LinkMAC  string `mapstructure:"sub_link_mac"`
}

func (p *Plugin) parentAttachedEndpointOperInfo(opts DHCPNetworkOptions, r InfoRequest) (InfoResponse, error) {
	res := InfoResponse{}
	name := subLinkName(r.EndpointID)

	info := parentAttachedOperInfo{
		Mode:     opts.effectiveMode(),
		Parent:   opts.Parent,
		HostLink: name,
	}
	// The link is in the container netns by the time anyone polls this, so
	// "not found" is expected and not an error.
	if link, err := netlink.LinkByName(name); err == nil {
		info.LinkMAC = link.Attrs().HardwareAddr.String()
	}
	if err := mapstructure.Decode(info, &res.Value); err != nil {
		return res, fmt.Errorf("failed to encode oper info: %w", err)
	}
	return res, nil
}
