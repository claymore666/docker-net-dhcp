package plugin

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"

	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/devplayer0/docker-net-dhcp/pkg/udhcpc"
	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

// CLIOptionsKey is the key used in create network options by the CLI for custom options
const CLIOptionsKey string = "com.docker.network.generic"

// Implementations of the endpoints described in
// https://github.com/moby/libnetwork/blob/master/docs/remote.md

// validateIPAMData enforces the null-IPAM-driver requirement that
// libnetwork passes us via the IPv4Data slice.
func validateIPAMData(ipv4 []*IPAMData) error {
	for _, d := range ipv4 {
		if d.AddressSpace != "null" || d.Pool != "0.0.0.0/0" {
			return util.ErrIPAM
		}
	}
	return nil
}

// validateModeOptions performs the pure-Go subset of CreateNetwork's
// validation: mode value, and which other options are required or
// forbidden for that mode. It does NOT touch netlink or the docker
// API; the kernel-facing checks (parent NIC up, bridge type, address
// conflicts) are layered on top in CreateNetwork itself.
//
// Returning an error wrapped with fmt.Errorf preserves errors.Is so
// the HTTP layer can map sentinels to 400 status codes.
func validateModeOptions(opts DHCPNetworkOptions) error {
	switch opts.effectiveMode() {
	case ModeMacvlan, ModeIPvlan:
		if opts.Parent == "" {
			return util.ErrParentRequired
		}
		if opts.Bridge != "" {
			return fmt.Errorf("%w: bridge cannot be set in mode=%v", util.ErrModeMismatch, opts.effectiveMode())
		}
	case ModeBridge:
		if opts.Bridge == "" {
			return util.ErrBridgeRequired
		}
		if opts.Parent != "" {
			return fmt.Errorf("%w: parent cannot be set in mode=bridge", util.ErrModeMismatch)
		}
	default:
		return fmt.Errorf("%w: %q", util.ErrInvalidMode, opts.Mode)
	}
	return nil
}

// CreateNetwork validates network creation: option shape (pure), then
// existence of the parent interface (bridge or NIC depending on mode),
// the null IPAM driver requirement, and — for bridge mode — that no
// other Docker network already owns this bridge's address space.
func (p *Plugin) CreateNetwork(r CreateNetworkRequest) error {
	log.WithField("options", r.Options).Debug("CreateNetwork options")

	opts, err := decodeOpts(r.Options[util.OptionsKeyGeneric])
	if err != nil {
		return fmt.Errorf("failed to decode network options: %w", err)
	}

	if err := validateIPAMData(r.IPv4Data); err != nil {
		return err
	}

	if err := validateModeOptions(opts); err != nil {
		return err
	}

	if mode := opts.effectiveMode(); mode == ModeMacvlan || mode == ModeIPvlan {
		if _, err := validateParentForChild(opts.Parent); err != nil {
			return err
		}
		if err := saveOptions(r.NetworkID, opts); err != nil {
			log.WithError(err).WithField("network", r.NetworkID).
				Warn("Failed to persist options; daemon-restart may need API fallback")
		}
		log.WithFields(log.Fields{
			"network": r.NetworkID,
			"mode":    mode,
			"parent":  opts.Parent,
			"ipv6":    opts.IPv6,
		}).Info("Network created")
		return nil
	}

	// Bridge mode: pure validation already passed; do the kernel-facing
	// and docker-API-facing checks.
	link, err := netlink.LinkByName(opts.Bridge)
	if err != nil {
		return fmt.Errorf("failed to lookup interface %v: %w", opts.Bridge, err)
	}
	if link.Type() != "bridge" {
		return util.ErrNotBridge
	}

	if !opts.IgnoreConflicts {
		v4Addrs, err := netlink.AddrList(link, unix.AF_INET)
		if err != nil {
			return fmt.Errorf("failed to retrieve IPv4 addresses for %v: %w", opts.Bridge, err)
		}
		v6Addrs, err := netlink.AddrList(link, unix.AF_INET6)
		if err != nil {
			return fmt.Errorf("failed to retrieve IPv6 addresses for %v: %w", opts.Bridge, err)
		}
		bridgeAddrs := append(v4Addrs, v6Addrs...)

		nets, err := p.docker.NetworkList(context.Background(), dNetwork.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to retrieve list of networks from Docker: %w", err)
		}

		// Make sure the addresses on this bridge aren't used by another network
		for _, n := range nets {
			if IsDHCPPlugin(n.Driver) {
				otherOpts, err := decodeOpts(n.Options)
				if err != nil {
					log.
						WithField("network", n.Name).
						WithError(err).
						Warn("Failed to parse other DHCP network's options")
				} else if otherOpts.Bridge == opts.Bridge {
					return util.ErrBridgeUsed
				}
			}
			if n.IPAM.Driver == "null" {
				// Null driver networks will have 0.0.0.0/0 which covers any address range!
				continue
			}

			for _, c := range n.IPAM.Config {
				_, dockerCIDR, err := net.ParseCIDR(c.Subnet)
				if err != nil {
					return fmt.Errorf("failed to parse subnet %v on Docker network %v: %w", c.Subnet, n.ID, err)
				}
				if bytes.Equal(dockerCIDR.Mask, net.CIDRMask(0, 32)) || bytes.Equal(dockerCIDR.Mask, net.CIDRMask(0, 128)) {
					// Last check to make sure the network isn't 0.0.0.0/0 or ::/0 (which would always pass the check below)
					continue
				}

				for _, bridgeAddr := range bridgeAddrs {
					if bridgeAddr.IPNet.Contains(dockerCIDR.IP) || dockerCIDR.Contains(bridgeAddr.IP) {
						return util.ErrBridgeUsed
					}
				}
			}
		}
	}

	if err := saveOptions(r.NetworkID, opts); err != nil {
		log.WithError(err).WithField("network", r.NetworkID).
			Warn("Failed to persist options; daemon-restart may need API fallback")
	}
	log.WithFields(log.Fields{
		"network": r.NetworkID,
		"bridge":  opts.Bridge,
		"ipv6":    opts.IPv6,
	}).Info("Network created")

	return nil
}

