// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// CLIOptionsKey is the key used in create network options by the CLI for custom options
const CLIOptionsKey string = "com.docker.network.generic"

// Endpoints of https://github.com/moby/libnetwork/blob/master/docs/remote.md

// validateIPAMData accepts `--ipam-driver null` (address space "null", pool 0.0.0.0/0) and this plugin's own IPAM
// spaces, and refuses Docker's built-in "LocalDefault", whose allocator would hand out LAN addresses (#110).
func validateIPAMData(ipv4 []*IPAMData) error {
	refuse := func(d *IPAMData) error {
		return fmt.Errorf("%w: this network was given the address pool %v from address space %q, and this plugin serves two IPAM drivers and no others. Addresses here come from the LAN's DHCP server, so an allocator that believes it owns a subnet of its own would hand out addresses that are already in use. Create the network with the null IPAM driver (`--ipam-driver null`), or with this plugin itself (`--ipam-driver <this plugin>`)",
			util.ErrIPAM, d.Pool, d.AddressSpace)
	}
	for _, d := range ipv4 {
		switch d.AddressSpace {
		case "null":
			if d.Pool != "0.0.0.0/0" {
				return refuse(d)
			}
		case ipamLocalAddressSpace, ipamGlobalAddressSpace:
		default:
			return refuse(d)
		}
	}
	return nil
}

// ipamDataIsOurs reads the address space, since CreateNetwork carries no IPAM driver name and only this plugin
// answers GetDefaultAddressSpaces with these two (#110).
func ipamDataIsOurs(ipv4 []*IPAMData) bool {
	for _, d := range ipv4 {
		if d.AddressSpace == ipamLocalAddressSpace || d.AddressSpace == ipamGlobalAddressSpace {
			return true
		}
	}
	return false
}

// ipamBindingFor keeps the gateway and aux addresses, since RequestAddress for them is wire-identical to an
// endpoint replay (#110).
func (p *Plugin) ipamBindingFor(networkID string, ipv4 []*IPAMData, iface string) (*ipamBinding, error) {
	var d *IPAMData
	for _, c := range ipv4 {
		if c != nil && (c.AddressSpace == ipamLocalAddressSpace || c.AddressSpace == ipamGlobalAddressSpace) {
			if d != nil {
				return nil, fmt.Errorf("%w: this plugin allocates one IPv4 pool per network and Docker asked for more than one", util.ErrIPAM)
			}
			d = c
		}
	}
	if d == nil {
		return nil, fmt.Errorf("%w: no IPv4 pool from this plugin's IPAM driver", util.ErrIPAM)
	}
	pool, err := ipamCanonicalPool(d.Pool)
	if err != nil {
		return nil, err
	}
	poolID, ok, otherName := p.ipamPools.take(d.AddressSpace, pool, iface, time.Now())
	if !ok {
		// The interface mismatch is reported first: the pool was minted for the `--ipam-opt` interface, not this one.
		if otherName != "" && otherName != iface {
			return nil, fmt.Errorf("%w: this network's pool identity was built for interface %q (from `--ipam-opt parent=` or `--ipam-opt bridge=`) and the network itself is being created on %q (from `-o parent=` or `-o bridge=`). The two have to name the same interface: the IPAM option exists only to tell two networks with the same subnet apart, and it cannot send the addresses somewhere else. Fix whichever of the two is wrong, or drop the `--ipam-opt` if this network is the only one on this subnet", util.ErrIPAM, otherName, iface)
		}
		// No issue for this space and pool: a plugin restart between the calls, a second unsuffixed create for the same
		// subnet, or an earlier failed create consumed it (#110).
		return nil, fmt.Errorf("%w: this plugin has no issued pool %v in address space %v to bind. Either the plugin restarted between `docker network create` asking for the pool and creating the network, or another `docker network create` for the same subnet is running on this host and consumed it -- two such networks derive one pool identity unless one of them names its interface with `--ipam-opt parent=<nic>` (or `--ipam-opt bridge=<name>`). Re-run `docker network create`, one at a time", util.ErrIPAM, pool, d.AddressSpace)
	}
	if other, taken := p.ipamIndex.boundTo(poolID, networkID); taken {
		return nil, fmt.Errorf("%w: network %v already holds pool %v. Two DHCP networks with the same subnet need one of them to name its interface: add `--ipam-opt parent=<nic>` (or `--ipam-opt bridge=<name>`), or give this one its own `--subnet`", util.ErrIPAM, shortID(other), pool)
	}
	b := &ipamBinding{PoolID: poolID, Space: d.AddressSpace, Pool: pool}
	if d.Gateway != "" {
		b.Gateway = bareAddress(d.Gateway)
	}
	for _, v := range d.AuxAddresses {
		if s, ok := v.(string); ok && s != "" {
			b.Aux = append(b.Aux, bareAddress(s))
		}
	}
	sort.Strings(b.Aux)
	return b, nil
}

// bareAddress strips a prefix length: CreateNetwork gets CIDR addresses and RequestAddress the same ones bare.
func bareAddress(s string) string {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Addr().String()
	}
	return s
}

// kernelIfaceName truncates at the first NUL, as the kernel reads IFLA_IFNAME, so names compared from Docker's
// record match what the kernel acts on; measured, LinkByName("docker0\x00evil") resolved docker0 (#705, #727).
func kernelIfaceName(name string) string {
	if i := strings.IndexByte(name, 0); i >= 0 {
		return name[:i]
	}
	return name
}

// validateModeOptions is CreateNetwork's pure validation. Interface names pass ValidIfaceName here, since the
// daemon forwards a NUL in a driver option verbatim and "br0\x00evil" would slip past ErrBridgeUsed (#705).
func validateModeOptions(opts DHCPNetworkOptions) error {
	if _, err := resolveServerPolicy(opts); err != nil {
		return err
	}

	// RFC 5227 conflict detection, keyed on the decoded mode and the operator's own lease_timeout (#882).
	mode, err := dhcp.ParseConflictCheck(opts.ConflictCheck)
	if err != nil {
		return fmt.Errorf("%w: %v", util.ErrIPAM, err)
	}
	if err := dhcp.CheckLeaseTimeout(opts.LeaseTimeout, mode); err != nil {
		return fmt.Errorf("%w: %v", util.ErrIPAM, err)
	}

	// Whether this network hands leases back (#962); every mode sends DHCP.
	if _, err := parseReleaseLease(opts.ReleaseLease); err != nil {
		return err
	}

	// The host link name value is checked in every mode; the mode refusal is below (#978).
	if _, err := parseHostIfname(opts.HostIfname); err != nil {
		return err
	}

	if err := validateMTUOption(opts); err != nil {
		return err
	}

	switch opts.effectiveMode() {
	case ModeMacvlan, ModeIPvlan:
		if opts.Parent == "" {
			return util.ErrParentRequired
		}
		// The child link moves into the container namespace, so nothing stays on the host to name (#978).
		if opts.HostIfname != HostIfnameOff {
			return fmt.Errorf("%w: host_ifname cannot be set in mode=%v: the host-side link is moved into the container and leaves nothing on the host to name",
				util.ErrModeMismatch, opts.effectiveMode())
		}
		if opts.Bridge != "" {
			return fmt.Errorf("%w: bridge cannot be set in mode=%v", util.ErrModeMismatch, opts.effectiveMode())
		}
		if !dhcp.ValidIfaceName(opts.Parent) {
			return fmt.Errorf("%w: invalid parent %q: not a kernel-legal interface name", util.ErrIPAM, opts.Parent)
		}
	case ModeBridge:
		if opts.Bridge == "" {
			return util.ErrBridgeRequired
		}
		if opts.Parent != "" {
			return fmt.Errorf("%w: parent cannot be set in mode=bridge", util.ErrModeMismatch)
		}
		if !dhcp.ValidIfaceName(opts.Bridge) {
			return fmt.Errorf("%w: invalid bridge %q: not a kernel-legal interface name", util.ErrIPAM, opts.Bridge)
		}
		// validate_dhcp has no bridge-mode probe path, so it is refused on bridge mode (#108).
		if opts.ValidateDHCP {
			return fmt.Errorf("%w: validate_dhcp is not supported in mode=bridge", util.ErrModeMismatch)
		}
	default:
		return fmt.Errorf("%w: %q", util.ErrInvalidMode, opts.Mode)
	}
	return nil
}

