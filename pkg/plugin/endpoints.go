package plugin

import (
	"net/http"
	"time"

	"github.com/devplayer0/docker-net-dhcp/pkg/util"
)

// Payloads are based on https://github.com/docker/go-plugins-helpers/blob/master/network/api.go

// CapabilitiesResponse returns whether or not this network is global or local
type CapabilitiesResponse struct {
	Scope             string
	ConnectivityScope string
}

func (p *Plugin) apiGetCapabilities(w http.ResponseWriter, r *http.Request) {
	util.JSONResponse(w, CapabilitiesResponse{
		Scope:             "local",
		ConnectivityScope: "global",
	}, http.StatusOK)
}

// IPAMData contains IPv4 or IPv6 addressing information
type IPAMData struct {
	AddressSpace string
	Pool         string
	Gateway      string
	AuxAddresses map[string]interface{}
}

// CreateNetworkRequest is sent by the daemon when a network needs to be created
type CreateNetworkRequest struct {
	NetworkID string
	Options   map[string]interface{}
	IPv4Data  []*IPAMData
	IPv6Data  []*IPAMData
}

func (p *Plugin) apiCreateNetwork(w http.ResponseWriter, r *http.Request) {
	var req CreateNetworkRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}

	if err := p.CreateNetwork(req); err != nil {
		util.JSONErrResponse(w, err, 0)
		return
	}

	util.JSONResponse(w, struct{}{}, http.StatusOK)
}

// DeleteNetworkRequest is sent by the daemon when a network needs to be removed
type DeleteNetworkRequest struct {
	NetworkID string
}

func (p *Plugin) apiDeleteNetwork(w http.ResponseWriter, r *http.Request) {
	var req DeleteNetworkRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}

	if err := p.DeleteNetwork(req); err != nil {
		util.JSONErrResponse(w, err, 0)
		return
	}

	util.JSONResponse(w, struct{}{}, http.StatusOK)
}

// EndpointInterface contains endpoint interface information
type EndpointInterface struct {
	Address     string
	AddressIPv6 string
	MacAddress  string
}

// CreateEndpointRequest is sent by the daemon when an endpoint should be created
type CreateEndpointRequest struct {
	NetworkID  string
	EndpointID string
	Interface  *EndpointInterface
	Options    map[string]interface{}
}

// CreateEndpointResponse is sent as a response to a CreateEndpointRequest
type CreateEndpointResponse struct {
	Interface *EndpointInterface
}

func (p *Plugin) apiCreateEndpoint(w http.ResponseWriter, r *http.Request) {
	var req CreateEndpointRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}

	res, err := p.CreateEndpoint(r.Context(), req)
	if err != nil {
		util.JSONErrResponse(w, err, 0)
		return
	}

	util.JSONResponse(w, res, http.StatusOK)
}

// InfoRequest is sent by the daemon when querying endpoint information
type InfoRequest struct {
	NetworkID  string
	EndpointID string
}

// InfoResponse is endpoint information sent in response to an InfoRequest
type InfoResponse struct {
	Value map[string]string
}

func (p *Plugin) apiEndpointOperInfo(w http.ResponseWriter, r *http.Request) {
	var req InfoRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}

	res, err := p.EndpointOperInfo(r.Context(), req)
	if err != nil {
		util.JSONErrResponse(w, err, 0)
		return
	}

	util.JSONResponse(w, res, http.StatusOK)
}

// DeleteEndpointRequest is sent by the daemon when an endpoint needs to be removed
type DeleteEndpointRequest struct {
	NetworkID  string
	EndpointID string
}

func (p *Plugin) apiDeleteEndpoint(w http.ResponseWriter, r *http.Request) {
	var req DeleteEndpointRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}

	if err := p.DeleteEndpoint(r.Context(), req); err != nil {
		util.JSONErrResponse(w, err, 0)
		return
	}

	util.JSONResponse(w, struct{}{}, http.StatusOK)
}

// JoinRequest is sent by the Daemon when an endpoint needs be joined to a network
type JoinRequest struct {
	NetworkID  string
	EndpointID string
	SandboxKey string
	Options    map[string]interface{}
}