// DeleteNetwork "deletes" a DHCP network (does nothing, the bridge is managed by the user)
func (p *Plugin) DeleteNetwork(r DeleteNetworkRequest) error {
	if err := deleteOptions(r.NetworkID); err != nil {
		log.WithError(err).WithField("network", r.NetworkID).
			Warn("Failed to remove persisted options; harmless leftover")
	}
	log.WithField("network", r.NetworkID).Info("Network deleted")
	return nil
}

func vethPairNames(id string) (string, string) {
	return "dh-" + id[:12], id[:12] + "-dh"
}

// parseExplicitV4 extracts the bare IPv4 address from an optional
// libnetwork-supplied Interface.Address (CIDR form, e.g. set by
// `docker run --ip=192.168.0.50`). Returns "" when the field is
// absent; an ErrIPAM-wrapped error when set but malformed or v6.
// The bare-IP form is what udhcpc wants for `-r ADDR`; the mask is
// supplied by the DHCP ACK, not the operator.
//
// Note: docker-engine itself rejects `--ip` for null-IPAM networks,
// so this path only fires when the operator has wired up a non-null
// IPAM driver, or when libnetwork synthesises an Interface.Address
// from elsewhere. The driver-opt path (`--driver-opt ip=...`) is the
// realistic UX for static-IP requests on this plugin's networks; see
// parseDriverOptIP.
func parseExplicitV4(iface *EndpointInterface) (string, error) {
	if iface == nil || iface.Address == "" {
		return "", nil
	}
	addr, err := netlink.ParseAddr(iface.Address)
	if err != nil {
		return "", fmt.Errorf("invalid Interface.Address %q (want CIDR): %w", iface.Address, util.ErrIPAM)
	}
	if addr.IP.To4() == nil {
		return "", fmt.Errorf("Interface.Address must be IPv4: got %q: %w", iface.Address, util.ErrIPAM)
	}
	return addr.IP.String(), nil
}

// resolveExplicitV4 collects an explicit IPv4 from either of the two
// libnetwork channels: Interface.Address (from `docker run --ip`) or
// the `ip` driver-opt (from `docker network connect --driver-opt
// ip=...`). Returns "" when neither is set, an error when both are
// set to different values, and the agreed value otherwise.
func resolveExplicitV4(r CreateEndpointRequest) (string, error) {
	fromIface, err := parseExplicitV4(r.Interface)
	if err != nil {
		return "", err
	}
	fromOpt, err := parseDriverOptIP(r.Options)
	if err != nil {
		return "", err
	}
	if fromIface != "" && fromOpt != "" && fromIface != fromOpt {
		return "", fmt.Errorf("conflicting static IP: --ip=%q vs --driver-opt ip=%q: %w", fromIface, fromOpt, util.ErrIPAM)
	}
	if fromIface != "" {
		return fromIface, nil
	}
	return fromOpt, nil
}

