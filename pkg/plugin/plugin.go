package plugin

import (
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/gorilla/handlers"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

// DriverName is the name of the Docker Network Driver
const DriverName string = "net-dhcp"

// Network attachment modes selected by the `mode` driver option.
const (
	ModeBridge  = "bridge"
	ModeMacvlan = "macvlan"
	ModeIPvlan  = "ipvlan"
)

// initialDHCPHostnameLookupTimeout caps how long CreateEndpoint waits
// for Docker to associate the container with the network so we can
// look up its hostname for the initial DISCOVER. Short on purpose: if
// the lookup misses, the persistent client will fill in the hostname
// on first renewal, so the worst case is "first lease appears in the
// upstream DHCP server's table without a hostname for a few minutes".
const initialDHCPHostnameLookupTimeout = 2 * time.Second

// clientIDFromEndpoint derives a stable DHCP option-61 client identifier
// from a Docker endpoint ID. Docker's endpoint IDs are 64 hex chars
// (32 bytes). We take the first 8 bytes — long enough to be unique
// in any realistic deployment, short enough to keep the option payload
// well below the 255-byte wire limit. The same endpoint ID is used
// across container restarts on the same network, so this client-id
// also stays stable, which is what makes Fritz.Box-style hostname
// reservations actually work for our containers.
//
// Returns nil if the endpoint ID isn't valid hex (which would only
// happen on a fundamentally broken libnetwork request).
func clientIDFromEndpoint(endpointID string) []byte {
	if len(endpointID) < 16 {
		return nil
	}
	b, err := hex.DecodeString(endpointID[:16])
	if err != nil {
		return nil
	}
	return b
}

const defaultLeaseTimeout = 10 * time.Second

// driverRegexp matches plugin references that this driver should treat as
// "another instance of itself" when scanning for bridge conflicts. Upstream
// pinned this to ghcr.io/devplayer0; we accept any registry namespace as
// long as the image name and tag are present, so forks published under a
// different namespace still cross-detect each other on the same host.
var driverRegexp = regexp.MustCompile(`(^|/)docker-net-dhcp:.+$`)

// IsDHCPPlugin checks if a Docker network driver is an instance of this plugin
func IsDHCPPlugin(driver string) bool {
	return driverRegexp.MatchString(driver)
}

// DHCPNetworkOptions contains options for the DHCP network driver
type DHCPNetworkOptions struct {
	// Mode selects the attachment strategy: "bridge" (default, requires
	// `bridge`) or "macvlan" (requires `parent`).
	Mode   string `mapstructure:"mode"`
	Bridge string
	Parent string `mapstructure:"parent"`
	// Gateway, if set, overrides the default gateway returned by the
	// upstream DHCP server. Useful for split-horizon LANs where
	// containers should egress via a different router than the one
	// the DHCP server advertises (e.g. VPN gateway).
	Gateway         string
	IPv6            bool
	LeaseTimeout    time.Duration `mapstructure:"lease_timeout"`
	IgnoreConflicts bool          `mapstructure:"ignore_conflicts"`
	SkipRoutes      bool          `mapstructure:"skip_routes"`
}

// effectiveMode returns Mode with the empty default normalized to ModeBridge.
func (o DHCPNetworkOptions) effectiveMode() string {
	if o.Mode == "" {
		return ModeBridge
	}
	return o.Mode
}

func decodeOpts(input interface{}) (DHCPNetworkOptions, error) {
	var opts DHCPNetworkOptions
	optsDecoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           &opts,
		ErrorUnused:      true,
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
		),
	})
	if err != nil {
		return opts, fmt.Errorf("failed to create options decoder: %w", err)
	}

	if err := optsDecoder.Decode(input); err != nil {
		return opts, err
	}

	return opts, nil
}

type joinHint struct {
	IPv4    *netlink.Addr
	IPv6    *netlink.Addr
	Gateway string
	// MacAddress is set in macvlan mode so the persistent DHCP client can
	// re-find the (renamed) macvlan link inside the container netns by MAC.
	MacAddress net.HardwareAddr
}