// ifnameOption is the endpoint option carrying a user-requested
// container-side interface name. Compose's
// `services.*.networks.*.interface_name` (engine 28+ / API 1.48+)
// ships as this key; `docker network connect --driver-opt` can set it
// on any engine version. The plugin honors it by returning DstName in
// the Join response (#125).
const ifnameOption = "com.docker.network.endpoint.ifname"

// InterfaceName consists of the name of the interface in the global netns and
// the desired prefix to be appended to the interface inside the container netns.
//
// DstName, when non-empty, asks libnetwork for that exact name inside
// the container instead of DstPrefix+index. The remote-driver API has
// carried the field for years, but as of moby master the remote proxy
// drops it (drivers/remote/driver.go calls
// `iface.SetNames(SrcName, DstPrefix, "")`), so engines do not yet
// apply it for plugin drivers — built-in drivers got per-driver
// interface_name support in engine 28, remote drivers were left out.
// We return it anyway: it is the documented response shape, costs
// nothing on engines that ignore it, and activates the moment the
// upstream pass-through lands (#125).
type InterfaceName struct {
	SrcName   string
	DstPrefix string
	DstName   string
}

// libnetwork's route-type encoding for StaticRoute.RouteType.
// See https://github.com/moby/libnetwork/blob/master/docs/remote.md
// — 0 ("via gateway") expects a NextHop; 1 ("on-link / connected")
// has no next hop.
const (
	RouteTypeNextHop = 0
	RouteTypeOnLink  = 1
)

// StaticRoute contains static route information
type StaticRoute struct {
	Destination string
	RouteType   int
	NextHop     string
}

// JoinResponse is sent in response to a JoinRequest
type JoinResponse struct {
	InterfaceName         InterfaceName
	Gateway               string
	GatewayIPv6           string
	StaticRoutes          []*StaticRoute
	DisableGatewayService bool
}

func (p *Plugin) apiJoin(w http.ResponseWriter, r *http.Request) {
	var req JoinRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}

	res, err := p.Join(r.Context(), req)
	if err != nil {
		util.JSONErrResponse(w, err, 0)
		return
	}

	util.JSONResponse(w, res, http.StatusOK)
}

// LeaveRequest is sent by the daemon when a endpoint is leaving a network
type LeaveRequest struct {
	NetworkID  string
	EndpointID string
}

func (p *Plugin) apiLeave(w http.ResponseWriter, r *http.Request) {
	var req LeaveRequest
	if err := util.ParseJSONOrErrorResponse(&req, w, r); err != nil {
		return
	}

	if err := p.Leave(r.Context(), req); err != nil {
		util.JSONErrResponse(w, err, 0)
		return
	}

	util.JSONResponse(w, struct{}{}, http.StatusOK)
}