// parseDriverOptIP extracts the bare IPv4 address from an optional
// `ip` driver-option. libnetwork places per-endpoint driver-opts
// (from `docker network connect --driver-opt KEY=VAL`) as flat keys
// in r.Options. Bare-IP form here, since that's how operators type
// it on the command line; netmask comes from DHCP regardless. A v4
// driver-opt `ip6` is also accepted but currently logged-and-skipped
// because busybox udhcpc6 has no equivalent of `-r`.
func parseDriverOptIP(options map[string]interface{}) (string, error) {
	raw, ok := options["ip"]
	if !ok {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("invalid driver-opt ip %v: expected non-empty string: %w", raw, util.ErrIPAM)
	}
	parsed := net.ParseIP(s)
	if parsed == nil {
		return "", fmt.Errorf("invalid driver-opt ip %q (want bare IPv4): %w", s, util.ErrIPAM)
	}
	v4 := parsed.To4()
	if v4 == nil {
		return "", fmt.Errorf("driver-opt ip must be IPv4: got %q: %w", s, util.ErrIPAM)
	}
	return v4.String(), nil
}

// netOptions returns the decoded options for a network, preferring the
// on-disk cache populated by CreateNetwork. The fallback to docker
// NetworkInspect is what makes existing networks (created before this
// fork added persistence) keep working after upgrade — but every fresh
// network has its options served from disk, which is what avoids the
// daemon-restart deadlock when dockerd is calling our endpoint
// handlers while not yet ready to serve API calls.
func (p *Plugin) netOptions(ctx context.Context, id string) (DHCPNetworkOptions, error) {
	if opts, err := loadOptions(id); err == nil {
		return opts, nil
	} else if !os.IsNotExist(err) {
		log.WithError(err).WithField("network", id).
			Warn("Failed to load persisted options; falling back to docker API")
	}

	dummy := DHCPNetworkOptions{}

	n, err := p.docker.NetworkInspect(ctx, id, dNetwork.InspectOptions{})
	if err != nil {
		return dummy, fmt.Errorf("failed to get info from Docker: %w", err)
	}

	opts, err := decodeOpts(n.Options)
	if err != nil {
		return dummy, fmt.Errorf("failed to parse options: %w", err)
	}

	// Backfill: persist options for networks that pre-date the
	// persistence feature so the next call hits the disk path.
	if err := saveOptions(id, opts); err != nil {
		log.WithError(err).WithField("network", id).
			Debug("Failed to backfill persisted options")
	}
	return opts, nil
}