// sandboxGone reads the filesystem, not the Docker API, since the API call is what times out when a container
// vanishes mid-attach (#373); an empty or unrecognised key is no evidence and returns false.
func sandboxGone(sandboxKey string) bool {
	return sandboxGoneIn(sandboxNetnsDirs, sandboxKey)
}

// joinAbortedByVanish classifies a failed Join as a vanished container on "no such container", fs.ErrNotExist in
// the chain (the only paths opened are the container's netns), or an unlinked sandbox key; anything else counts a
// fault (#373, #376, #401).
func joinAbortedByVanish(err error, sandboxKey string) bool {
	if cerrdefs.IsNotFound(err) {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return sandboxGone(sandboxKey)
}

// joinFailureLeavesAddressUnused is true only for ErrNoContainer, settled after the whole attach budget; every other
// failure may have a running container using the address, and releasing it would be #524's failure (#566).
func joinFailureLeavesAddressUnused(err error) bool {
	return errors.Is(err, util.ErrNoContainer)
}

// libnetwork bind-mounts each sandbox netns as /var/run/docker/netns/<id>, and /var/run is often a symlink to /run.
var sandboxNetnsDirs = []string{
	"/var/run/docker/netns",
	"/run/docker/netns",
}

// sandboxNetnsVisibleIn counts the first readable directory's entries, or -1, as a diagnostic that never feeds the
// decision; summing would double-count the /run symlink (#567).
func sandboxNetnsVisibleIn(dirs []string) int32 {
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		return int32(len(entries))
	}
	return -1
}

// sandboxGoneIn compares the key's name against a directory listing, so no request-derived path reaches the
// filesystem; os.Stat(filepath.Join(dir, name)) reintroduces CodeQL go/path-injection (#374).
func sandboxGoneIn(dirs []string, sandboxKey string) bool {
	dir, name := splitSandboxKeyIn(dirs, sandboxKey)
	if dir == "" {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// /var/run/docker is 0700, so EACCES is no evidence, not a vanished container.
		return false
	}
	for _, e := range entries {
		if e.Name() == name {
			return false
		}
	}
	return true
}

func splitSandboxKeyIn(dirs []string, sandboxKey string) (dir, name string) {
	if sandboxKey == "" {
		return "", ""
	}
	clean := filepath.Clean(sandboxKey)
	name = filepath.Base(clean)
	if name == "." || name == ".." || name == string(os.PathSeparator) {
		return "", ""
	}
	parent := filepath.Dir(clean)
	for _, known := range dirs {
		if parent == known {
			return known, name
		}
	}
	return "", ""
}