// HealthResponse is the payload returned by /Plugin.Health.
//
// Healthy is false when at least one plugin-restart recovery failed —
// the plugin keeps serving requests for fresh attaches, but containers
// that were running before the restart and got a recovery failure are
// now running without lease renewal and will lose their IP at lease
// expiry. Operators should restart those containers (which produces a
// fresh CreateEndpoint and gets them back into the persistent map).
type HealthResponse struct {
	Healthy         bool    `json:"healthy"`
	UptimeSeconds   float64 `json:"uptime_seconds"`
	ActiveEndpoints int     `json:"active_endpoints"`
	PendingHints    int     `json:"pending_hints"`
	RecoveredOK     int32   `json:"recovered_ok"`
	// RecoveryFailed counts post-restart recoveries that failed for a
	// container that was still running: it has no renewal client and
	// will lose its lease at expiry. Healthy-affecting.
	//
	// Two conditions were folded into this counter historically and are
	// now split out, because neither leaves a running container without
	// a renewal client and both are routine after a daemon restart:
	// RecoveryDeferred (#383) and RecoveryAbortedContainerGone (#376).
	RecoveryFailed int32 `json:"recovery_failed"`
	// RecoveryDeferred counts the times recovery met a daemon that was
	// not serving yet and was retried once the socket came up (#383).
	// Docker respawns the plugin during its own startup, so this is the
	// expected state at that moment, not a fault — NOT Healthy-affecting.
	// A rise paired with recovery_failed means the retry ran out too:
	// that pair is the signal that endpoints really are unrecovered.
	RecoveryDeferred int32 `json:"recovery_deferred"`
	// RecoveryAbortedContainerGone counts recoveries abandoned because
	// the container had already exited or been removed (#376). Not
	// Healthy-affecting: nothing is running without a renewal client.
	// The recovery-side twin of JoinAbortedContainerGone, and normal
	// after a daemon restart that outlived some containers.
	RecoveryAbortedContainerGone int32 `json:"recovery_aborted_container_gone"`
	// JoinStartFailures counts persistent-client Start failures at
	// Join time (#317): a running container with no renewal client.
	// Healthy-affecting — same operator action as recovery_failed
	// (find the cause in the plugin log, restart the container).
	JoinStartFailures int32 `json:"join_start_failures"`
	// JoinAbortedContainerGone counts attaches abandoned because the
	// container exited before the persistent client was up (#373). Not
	// Healthy-affecting: there is no running container without a
	// renewal client. Worth watching anyway — a rise means containers
	// are dying seconds after start.
	JoinAbortedContainerGone int32 `json:"join_aborted_container_gone"`

	// JoinAttachSlow counts attaches that succeeded only after
	// outlasting AwaitTimeout, waiting on a daemon that was busy with
	// the container being attached. Not healthy-affecting — these are
	// successes — but a rising count is the visible form of #406.
	JoinAttachSlow int32 `json:"join_attach_slow"`

	// JoinAbortedEndpointLeft counts attaches cancelled because the
	// endpoint left while the attach was still running. Not
	// healthy-affecting: there is no running container missing a
	// renewal client.
	JoinAbortedEndpointLeft int32 `json:"join_aborted_endpoint_left"`
	TombstoneWriteFailures  int32 `json:"tombstone_write_failures"`
	// LeaseChanged counts renewals where dhcpcd returned a different
	// IP than the manager last recorded. Not Healthy-affecting (it
	// doesn't break Docker's view fatally — see plugin.go for the
	// truthfulness-gap discussion), but worth alerting on for
	// long-running containers.
	LeaseChanged int32 `json:"lease_changed"`

	// DHCP-wire counters (T2-4). Naming intentionally drops the
	// Prometheus `_total` suffix to stay consistent with the
	// existing fields above; the issue's proposal listed them with
	// `_total` for documentation clarity but the wire field is the
	// shorter form.
	LeasesObtained       int32 `json:"leases_obtained"`
	LeasesRenewed        int32 `json:"leases_renewed"`
	DHCPTimeouts         int32 `json:"dhcp_timeouts"`
	LeaseReleaseFailures int32 `json:"lease_release_failures"`
	// NAKsReceived counts server NAKs on renewal/rebind. Not
	// Healthy-affecting on its own — dhcpcd recovers by
	// re-DISCOVERing — but each NAK-triggered re-bind widens the
	// docker-inspect divergence tracked by lease_changed (#128).
	NAKsReceived int32 `json:"naks_received"`
	// DisplacedStops counts managers displaced at Join — a Join that
	// found a recovery-registered manager still in the registry for
	// the same endpoint (plugin restart racing a container restart).
	// Not Healthy-affecting: the displaced client is stopped and
	// released, and the new one takes over. A climbing value means
	// containers are restarting into a plugin that had recovered them,
	// so pair it with recovered_ok when diagnosing a restart loop.
	DisplacedStops int32 `json:"displaced_stops"`
	// OrphanedLeasesReleased / OrphanedLeaseReleaseFailures cover the
	// lease acquired by the CreateEndpoint one-shot when no persistent
	// client ever took ownership of it, because the container exited
	// before Join's async Start could attach (#370). The plugin
	// synthesises a release rather than leaving the address held until
	// its own expiry.
	//
	// Neither is Healthy-affecting. A short-lived container is an
	// ordinary lifecycle, and a failed synthesised release costs one
	// lease until it expires — alert on the failure rate, not on a
	// latched unhealthy. Read the two together: releases climbing with
	// failures flat is the mechanism working.
	OrphanedLeasesReleased       int32 `json:"orphaned_leases_released"`
	OrphanedLeaseReleaseFailures int32 `json:"orphaned_lease_release_failures"`
	// LedgerWriteFailures counts failed appends to the audit_log
	// lease ledger (#109). Not Healthy-affecting — a lost audit line
	// degrades forensics, not networking; operators using audit_log
	// alert on this directly.
	LedgerWriteFailures int32 `json:"ledger_write_failures"`

	// Per-family (IPv6) breakdown of the wire counters (#212). Each
	// counts only the v6 client's events; the un-suffixed fields above
	// remain v4+v6 aggregates, so the v4 share is the aggregate minus
	// the matching *_v6 value. On a dual-stack host this isolates the
	// v6-specific failure signal (NAK/timeout) the aggregate hides.
	LeaseChangedV6   int32 `json:"lease_changed_v6"`
	LeasesObtainedV6 int32 `json:"leases_obtained_v6"`
	LeasesRenewedV6  int32 `json:"leases_renewed_v6"`
	DHCPTimeoutsV6   int32 `json:"dhcp_timeouts_v6"`
	NAKsReceivedV6   int32 `json:"naks_received_v6"`
}