// CreateEndpoint creates the per-endpoint host-side network plumbing
// (veth pair in bridge mode, macvlan child in macvlan mode), runs udhcpc
// once to acquire an initial lease, and stashes the result for Join.
// Docker moves the link into the container's netns when it acts on our
// Join response.
func (p *Plugin) CreateEndpoint(ctx context.Context, r CreateEndpointRequest) (CreateEndpointResponse, error) {
	log.WithField("options", r.Options).Debug("CreateEndpoint options")
	res := CreateEndpointResponse{
		Interface: &EndpointInterface{},
	}

	// libnetwork passes Interface.AddressIPv6 when the user supplied
	// `--ip6` on `docker run`. We don't yet wire that through to
	// udhcpc6 (RequestedIP is v4-only in busybox), so honor it on a
	// best-effort basis: warn loudly but don't fail the endpoint.
	if r.Interface != nil && r.Interface.AddressIPv6 != "" {
		log.WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
			"ipv6":     r.Interface.AddressIPv6,
		}).Warn("Static IPv6 address requested but not yet wired through to udhcpc6; lease will come unhinted")
	}

	explicitV4, err := resolveExplicitV4(r)
	if err != nil {
		return res, err
	}

	opts, err := p.netOptions(ctx, r.NetworkID)
	if err != nil {
		return res, fmt.Errorf("failed to get network options: %w", err)
	}

	if m := opts.effectiveMode(); m == ModeMacvlan || m == ModeIPvlan {
		return p.createParentAttachedEndpoint(ctx, r, opts)
	}

	bridge, err := netlink.LinkByName(opts.Bridge)
	if err != nil {
		return res, fmt.Errorf("failed to get bridge interface: %w", err)
	}

	// MAC/IP selection priority:
	//   1. Explicit values from libnetwork (`--mac-address`, `--ip`)
	//   2. Tombstone (recently-deleted endpoint on the same network)
	//   3. Kernel-picked MAC, server-picked IP
	// Tombstones are only consumed when no explicit MAC was supplied
	// — explicit MAC means the operator is taking responsibility for
	// identity, and we don't want to surprise-mix in a stale neighbor.
	effectiveMAC := r.Interface.MacAddress
	requestedIP := explicitV4
	if effectiveMAC == "" {
		if mac, ip, ipv6, ok := p.consumeTombstone(r.NetworkID); ok {
			effectiveMAC = mac
			if requestedIP == "" {
				requestedIP = ip
			}
			// IPv6 from the tombstone is logged but not yet wired to
			// udhcpc6 (busybox has no `-r` equivalent for v6). The
			// data is preserved through the full lifecycle so a
			// future change can request preferred-address from a
			// DHCPv6 client without a wire-format change.
			log.WithFields(log.Fields{
				"network":      shortID(r.NetworkID),
				"endpoint":     shortID(r.EndpointID),
				"mac_address":  mac,
				"requested_ip": requestedIP,
				"prior_ipv6":   ipv6,
			}).Info("Inherited MAC/IP from recent endpoint on same network (likely container restart)")
		}
	}

	hostName, ctrName := vethPairNames(r.EndpointID)
	la := netlink.NewLinkAttrs()
	la.Name = hostName
	hostLink := &netlink.Veth{
		LinkAttrs: la,
		PeerName:  ctrName,
	}
	if effectiveMAC != "" {
		addr, err := net.ParseMAC(effectiveMAC)
		if err != nil {
			return res, util.ErrMACAddress
		}

		hostLink.PeerHardwareAddr = addr
	}

	if err := netlink.LinkAdd(hostLink); err != nil {
		return res, fmt.Errorf("failed to create veth pair: %w", err)
	}
	if err := func() error {
		if err := netlink.LinkSetUp(hostLink); err != nil {
			return fmt.Errorf("failed to set host side link of veth pair up: %w", err)
		}

		ctrLink, err := netlink.LinkByName(ctrName)
		if err != nil {
			return fmt.Errorf("failed to find container side of veth pair: %w", err)
		}
		if err := netlink.LinkSetUp(ctrLink); err != nil {
			return fmt.Errorf("failed to set container side link of veth pair up: %w", err)
		}

		// Pin the container-side MAC. The kernel will often reset a
		// randomly assigned MAC after actions like LinkSetMaster, and
		// we need it to stay the value we (or the tombstone) chose.
		if effectiveMAC == "" {
			if err := netlink.LinkSetHardwareAddr(ctrLink, ctrLink.Attrs().HardwareAddr); err != nil {
				return fmt.Errorf("failed to set container side of veth pair's MAC address: %w", err)
			}
		}
		// Tell libnetwork the MAC iff it didn't tell us. The
		// tombstone-inherited case falls into this branch — libnetwork
		// passed an empty MAC and we picked one, so docker inspect
		// needs us to surface it. For the libnetwork-provided case,
		// res.Interface.MacAddress stays empty (signals "we kept what
		// you sent").
		if r.Interface.MacAddress == "" {
			res.Interface.MacAddress = ctrLink.Attrs().HardwareAddr.String()
		}

		if err := netlink.LinkSetMaster(hostLink, bridge); err != nil {
			return fmt.Errorf("failed to attach host side link of veth peer to bridge: %w", err)
		}

		timeout := defaultLeaseTimeout
		if opts.LeaseTimeout != 0 {
			timeout = opts.LeaseTimeout
		}
		// Best-effort hostname for the initial DISCOVER. Empty if the
		// container isn't yet registered with this network — the
		// persistent renewal client will fill it in later.
		hostname := p.initialDHCPHostname(ctx, r.NetworkID, r.EndpointID)
		clientID := clientIDFromEndpoint(r.EndpointID)
		initialIP := func(v6 bool) error {
			v6str := ""
			if v6 {
				v6str = "v6"
			}

			timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			clientOpts := &udhcpc.DHCPClientOptions{
				V6:       v6,
				Hostname: hostname,
				ClientID: clientID,
			}
			// RequestedIP is v4-only in udhcpc; passing it for v6
			// would be silently ignored anyway, but keep it explicit.
			if !v6 {
				clientOpts.RequestedIP = requestedIP
			}
			info, err := udhcpc.GetIP(timeoutCtx, ctrName, clientOpts)
			if err != nil {
				return fmt.Errorf("failed to get initial IP%v address via DHCP%v: %w", v6str, v6str, err)
			}
			ip, err := netlink.ParseAddr(info.IP)
			if err != nil {
				return fmt.Errorf("failed to parse initial IP%v address: %w", v6str, err)
			}

			p.updateJoinHint(r.EndpointID, func(hint *joinHint) {
				if v6 {
					res.Interface.AddressIPv6 = info.IP
					hint.IPv6 = ip
					// No gateways in DHCPv6!
				} else {
					res.Interface.Address = info.IP
					hint.IPv4 = ip
					hint.Gateway = info.Gateway
					if opts.Gateway != "" {
						hint.Gateway = opts.Gateway
					}
				}
			})

			return nil
		}

		if err := initialIP(false); err != nil {
			return err
		}
		if opts.IPv6 {
			if err := initialIP(true); err != nil {
				return err
			}
		}

		return nil
	}(); err != nil {
		// Be sure to clean up the veth pair if any of this fails.
		// Best-effort cleanup; ignore secondary error.
		_ = netlink.LinkDel(hostLink)
		return res, err
	}

	gateway := ""
	var v4IP, v6IP string
	p.updateJoinHint(r.EndpointID, func(h *joinHint) {
		gateway = h.Gateway
		if h.IPv4 != nil {
			v4IP = h.IPv4.IP.String()
		}
		if h.IPv6 != nil {
			v6IP = h.IPv6.IP.String()
		}
	})

	// Remember the chosen MAC and IPs so DeleteEndpoint can stash
	// them as a tombstone for the next CreateEndpoint on the same
	// network.
	mac := r.Interface.MacAddress
	if mac == "" {
		mac = res.Interface.MacAddress
	}
	p.rememberEndpoint(r.EndpointID, endpointFingerprint{MAC: mac, IPv4: v4IP, IPv6: v6IP})

	log.WithFields(log.Fields{
		"network":     shortID(r.NetworkID),
		"endpoint":    shortID(r.EndpointID),
		"mac_address": mac,
		"ip":          res.Interface.Address,
		"ipv6":        res.Interface.AddressIPv6,
		"gateway":     gateway,
	}).Info("Endpoint created")

	return res, nil
}

