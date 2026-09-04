// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
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

// kernelIfaceName returns the name the KERNEL will act on for a given
// Go string, which is not always the Go string.
//
// netlink puts the name in IFLA_IFNAME and the kernel reads it as a C
// string, so it stops at the first NUL. Measured on this project's own
// hardware for #705: netlink.LinkByName("docker0\x00evil") resolved
// docker0 at index 7, while "docker0evil" was not found at all. A NUL in
// a driver option also transports through dockerd untouched — the same
// measurement — so a stored name can carry one.
//
// This exists because a guard that compares two Go strings while the
// kernel compares truncated prefixes is not comparing the same thing.
// #705 closed that for the name being CREATED, by validating it. It did
// not close it for the names being COMPARED AGAINST: those come out of
// Docker's record for other networks, and no write path of ours ever
// touched them. "br0" != "br0\x00evil" is false to Go and true to the
// kernel, so ErrBridgeUsed was skipped and two DHCP networks shared one
// bridge.
//
// Truncation is the whole rule, deliberately, and not a call to
// ValidIfaceName: NUL is the only way two different Go strings can name
// one interface. Over-length names and names containing "/" are refused
// by the kernel outright rather than aliased onto something else, so
// they cannot collide with a name we would accept.
func kernelIfaceName(name string) string {
	if i := strings.IndexByte(name, 0); i >= 0 {
		return name[:i]
	}
	return name
}