// CreateNetwork validates the options, the parent interface, the IPAM driver and bridge ownership.
func (p *Plugin) CreateNetwork(r CreateNetworkRequest) error {
	log.WithField("options", r.Options).Debug("CreateNetwork options")

	// decodeOptsSet, since the IPv6 refusals need to know which fields the operator wrote; see ipv6Mode.
	opts, optsSet, err := decodeOptsSet(r.Options[util.OptionsKeyGeneric])
	if err != nil {
		return fmt.Errorf("failed to decode network options: %w", err)
	}

	if err := validateIPAMData(r.IPv4Data); err != nil {
		return err
	}

	if err := validateModeOptions(opts); err != nil {
		return err
	}

	if err := validateIPv6Options(opts, optsSet); err != nil {
		return err
	}

	var binding *ipamBinding
	if ipamDataIsOurs(r.IPv4Data) {
		if err := ipamRefuseIPvlan(opts.effectiveMode()); err != nil {
			return err
		}
		// validateIPv6Options already resolved this pair, so this cannot fail today; the branch keeps an off mode distinct.
		mode6, err := opts.ipv6Mode()
		if err != nil {
			return err
		}
		if err := ipamRefuseIPv6(mode6); err != nil {
			return err
		}
		iface := opts.Bridge
		if m := opts.effectiveMode(); m == ModeMacvlan || m == ModeIPvlan {
			iface = opts.Parent
		}
		b, err := p.ipamBindingFor(r.NetworkID, r.IPv4Data, iface)
		if err != nil {
			return err
		}
		binding = b
	}

	if mode := opts.effectiveMode(); mode == ModeMacvlan || mode == ModeIPvlan {
		parent, err := validateParentForChild(opts.Parent)
		if err != nil {
			return err
		}
		if err := mtuUnderParent(opts.MTU, parent); err != nil {
			return err
		}
		// Pre-flight DHCP probe, opt-in via validate_dhcp, before saveOptions so a failed probe leaves no state (#108).
		if opts.ValidateDHCP {
			// The budget covers the probe and its wait for the parent gate, which runDHCPProbe takes itself (#577).
			probePolicy, err := resolveServerPolicy(opts)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), preflightProbeBudget+5*time.Second)
			err = p.runDHCPProbe(ctx, opts.Parent, mode, probePolicy)
			cancel()
			if err != nil {
				return err
			}
		}
		if err := p.saveNetworkAndBind(r.NetworkID, opts, binding); err != nil {
			return err
		}
		log.WithFields(log.Fields{
			"network":       r.NetworkID,
			"mode":          mode,
			"parent":        opts.Parent,
			"ipv6":          opts.ipv6Enabled(),
			"ipv6_mode":     opts.IPv6Mode,
			"validate_dhcp": opts.ValidateDHCP,
			"ipam":          binding != nil,
		}).Info("Network created")
		return nil
	}

	// Bridge mode goes through the netlink seam so the bridge-reuse guard is reachable without CAP_NET_ADMIN (#727).
	link, err := nlLinkByName(opts.Bridge)
	if err != nil {
		return fmt.Errorf("failed to lookup interface %v: %w", opts.Bridge, err)
	}
	if link.Type() != "bridge" {
		return util.ErrNotBridge
	}
	if err := mtuUnderParent(opts.MTU, link); err != nil {
		return err
	}

	if !opts.IgnoreConflicts {
		v4Addrs, err := util.DumpResult(nlAddrList(link, unix.AF_INET))
		if err != nil {
			return fmt.Errorf("failed to retrieve IPv4 addresses for %v: %w", opts.Bridge, err)
		}
		v6Addrs, err := util.DumpResult(nlAddrList(link, unix.AF_INET6))
		if err != nil {
			return fmt.Errorf("failed to retrieve IPv6 addresses for %v: %w", opts.Bridge, err)
		}
		bridgeAddrs := append(v4Addrs, v6Addrs...)

		nets, err := p.docker.NetworkList(context.Background(), dNetwork.ListOptions{})
		if err != nil {
			return fmt.Errorf("failed to retrieve list of networks from Docker: %w", err)
		}

		for _, n := range nets {
			if IsDHCPPlugin(n.Driver) {
				otherOpts, err := decodeOpts(n.Options)
				if err != nil {
					log.
						WithField("network", n.Name).
						WithError(err).
						Warn("Failed to parse other DHCP network's options")
				} else if kernelIfaceName(otherOpts.Bridge) == kernelIfaceName(opts.Bridge) {
					return util.ErrBridgeUsed
				}
			}
			if n.IPAM.Driver == "null" || IsDHCPPlugin(n.IPAM.Driver) {
				// A null-driver network carries 0.0.0.0/0 and an IPAM-mode one the LAN subnet, so neither is compared
				// here (#110).
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

	if err := p.saveNetworkAndBind(r.NetworkID, opts, binding); err != nil {
		return err
	}
	log.WithFields(log.Fields{
		"network":   r.NetworkID,
		"bridge":    opts.Bridge,
		"ipv6":      opts.ipv6Enabled(),
		"ipv6_mode": opts.IPv6Mode,
		"ipam":      binding != nil,
	}).Info("Network created")

	return nil
}

// saveNetworkAndBind fails the create when an IPAM network's write fails, since the binding is in no Docker record;
// null-mode options are recoverable from the Docker API (#110).
func (p *Plugin) saveNetworkAndBind(networkID string, opts DHCPNetworkOptions, binding *ipamBinding) error {
	if err := saveNetwork(networkID, opts, binding); err != nil {
		if binding != nil {
			return fmt.Errorf("failed to persist this network's address pool: %w", err)
		}
		log.WithError(err).WithField("network", networkID).
			Warn("Failed to persist options; daemon-restart may need API fallback")
		return nil
	}
	if binding != nil {
		p.ipamIndex.bind(binding.PoolID, networkID)
	}
	return nil
}

// DeleteNetwork removes the network's state and stops its managers, since libnetwork sends no Leave for stopped
// containers (#46).
func (p *Plugin) DeleteNetwork(r DeleteNetworkRequest) error {
	// Release first: whether to release and by which interface are read from the options deleteOptions removes, and
	// the tombstones keyed by this network die with it (#984).
	if released := p.releaseNetworkRecords(r.NetworkID); released > 0 {
		log.WithFields(log.Fields{
			"network":  r.NetworkID,
			"released": released,
		}).Info("release_lease=on_remove: handed this network's still-held addresses back before removing it")
	}

	// The binding goes here, not in ReleasePool, which libnetwork also calls for a failed create on a shared PoolID
	// (#110).
	p.ipamIndex.unbindNetwork(r.NetworkID)

	if err := deleteOptions(r.NetworkID); err != nil {
		log.WithError(err).WithField("network", r.NetworkID).
			Warn("Failed to remove persisted options; harmless leftover")
	}

	orphaned := p.takeDHCPManagersForNetwork(r.NetworkID)
	if len(orphaned) > 0 {
		log.WithFields(log.Fields{
			"network": r.NetworkID,
			"count":   len(orphaned),
		}).Info("Stopping orphaned DHCP managers on network removal")
		var wg sync.WaitGroup
		for _, m := range orphaned {
			wg.Add(1)
			go func(m *dhcpManager) {
				defer wg.Done()
				if err := m.Stop(); err != nil {
					log.WithError(err).WithField("network", r.NetworkID).
						Warn("Orphaned manager stop returned error; manager already removed from registry")
				}
			}(m)
		}
		wg.Wait()
	}

	log.WithField("network", r.NetworkID).Info("Network deleted")
	return nil
}

// vethPairNames tolerates a short EndpointID from a malformed response, at the cost of pair uniqueness.
func vethPairNames(id string) (string, string) {
	prefix := id
	if len(id) > 12 {
		prefix = id[:12]
	}
	return "dh-" + prefix, prefix + "-dh"
}

// parseExplicitV4 returns the bare IPv4 of a `docker run --ip` Interface.Address, which the engine rejects on
// null-IPAM networks, so `--driver-opt ip=` is the usual channel (#46).
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
	if addr.IP.IsUnspecified() {
		return "", fmt.Errorf("Interface.Address must be a unicast IPv4: got %q: %w", iface.Address, util.ErrIPAM)
	}
	return addr.IP.String(), nil
}

// resolveExplicitV4 merges `--ip` and the `ip` driver-opt, refusing two different values.
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

// resolveExplicitV6 returns the `--ip6` address, sent as the IA Address in the Solicit's IA_NA; there is no `ip6`
// driver-opt (#213).
func resolveExplicitV6(r CreateEndpointRequest) (string, error) {
	if r.Interface == nil || r.Interface.AddressIPv6 == "" {
		return "", nil
	}
	addr, err := netlink.ParseAddr(r.Interface.AddressIPv6)
	if err != nil {
		return "", fmt.Errorf("invalid Interface.AddressIPv6 %q (want CIDR): %w", r.Interface.AddressIPv6, util.ErrIPAM)
	}
	if addr.IP.To4() != nil {
		return "", fmt.Errorf("Interface.AddressIPv6 must be IPv6: got %q: %w", r.Interface.AddressIPv6, util.ErrIPAM)
	}
	if addr.IP.IsUnspecified() {
		return "", fmt.Errorf("Interface.AddressIPv6 must be a unicast IPv6: got %q: %w", r.Interface.AddressIPv6, util.ErrIPAM)
	}
	return addr.IP.String(), nil
}

// parseDriverOptIP reads the bare IPv4 of the `ip` driver-opt, a flat key in r.Options (#46).
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
	if v4.IsUnspecified() {
		return "", fmt.Errorf("driver-opt ip must be a unicast IPv4: got %q: %w", s, util.ErrIPAM)
	}
	return v4.String(), nil
}

// netOptions validates every stored record it returns, since an older build, a hand edit or a pre-guard
// CreateNetwork may have written it (#727).
func (p *Plugin) netOptions(ctx context.Context, id string) (DHCPNetworkOptions, error) {
	opts, err := p.netOptionsRaw(ctx, id)
	if err != nil {
		return DHCPNetworkOptions{}, err
	}
	if err := p.checkStoredOptions(id, opts); err != nil {
		return DHCPNetworkOptions{}, err
	}
	return opts, nil
}

// netMode returns only the mode, so DeleteEndpoint tears down even a refused network and gets no stored name to
// misuse; bridge teardown derives its link from vethPairNames (#402, #408, #727).
func (p *Plugin) netMode(ctx context.Context, id string) (mode string, known bool, err error) {
	opts, err := p.netOptionsRaw(ctx, id)
	if err != nil {
		return "", false, err
	}
	m := opts.effectiveMode()
	return m, knownMode(m), nil
}

// knownMode catches an unknown stored mode, which effectiveMode returns verbatim and DeleteEndpoint's bridge
// branch would report as deleted while the child link and lease survive (#727).
func knownMode(mode string) bool {
	switch mode {
	case ModeBridge, ModeMacvlan, ModeIPvlan:
		return true
	default:
		return false
	}
}