type operInfo struct {
	Bridge      string `mapstructure:"bridge"`
	HostVEth    string `mapstructure:"veth_host"`
	HostVEthMAC string `mapstructure:"veth_host_mac"`
}

// EndpointOperInfo retrieves some info about an existing endpoint
func (p *Plugin) EndpointOperInfo(ctx context.Context, r InfoRequest) (InfoResponse, error) {
	res := InfoResponse{}

	opts, err := p.netOptions(ctx, r.NetworkID)
	if err != nil {
		return res, fmt.Errorf("failed to get network options: %w", err)
	}

	if m := opts.effectiveMode(); m == ModeMacvlan || m == ModeIPvlan {
		return p.parentAttachedEndpointOperInfo(opts, r)
	}

	hostName, _ := vethPairNames(r.EndpointID)
	hostLink, err := netlink.LinkByName(hostName)
	if err != nil {
		return res, fmt.Errorf("failed to find host side of veth pair: %w", err)
	}

	info := operInfo{
		Bridge:      opts.Bridge,
		HostVEth:    hostName,
		HostVEthMAC: hostLink.Attrs().HardwareAddr.String(),
	}
	if err := mapstructure.Decode(info, &res.Value); err != nil {
		return res, fmt.Errorf("failed to encode OperInfo: %w", err)
	}

	return res, nil
}