// validateModeOptions performs the pure-Go subset of CreateNetwork's
// validation: mode value, and which other options are required or
// forbidden for that mode. It does NOT touch netlink or the docker
// API; the kernel-facing checks (parent NIC up, bridge type, address
// conflicts) are layered on top in CreateNetwork itself.
//
// Returning an error wrapped with fmt.Errorf preserves errors.Is so
// the HTTP layer can map sentinels to 400 status codes.
// Interface names in the options are validated here, in the PURE phase,
// before any kernel-facing call.
//
// opts.Bridge and opts.Parent come straight out of decodeOpts with no
// name validation of their own, and netlink hands a name to the kernel
// zero-terminated: the kernel reads it as a C string and stops at the
// first NUL. So "br0\x00evil" resolves br0, while the reuse guard below
// compares the full Go string (otherOpts.Bridge == opts.Bridge) and
// misses -- two DHCP networks then share one bridge, which is exactly
// what ErrBridgeUsed exists to prevent.
//
// BOTH HALVES MEASURED, and the first is why this is reachable rather
// than latent: the daemon forwards a NUL in a driver option value
// verbatim (a create carrying one reached fork/exec of iptables, which
// rejected it only because execve refuses NUL in argv), and
// netlink.LinkByName("docker0\x00evil") resolved docker0 index 7 while
// "docker0evil" was not found. #705.
//
// ValidIfaceName is the repo's existing rule for exactly this, applied
// to the client interface since v1.0; it also rejects over-length names,
// ".." and "/", all of which reached the kernel before.
func validateModeOptions(opts DHCPNetworkOptions) error {
	// Mode-independent: both server lists apply to every mode, and a
	// malformed or self-contradicting one must fail the create rather
	// than be discovered as "the container got an address from the
	// wrong server" later.
	if _, err := resolveServerPolicy(opts); err != nil {
		return err
	}

	// IPv4 only, for every mode (P-8, M7).
	//
	// Keyed on the DECODED FIELD and not on an option key. decodeOpts
	// runs mapstructure with no MatchName, so the match is
	// case-insensitive against the field name: `-o ipv6=true`,
	// `-o IPv6=true` and `-o Ipv6=true` all set this one bool. A
	// refusal that looked for a key string would enumerate two
	// spellings and miss the third, and the network would be created
	// with IPv6 silently doing nothing — which is what this refusal
	// exists to prevent.
	if opts.IPv6 {
		return util.ErrIPv6Beta
	}

	// RFC 5227 conflict detection, per network (D23). Two refusals and
	// they are separate questions: whether the mode NAMES anything, and
	// whether lease_timeout can fund an acquisition in it.
	mode, err := dhcp.ParseConflictCheck(opts.ConflictCheck)
	if err != nil {
		return fmt.Errorf("%w: %v", util.ErrIPAM, err)
	}
	// Keyed on the DECODED mode and on the operator's own
	// lease_timeout, not on either after defaulting. A check that read
	// the mode back after normalising it to the default would refuse
	// nothing on an `async` network, and one that computed the window
	// from the operator's timeout would compare a number with itself.
	if err := dhcp.CheckLeaseTimeout(opts.LeaseTimeout, mode); err != nil {
		return fmt.Errorf("%w: %v", util.ErrIPAM, err)
	}

	switch opts.effectiveMode() {
	case ModeMacvlan, ModeIPvlan:
		if opts.Parent == "" {
			return util.ErrParentRequired
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
		// validate_dhcp on bridge mode is a v0.9.0 carve-out: the
		// probe semantics differ (parent is an existing bridge, not
		// a NIC) and adding the bridge-mode probe path adds scope
		// without a clear consumer. Reject loudly so an operator
		// who set the opt understands it doesn't apply here, instead
		// of silently no-op'ing and missing a real misconfig.
		if opts.ValidateDHCP {
			return fmt.Errorf("%w: validate_dhcp is not supported in mode=bridge", util.ErrModeMismatch)
		}
	default:
		return fmt.Errorf("%w: %q", util.ErrInvalidMode, opts.Mode)
	}
	return nil
}

// sandboxGone reports whether a Join's sandbox key has been unlinked,
// which is how the container's network namespace disappears when the
// container exits.
//
// Deliberately a filesystem check and not a Docker API call: the API
// round-trip is itself what times out when a container vanishes
// mid-attach (the `failed to get Docker container info: context deadline
// exceeded` in #373), so asking Docker to confirm would be both slower
// and less reliable than looking at the artifact directly.
//
// An empty key returns false — no evidence is not evidence of absence,
// and the caller must fall back to treating the failure as real. The
// same applies to any key that isn't a plain entry in a known netns
// directory: an unrecognised shape is not evidence the container went
// away, so it degrades to the pre-#373 behaviour of counting a real
// failure rather than silently excusing one.
func sandboxGone(sandboxKey string) bool {
	return sandboxGoneIn(sandboxNetnsDirs, sandboxKey)
}

// joinAbortedByVanish reports whether a failed Join failed BECAUSE the
// container went away, rather than because the plugin could not do its
// job for a container that was still there.
//
// #373 established the distinction and answered it one way: has the
// sandbox key been unlinked. That is sound evidence when it fires, and
// it misses the cases where the container's own resources are already
// gone while libnetwork has not yet unlinked the key. Those turned into
// counted plugin faults, nine to twelve per integration run, and read
// as "the CI host is slow" for long enough to send a PR chasing a
// regression that was not there (#401).
//
// The error carries the answer more directly than the filesystem does:
//
//   - "no such container" from the daemon. Every caller resolved the
//     container ID moments earlier, so absence now means removed.
//   - fs.ErrNotExist anywhere in the chain. The only paths a failing
//     Join opens are the sandbox netns and /proc/<pid>/ns/net, both
//     owned by the container; neither can be missing while the
//     container is running. This is why the await helpers now keep the
//     last attempt's error in the chain rather than only in its text.
//
// The sandbox-key check stays as the third answer, unchanged. An error
// this cannot classify still counts a real fault: no usable evidence is
// not evidence of absence, which is the stance #373 took and #376 took
// after it.
func joinAbortedByVanish(err error, sandboxKey string) bool {
	if cerrdefs.IsNotFound(err) {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return sandboxGone(sandboxKey)
}

// joinFailureLeavesAddressUnused reports whether a failed attach proves
// that nothing is using the address the CreateEndpoint one-shot took,
// and that the plugin should therefore hand it back (#566).
//
// It is an allowlist of one, and the narrowness is the whole point.
// Every start failure looks the same from the caller — an error and no
// persistent client — but they divide into opposites:
//
//   - ErrNoContainer means no container holds this endpoint on the
//     network. AwaitCondition has already retried for the entire attach
//     budget before this surfaces, so it is a settled answer, not a
//     glimpse of a container mid-registration. Nothing can be using the
//     address.
//   - Everything else — a missing binary, a netns we could not enter, a
//     timeout — is compatible with a RUNNING container that is using
//     that address right now. Releasing there would hand a live
//     container's address back to the pool for reassignment, which is
//     #524's duplicate-assignment failure with us as the cause.
//
// The costs are not symmetric. A reclaim we skip leaves a lease to
// expire on its own; a reclaim we should not have made takes an address
// away from something using it. So this returns true only where the
// evidence is positive, and new errors are non-reclaiming by default
// rather than by omission.
func joinFailureLeavesAddressUnused(err error) bool {
	return errors.Is(err, util.ErrNoContainer)
}

// sandboxNetnsDirs are the only directories a Join's sandbox key is
// expected to live in. libnetwork bind-mounts each sandbox's netns as
// /var/run/docker/netns/<id>; hosts where /var/run is a symlink to
// /run report the same file under the second form.
var sandboxNetnsDirs = []string{
	"/var/run/docker/netns",
	"/run/docker/netns",
}

// sandboxNetnsVisibleIn reports how many sandbox netns entries the
// plugin can see across the permitted directories, or -1 if it cannot
// read any of them (#567).
//
// This exists to make the difference between "no containers" and "no
// evidence" observable from outside the process. sandboxGoneIn folds
// both into false, deliberately and correctly — for its purpose an
// unreadable directory and a present entry mean the same thing, which
// is "do not conclude the container vanished". The cost of that folding
// is that a directory unreachable for the entire life of the plugin
// looks exactly like a healthy one, and did, for every release up to
// #567.
//
// Separated from sandboxGoneIn rather than folded into it: this is a
// diagnostic and must never influence the decision. A count that could
// change an answer would be a second source of truth for the same
// question.
//
// -1 rather than an error because this feeds a health field sampled on
// every request; the caller has nothing useful to do with an error, and
// a sentinel keeps the JSON shape one integer wide.
// The FIRST readable directory wins rather than a sum across all of
// them. The two entries in sandboxNetnsDirs are usually the same
// directory reached two ways — /var/run is a symlink to /run on most
// hosts — so adding them would report double the real count and make
// the number meaningless exactly where it is being read for a
// comparison against active_endpoints.
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

// sandboxGoneIn is sandboxGone with the permitted directories injected,
// so both answers can be tested without root or a live Docker sandbox.
// Production always passes sandboxNetnsDirs.
//
// It lists the directory and compares names rather than stat'ing the
// key. That looks like the long way round, and it is load-bearing: the
// sandbox key is the only path the plugin takes from a Join request into
// a filesystem call — everywhere else it is merely logged — so no path
// derived from it is ever handed to the filesystem. The only value that
// reaches the OS is one of the compile-time directories above; the
// request-supplied name is used solely in a string comparison. Rewriting
// this as os.Stat(filepath.Join(dir, name)) reintroduces CodeQL
// go/path-injection (flagged on #374) even with the name validated to a
// bare filename first — filepath.Base is not treated as a barrier.
//
// The cost is reading one directory instead of one stat, on a path that
// only runs when starting the persistent client has already failed.
func sandboxGoneIn(dirs []string, sandboxKey string) bool {
	dir, name := splitSandboxKeyIn(dirs, sandboxKey)
	if dir == "" {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// No usable evidence — not a negative result. A permission
		// error must not read as "the container went away": the plugin
		// runs as root and sees the real listing, while /var/run/docker
		// is 0700, so anything less privileged gets EACCES for every
		// key and would otherwise conclude every container had vanished.
		return false
	}
	for _, e := range entries {
		if e.Name() == name {
			return false
		}
	}
	return true
}

// splitSandboxKeyIn validates a sandbox key and splits it into one of
// the permitted directories plus a bare filename. It returns an empty
// dir for anything it does not recognise, which callers must treat as
// "no usable evidence" rather than as a negative result.
func splitSandboxKeyIn(dirs []string, sandboxKey string) (dir, name string) {
	if sandboxKey == "" {
		return "", ""
	}
	clean := filepath.Clean(sandboxKey)
	// filepath.Base strips every directory component, so the name is a
	// bare entry to compare against the directory listing; Clean has
	// already resolved any interior ".." segments.
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
		// Pre-flight DHCP probe (T2-5). Runs before saveOptions so
		// a network that fails the probe leaves no on-disk state
		// behind — the operator's `docker network create` fails
		// cleanly and they can re-issue once the upstream is fixed.
		// Default off; opt-in via -o validate_dhcp=true.
		if opts.ValidateDHCP {
			// The budget covers the probe AND its wait for the parent
			// gate: runDHCPProbe takes that gate itself (#577), because
			// it puts its own child on the parent for up to
			// preflightProbeBudget and so is a holder as well as a
			// waiter.
			// Already validated by validateModeOptions above, so this
			// cannot fail here; resolved again rather than threaded so
			// the probe reads the same source of truth as acquisition.
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
		if err := saveOptions(r.NetworkID, opts); err != nil {
			log.WithError(err).WithField("network", r.NetworkID).
				Warn("Failed to persist options; daemon-restart may need API fallback")
		}
		log.WithFields(log.Fields{
			"network":       r.NetworkID,
			"mode":          mode,
			"parent":        opts.Parent,
			"ipv6":          opts.IPv6,
			"validate_dhcp": opts.ValidateDHCP,
		}).Info("Network created")
		return nil
	}

	// Bridge mode: pure validation already passed; do the kernel-facing
	// and docker-API-facing checks.
	// Through the seam (netlink_seam.go) rather than the package
	// function: the bridge-reuse guard below is pure Go operating on
	// values Docker hands us, and reaching it in a unit test otherwise
	// costs CAP_NET_ADMIN and a real bridge. It was reachable only from
	// the integration suite, which is why the NUL bypass below had no
	// test at all.
	link, err := nlLinkByName(opts.Bridge)
	if err != nil {
		return fmt.Errorf("failed to lookup interface %v: %w", opts.Bridge, err)
	}
	if link.Type() != "bridge" {
		return util.ErrNotBridge
	}

	if !opts.IgnoreConflicts {
		v4Addrs, err := nlAddrList(link, unix.AF_INET)
		if err != nil {
			return fmt.Errorf("failed to retrieve IPv4 addresses for %v: %w", opts.Bridge, err)
		}
		v6Addrs, err := nlAddrList(link, unix.AF_INET6)
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
				} else if kernelIfaceName(otherOpts.Bridge) == kernelIfaceName(opts.Bridge) {
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

// DeleteNetwork "deletes" a DHCP network (the bridge is managed by the
// user). We also evict any persistent DHCP managers attached to this
// network: libnetwork doesn't issue Leave for endpoints in stopped
// containers when the network is removed, so without this prune they
// linger as ghost entries in /Plugin.Health.active_endpoints. Stop is
// safe to call against a manager whose underlying netns is gone — it
// just unblocks the dhcpcd-events loop and returns; dhcpcd itself may
// have already self-exited because its netns vanished.
func (p *Plugin) DeleteNetwork(r DeleteNetworkRequest) error {
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

// vethPairNames derives the host-side and container-side veth names from
// an endpoint ID. Docker EndpointIDs are 64 hex chars in production, but
// recovery / malformed daemon responses can in principle hand us a
// shorter ID — same defensive shape as shortID. The pair-uniqueness
// guarantee is weakened in that case (two short IDs sharing a prefix
// would collide), but the function won't panic.
func vethPairNames(id string) (string, string) {
	prefix := id
	if len(id) > 12 {
		prefix = id[:12]
	}
	return "dh-" + prefix, prefix + "-dh"
}

// parseExplicitV4 extracts the bare IPv4 address from an optional
// libnetwork-supplied Interface.Address (CIDR form, e.g. set by
// `docker run --ip=192.168.0.50`). Returns "" when the field is
// absent; an ErrIPAM-wrapped error when set but malformed or v6.
// The bare-IP form is what dhcpcd's `request` directive (DHCP option
// 50) wants; the mask is supplied by the DHCP ACK, not the operator.
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
	if addr.IP.IsUnspecified() {
		return "", fmt.Errorf("Interface.Address must be a unicast IPv4: got %q: %w", iface.Address, util.ErrIPAM)
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

// resolveExplicitV6 returns the bare IPv6 address the user requested via
// `docker run --ip6` (libnetwork Interface.AddressIPv6), or "" when none
// was supplied. The v6 counterpart of resolveExplicitV4, minus the
// driver-opt channel (there is no `ip6` driver-opt — #213 scope is
// `--ip6` and the tombstone v6 hint). libnetwork passes AddressIPv6 in
// CIDR form; we hand the bare address to dhcpcd's `ia_na / ADDR`
// preferred-address request (#213).
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

// parseDriverOptIP extracts the bare IPv4 address from an optional
// `ip` driver-option. libnetwork places per-endpoint driver-opts
// (from `docker network connect --driver-opt KEY=VAL`) as flat keys
// in r.Options. Bare-IP form here, since that's how operators type
// it on the command line; netmask comes from DHCP regardless. There
// is no `ip6` driver-opt channel: a requested v6 address arrives via
// `--ip6` (Interface.AddressIPv6) and is honoured through dhcpcd's
// `ia_na <iaid> / ADDR` preferred address (see resolveExplicitV6, #213).
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

// netOptions returns the decoded options for a network, CHECKED. It is
// the funnel every caller that acts on a stored record goes through,
// and the check is here rather than at the callers because validation
// is a write-path habit while every sink is on the read path: the file
// was written by an older build, or by hand, or by a build whose
// CreateNetwork guard did not yet exist. An upgrade is the
// reproduction; no attacker is required (#727).
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

// netMode returns ONLY the mode of a stored network, for the one caller
// that needs nothing else.
//
// DeleteEndpoint must run even for a network whose stored options are
// refused, or the veth pair, the ledger entry and the lease outlive the
// container that owned them — a refusal that leaks is worse than the
// name it refused. It also touches no stored name: bridge teardown
// derives its link from vethPairNames(r.EndpointID), and
// deleteParentAttachedEndpoint takes the request alone. The mode is the
// whole of what it reads.
//
// So this returns the mode and nothing else. That is the point of the
// signature: it is not "netOptions with the check turned off" — it
// cannot hand a caller a name to misuse, because it does not return
// one. An opt-out helper would have been the same code with a worse
// shape, and this repo has already paid for one of those (#402/#408).
func (p *Plugin) netMode(ctx context.Context, id string) (mode string, known bool, err error) {
	opts, err := p.netOptionsRaw(ctx, id)
	if err != nil {
		return "", false, err
	}
	m := opts.effectiveMode()
	return m, knownMode(m), nil
}

// knownMode reports whether a mode string is one the plugin implements.
//
// effectiveMode normalises only the EMPTY value, to bridge. Anything
// else it returns verbatim, so an unrecognised mode is not rejected and
// is not defaulted -- it simply fails every `== ModeMacvlan` test it
// meets and lands in whichever branch is written last. In DeleteEndpoint
// that branch is the bridge teardown, which looks up a veth that a
// macvlan endpoint never had, finds nothing, treats "nothing" as
// already-cleaned-up and returns success. The child link and its lease
// survive a teardown that reported it deleted.
//
// CreateNetwork rejects unknown modes, so this only matters for a record
// CreateNetwork did not write -- the same provenance as the unvalidated
// names above, and the same answer: check it where it is read.
func knownMode(mode string) bool {
	switch mode {
	case ModeBridge, ModeMacvlan, ModeIPvlan:
		return true
	default:
		return false
	}
}

// checkStoredOptions refuses a stored record the plugin will not act
// on as written -- an unknown mode, or an interface name that is not
// kernel-legal -- on the READ path, before any caller can use one.
//
// # WHY THE READ PATH AND NOT JUST CreateNetwork
//
// #705 validated opts.Bridge and opts.Parent in validateModeOptions, on
// the CREATE path, and that is where a name first arrives. It is not
// where a name is first USED. Every endpoint handler re-reads the
// options through netOptions and hands the name it finds straight to
// netlink: CreateEndpoint's LinkByName, Join's dstPrefix and route
// copy, EndpointOperInfo's report back to Docker, the parent-attached
// paths and daemon-restart recovery. None of them
// re-validates, because CreateNetwork was assumed to have.
//
// That assumption is false for a network CreateNetwork never validated,
// and those exist and are not exotic:
//
//   - Any network created before #705 shipped. Its unvalidated name was
//     persisted then and is replayed on every endpoint call now, so an
//     upgrade is the reproduction — no attacker required.
//   - The NetworkInspect fallback, which serves networks that pre-date
//     option persistence entirely. It runs decodeOpts and backfills the
//     result to disk without ever calling validateModeOptions.
//   - The state directory itself, which is a plain file tree.
//
// A validator on the write path defends the writes it saw. This one
// defends the reads, which is where the kernel is.
//
// Both names are checked in every mode, deliberately. The mode comes
// out of the same record as the names; if it is the field that is
// wrong, a mode-gated check reads the trusted field to decide whether
// to distrust the others. Refusing an unused name costs nothing —
// CreateNetwork forbids the pairing anyway — and the cost of the
// opposite mistake is a name reaching netlink.
func (p *Plugin) checkStoredOptions(id string, opts DHCPNetworkOptions) error {
	if m := opts.effectiveMode(); !knownMode(m) {
		p.networkOptionsRejected.Add(1)
		log.WithFields(log.Fields{
			"network": shortID(id),
			"mode":    fmt.Sprintf("%q", m),
		}).Error("Refusing stored network options: unknown mode")
		return fmt.Errorf("stored mode %q is not one this plugin implements: %w", m, util.ErrInvalidMode)
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
			// %q so a control character or a NUL is visible in the
			// log rather than mangling the line that reports it.
			"value": fmt.Sprintf("%q", f.name),
		}).Error("Refusing stored network options: interface name is not kernel-legal")
		// ErrIPAM because ErrToStatus maps it to 400, matching the
		// identical refusal validateModeOptions raises at create
		// time. Wrapped LAST, not first, so the operator reads the
		// true sentence before the sentinel's generic one -- the
		// sibling message leads with "only the null IPAM driver is
		// supported", which names the wrong problem.
		return fmt.Errorf("stored %s %q is not a kernel-legal interface name: %w",
			f.field, f.name, util.ErrIPAM)
	}
	return nil
}

// netOptionsRaw is the decode half of netOptions, WITHOUT the name
// check. It prefers the on-disk cache populated by CreateNetwork; the
// fallback to docker NetworkInspect is what makes networks created
// before this fork added persistence keep working after upgrade, while
// every fresh network is served from disk, which is what avoids the
// daemon-restart deadlock when dockerd is calling our endpoint handlers
// while not yet ready to serve API calls.
//
// Its only legitimate callers are netOptions and netMode, and
// TestNetOptionsRaw_HasNoOtherCallers keeps it that way: a third caller
// would be a sink reading a stored name with the guard bypassed, which
// is the entire defect this file just fixed.
func (p *Plugin) netOptionsRaw(ctx context.Context, id string) (DHCPNetworkOptions, error) {
	cached, loadErr := loadOptions(id)
	if loadErr == nil {
		return cached, nil
	}

	// THE BACKFILL BELOW MAY ONLY RUN WHEN THERE WAS NOTHING TO READ.
	//
	// Everything that is not os.IsNotExist means the file exists and we
	// declined or failed to read it -- a schema from a newer build
	// (errStateSchemaTooNew), a corrupt file, or a transient EIO/EMFILE.
	// Falling back to the docker API for THIS CALL is right in all of
	// those cases; the API is authoritative for everything in the
	// struct. Writing afterwards is not. Backfilling on a schema refusal
	// would replace the v2 file with a v1 one, so a downgrade would
	// destroy the newer file instead of declining to read it -- the
	// exact failure stateSchemaVersion exists to prevent. Backfilling on
	// a transient read error would overwrite a good file because the
	// disk had a bad moment.
	//
	// This is the same split tombstoneStore.add draws: a refusal and an
	// absence must not reach a writer as the same thing. Read-only
	// fallback for every failure, a write for absence alone (#724).
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

	opts, err := decodeOpts(n.Options)
	if err != nil {
		return dummy, fmt.Errorf("failed to parse options: %w", err)
	}

	// Backfill: persist options for networks that pre-date the
	// persistence feature so the next call hits the disk path. Guarded
	// on absence for the reason above -- there is no file to lose.
	if absent {
		if err := saveOptions(id, opts); err != nil {
			log.WithError(err).WithField("network", id).
				Debug("Failed to backfill persisted options")
		}
	}
	return opts, nil
}

// CreateEndpoint creates the per-endpoint host-side network plumbing
// (veth pair in bridge mode, macvlan child in macvlan mode), runs dhcpcd
// once to acquire an initial lease, and stashes the result for Join.
// Docker moves the link into the container's netns when it acts on our
// Join response.
func (p *Plugin) CreateEndpoint(ctx context.Context, r CreateEndpointRequest) (CreateEndpointResponse, error) {
	log.WithField("options", r.Options).Debug("CreateEndpoint options")
	res := CreateEndpointResponse{
		Interface: &EndpointInterface{},
	}

	explicitV4, err := resolveExplicitV4(r)
	if err != nil {
		return res, err
	}
	// `docker run --ip6` arrives as Interface.AddressIPv6. Since #152
	// pins the dhcpcd IA_NA we can now request it as the DHCPv6
	// preferred address, so validate it here and ride it into the
	// one-shot below (mirrors explicitV4 / RequestedIP for v4) (#213).
	explicitV6, err := resolveExplicitV6(r)
	if err != nil {
		return res, err
	}

	// Custom interface name (#125): the option only arrives here —
	// libnetwork's remote proxy sends sandbox labels, not endpoint
	// options, to Join — so validate now (rejecting a kernel-invalid
	// name fails the attach loudly at create time) and ride the hint
	// into Join, which returns it as DstName.
	ifname, err := parseIfnameOption(r.Options)
	if err != nil {
		return res, err
	}
	if ifname != "" {
		log.WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
			"ifname":   ifname,
		}).Info("[CreateEndpoint] Honoring custom interface name")
		p.updateJoinHint(r.EndpointID, func(h *joinHint) { h.Ifname = ifname })
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

	// Look up the hostname up front so we can scope tombstone matching
	// to the same container (prevents identity swap during sequential
	// `compose restart`). Best-effort: if the lookup misses or returns
	// empty, consumeTombstone falls back to network-only matching.
	hostname := p.initialDHCPHostname(ctx, r.NetworkID, r.EndpointID)

	// MAC/IP selection priority:
	//   1. Explicit values from libnetwork (`--mac-address`, `--ip`)
	//   2. Tombstone (recently-deleted endpoint on the same network)
	//   3. Kernel-picked MAC, server-picked IP
	// Tombstones are only consumed when no explicit MAC was supplied
	// — explicit MAC means the operator is taking responsibility for
	// identity, and we don't want to surprise-mix in a stale neighbor.
	effectiveMAC := r.Interface.MacAddress
	requestedIP := explicitV4
	requestedV6 := explicitV6
	if effectiveMAC == "" {
		if mac, ip, ipv6, ok := p.consumeTombstone(r.NetworkID, hostname); ok {
			effectiveMAC = mac
			if requestedIP == "" {
				requestedIP = ip
			}
			// Inherit the prior endpoint's IPv6 as the DHCPv6
			// preferred address too, unless `--ip6` already named one,
			// so a restarting container keeps its v6 lease the same
			// way it keeps its v4 lease (#213). The tombstone preserves
			// the bare address end-to-end for exactly this.
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
	// Hoisted out of the closure so the failure path below can close
	// the record it opened. A CREATED record whose CreateEndpoint
	// failed holds no lease and so offers nothing to resume, but it is
	// a line in an append-only file that nothing would ever remove.
	var recordID string

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
		// Record the MAC this endpoint's DHCP identity is keyed to, so
		// Join can re-derive the same id without re-reading a link
		// (dhcpManager.clientID). The orphan-release path needs it after
		// the container is already gone, when there is no link left to
		// read it from at all.
		//
		// Bridge mode does not use MacAddress to *locate* the container
		// link — that is the macvlan/ipvlan branch of
		// locateContainerLink, which bridge never enters — so populating
		// it here changes nothing about link location.
		p.updateJoinHint(r.EndpointID, func(hint *joinHint) {
			hint.MacAddress = ctrLink.Attrs().HardwareAddr
		})
		// MAC-derived so the IPv4 lease survives a restart the way the
		// v6 binding always has; see resolveClientID (#371). Same link,
		// same MAC the DUID-LL/IAID below is pinned to.
		clientID := resolveClientID(opts, r.EndpointID, ctrLink.Attrs().HardwareAddr)

		// The CREATED record (D10). Identity is generated once, here,
		// and written with the record: the option-61 value AS SENT,
		// type byte included, because that is what the server files
		// the lease under. The one-shot below writes its own events to
		// this record, and the Join manager reads them back as an
		// INIT-REBOOT rather than starting a fresh DISCOVER.
		recordID = p.recordCreated(r.NetworkID, ctrLink.Attrs().HardwareAddr, dhcp.ClientIdentity(clientID))
		p.updateJoinHint(r.EndpointID, func(hint *joinHint) {
			hint.RecordID = recordID
		})
		initialIP := func(v6 bool) error {
			v6str := ""
			if v6 {
				v6str = "v6"
			}

			// Server preference ladder (#111) / deny-list (#669). With
			// neither option set this is a single unrestricted attempt
			// with the whole budget — the historical behaviour.
			pol, err := resolveServerPolicy(opts)
			if err != nil {
				return err
			}

			base := dhcp.DHCPClientOptions{
				// .name, not the whole value: this is the DHCP
				// exchange, which is config and not an identity
				// decision. A refused hostname is simply absent here.
				Hostname:    hostname.name,
				FQDN:        opts.fqdnMode(),
				ClientID:    clientID,
				VendorClass: opts.VendorClass,
				// Pin the dhcpcd DUID-LL/IAID off the container veth's
				// MAC so this one-shot and the persistent client (same
				// link, same MAC, post-move) share one identity and the
				// server returns a single binding (#152).
				MAC:      ctrLink.Attrs().HardwareAddr,
				Records:  p.records,
				RecordID: recordID,
			}
			// RFC 5227 conflict detection, from the network's stored
			// conflict_check (D23). Set on the BASE, so every attempt
			// down the dhcp_servers ladder runs in the same mode.
			if err := p.conflictWiring(&base, opts, roleAcquire, r.NetworkID, r.EndpointID); err != nil {
				return err
			}
			// Hint the preferred address per family: `request ADDR`
			// for v4, `ia_na / ADDR` for v6 (#213). Empty values omit
			// the directive, so an unhinted endpoint behaves as before.
			if v6 {
				base.PreferredV6 = requestedV6
			} else {
				base.RequestedIP = requestedIP
			}

			info, ra, err := p.acquireWithPolicy(ctx, ctrName, pol, v6, timeout, r.EndpointID, base)
			if err != nil {
				// A DHCPv6 acquisition that produced nothing is not
				// automatically a failure: on a stateless or SLAAC
				// segment there is no DHCPv6 address by definition, and
				// treating the timeout as fatal meant no container
				// started at all on those networks (#868). What the
				// segment ADVERTISED decides, not how long we waited --
				// a segment offering managed DHCPv6 that then goes
				// quiet is still fatal, here as before.
				if v6 && p.noteV6Absence(ra, ctrName, r.EndpointID, err) {
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
					// No gateways in DHCPv6!
				} else {
					res.Interface.Address = info.IP
					hint.IPv4 = ip
					hint.Gateway = info.Gateway
					if opts.Gateway != "" {
						hint.Gateway = opts.Gateway
					}
					// DHCP option-121 classless static routes (RFC 3442).
					// The parser folded a LITERAL 0.0.0.0/0 entry into
					// info.Gateway, so none of these is a default route
					// by itself. That is all it guarantees: a set of
					// non-default prefixes can still cover the whole
					// address space between them and win on
					// longest-prefix match. See routesSupersedeDefault,
					// which is what says so out loud at Join.
					hint.Routes = dhcpStaticRoutes(info.Routes)
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
		p.closeRecord(recordID)
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
	// netMode, not netOptions: teardown must not be blocked by a
	// stored name it never reads. See netMode's comment.
	mode, modeKnown, err := p.netMode(ctx, r.NetworkID)
	if err != nil {
		return fmt.Errorf("failed to get network options: %w", err)
	}

	// An unrecognised mode does NOT strand the link, and the reason is
	// worth writing down because the obvious fear is wrong.
	//
	// The two teardown branches resolve the SAME name: subLinkName and
	// vethPairNames' host half are both "dh-" + the first 12 bytes of
	// the endpoint ID, by construction and by intent -- subLinkName's
	// own comment says it mirrors the bridge-mode veth prefix. So the
	// bridge branch running against a macvlan endpoint looks up the
	// child link, finds it, and deletes it. Wrong branch, right link.
	// TestTeardownBranchesResolveTheSameLinkName pins that, because it
	// is the whole reason this function can tolerate a mode it cannot
	// read, and nothing else checks that the two names still agree.
	//
	// What an unreadable mode DOES cost is the tombstone below, which
	// is gated on the mode and on nothing else.
	if !modeKnown {
		p.networkOptionsRejected.Add(1)
		log.WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
			"mode":     fmt.Sprintf("%q", mode),
		}).Error("Stored network options carry an unknown mode; running every teardown path rather than guessing one")
	}

	// Lay down a tombstone for the next CreateEndpoint on this
	// network to inherit. ipvlan children share the parent MAC, so
	// the tombstone is meaningless there (we'd just be re-handing the
	// parent MAC back, which the kernel inherits anyway) — skip it.
	//
	// An unknown mode skips it for the opposite reason: it MIGHT be
	// ipvlan, and a tombstone laid for an ipvlan endpoint hands the
	// parent MAC to whichever container consumes it next and occupies
	// the slot the "exactly one match" rule counts. Losing MAC
	// stability once, on a network whose record is already broken, is
	// the cheaper mistake.
	//
	// A REFUSED hostname skips it as well, and for a reason that is
	// the opposite of an absent one. Both reach here as Hostname ==
	// "", and "" is the matcher's wildcard: a tombstone carrying it
	// matches every container on the network, so the hostname we
	// declined to trust for a narrow match would have become a match
	// against everything -- handing this container's MAC and IP to
	// whichever unrelated container next started on the network. A
	// refusal must not look like an absence (#693, #726). The cost is
	// that this one container does not keep its MAC across a restart,
	// which is the correct price for a hostname the plugin would not
	// put in a DHCP packet.
	if fp, ok := p.takeEndpoint(r.EndpointID); ok {
		if modeKnown && mode != ModeIPvlan && !fp.HostnameRefused {
			p.addTombstone(r.NetworkID, fp.Hostname, fp.MAC, fp.IPv4, fp.IPv6)
		}
		// RETAINED, on every mode and every hostname decision, which is
		// wider than the tombstone above deliberately. The tombstone
		// decides whether the next container MAY INHERIT this MAC, and
		// the skips above are about that inheritance being unsafe. The
		// record's tombstone phase decides when this record stops being
		// the answer for this identity, and leaving a record in JOINED
		// after its endpoint is gone would have plugin-restart recovery
		// resume a lease for a container that no longer exists.
		p.retainRecordFor(r.NetworkID, fp.MAC)
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
	// Through the seam, like the parent-attached teardown beside it,
	// so a unit test can prove which paths a delete actually ran
	// rather than infer it from a return value that is nil either way.
	link, err := nlLinkByName(hostName)
	if err != nil {
		// A veth pair dies whole when the container-side end's netns is
		// destroyed (OOM-kill, `docker rm -f`, host reboot race), so a
		// missing host-side link means the cleanup already happened —
		// the same happy-path treatment the macvlan/ipvlan delete path
		// gives it. Hard-failing here 500s the DeleteEndpoint and can
		// wedge `docker network rm`. Anything other than not-found is
		// still a real error.
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

// dhcpStaticRoutes converts DHCP option-121 classless static routes
// (dhcp.Route, captured at CreateEndpoint) into libnetwork
// StaticRoute responses. An empty Gateway means the route is on-link
// (dhcpcd reported the gateway as 0.0.0.0); otherwise it is a next-hop
// route. Destinations are already canonical CIDRs from the parser.
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

// appendDHCPStaticRoutes hands Docker the DHCP option-121 classless
// static routes (RFC 3442) captured from the initial v4 exchange in
// CreateEndpoint. These ride the hint alongside the gateway;
// `skip_routes=true` opts out, matching the host-link copy in addRoutes
// (the opt-121 default route, folded into res.Gateway, is unaffected --
// skip_routes governs static routes, not the default gateway).
//
// Split out of Join so the evidence it produces is testable without a
// container: the routes below can take every destination away from the
// gateway in the same response without changing a byte of it (#700).
func (p *Plugin) appendDHCPStaticRoutes(opts DHCPNetworkOptions, r JoinRequest, hint joinHint, res *JoinResponse) {
	if opts.SkipRoutes || len(hint.Routes) == 0 {
		return
	}

	res.StaticRoutes = append(res.StaticRoutes, hint.Routes...)
	p.dhcpRoutesApplied.Add(int32(len(hint.Routes)))

	// Log the destinations and next hops, not a count. A count cannot
	// answer "where did this container's traffic go" after the fact,
	// and that is the only question these routes raise.
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

// describeStaticRoutes renders routes for a log field as
// "dest via nexthop" / "dest onlink", so the log carries the routing
// decision itself rather than how many of them there were.
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

// addRoutes copies non-default, non-kernel-protocol, non-DHCP-subnet
// routes from a host link into the container's StaticRoutes
// response. Used in bridge mode (link = the configured Linux bridge)
// and in macvlan/ipvlan modes (link = the configured parent NIC) so
// containers inherit operator-added routes the same way regardless
// of attachment mode.
//
// Parent-attached parity was deferred from the macvlan rollout
// (v0.3.0) because the original macvlan use case was "containers
// share the LAN, no extra routes". v0.9.0's DHCP-helper polish
// (#102) extends the bridge-mode behaviour to the parent-attached
// modes for symmetry; `-o skip_routes=true` opts out of either.
func (p *Plugin) addRoutes(opts *DHCPNetworkOptions, v6 bool, link netlink.Link, r JoinRequest, hint joinHint, res *JoinResponse) error {
	family := unix.AF_INET
	if v6 {
		family = unix.AF_INET6
	}

	routes, err := nlRouteListFiltered(family, &netlink.Route{
		LinkIndex: link.Attrs().Index,
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
						Info("[Join] Setting IPv4 gateway retrieved from host parent interface routing table")
				}
			case unix.AF_INET6:
				if res.GatewayIPv6 == "" {
					res.GatewayIPv6 = route.Gw.String()
					log.
						WithFields(logFields).
						WithField("gateway", res.GatewayIPv6).
						Info("[Join] Setting IPv6 gateway retrieved from host parent interface routing table")
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

// parseIfnameOption extracts and validates the optional custom
// container-side interface name (Compose `interface_name`, endpoint
// option com.docker.network.endpoint.ifname — see ifnameOption).
// Returns "" when absent; an error when present but unusable — a name
// the kernel would reject should fail the attach loudly at Join
// rather than surface as an inscrutable rename error inside
// libnetwork. Validation mirrors the kernel's dev_valid_name: 1-15
// bytes (IFNAMSIZ-1), not "." or "..", no '/', no whitespace.
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
	// The kernel is NOT the guard here. Measured: it accepts "-cfoo",
	// "-c", "-" and ".x" as link names and refuses only embedded
	// whitespace -- and this name becomes DstName, the container link is
	// renamed to it, and the name is read back and placed LAST in the
	// dhcpcd argv, where getopt permutation re-reads a flag-shaped
	// trailing positional as an option. Apply the same rule the client
	// side has always applied, so the request fails at CreateEndpoint
	// rather than surviving to the argv (#706).
	if !dhcp.ValidIfaceName(s) {
		return "", fmt.Errorf("invalid interface_name %q: must start with a letter or digit and contain only letters, digits, '.', '-' and '_': %w", s, util.ErrIPAM)
	}
	return s, nil
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
// noteSlowAttach records an attach that succeeded, but only after
// outlasting AwaitTimeout — i.e. one the #406 grace is carrying.
// Reports whether it counted.
//
// Split out of Join's attach goroutine so it can be exercised
// directly (#431). The counter existed for a release without a single
// test asserting it ever moves, which made its constant zero
// uninterpretable: "the daemon-busy window never arose" and "the
// increment cannot fire" produce identical readings, and the v1.4.0
// evidence needed to tell them apart. Reaching this code in the
// goroutine requires a *successful* Start, which needs a real network
// namespace, so no unit test can get here through Join.
//
// Caller must only invoke this for a successful attach. A failed one
// has its own classification below, and counting it here would put a
// fault in a counter documented as not healthy-affecting.
func (p *Plugin) noteSlowAttach(r JoinRequest, elapsed time.Duration) bool {
	// Strictly greater: an attach that finishes exactly on budget did
	// not need the grace.
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

	if hint.Ifname == "" {
		// Container restart: the original hint went with the first
		// Join and libnetwork doesn't re-send endpoint options; the
		// live-endpoint fingerprint keeps the custom name alive.
		hint.Ifname = p.fingerprintIfname(r.EndpointID)
	}
	if hint.Ifname != "" {
		// The persistent DHCP client is rename-proof: it locates the
		// container-side link by MAC (macvlan/ipvlan) or veth peer
		// index (bridge), never by name — honoring a custom name
		// needs no renewal-side changes (#125).
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

	// Copy non-default static routes from the host parent (bridge or
	// macvlan/ipvlan parent NIC) into the container. Operator-added
	// routes on the parent (e.g. "VLAN 250 reachable through the
	// same bridge but not in the DHCP subnet") otherwise stop at the
	// host. `-o skip_routes=true` opts out for either mode.
	//
	// Parent-attached parity (#102) is new in v0.9.0; bridge mode
	// has done this since the upstream's bridge-only era. Pre-v0.9.0
	// macvlan users who depended on the no-copy behaviour can set
	// skip_routes=true to restore it.
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
	if opts.IPv6 {
		if err := p.addRoutes(&opts, true, routeSrc, r, hint, &res); err != nil {
			return res, err
		}
	}

	p.appendDHCPStaticRoutes(opts, r, hint, &res)

	// Register the manager BEFORE spawning the start goroutine so that a
	// fast Leave can find it. Stop blocks until Start has completed
	// (success or failure), so it's safe to call against a manager whose
	// Start is still in flight.
	m := newDHCPManager(p.docker, r, opts).withPlugin(p)
	m.setLastIP(false, hint.IPv4)
	m.setLastIP(true, hint.IPv6)
	m.MacAddress = hint.MacAddress

	// Set BEFORE registerDHCPManager publishes this manager, not after.
	// The comment above says a fast Leave can find it the moment it is
	// registered, and Stop reads attachCancel — so assigning it later
	// is a data race, and worse, a Leave that wins the race reads nil
	// and does not cancel, which is the exact case
	// TestStop_CancelsAnInFlightAttach exists to prevent (#406).
	attachCtx, cancelAttach := context.WithTimeout(context.Background(), p.awaitTimeout+attachDaemonBusyGrace)
	m.attachCancel = cancelAttach
	if displaced := p.registerDHCPManager(r.EndpointID, m); displaced != nil {
		// A recovery-registered manager for this endpoint was still in
		// the registry (Join with no preceding Leave to this plugin
		// instance — plugin restart racing a container restart). Stop
		// it so its dhcpcd doesn't run untracked forever and collide
		// with the new client on the same interface. Asynchronously:
		// Stop blocks on the dhcpcd release cycle and Join shouldn't.
		//
		// Tracked on p.displacedStops so Close can wait for the release
		// to finish rather than let process exit cut it short (#338).
		// Add() runs HERE, synchronously — adding from inside the
		// goroutine would let Close observe an empty group and return
		// before this stop was ever accounted for.
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
		// AwaitTimeout plus a grace, because part of this attach is
		// spent waiting for the daemon that is calling us.
		//
		// Measured (#406): the attach asks Docker about the container
		// being joined while Docker is inside ContainerStart for that
		// same container, and Docker does not answer until it is done.
		// The client's own 2s timeout turns each request into a fast
		// failure, so five of them consume a 10s budget and the attach
		// is abandoned — leaving a RUNNING container with no renewal
		// client, whose lease then expires unrenewed. Three to six per
		// integration run.
		//
		// The budget was never the problem in the sense the first pass
		// at #401 assumed (a slow host); it is that a fixed budget was
		// racing our own caller. The grace covers that window. Stop
		// cancels it, so a container that leaves during the wait does
		// not pay for it.
		defer cancelAttach()

		attachStart := time.Now()
		err := m.Start(attachCtx)
		if err == nil {
			p.noteSlowAttach(r, time.Since(attachStart))
		}
		if err != nil {
			fields := log.Fields{
				"network":  shortID(r.NetworkID),
				"endpoint": shortID(r.EndpointID),
				"sandbox":  r.SandboxKey,
			}
			// Per-phase timing rides the failure line rather than a
			// separate one. "context deadline exceeded" on its own does
			// not say whether the budget went on resolving the container
			// ID or on inspecting it, and those want opposite fixes; a
			// reader correlating two log lines by timestamp will guess
			// instead, which is how #401 was first misdiagnosed (#406).
			if m.startPhases != "" {
				fields["phases"] = m.startPhases
				fields["phase_total"] = m.startTotal
			}
			// A container that exited while we were still attaching to it
			// is not a plugin failure. join_start_failures means "a
			// RUNNING container has no renewal client" and flips healthy;
			// firing it for a container that is simply gone would page an
			// operator over a normal exit, and nothing is missing a
			// renewal client because nothing is there (#373).
			//
			// Prompt exits are the common case, not the exotic one: an
			// application that handles SIGTERM is gone in milliseconds.
			// The suite only stopped hiding this when its containers got
			// an init PID 1 (#367) — `sleep infinity` ignoring SIGTERM
			// had been holding every teardown open for 10s.
			// An attach we cancelled ourselves because the endpoint is
			// leaving. Not a fault, and specifically not the fault this
			// counter names: nothing is left running without a renewal
			// client, because the endpoint is going away. Checked before
			// joinAbortedByVanish because the evidence here is stronger
			// than any of that function's three — we know why the attach
			// stopped, rather than inferring it (#406).
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
				// No persistent client is needed, and the address the
				// CreateEndpoint one-shot took is left where it is: it
				// expires on the server like any other lease nobody
				// comes back for (#800).
				return
			}
			// No container ever claimed this endpoint on the network
			// (#566). Distinguished from the join_start_failures case
			// below because it is not a plugin fault and must not flip
			// Healthy: that counter means "a RUNNING container has no
			// renewal client", and here there is no container at all.
			//
			// This branch used to hand the one-shot's address back, and
			// the paragraph that stood here explained at length why it
			// was narrowed to ErrNoContainer: every other start failure
			// leaves a RUNNING container using the address, so releasing
			// would manufacture #524's duplicate assignment. That
			// asymmetry is still real; the reclaim it was guarding is
			// gone (#800). The address stays leased until it expires,
			// which is the direction that paragraph already called the
			// safe one.
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
			// If Start failed, take ourselves out of the registry so a
			// later Leave doesn't try to Stop() us. Stop() is safe to
			// call against a failed-Start manager (it returns the start
			// error), but de-registering keeps the map tidy. Identity-
			// checked: a fast Leave+Join can already have installed a
			// new healthy manager under this key, which we must not
			// evict.
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

	// LEFT: the manager stopped and the last lease snapshot stays.
	// Written on the error path too, because what it records is that
	// no manager is renewing this lease any more, and that is true
	// whether the stop was clean or wedged. NO RELEASE goes on the
	// wire (D-7, #800) — the address is left to expire on the server's
	// clock, exactly as any other host on the segment leaves it.
	p.recordLeft(manager.recordID)

	// Refresh the endpoint fingerprint with the most recent v4/v6 IPs
	// the persistent client saw, *whether or not Stop succeeded*. Stop
	// drains the event goroutine before returning even on error, so
	// the read here is sequenced after every renew that's going to
	// happen — but go through ipMu anyway so the race detector doesn't
	// have to reason through `select`. Doing this on the error path too
	// means a wedged-dhcpcd shutdown still produces a tombstone with
	// the latest known lease (W-4) — otherwise DeleteEndpoint would
	// lay down a tombstone with the stale initial-DISCOVER IPs.
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