// checkStoredOptions refuses an unknown mode or a kernel-illegal name on the read path, since networks from before
// #705, the NetworkInspect fallback and hand-edited state never passed validateModeOptions. Both names are checked
// in every mode, since the mode comes from the same record (#727).
func (p *Plugin) checkStoredOptions(id string, opts DHCPNetworkOptions) error {
	if m := opts.effectiveMode(); !knownMode(m) {
		p.networkOptionsRejected.Add(1)
		log.WithFields(log.Fields{
			"network": shortID(id),
			"mode":    fmt.Sprintf("%q", m),
		}).Error("Refusing stored network options: unknown mode")
		return fmt.Errorf("stored mode %q is not one this plugin implements: %w", m, util.ErrInvalidMode)
	}

	// An unknown stored release_lease is refused, not read as "no release"; DeleteEndpoint reads only netMode (#962).
	if _, err := parseReleaseLease(opts.ReleaseLease); err != nil {
		p.networkOptionsRejected.Add(1)
		log.WithFields(log.Fields{
			"network": shortID(id),
			"value":   fmt.Sprintf("%q", opts.ReleaseLease),
		}).Error("Refusing stored network options: release_lease is not a value this plugin implements")
		return err
	}

	// The stored IPv6 options pass the function CreateNetwork calls, with no written-key set, so a restart cannot get
	// a refused pair past it (#817).
	if err := validateIPv6Options(opts, nil); err != nil {
		p.networkOptionsRejected.Add(1)
		log.WithFields(log.Fields{
			"network":   shortID(id),
			"mode":      opts.effectiveMode(),
			"ipv6":      opts.IPv6,
			"ipv6_mode": fmt.Sprintf("%q", opts.IPv6Mode),
		}).Error("Refusing stored network options: this plugin cannot act on the IPv6 options as stored")
		return err
	}

	for _, f := range []struct{ field, name string }{
		{"bridge", opts.Bridge},
		{"parent", opts.Parent},
	} {
		if f.name == "" || dhcp.ValidIfaceName(f.name) {
			continue
		}
		p.networkOptionsRejected.Add(1)
		log.WithFields(log.Fields{
			"network": shortID(id),
			"field":   f.field,
			// %q, so a control character or NUL shows in the log.
			"value": fmt.Sprintf("%q", f.name),
		}).Error("Refusing stored network options: interface name is not kernel-legal")
		// ErrIPAM maps to 400 like the create-time refusal, wrapped last so the true sentence leads.
		return fmt.Errorf("stored %s %q is not a kernel-legal interface name: %w",
			f.field, f.name, util.ErrIPAM)
	}
	return nil
}

// netOptionsRaw decodes without the name check, from disk first, falling back to NetworkInspect for networks
// older than persistence; TestNetOptionsRaw_HasNoOtherCallers keeps netOptions and netMode its only callers (#727).
func (p *Plugin) netOptionsRaw(ctx context.Context, id string) (DHCPNetworkOptions, error) {
	cached, loadErr := loadOptions(id)
	if loadErr == nil {
		return cached, nil
	}

	// The backfill runs only on os.IsNotExist: a newer schema, a corrupt file or a transient EIO falls back to the
	// docker API read-only, so a downgrade or a bad disk moment never overwrites the file (#724).
	absent := os.IsNotExist(loadErr)
	if !absent {
		log.WithError(loadErr).WithField("network", id).
			Warn("Failed to load persisted options; falling back to the docker API for this call and leaving the file on disk untouched")
	}

	dummy := DHCPNetworkOptions{}

	n, err := p.docker.NetworkInspect(ctx, id, dNetwork.InspectOptions{})
	if err != nil {
		return dummy, fmt.Errorf("failed to get info from Docker: %w", err)
	}

	// An IPAM-mode network stops here: Docker's record has no pool binding, and the null path would run a second
	// exchange and answer an address libnetwork already allocated. IPAM.Driver discriminates, since the state file is
	// what failed; the IPAM handlers never reach this, as they run inside the daemon's start-up replay (#110).
	if ipamDriverIsRemote(n.IPAM.Driver) {
		return dummy, fmt.Errorf("%w: %v: %w", errIPAMBindingLost, id, loadErr)
	}

	opts, err := decodeOpts(n.Options)
	if err != nil {
		return dummy, fmt.Errorf("failed to parse options: %w", err)
	}

	// Backfill a network that predates persistence; guarded on absence, so no file is lost.
	if absent {
		if err := saveOptions(id, opts); err != nil {
			log.WithError(err).WithField("network", id).
				Debug("Failed to backfill persisted options")
		}
	}
	return opts, nil
}

// ipamDriverIsRemote treats any IPAM driver other than "null", "default" or "" as this plugin's, since
// `docker plugin install --alias` changes the stored name and validateIPAMData admits no other remote (#110).
func ipamDriverIsRemote(name string) bool {
	switch name {
	case "", "null", "default":
		return false
	default:
		return true
	}
}