// DeleteEndpoint deletes the host-side network plumbing for an endpoint.
// In bridge mode that's the veth pair (deleting one side removes the
// peer). In macvlan mode the link has typically already been moved into
// the container netns and reaped with it, so cleanup is best-effort.
func (p *Plugin) DeleteEndpoint(ctx context.Context, r DeleteEndpointRequest) error {
	opts, err := p.netOptions(ctx, r.NetworkID)
	if err != nil {
		return fmt.Errorf("failed to get network options: %w", err)
	}

	// Lay down a tombstone for the next CreateEndpoint on this
	// network to inherit. ipvlan children share the parent MAC, so
	// the tombstone is meaningless there (we'd just be re-handing the
	// parent MAC back, which the kernel inherits anyway) — skip it.
	if fp, ok := p.takeEndpoint(r.EndpointID); ok && opts.effectiveMode() != ModeIPvlan {
		p.addTombstone(r.NetworkID, fp.MAC, fp.IPv4, fp.IPv6)
	}

	if m := opts.effectiveMode(); m == ModeMacvlan || m == ModeIPvlan {
		if err := p.deleteParentAttachedEndpoint(r); err != nil {
			return err
		}
		log.WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
		}).Info("Endpoint deleted")
		return nil
	}

	hostName, _ := vethPairNames(r.EndpointID)
	link, err := netlink.LinkByName(hostName)
	if err != nil {
		return fmt.Errorf("failed to lookup host veth interface %v: %w", hostName, err)
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("failed to delete veth pair: %w", err)
	}

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
	}).Info("Endpoint deleted")

	return nil
}

func (p *Plugin) addRoutes(opts *DHCPNetworkOptions, v6 bool, bridge netlink.Link, r JoinRequest, hint joinHint, res *JoinResponse) error {
	family := unix.AF_INET
	if v6 {
		family = unix.AF_INET6
	}

	routes, err := netlink.RouteListFiltered(family, &netlink.Route{
		LinkIndex: bridge.Attrs().Index,
		Type:      unix.RTN_UNICAST,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TYPE)
	if err != nil {
		return fmt.Errorf("failed to list routes: %w", err)
	}

	logFields := log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"sandbox":  r.SandboxKey,
	}
	for _, route := range routes {
		if route.Dst == nil {
			// Default route
			switch family {
			case unix.AF_INET:
				if res.Gateway == "" {
					res.Gateway = route.Gw.String()
					log.
						WithFields(logFields).
						WithField("gateway", res.Gateway).
						Info("[Join] Setting IPv4 gateway retrieved from bridge interface on host routing table")
				}
			case unix.AF_INET6:
				if res.GatewayIPv6 == "" {
					res.GatewayIPv6 = route.Gw.String()
					log.
						WithFields(logFields).
						WithField("gateway", res.GatewayIPv6).
						Info("[Join] Setting IPv6 gateway retrieved from bridge interface on host routing table")
				}
			}

			continue
		}

		if opts.SkipRoutes {
			// Don't do static routes at all
			continue
		}

		if route.Protocol == unix.RTPROT_KERNEL ||
			(family == unix.AF_INET && route.Dst.Contains(hint.IPv4.IP)) ||
			(family == unix.AF_INET6 && route.Dst.Contains(hint.IPv6.IP)) {
			// Make sure to leave out the default on-link route created automatically for the IP(s) acquired by DHCP
			continue
		}

		staticRoute := &StaticRoute{
			Destination: route.Dst.String(),
			// Default to an on-link route
			RouteType: 1,
		}
		res.StaticRoutes = append(res.StaticRoutes, staticRoute)

		if route.Gw != nil {
			staticRoute.RouteType = 0
			staticRoute.NextHop = route.Gw.String()

			log.
				WithFields(logFields).
				WithField("route", staticRoute.Destination).
				WithField("gateway", staticRoute.NextHop).
				Info("[Join] Adding route (via gateway) retrieved from bridge interface on host routing table")
		} else {
			log.
				WithFields(logFields).
				WithField("route", staticRoute.Destination).
				Info("[Join] Adding on-link route retrieved from bridge interface on host routing table")
		}
	}

	return nil
}