// Plugin is the DHCP network plugin
type Plugin struct {
	awaitTimeout time.Duration

	docker *docker.Client
	server http.Server

	// mu guards joinHints and persistentDHCP. libnetwork dispatches
	// CreateEndpoint / Join / Leave from concurrent HTTP handlers,
	// each of which touches one or both maps; without the mutex the
	// race detector reproduces a concurrent map read+write.
	mu             sync.Mutex
	joinHints      map[string]joinHint
	persistentDHCP map[string]*dhcpManager
}

// storeJoinHint records the state collected during CreateEndpoint so
// Join can pick it up.
func (p *Plugin) storeJoinHint(endpointID string, h joinHint) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.joinHints[endpointID] = h
}

// updateJoinHint applies fn to the (possibly-zero) hint for endpointID
// and stores the result. Allows the read-modify-write pattern used in
// CreateEndpoint without exposing the map directly. fn runs under the
// lock — keep it short; do not call back into Plugin from inside fn.
func (p *Plugin) updateJoinHint(endpointID string, fn func(*joinHint)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.joinHints[endpointID]
	fn(&h)
	p.joinHints[endpointID] = h
}

// takeJoinHint atomically retrieves and deletes the join hint for an
// endpoint. Returns ok=false if no hint was registered.
func (p *Plugin) takeJoinHint(endpointID string) (joinHint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.joinHints[endpointID]
	if ok {
		delete(p.joinHints, endpointID)
	}
	return h, ok
}

// registerDHCPManager stores a running per-endpoint DHCP client so Leave
// can find it. Caller registers the manager *before* spawning the
// goroutine that runs dhcpManager.Start; dhcpManager.Stop is safe to
// call against a manager whose Start is still in flight.
func (p *Plugin) registerDHCPManager(endpointID string, m *dhcpManager) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.persistentDHCP[endpointID] = m
}

// takeDHCPManager atomically retrieves and deletes the DHCP manager for
// an endpoint, suitable for Leave's Stop-then-discard pattern.
func (p *Plugin) takeDHCPManager(endpointID string) (*dhcpManager, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.persistentDHCP[endpointID]
	if ok {
		delete(p.persistentDHCP, endpointID)
	}
	return m, ok
}

// recoverEndpoints walks Docker's networks, finds the ones served by
// this plugin, and rebuilds an in-memory dhcpManager for each attached
// endpoint. This restores the lease-renewal goroutines after a plugin
// process restart (e.g. `docker plugin disable` + `enable`, or after
// the plugin container has crashed and been restarted by Docker).
//
// Recovery sources state from Docker rather than persisting our own
// per-endpoint files: NetworkInspect gives us the MAC and IP of each
// attached endpoint, ContainerInspect gives the hostname and the
// container's PID for netns access. udhcpc is invoked with `-r <IP>`
// so the upstream DHCP server can ACK the lease the container is
// already using rather than handing out a fresh one.
func (p *Plugin) recoverEndpoints(ctx context.Context) {
	nets, err := p.docker.NetworkList(ctx, dNetwork.ListOptions{})
	if err != nil {
		log.WithError(err).Warn("recovery: failed to list networks; skipping")
		return
	}
	var recovered, failed int
	for _, n := range nets {
		if !IsDHCPPlugin(n.Driver) {
			continue
		}
		// Re-fetch with full container details (NetworkList is summary-only).
		netInfo, err := p.docker.NetworkInspect(ctx, n.ID, dNetwork.InspectOptions{})
		if err != nil {
			log.WithError(err).WithField("network", n.ID[:12]).
				Warn("recovery: NetworkInspect failed; skipping")
			failed++
			continue
		}
		opts, err := p.netOptions(ctx, n.ID)
		if err != nil {
			log.WithError(err).WithField("network", n.ID[:12]).
				Warn("recovery: failed to load network options; skipping")
			failed++
			continue
		}
		for cid, info := range netInfo.Containers {
			// Skip libnetwork's "ep-<endpoint>" placeholder: it means
			// the container is mid-creation. Either CreateEndpoint /
			// Join will run for it shortly (and our normal flow will
			// take over), or it'll never come up.
			if strings.HasPrefix(cid, "ep-") {
				continue
			}
			if err := p.recoverOneEndpoint(ctx, n.ID, info.EndpointID, info.MacAddress, info.IPv4Address, info.IPv6Address, opts); err != nil {
				log.WithError(err).WithFields(log.Fields{
					"network":  n.ID[:12],
					"endpoint": info.EndpointID[:12],
				}).Warn("recovery: endpoint recovery failed")
				failed++
				continue
			}
			recovered++
		}
	}
	if recovered > 0 || failed > 0 {
		log.WithFields(log.Fields{
			"recovered": recovered,
			"failed":    failed,
		}).Info("Plugin recovery complete")
	}
}