// CreateEndpoint builds the host-side link, runs a one-shot DHCP acquisition and stashes the result for Join.
func (p *Plugin) CreateEndpoint(ctx context.Context, r CreateEndpointRequest) (CreateEndpointResponse, error) {
	// The daemon's deadline on this call comes first; see v6AcquisitionDeadline.
	callStart := time.Now()
	log.WithField("options", r.Options).Debug("CreateEndpoint options")
	res := CreateEndpointResponse{
		Interface: &EndpointInterface{},
	}

	explicitV4, err := resolveExplicitV4(r)
	if err != nil {
		return res, err
	}
	// `docker run --ip6` arrives as Interface.AddressIPv6 and is requested as the DHCPv6 preferred address (#152,
	// #213).
	explicitV6, err := resolveExplicitV6(r)
	if err != nil {
		return res, err
	}

	// The interface name option arrives only here, since libnetwork's remote proxy sends Join no endpoint options
	// (#125).
	ifname, err := parseIfnameOption(r.Options)
	if err != nil {
		return res, err
	}
	if ifname != "" {
		// The hint reaches Join on every engine; noteIfnameRequest states whether it applies (#670).
		p.noteIfnameRequest(r.NetworkID, r.EndpointID, ifname)
		p.updateJoinHint(r.EndpointID, func(h *joinHint) { h.Ifname = ifname })
	}

	opts, err := p.netOptions(ctx, r.NetworkID)
	if err != nil {
		return res, fmt.Errorf("failed to get network options: %w", err)
	}

	// Before the mode split: in IPAM mode the address is already leased, so neither branch runs its exchange (#110).
	if binding := ipamBindingOf(r.NetworkID); binding != nil {
		return p.createIPAMEndpoint(ctx, r, opts, binding)
	}

	if m := opts.effectiveMode(); m == ModeMacvlan || m == ModeIPvlan {
		return p.createParentAttachedEndpoint(ctx, callStart, r, opts)
	}

	bridge, err := netlink.LinkByName(opts.Bridge)
	if err != nil {
		return res, fmt.Errorf("failed to get bridge interface: %w", err)
	}

	// The hostname scopes tombstone matching to one container; a failed lookup falls back to network-only (#46).
	hostname := p.initialDHCPHostname(ctx, r.NetworkID, r.EndpointID)

	// MAC and IP priority: explicit `--mac-address`/`--ip`, then a tombstone, then kernel MAC and server IP; an
	// explicit MAC consumes no tombstone (#46).
	effectiveMAC := r.Interface.MacAddress
	requestedIP := explicitV4
	requestedV6 := explicitV6
	if effectiveMAC == "" {
		if mac, ip, ipv6, ok := p.consumeTombstone(r.NetworkID, hostname); ok {
			effectiveMAC = mac
			if requestedIP == "" {
				requestedIP = ip
			}
			// A tombstone's IPv6 is the DHCPv6 preferred address too, unless `--ip6` named one (#213).
			if requestedV6 == "" {
				requestedV6 = ipv6
			}
			log.WithFields(log.Fields{
				"network":      shortID(r.NetworkID),
				"endpoint":     shortID(r.EndpointID),
				"hostname":     hostname.name,
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
	// Hoisted, so a failed CreateEndpoint closes the CREATED record it opened in the append-only journal (#899).
	var (
		recordID  string
		recordID6 string
		identity6 dhcp.Identity6
	)

	if err := func() error {
		// Both ends: veth ends are independent, and with the host end at 1500 and the container end at
		// 1400 a 1428-byte DF frame is dropped silently (measured 6.12, 2026-09-24, #1037).
		if err := applyEndpointMTU(opts.MTU, hostLink, &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: ctrName}}); err != nil {
			return err
		}
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

		// Pin the container-side MAC, which the kernel often resets after LinkSetMaster.
		if effectiveMAC == "" {
			if err := netlink.LinkSetHardwareAddr(ctrLink, ctrLink.Attrs().HardwareAddr); err != nil {
				return fmt.Errorf("failed to set container side of veth pair's MAC address: %w", err)
			}
		}
		// Report the MAC only when libnetwork sent none, as for a tombstone-inherited one; empty means "kept what you sent".
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
		// The MAC keys the DHCP identity, so Join and the orphan-release path re-derive it without a link to read.
		p.updateJoinHint(r.EndpointID, func(hint *joinHint) {
			hint.MacAddress = ctrLink.Attrs().HardwareAddr
		})
		// MAC-derived, so the IPv4 lease survives a restart as the v6 binding does (#371).
		clientID := resolveClientID(opts, r.EndpointID, ctrLink.Attrs().HardwareAddr)

		// The CREATED record holds the option-61 value as sent, type byte included, and Join resumes it as
		// INIT-REBOOT (#899).
		recordID = p.recordCreated(r.NetworkID,
			endpointRecordKey(opts.effectiveMode(), r.EndpointID, ctrLink.Attrs().HardwareAddr),
			dhcp.ClientIdentity(clientID))
		p.updateJoinHint(r.EndpointID, func(hint *joinHint) {
			hint.RecordID = recordID
		})

		// The DHCPv6 identity has its own record, kept apart by dhcp.Scope6 since both share a chaddr, and minted once:
		// RFC 9915 section 11 says a DUID "SHOULD NOT change over time if at all possible" (#911).
		if opts.ipv6Enabled() {
			id6, err := resolveIdentity6(opts, r.EndpointID, ctrLink.Attrs().HardwareAddr)
			if err != nil {
				return err
			}
			identity6 = id6
			recordID6 = p.recordCreated6(r.NetworkID,
				endpointRecordKey(opts.effectiveMode(), r.EndpointID, ctrLink.Attrs().HardwareAddr), id6)
		}
		initialIP := func(v6 bool) error {
			v6str := ""
			if v6 {
				v6str = "v6"
			}

			// Server preference ladder (#111) and deny-list (#669); neither set is one unrestricted attempt.
			pol, err := resolveServerPolicy(opts)
			if err != nil {
				return err
			}

			base := dhcp.DHCPClientOptions{
				// .name only: a refused hostname is simply absent from the exchange.
				Hostname:    hostname.name,
				FQDN:        opts.fqdnMode(),
				ClientID:    clientID,
				VendorClass: opts.VendorClass,
				// Pin the DUID-LL and IAID to the container veth's MAC, so this one-shot and the persistent client
				// share one binding (#152).
				MAC:      ctrLink.Attrs().HardwareAddr,
				Records:  p.records,
				RecordID: recordID,
			}
			if v6 {
				if err := p.v6Wiring(&base, opts, identity6, recordID6, requestedV6, r.EndpointID); err != nil {
					return err
				}
			}
			// Conflict detection from the stored conflict_check, set on the base so every dhcp_servers attempt shares
			// it (#882).
			if err := p.conflictWiring(&base, opts, roleAcquire, r.NetworkID, r.EndpointID, v6); err != nil {
				return err
			}
			// Preferred address per family, `request ADDR` for v4 and `ia_na / ADDR` for v6; empty omits it (#213).
			if !v6 {
				base.RequestedIP = requestedIP
			}

			// The v6 half runs second and gets what is left of the daemon's deadline; see v6AcquisitionDeadline.
			acqCtx := ctx
			if v6 {
				var endV6 context.CancelFunc
				acqCtx, endV6 = withV6AcquisitionDeadline(ctx, callStart)
				defer endV6()
			}

			info, ra, err := p.acquireWithPolicy(acqCtx, ctrName, pol, v6, timeout, r.EndpointID, base)
			if err != nil {
				// An empty DHCPv6 acquisition fails only when the segment advertised managed DHCPv6; stateless and
				// SLAAC segments have no DHCPv6 address to get (#868).
				if v6 && p.noteV6Absence(ra, ctrName, r.EndpointID, err, base.Mode6) {
					return nil
				}
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
					// The IPv6 gateway is the Router Advertisement's source, link-local under RFC 4861 section 4.2,
					// read by the library's client (#821); the v4 `-o gateway=` override is not consulted for it.
					fillV6Hint(hint, info)
				} else {
					res.Interface.Address = info.IP
					hint.IPv4 = ip
					hint.Gateway = info.Gateway
					if opts.Gateway != "" {
						hint.Gateway = opts.Gateway
					}
					// Option-121 routes (RFC 3442) exclude a literal 0.0.0.0/0, folded into info.Gateway, but
					// together they can still cover the whole space; see routesSupersedeDefault.
					hint.Routes = dhcpStaticRoutes(info.Routes)
				}
			})

			return nil
		}

		if err := initialIP(false); err != nil {
			return err
		}
		if opts.ipv6Enabled() {
			if err := initialIP(true); err != nil {
				return err
			}
		}

		return nil
	}(); err != nil {
		// Best-effort veth cleanup on failure.
		p.closeRecord(recordID)
		p.closeRecord(recordID6)
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

	// Remember the MAC and IPs, so DeleteEndpoint can lay a tombstone (#46).
	mac := r.Interface.MacAddress
	if mac == "" {
		mac = res.Interface.MacAddress
	}
	p.rememberEndpoint(r.EndpointID, endpointFingerprint{MAC: mac, IPv4: v4IP, IPv6: v6IP, Ifname: p.hintIfname(r.EndpointID)}, hostname)

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
	}).Info("Endpoint created")
	log.WithFields(log.Fields{
		"network":     shortID(r.NetworkID),
		"endpoint":    shortID(r.EndpointID),
		"mac_address": mac,
		"ip":          res.Interface.Address,
		"ipv6":        res.Interface.AddressIPv6,
		"gateway":     gateway,
	}).Debug("Endpoint details")

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
	// Through the seam and the rename guard, since a host_ifname rename takes two kernel calls and this call is not
	// serialised with an attach (#1051).
	hostLink, err := hostLinkByGeneratedName(hostName)
	if err != nil {
		return res, fmt.Errorf("failed to find host side of veth pair: %w", err)
	}

	info := operInfo{
		Bridge: opts.Bridge,
		// Publish the link's own name, since a `host_ifname` rename keeps the generated one only as an altname (#978).
		HostVEth:    hostLink.Attrs().Name,
		HostVEthMAC: hostLink.Attrs().HardwareAddr.String(),
	}
	if err := mapstructure.Decode(info, &res.Value); err != nil {
		return res, fmt.Errorf("failed to encode OperInfo: %w", err)
	}

	return res, nil
}