// Join hands the per-endpoint host-side link to Docker (so it can move it
// into the container netns) along with route information, then starts a
// persistent DHCP client to keep the lease alive for the life of the
// endpoint.
//
// Bridge mode also copies static routes from the host bridge — those
// routes are how the upstream propagates LAN topology when the bridge is
// the host's L3 gateway. Macvlan mode skips that: the parent NIC's host
// routes belong to the host, not the container, and the DHCP gateway is
// the only route the container needs.
func (p *Plugin) Join(ctx context.Context, r JoinRequest) (JoinResponse, error) {
	log.WithField("options", r.Options).Debug("Join options")
	res := JoinResponse{}

	opts, err := p.netOptions(ctx, r.NetworkID)
	if err != nil {
		return res, fmt.Errorf("failed to get network options: %w", err)
	}

	parentAttached := false
	switch opts.effectiveMode() {
	case ModeMacvlan, ModeIPvlan:
		parentAttached = true
	}

	var srcName, dstPrefix string
	if parentAttached {
		srcName = subLinkName(r.EndpointID)
		dstPrefix = "eth"
	} else {
		_, srcName = vethPairNames(r.EndpointID)
		dstPrefix = opts.Bridge
	}
	res.InterfaceName = InterfaceName{
		SrcName:   srcName,
		DstPrefix: dstPrefix,
	}

	hint, ok := p.takeJoinHint(r.EndpointID)
	if !ok {
		// Most likely cause: the container was restarted. libnetwork's
		// flow on `docker restart` is Leave (old sandbox) -> Join (new
		// sandbox) on the same EndpointID, *without* a fresh
		// CreateEndpoint — so the hint our first Join consumed is gone
		// and the link in the destroyed sandbox is gone with it.
		// Reacquire from scratch.
		log.WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
			"sandbox":  r.SandboxKey,
		}).Info("[Join] No hint; attempting endpoint reacquisition (likely container restart)")
		if err := p.reacquireEndpoint(ctx, r, opts); err != nil {
			return res, fmt.Errorf("failed to reacquire endpoint after restart: %w", err)
		}
		hint, ok = p.takeJoinHint(r.EndpointID)
		if !ok {
			return res, util.ErrNoHint
		}
	}

	if hint.Gateway != "" {
		log.WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
			"sandbox":  r.SandboxKey,
			"gateway":  hint.Gateway,
		}).Info("[Join] Setting IPv4 gateway retrieved from initial DHCP in CreateEndpoint")
		res.Gateway = hint.Gateway
	}

	if !parentAttached {
		bridge, err := netlink.LinkByName(opts.Bridge)
		if err != nil {
			return res, fmt.Errorf("failed to get bridge interface: %w", err)
		}

		if err := p.addRoutes(&opts, false, bridge, r, hint, &res); err != nil {
			return res, err
		}
		if opts.IPv6 {
			if err := p.addRoutes(&opts, true, bridge, r, hint, &res); err != nil {
				return res, err
			}
		}
	}

	// Register the manager BEFORE spawning the start goroutine so that a
	// fast Leave can find it. Stop blocks until Start has completed
	// (success or failure), so it's safe to call against a manager whose
	// Start is still in flight.
	m := newDHCPManager(p.docker, r, opts)
	m.LastIP = hint.IPv4
	m.LastIPv6 = hint.IPv6
	m.MacAddress = hint.MacAddress
	p.registerDHCPManager(r.EndpointID, m)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), p.awaitTimeout)
		defer cancel()

		if err := m.Start(ctx); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"network":  shortID(r.NetworkID),
				"endpoint": shortID(r.EndpointID),
				"sandbox":  r.SandboxKey,
			}).Error("Failed to start persistent DHCP client; lease will not be renewed")
			// If Start failed, take ourselves out of the registry so a
			// later Leave doesn't try to Stop() us. Stop() is safe to
			// call against a failed-Start manager (it returns the start
			// error), but de-registering keeps the map tidy.
			p.takeDHCPManager(r.EndpointID)
		}
	}()

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"sandbox":  r.SandboxKey,
	}).Info("Joined sandbox to endpoint")

	return res, nil
}

// Leave stops the persistent DHCP client for an endpoint
func (p *Plugin) Leave(ctx context.Context, r LeaveRequest) error {
	manager, ok := p.takeDHCPManager(r.EndpointID)
	if !ok {
		return util.ErrNoSandbox
	}

	if err := manager.Stop(); err != nil {
		return err
	}

	// Refresh the endpoint fingerprint with the most recent v4/v6 IPs
	// the persistent client saw. Stop has already drained the event
	// goroutine, so manager.LastIP* are stable to read here. The
	// tombstone DeleteEndpoint lays down next will then carry the
	// renewed addresses rather than the initial-DISCOVER ones.
	v4, v6 := "", ""
	if manager.LastIP != nil && manager.LastIP.IP != nil {
		v4 = manager.LastIP.IP.String()
	}
	if manager.LastIPv6 != nil && manager.LastIPv6.IP != nil {
		v6 = manager.LastIPv6.IP.String()
	}
	p.updateEndpointIPs(r.EndpointID, v4, v6)

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
	}).Info("Sandbox left endpoint")

	return nil
}
