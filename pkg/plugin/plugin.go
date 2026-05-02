package plugin

import (
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	docker "github.com/docker/docker/client"
	"github.com/gorilla/handlers"
	"github.com/mitchellh/mapstructure"
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