// DeleteEndpoint removes the endpoint's host-side link, best-effort in macvlan mode where the netns reaped it.
func (p *Plugin) DeleteEndpoint(ctx context.Context, r DeleteEndpointRequest) error {
	p.autoFallbackCounted.Delete(r.EndpointID)
	// netMode, not netOptions: teardown must not be blocked by a stored name it never reads (#727).
	mode, modeKnown, err := p.netMode(ctx, r.NetworkID)
	if err != nil {
		// Teardown survives errIPAMBindingLost: both branches resolve one link name, and refusing would wedge
		// `docker network rm` (#110).
		if !errors.Is(err, errIPAMBindingLost) {
			// The fingerprint goes whether or not this call succeeds, since the engine releases the address anyway
			// (moby 406bdd8c82, daemon/libnetwork/endpoint.go:968-1000; v26.1.5 libnetwork/endpoint.go:861-863), and
			// a stale fingerprint would make ReleaseAddress read the endpoint as up (#1047).
			p.takeEndpoint(r.EndpointID)
			return fmt.Errorf("failed to get network options: %w", err)
		}
		mode, modeKnown = "", false
		log.WithError(err).WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
		}).Error("This network's pool binding could not be read; tearing the endpoint down without it")
	}

	// IPAM mode lays no JSON tombstone: the retained record and libnetwork's ReleaseAddress already answer it, and a
	// tombstone's MAC would contradict the MAC libnetwork generated (#110).
	ipamMode := ipamBindingOf(r.NetworkID) != nil

	// An unrecognised mode still removes the link, since subLinkName and vethPairNames' host half are both "dh-" plus
	// 12 bytes of the endpoint ID (TestTeardownBranchesResolveTheSameLinkName); it costs the tombstone (#727).
	if !modeKnown {
		p.networkOptionsRejected.Add(1)
		log.WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
			"mode":     fmt.Sprintf("%q", mode),
		}).Error("Stored network options carry an unknown mode; running every teardown path rather than guessing one")
	}

	// No tombstone for ipvlan, whose children share the parent MAC, nor an unknown mode that might be ipvlan. None for
	// a refused hostname, since "" is the matcher's wildcard and would match every container (#693, #726). None after
	// a release, since the server may have handed the address on (#524, #962); the unreleased family's record still
	// takes the tombstone phase below, since a released family's record is already CLOSED.
	if fp, ok := p.takeEndpoint(r.EndpointID); ok {
		if fp.Released {
			log.WithFields(log.Fields{
				"network":  shortID(r.NetworkID),
				"endpoint": shortID(r.EndpointID),
			}).Info("Endpoint released its lease at Leave; laying no tombstone for it")
		}
		if modeKnown && mode != ModeIPvlan && !fp.HostnameRefused && !ipamMode && !fp.Released {
			p.addTombstone(r.NetworkID, fp.Hostname, fp.MAC, fp.IPv4, fp.IPv6)
		}
		// RETAINED on every mode and hostname decision, so plugin-restart recovery never resumes a gone endpoint's
		// lease; keyed as the record was filed, since fp.MAC is empty on ipvlan (#899).
		hw, _ := net.ParseMAC(fp.MAC)
		p.retainRecordFor(r.NetworkID, endpointRecordKey(mode, r.EndpointID, hw))
	}

	if mode == ModeMacvlan || mode == ModeIPvlan {
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
	// Through the seam, so a unit test sees which paths ran, and through the rename guard, since a miss reads as a
	// finished teardown (#1051).
	link, err := hostLinkByGeneratedName(hostName)
	if err != nil {
		// A veth pair dies whole with its container-side netns (OOM-kill, `docker rm -f`), so not-found is success;
		// failing would wedge `docker network rm` (#330).
		var lnf netlink.LinkNotFoundError
		if errors.As(err, &lnf) {
			log.WithFields(log.Fields{
				"network":  shortID(r.NetworkID),
				"endpoint": shortID(r.EndpointID),
			}).Debug("Host veth already gone (expected on forced teardown)")
			return nil
		}
		return fmt.Errorf("failed to lookup host veth interface %v: %w", hostName, err)
	}

	if err := nlLinkDel(link); err != nil {
		return fmt.Errorf("failed to delete veth pair: %w", err)
	}

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
	}).Info("Endpoint deleted")

	return nil
}

// dhcpStaticRoutes maps option-121 routes to libnetwork StaticRoutes; an empty Gateway is on-link.
func dhcpStaticRoutes(routes []dhcp.Route) []*StaticRoute {
	out := make([]*StaticRoute, 0, len(routes))
	for _, r := range routes {
		sr := &StaticRoute{Destination: r.Destination, RouteType: RouteTypeOnLink}
		if r.Gateway != "" {
			sr.RouteType = RouteTypeNextHop
			sr.NextHop = r.Gateway
		}
		out = append(out, sr)
	}
	return out
}

// v6AdvertisedRoutes turns RFC 4861 section 4.6.2 on-link prefixes into on-link routes, needed since the DHCPv6
// address is a /128 (RFC 9915 section 18.2.10.1, RFC 5942 section 4), and RFC 4191 Route Information into next-hop
// routes, ::/0 already filtered (RFC 4191 section 2.3). On-link wins for a prefix seen twice (#821).
func v6AdvertisedRoutes(info dhcp.Info) []*StaticRoute {
	out := make([]*StaticRoute, 0, len(info.OnLinkPrefixes)+len(info.Routes))
	seen := map[string]bool{}
	for _, p := range info.OnLinkPrefixes {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, &StaticRoute{Destination: p, RouteType: RouteTypeOnLink})
	}
	for _, r := range info.Routes {
		if seen[r.Destination] {
			continue
		}
		seen[r.Destination] = true
		sr := &StaticRoute{Destination: r.Destination, RouteType: RouteTypeOnLink}
		if r.Gateway != "" {
			sr.RouteType = RouteTypeNextHop
			sr.NextHop = r.Gateway
		}
		out = append(out, sr)
	}
	if len(out) == 0 {
		// nil, so no advertisement and no extra routes are the same value.
		return nil
	}
	return out
}

// fillV6Hint is shared by the bridge and parent-attached acquisition loops (#821).
func fillV6Hint(hint *joinHint, info dhcp.Info) {
	hint.GatewayIPv6 = info.Gateway
	hint.RoutesIPv6 = v6AdvertisedRoutes(info)
}