// recoverOneEndpoint synthesises a JoinRequest and dhcpManager for a
// single existing endpoint, then spawns Start in a goroutine. Idempotent:
// if a manager already exists for the endpoint (e.g. because libnetwork
// raced with us and called Join concurrently), we skip.
func (p *Plugin) recoverOneEndpoint(ctx context.Context, networkID, endpointID, macStr, ipv4Cidr, ipv6Cidr string, opts DHCPNetworkOptions) error {
	p.mu.Lock()
	_, exists := p.persistentDHCP[endpointID]
	p.mu.Unlock()
	if exists {
		return nil
	}

	mac, err := net.ParseMAC(macStr)
	if err != nil {
		return fmt.Errorf("parse MAC %q: %w", macStr, err)
	}

	var ipv4, ipv6 *netlink.Addr
	if ipv4Cidr != "" {
		if a, err := netlink.ParseAddr(ipv4Cidr); err == nil {
			ipv4 = a
		}
	}
	if ipv6Cidr != "" {
		if a, err := netlink.ParseAddr(ipv6Cidr); err == nil {
			ipv6 = a
		}
	}

	fakeJoin := JoinRequest{
		NetworkID:  networkID,
		EndpointID: endpointID,
	}
	m := newDHCPManager(p.docker, fakeJoin, opts)
	m.LastIP = ipv4
	m.LastIPv6 = ipv6
	m.MacAddress = mac
	p.registerDHCPManager(endpointID, m)

	go func() {
		startCtx, cancel := context.WithTimeout(context.Background(), p.awaitTimeout)
		defer cancel()
		if err := m.Start(startCtx); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"network":  networkID[:12],
				"endpoint": endpointID[:12],
			}).Warn("recovery: persistent DHCP client Start failed; lease will not renew until next restart")
			p.takeDHCPManager(endpointID)
		}
	}()
	return nil
}

// lookupEndpointMAC reads the MAC address Docker has stored for an
// endpoint by inspecting the network it belongs to. We use this on the
// container-restart path so the rebuilt link can be given the same MAC
// libnetwork already returned to Docker — keeping `docker inspect`'s
// view consistent with the actual interface inside the container.
//
// Returns ErrNoHint-equivalent if the endpoint can't be found, which
// callers treat as "give up and let libnetwork error this Join".
func (p *Plugin) lookupEndpointMAC(ctx context.Context, networkID, endpointID string) (string, error) {
	dockerNet, err := p.docker.NetworkInspect(ctx, networkID, dNetwork.InspectOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to inspect network: %w", err)
	}
	for _, info := range dockerNet.Containers {
		if info.EndpointID == endpointID {
			return info.MacAddress, nil
		}
	}
	return "", fmt.Errorf("endpoint %v not found in network %v's container list", endpointID, networkID)
}

// reacquireEndpoint rebuilds the host-side link and re-runs the initial
// DHCP exchange for an endpoint whose state was lost. Invoked from
// Join when no joinHint is present, which happens when libnetwork
// drives Leave -> Join on the same EndpointID (Docker container restart).
//
// Implementation: synthesise the equivalent CreateEndpointRequest and
// reuse CreateEndpoint's logic. For ipvlan we deliberately leave the
// MAC blank — ipvlan children share the parent's MAC, so passing an
// explicit one would just trip the ipvlan-rejects-custom-MAC check;
// the rebuilt link will inherit the parent's MAC the same way the
// original did.
func (p *Plugin) reacquireEndpoint(ctx context.Context, r JoinRequest, opts DHCPNetworkOptions) error {
	macAddr := ""
	if opts.effectiveMode() != ModeIPvlan {
		mac, err := p.lookupEndpointMAC(ctx, r.NetworkID, r.EndpointID)
		if err != nil {
			return fmt.Errorf("failed to look up original endpoint MAC: %w", err)
		}
		macAddr = mac
	}
	fakeReq := CreateEndpointRequest{
		NetworkID:  r.NetworkID,
		EndpointID: r.EndpointID,
		Interface:  &EndpointInterface{MacAddress: macAddr},
	}
	if _, err := p.CreateEndpoint(ctx, fakeReq); err != nil {
		return fmt.Errorf("CreateEndpoint replay failed: %w", err)
	}
	return nil
}