func (p *Plugin) apiHealth(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	active := len(p.persistentDHCP)
	pending := len(p.joinHints)
	p.mu.Unlock()

	failed := p.recoveryFailed.Load()
	joinFails := p.joinStartFailures.Load()
	tsFails := p.tombstoneWriteFailures.Load()
	util.JSONResponse(w, HealthResponse{
		// Healthy is false on any condition that means an operator
		// should look: a recovery or join-start failure means a running
		// container has no renewal goroutine; a tombstone-write failure
		// means the next restart of some container will pick a new
		// MAC/IP.
		Healthy:           failed == 0 && joinFails == 0 && tsFails == 0,
		UptimeSeconds:     time.Since(p.startTime).Seconds(),
		ActiveEndpoints:   active,
		PendingHints:      pending,
		RecoveredOK:       p.recoveredOK.Load(),
		RecoveryFailed:    failed,
		JoinStartFailures: joinFails,
		// The two below are deliberately absent from the Healthy
		// expression above, like JoinAbortedContainerGone. A daemon that
		// was still starting (#383) and a container that had already
		// exited when recovery reached it (#376) both leave nothing
		// behind to be unhealthy about.
		RecoveryDeferred:             p.recoveryDeferred.Load(),
		RecoveryAbortedContainerGone: p.recoveryAbortedContainerGone.Load(),
		JoinAbortedContainerGone:     p.joinAbortedContainerGone.Load(),
		JoinAttachSlow:               p.joinAttachSlow.Load(),
		JoinAbortedEndpointLeft:      p.joinAbortedEndpointLeft.Load(),
		TombstoneWriteFailures:       tsFails,
		LeaseChanged:                 p.leaseChanged.Load(),
		LeasesObtained:               p.leasesObtained.Load(),
		LeasesRenewed:                p.leasesRenewed.Load(),
		DHCPTimeouts:                 p.dhcpTimeouts.Load(),
		LeaseReleaseFailures:         p.leaseReleaseFailures.Load(),
		NAKsReceived:                 p.naksReceived.Load(),
		DisplacedStops:               p.displacedStopsTotal.Load(),
		OrphanedLeasesReleased:       p.orphanedLeasesReleased.Load(),
		OrphanedLeaseReleaseFailures: p.orphanedLeaseReleaseFailures.Load(),
		LedgerWriteFailures:          p.ledgerWriteFailures.Load(),
		LeaseChangedV6:               p.leaseChangedV6.Load(),
		LeasesObtainedV6:             p.leasesObtainedV6.Load(),
		LeasesRenewedV6:              p.leasesRenewedV6.Load(),
		DHCPTimeoutsV6:               p.dhcpTimeoutsV6.Load(),
		NAKsReceivedV6:               p.naksReceivedV6.Load(),
	}, http.StatusOK)
}