// appendDHCPStaticRoutes adds option-121 routes (RFC 3442) to the hint unless `skip_routes=true`, which leaves the
// folded default gateway alone; split from Join so the evidence is testable (#700).
func (p *Plugin) appendDHCPStaticRoutes(opts DHCPNetworkOptions, r JoinRequest, hint joinHint, res *JoinResponse) {
	if opts.SkipRoutes || len(hint.Routes) == 0 {
		return
	}

	res.StaticRoutes = append(res.StaticRoutes, hint.Routes...)
	p.dhcpRoutesApplied.Add(int32(len(hint.Routes)))

	// Log destinations and next hops, not a count, to answer where traffic went.
	fields := log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"sandbox":  r.SandboxKey,
		"routes":   describeStaticRoutes(hint.Routes),
		"gateway":  res.Gateway,
	}
	if routesSupersedeDefault(hint.Routes) {
		p.dhcpDefaultRouteSuperseded.Add(1)
		log.WithFields(fields).Warn("[Join] DHCP classless static routes (option 121) cover the whole address space; they supersede the gateway above on longest-prefix match")
		return
	}
	log.WithFields(fields).Info("[Join] Adding DHCP classless static routes (option 121)")
}

// applyV6JoinHint sets the RA gateway and routes from the hint, and `skip_routes=true` drops the routes but keeps
// the gateway, as on v4; nothing reads the host's table (#821).
func (p *Plugin) applyV6JoinHint(opts DHCPNetworkOptions, r JoinRequest, hint joinHint, res *JoinResponse) {
	if hint.GatewayIPv6 != "" {
		log.WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
			"sandbox":  r.SandboxKey,
			"gateway":  hint.GatewayIPv6,
		}).Info("[Join] Setting IPv6 gateway from the Router Advertisement seen in CreateEndpoint")
		res.GatewayIPv6 = hint.GatewayIPv6
	}

	if opts.SkipRoutes || len(hint.RoutesIPv6) == 0 {
		return
	}

	res.StaticRoutes = append(res.StaticRoutes, hint.RoutesIPv6...)
	p.dhcpRoutesApplied.Add(int32(len(hint.RoutesIPv6)))

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"sandbox":  r.SandboxKey,
		"routes":   describeStaticRoutes(hint.RoutesIPv6),
		"gateway":  res.GatewayIPv6,
	}).Info("[Join] Adding IPv6 routes from the Router Advertisement")
}

// describeStaticRoutes renders "dest via nexthop" or "dest onlink" for the log.
func describeStaticRoutes(routes []*StaticRoute) []string {
	out := make([]string, 0, len(routes))
	for _, r := range routes {
		if r == nil {
			continue
		}
		if r.NextHop != "" {
			out = append(out, r.Destination+" via "+r.NextHop)
			continue
		}
		out = append(out, r.Destination+" onlink")
	}
	return out
}

// addRoutes copies the host link's non-default, non-kernel, non-DHCP-subnet routes into StaticRoutes in every
// mode, the bridge or the parent NIC; `-o skip_routes=true` opts out (#102).
func (p *Plugin) addRoutes(opts *DHCPNetworkOptions, v6 bool, link netlink.Link, r JoinRequest, hint joinHint, res *JoinResponse) error {
	family := unix.AF_INET
	if v6 {
		family = unix.AF_INET6
	}

	routes, err := util.DumpResult(nlRouteListFiltered(family, &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Type:      unix.RTN_UNICAST,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_TYPE))
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
			// Only the IPv4 default comes from the host table: the host's v6 default is its own RA on another link,
			// and the container's v6 gateway arrives on the hint (#821).
			if family == unix.AF_INET && res.Gateway == "" {
				res.Gateway = route.Gw.String()
				log.
					WithFields(logFields).
					WithField("gateway", res.Gateway).
					Info("[Join] Setting IPv4 gateway retrieved from host parent interface routing table")
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
			RouteType: RouteTypeOnLink,
		}
		res.StaticRoutes = append(res.StaticRoutes, staticRoute)

		if route.Gw != nil {
			staticRoute.RouteType = RouteTypeNextHop
			staticRoute.NextHop = route.Gw.String()

			log.
				WithFields(logFields).
				WithField("route", staticRoute.Destination).
				WithField("gateway", staticRoute.NextHop).
				Info("[Join] Adding route (via gateway) retrieved from host parent interface routing table")
		} else {
			log.
				WithFields(logFields).
				WithField("route", staticRoute.Destination).
				Info("[Join] Adding on-link route retrieved from host parent interface routing table")
		}
	}

	return nil
}

// parseIfnameOption validates the optional interface name as the kernel's dev_valid_name does: 1-15 bytes, not
// "." or "..", no '/', no whitespace (#125).
func parseIfnameOption(options map[string]interface{}) (string, error) {
	raw, ok := options[ifnameOption]
	if !ok {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("invalid %s %v: expected non-empty string: %w", ifnameOption, raw, util.ErrIPAM)
	}
	if len(s) > 15 {
		return "", fmt.Errorf("invalid interface_name %q: longer than 15 bytes (IFNAMSIZ): %w", s, util.ErrIPAM)
	}
	if s == "." || s == ".." || strings.ContainsAny(s, "/ \t\n\r") {
		return "", fmt.Errorf("invalid interface_name %q: must not contain '/', whitespace, or be '.'/'..': %w", s, util.ErrIPAM)
	}
	// Measured, the kernel accepts "-cfoo", "-c", "-" and ".x" as link names and refuses only whitespace, so
	// dhcp.ValidIfaceName applies here and fails at CreateEndpoint (#706).
	if !dhcp.ValidIfaceName(s) {
		return "", fmt.Errorf("invalid interface_name %q: must start with a letter or digit and contain only letters, digits, '.', '-' and '_': %w", s, util.ErrIPAM)
	}
	return s, nil
}

// noteAttachDuration fills the attach buckets, since the timing line is Debug and config.json ships
// LOG_LEVEL=info (#403). The budget is tested first, so an AwaitTimeout under a second still partitions.
func (p *Plugin) noteAttachDuration(elapsed time.Duration) {
	p.joinAttachCompleted.Add(1)
	switch {
	case elapsed > p.awaitTimeout:
	case elapsed < time.Second:
		p.joinAttachUnder1s.Add(1)
	default:
		p.joinAttach1sToBudget.Add(1)
	}

	ms := elapsed.Milliseconds()
	if ms > math.MaxInt32 {
		ms = math.MaxInt32
	}
	for {
		old := p.joinAttachMsMax.Load()
		if int32(ms) <= old || p.joinAttachMsMax.CompareAndSwap(old, int32(ms)) {
			return
		}
	}
}

// noteSlowAttach counts a successful attach that outlasted AwaitTimeout (#406), split from Join's goroutine so a
// unit test reaches it without a network namespace (#431).
func (p *Plugin) noteSlowAttach(r JoinRequest, elapsed time.Duration) bool {
	// Strictly greater: an attach finishing on budget did not need the grace.
	if elapsed <= p.awaitTimeout {
		return false
	}
	p.joinAttachSlow.Add(1)
	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"took":     elapsed.Round(100 * time.Millisecond).String(),
		"budget":   p.awaitTimeout.String(),
	}).Warn("Attach outlasted AwaitTimeout; the daemon was busy with this container")
	return true
}