// initialDHCPHostname makes a best-effort attempt to find the hostname
// of the container we're about to attach an endpoint to, so we can pass
// it in the initial DHCPDISCOVER. Polls the network's Containers map
// for up to initialDHCPHostnameLookupTimeout; if the container hasn't
// been registered yet (it's a race; sometimes Docker calls
// CreateEndpoint before the container appears in the network's
// container list), we fall through with an empty hostname. The
// persistent renewal client populates the hostname later regardless,
// so the worst case is "first lease appears in the upstream DHCP
// server's UI without a hostname for a few minutes".
func (p *Plugin) initialDHCPHostname(ctx context.Context, networkID, endpointID string) string {
	ctx, cancel := context.WithTimeout(ctx, initialDHCPHostnameLookupTimeout)
	defer cancel()

	var hostname string
	_ = util.AwaitCondition(ctx, func() (bool, error) {
		dockerNet, err := p.docker.NetworkInspect(ctx, networkID, dNetwork.InspectOptions{})
		if err != nil {
			// Don't propagate the error — we want to keep retrying
			// while the timeout has time. The caller treats an empty
			// hostname as "not yet known" and lets renewal handle it.
			return false, nil
		}
		for ctrID, info := range dockerNet.Containers {
			if info.EndpointID != endpointID {
				continue
			}
			// Docker uses an "ep-<endpointID>" placeholder until the
			// real container ID is bound. Wait for the real one.
			if strings.HasPrefix(ctrID, "ep-") {
				return false, nil
			}
			ctr, err := p.docker.ContainerInspect(ctx, ctrID)
			if err != nil {
				return false, nil
			}
			hostname = ctr.Config.Hostname
			return true, nil
		}
		return false, nil
	}, 100*time.Millisecond)
	return hostname
}

// NewPlugin creates a new Plugin
func NewPlugin(awaitTimeout time.Duration) (*Plugin, error) {
	client, err := docker.NewClientWithOpts(
		docker.WithHost("unix:///run/docker.sock"),
		docker.WithAPIVersionNegotiation(),
		// Fail fast on hung API calls. Concretely defends against the
		// daemon-startup window where dockerd may be calling into us
		// before it can respond to our own NetworkInspect / etc.
		docker.WithTimeout(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}

	p := Plugin{
		awaitTimeout: awaitTimeout,

		docker: client,

		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/NetworkDriver.GetCapabilities", p.apiGetCapabilities)

	mux.HandleFunc("/NetworkDriver.CreateNetwork", p.apiCreateNetwork)
	mux.HandleFunc("/NetworkDriver.DeleteNetwork", p.apiDeleteNetwork)

	mux.HandleFunc("/NetworkDriver.CreateEndpoint", p.apiCreateEndpoint)
	mux.HandleFunc("/NetworkDriver.EndpointOperInfo", p.apiEndpointOperInfo)
	mux.HandleFunc("/NetworkDriver.DeleteEndpoint", p.apiDeleteEndpoint)

	mux.HandleFunc("/NetworkDriver.Join", p.apiJoin)
	mux.HandleFunc("/NetworkDriver.Leave", p.apiLeave)

	p.server = http.Server{
		Handler: handlers.CustomLoggingHandler(nil, mux, util.WriteAccessLog),
	}

	// Kick off endpoint recovery in the background. We don't block
	// plugin startup on it: libnetwork RPCs for fresh networks should
	// be served immediately. Recovery serialises against those via the
	// Plugin mutex on the maps.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p.recoverEndpoints(ctx)
	}()

	return &p, nil
}

// Listen starts the plugin server
func (p *Plugin) Listen(bindSock string) error {
	l, err := net.Listen("unix", bindSock)
	if err != nil {
		return err
	}

	return p.server.Serve(l)
}

// Close stops the plugin server
func (p *Plugin) Close() error {
	if err := p.docker.Close(); err != nil {
		return fmt.Errorf("failed to close docker client: %w", err)
	}

	if err := p.server.Close(); err != nil {
		return fmt.Errorf("failed to close http server: %w", err)
	}

	return nil
}