// newJoinManager builds the persistent manager from the finished Join answer, marking each family whose gateway the
// engine will install (#1084).
func (p *Plugin) newJoinManager(r JoinRequest, opts DHCPNetworkOptions, hint joinHint, res JoinResponse) *dhcpManager {
	m := newDHCPManager(p.docker, r, opts).withPlugin(p)
	m.setLastIP(false, hint.IPv4)
	m.setLastIP(true, hint.IPv6)
	m.MacAddress = hint.MacAddress
	m.engineGateway(false).Store(res.Gateway != "")
	m.engineGateway(true).Store(res.GatewayIPv6 != "")
	return m
}

// Join hands Docker the host-side link and routes, then starts the persistent DHCP client for the endpoint.
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
		// `docker restart` sends Leave then Join on the same EndpointID without a CreateEndpoint, so the hint and
		// link are gone; reacquire (#46).
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

	if hint.Ifname == "" {
		// On a restart libnetwork re-sends no endpoint options, so the fingerprint carries the custom name (#125).
		hint.Ifname = p.fingerprintIfname(r.EndpointID)
	}
	if hint.Ifname != "" {
		// The persistent client finds the link by MAC or veth peer index, never by name (#125).
		res.InterfaceName.DstName = hint.Ifname
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

	// Copy the host parent's non-default static routes into the container; `-o skip_routes=true` opts out (#102).
	var routeSrc netlink.Link
	if parentAttached {
		routeSrc, err = netlink.LinkByName(opts.Parent)
		if err != nil {
			return res, fmt.Errorf("failed to get parent interface for route copy: %w", err)
		}
	} else {
		routeSrc, err = netlink.LinkByName(opts.Bridge)
		if err != nil {
			return res, fmt.Errorf("failed to get bridge interface: %w", err)
		}
	}

	if err := p.addRoutes(&opts, false, routeSrc, r, hint, &res); err != nil {
		return res, err
	}
	if opts.ipv6Enabled() {
		if err := p.addRoutes(&opts, true, routeSrc, r, hint, &res); err != nil {
			return res, err
		}
	}

	p.appendDHCPStaticRoutes(opts, r, hint, &res)
	if opts.IPv6 {
		p.applyV6JoinHint(opts, r, hint, &res)
	}

	// Register before the start goroutine so a fast Leave finds the manager; Stop waits for Start.
	m := p.newJoinManager(r, opts, hint, res)

	// Set before registerDHCPManager publishes the manager, since Stop reads attachCancel (#406).
	attachCtx, cancelAttach := context.WithTimeout(context.Background(), p.awaitTimeout+attachDaemonBusyGrace)
	m.attachCancel = cancelAttach
	if displaced := p.registerDHCPManager(r.EndpointID, m); displaced != nil {
		// A displaced recovery-registered manager is stopped asynchronously, tracked on p.displacedStops with Add()
		// here so Close waits for it (#338).
		p.displacedStops.Add(1)
		p.displacedStopsTotal.Add(1)
		go func() {
			defer p.displacedStops.Done()
			if err := displaced.Stop(); err != nil {
				log.WithError(err).WithFields(log.Fields{
					"network":  shortID(r.NetworkID),
					"endpoint": shortID(r.EndpointID),
				}).Warn("Failed to stop displaced DHCP manager")
			}
		}()
	}

	go func() {
		// AwaitTimeout plus a grace, since Docker answers nothing about the container inside its own ContainerStart;
		// measured, a fixed 10s budget abandoned three to six attaches per integration run (#401, #406).
		defer cancelAttach()

		attachStart := time.Now()
		err := m.Start(attachCtx)
		if err == nil {
			elapsed := time.Since(attachStart)
			p.noteSlowAttach(r, elapsed)
			p.noteAttachDuration(elapsed)
			// One timing line per successful attach, with the failure line's phase names (#403).
			log.WithFields(log.Fields{
				"network":     shortID(r.NetworkID),
				"endpoint":    shortID(r.EndpointID),
				"took":        elapsed.Round(time.Millisecond).String(),
				"phases":      m.startPhases,
				"phase_total": m.startTotal,
			}).Debug("Attach completed")
		}
		if err != nil {
			fields := log.Fields{
				"network":  shortID(r.NetworkID),
				"endpoint": shortID(r.EndpointID),
				"sandbox":  r.SandboxKey,
			}
			// Per-phase timing rides the failure line, since a bare deadline hides which phase spent the budget
			// (#401, #406).
			if m.startPhases != "" {
				fields["phases"] = m.startPhases
				fields["phase_total"] = m.startTotal
			}
			// An exited container is not join_start_failures, which means a running container without a renewal
			// client (#373, #367); an attach cancelled because the endpoint left is checked first, being the stronger
			// evidence (#406).
			if m.attachAborted.Load() {
				p.joinAbortedEndpointLeft.Add(1)
				log.WithError(err).WithFields(fields).
					Info("Attach cancelled because the endpoint is leaving; no persistent client needed")
				p.removeDHCPManagerIfSame(r.EndpointID, m)
				return
			}
			if joinAbortedByVanish(err, r.SandboxKey) {
				p.joinAbortedContainerGone.Add(1)
				log.WithError(err).WithFields(fields).
					Info("Container went away during attach; no persistent client needed")
				p.removeDHCPManagerIfSame(r.EndpointID, m)
				// No persistent client; the one-shot's address expires on the server (#800).
				return
			}
			// No container claimed the endpoint (#566), not a plugin fault; the address is left to expire (#800).
			if joinFailureLeavesAddressUnused(err) {
				p.joinAbortedNoContainer.Add(1)
				log.WithError(err).WithFields(fields).
					Info("No container claimed the endpoint; its address is left to expire on the server")
				p.removeDHCPManagerIfSame(r.EndpointID, m)
				return
			}

			p.joinStartFailures.Add(1)
			log.WithError(err).WithFields(fields).
				Error("Failed to start persistent DHCP client; lease will not be renewed")
			// De-register a failed Start, identity-checked, since a fast Leave and Join may have installed a new manager.
			p.removeDHCPManagerIfSame(r.EndpointID, m)
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

	stopErr := manager.StopForLeave()

	// LEFT on every stop, since no manager renews the lease; under `release_lease=never` nothing goes on the wire
	// (#800). A family whose lease was handed back is CLOSED, so no INIT-REBOOT names a returned address, and the v6
	// record takes its own phase (#962).
	p.settleReleasedRecord(manager.recordID, manager.releasedV4.Load())
	p.settleReleasedRecord(manager.recordID6, manager.releasedV6.Load())
	if manager.releasedAny() {
		// The tombstone carries both families, so either release skips it; marked here since DeleteEndpoint reads no
		// options (#962).
		p.markEndpointReleased(r.EndpointID)
	}

	// Refresh the fingerprint with the client's last IPs even when Stop failed, since Stop drains the event goroutine
	// first; otherwise the tombstone would carry the initial-DISCOVER IPs (#338).
	v4Addr, v6Addr := manager.lastIPs()
	v4, v6 := "", ""
	if v4Addr != nil && v4Addr.IP != nil {
		v4 = v4Addr.IP.String()
	}
	if v6Addr != nil && v6Addr.IP != nil {
		v6 = v6Addr.IP.String()
	}
	p.updateEndpointIPs(r.EndpointID, v4, v6)

	if stopErr != nil {
		return stopErr
	}

	log.WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
	}).Info("Sandbox left endpoint")

	return nil
}
