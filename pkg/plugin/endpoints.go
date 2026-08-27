// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net/http"
	"time"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
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
// carried the field for years, but the remote proxy dropped it
// (drivers/remote/driver.go called `iface.SetNames(SrcName, DstPrefix,
// "")`) until moby/moby#52866, merged 2026-08-26 and milestoned for
// engine 29.8.0. Built-in drivers got per-driver interface_name in
// engine 28; remote drivers were left out until that fix. No released
// engine carries it yet, so on 29.7.x and older the field is still
// ignored. We return it either way: it is the documented response
// shape, costs nothing on engines that ignore it, and activates by
// itself on the first engine that honours it (#125).
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
// # WHAT `Healthy` MEANS
//
// False when any of FIVE counters is non-zero: recovery_failed,
// join_start_failures, tombstone_write_failures, address_conflicts and
// tombstone_quarantines. Each is marked Healthy-affecting on its field
// below, and docs/reference.md states the same set in four more places;
// scripts/check-health-contract.sh keeps those in step.
//
// This comment said "at least one plugin-restart recovery failed" —
// ONE counter — from before v1.6.0 until #724. The expression 350 lines
// below had four by then. It is the comment a developer reads first
// when adding a counter, which is exactly how it stayed wrong for two
// releases: the gate reads reference.md and the expression, not this.
// Corrected here rather than only in the docs, because the next person
// to add a Healthy-affecting counter reads this file (#638, #724).
//
// # IT LATCHES, AND THE OBVIOUS REMEDY DOES NOT CLEAR IT
//
// Every counter behind the flag is a monotonic atomic; nothing
// decrements them. So `healthy: false` means "a fault occurred at some
// point during THIS plugin process", not "something is wrong right
// now". An operator who restarts the affected containers fixes the
// condition — and the flag stays false. The only thing that clears it
// is restarting the plugin, which tears down the renewal client of
// every managed endpoint on the host, so it is not a free action and
// must not be taken as routine hygiene. Pair a reading with InstanceID
// to tell "still the same process, still latched" from "a new process
// that has already gone bad".
//
// That is deliberate. An alert that goes quiet on its own is worse than
// one that never clears, because the operator learns nothing from the
// silence. If "unhealthy right now" is ever wanted, it is a new field,
// not a change to this one.
type HealthResponse struct {
	Healthy bool `json:"healthy"`
	// InstanceID identifies the plugin process that served this
	// response. Every counter below is in-memory and returns to zero
	// when the process does, so two reads are only comparable as a
	// delta when their InstanceID matches (#405).
	//
	// uptime_seconds is a weaker version of the same signal: it does
	// reset, but a plugin that restarts early in a long window and then
	// runs longer than the first reading shows uptime going *up* across
	// the pair, and the reset goes unnoticed. Comparing ids has no such
	// blind spot.
	InstanceID      string  `json:"instance_id"`
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
	// RecoveryNetworkGone counts networks skipped during post-restart
	// recovery because they had been removed between the NetworkList
	// that found them and the NetworkInspect that reads their detail
	// (#648). Not Healthy-affecting: a network that is gone leaves no
	// running container without a renewal client. Counted rather than
	// silent so a host churning networks under a restarting daemon is
	// still visible. It landed in recovery_failed until #648, where it
	// was fatal.
	RecoveryNetworkGone int32 `json:"recovery_network_gone"`
	// RecoveryFingerprintsSkipped counts endpoints recovery adopted but
	// could not describe: the ContainerInspect that would have supplied
	// the hostname did not answer, or answered with no hostname (#721).
	// Not Healthy-affecting: the endpoint has a renewal client, so no
	// running container is without one — what it has lost is the
	// tombstone that would have carried its MAC and address across its
	// next `docker restart`.
	//
	// It exists because #721's fix would otherwise have inherited the
	// invisibility of the bug it closes. A skipped fingerprint means no
	// tombstone, and the only outward sign of that was
	// tombstones_consumed staying flat — indistinguishable from a quiet
	// host. A hostname REFUSED by safeHostname is not counted here; it
	// moves unsafe_hostnames_rejected instead, so "the daemon would not
	// answer me" stays distinguishable from "a container sent a hostname
	// nobody should send".
	RecoveryFingerprintsSkipped int32 `json:"recovery_fingerprints_skipped"`
	// RecoveryAlreadyManaged counts endpoints a recovery walk found
	// already registered to another manager and therefore left alone —
	// a Join reached them first. Not Healthy-affecting: the endpoint has
	// a renewal client, it just is not the one this walk would have
	// built. Counted because it is the only outward evidence of recovery
	// racing a Join, and because the completion log used to report those
	// endpoints as recovered (#480).
	RecoveryAlreadyManaged int32 `json:"recovery_already_managed"`
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
	// JoinAbortedNoContainer counts attaches abandoned because no
	// container ever claimed the endpoint on the network, and whose
	// address was therefore released rather than left to expire (#566).
	// Not Healthy-affecting: nothing is running without a renewal
	// client, because nothing is running. A rise means endpoints are
	// being created for containers that never attach.
	JoinAbortedNoContainer int32 `json:"join_aborted_no_container"`

	// JoinAttachSlow counts attaches that succeeded only after
	// outlasting AwaitTimeout, waiting on a daemon that was busy with
	// the container being attached. Not healthy-affecting — these are
	// successes — but a rising count is the visible form of #406.
	JoinAttachSlow int32 `json:"join_attach_slow"`

	// RestartLinkUpWaited counts child links brought up only after
	// waiting out the departing link's hold on the address (#408). Not
	// healthy-affecting: this is the fix working, and it is counted so
	// the window is visible rather than inferred — the same reason
	// JoinAttachSlow exists.
	RestartLinkUpWaited int32 `json:"restart_link_up_waited"`
	// RestartLinkUpTimeouts counts that wait outlasting its budget. The
	// restart then fails with `address already in use`. Not
	// healthy-affecting despite being a real failure: it surfaces
	// through CreateEndpoint to the operator directly, and `healthy`
	// is for faults nothing else reports (#422).
	RestartLinkUpTimeouts int32 `json:"restart_link_up_timeouts"`

	// JoinAbortedEndpointLeft counts attaches cancelled because the
	// endpoint left while the attach was still running. Not
	// healthy-affecting: there is no running container missing a
	// renewal client.
	JoinAbortedEndpointLeft int32 `json:"join_aborted_endpoint_left"`
	// TombstoneWriteFailures counts tombstone persistence failures.
	// Healthy-affecting: an endpoint will not keep its address across a
	// restart.
	//
	// It moves on a failed READ as well as a failed write. Since #724,
	// a transient read error (EIO, EMFILE, a read racing a writer) makes
	// the write path refuse rather than rewrite the file from nothing,
	// and that refusal is counted here — the consequence is identical to
	// a failed write, and the name being narrower than the meaning is
	// worth one sentence rather than a fourth counter.
	TombstoneWriteFailures int32 `json:"tombstone_write_failures"`
	// TombstoneQuarantines counts times the tombstone file was found
	// unparseable and moved aside as tombstones.json.corrupt-<ts>
	// (#724). Healthy-affecting, and the counter that costs the most
	// when it moves: a write failure loses ONE container's MAC and
	// address, a quarantine loses every live tombstone on the host, so
	// every container restarting for the rest of the TTL window comes
	// back with a new identity.
	//
	// Separate from TombstoneWriteFailures on purpose. The two have
	// different remedies — a write failure means the disk is full or
	// read-only, a quarantine leaves a file to read — and merging them
	// would leave an operator unable to tell which one they are being
	// paged for.
	//
	// WHY IT LATCHES `healthy`, WHICH IS NOT OBVIOUS. The argument
	// against is real: the condition is self-healing by construction —
	// the file is renamed away, the plugin continues correctly from an
	// empty set, and the cost is bounded at one TTL window of address
	// instability for containers that happen to restart in it. Against
	// that, the remedy for a latched `healthy` is to restart the
	// plugin, which tears down every managed endpoint's renewal client:
	// strictly more damaging than the fault. On those terms alone it
	// would not latch.
	//
	// It latches anyway, for two reasons. Consistency first:
	// TombstoneWriteFailures is already healthy-affecting, and a
	// quarantine is the same family — tombstones did not work. Splitting
	// them would mean an I/O error latches and actual file corruption
	// does not. And the one that decides it: a quarantine does not mean
	// tombstones had a bad minute, it means SOMETHING WROTE GARBAGE
	// into stateDir — a host bind mount that survives `docker plugin rm`
	// and upgrade, and that now also holds the versioned options file.
	// The self-healing is about the tombstones. The signal is about the
	// disk, and that is worth an operator's attention even though this
	// particular symptom cleared itself.
	TombstoneQuarantines int32 `json:"tombstone_quarantines"`
	// UnsafeHostnamesRejected counts container hostnames dropped before
	// reaching the generated DHCP client config because they carried a
	// control character (#692). NOT healthy-affecting: the drop is the
	// safe outcome and the lease proceeds. It is reported because a
	// legitimate hostname never contains one, so a rising value is
	// somebody probing rather than background noise.
	UnsafeHostnamesRejected int32 `json:"unsafe_hostnames_rejected"`
	// UnsafeOptionValuesDropped counts server-chosen DHCP string
	// values refused before use because they carried a control
	// character, plus option-15 domains truncated at their first space.
	// NOT healthy-affecting: dropping is the safe outcome and the lease
	// proceeds. Its sibling above covers the value the CONTAINER
	// chooses; this one covers the values the SERVER chooses, which is
	// the larger set and the one nothing filtered before (#703, #704).
	UnsafeOptionValuesDropped int32 `json:"unsafe_option_values_dropped"`
	// NetworkOptionsRejected counts endpoint operations that met a
	// network's stored options and would not act on them as written:
	// an interface name the kernel would not accept, or a mode this
	// plugin does not implement (#727). DeleteEndpoint counts without
	// refusing, so a rise does not mean nothing was torn down. NOT
	// healthy-affecting: refusing is the safe outcome and the
	// operation already fails visibly to Docker; one network's record
	// is broken, not the plugin. A non-zero value means options
	// written before name validation existed (#705), or a hand-edited
	// state directory.
	NetworkOptionsRejected int32 `json:"network_options_rejected"`
	// DNSPropagationPIDMismatches counts DNS propagations refused
	// because the container PID resolved through Docker no longer
	// belonged to that container by the time the plugin acted on it
	// (#688). NOT healthy-affecting: refusing is the safe outcome and
	// the container keeps the resolv.conf it had. It is reported
	// because the plugin shares the host PID namespace, so each one is
	// a write that would otherwise have gone to an unrelated host
	// process.
	DNSPropagationPIDMismatches int32 `json:"dns_propagation_pid_mismatches"`
	// NetnsPIDMismatches counts sandbox network-namespace opens refused
	// because the container PID resolved through Docker no longer named
	// that container. The attach fails, so this is not silent -- but the
	// failure looks like a slow start; only this counter distinguishes a
	// recycled PID from one.
	NetnsPIDMismatches int32 `json:"netns_pid_mismatches"`
	// DHCPRoutesApplied counts DHCP option-121 classless static routes
	// handed to Docker. DHCPDefaultRouteSuperseded counts the Joins
	// where those routes cover 0.0.0.0/0 by union rather than by a
	// literal default entry -- i.e. the container's egress goes to the
	// option-121 next hop even though the reported gateway, and
	// `docker inspect`, still name the router from option 3. Neither is
	// healthy-affecting: this is legitimate split-tunnel behaviour as
	// often as it is not. They are the evidence trail (#700).
	DHCPRoutesApplied          int32 `json:"dhcp_routes_applied"`
	DHCPDefaultRouteSuperseded int32 `json:"dhcp_default_route_superseded"`
	// LeaseTimeClamped counts option-51 lifetimes cut down before use
	// as the outage watchdog's deadline. NOT healthy-affecting -- the
	// clamp is the safe outcome and the lease time reported to
	// operators is unchanged. Any non-zero value is worth reading: an
	// over-long lease is how a server switches this plugin's only
	// silent-lapse detector off (#701).
	LeaseTimeClamped int32 `json:"lease_time_clamped"`
	// MTURefused counts option-26 MTUs outside the range the plugin
	// will apply; the link keeps the MTU it had. NOT healthy-affecting.
	// Read it because the alternative was silent: a link clamped near
	// the RFC floor black-holes path MTU discovery and looks like a
	// slow network, not a misconfiguration (#702).
	MTURefused int32 `json:"mtu_refused"`
	// TombstonesConsumed counts CreateEndpoints that replayed a fresh
	// tombstone and so handed a recreated container its previous
	// MAC/IP. Not Healthy-affecting: this is the address-stability
	// mechanism working.
	//
	// It is the counterpart to RecoveredOK. Between them they say which
	// of the two paths preserved an address across a restart, which is
	// what makes "the address survived, but via neither path" a
	// detectable state rather than a silent pass (#386).
	TombstonesConsumed int32 `json:"tombstones_consumed"`
	// LeaseChanged counts renewals where dhcpcd returned a different
	// IP than the manager last recorded. Not Healthy-affecting (it
	// doesn't break Docker's view fatally — see plugin.go for the
	// truthfulness-gap discussion), but worth alerting on for
	// long-running containers.
	LeaseChanged int32 `json:"lease_changed"`
	// AddressConflicts counts leases whose address was already held by
	// another device on the segment, found by probing after the lease
	// (#524). Healthy-affecting: the endpoint is up and reporting an
	// address that does not work, and no other counter moves for it.
	//
	// ConflictProbeFailures counts probes that could not run. NOT
	// Healthy-affecting — it says the question went unasked, not that
	// the answer was bad. Watch it anyway: a detector that has stopped
	// running looks identical to a clean segment.
	AddressConflicts      int32 `json:"address_conflicts"`
	ConflictProbeFailures int32 `json:"conflict_probe_failures"`
	// ConflictProbeStaleRoutes counts leftover probe routes reclaimed
	// from a probe that was cut short before it could clean up (#572).
	// Not Healthy-affecting — the probe that reclaimed it went on to
	// run — but a rising count means the plugin is being stopped inside
	// probe windows.
	ConflictProbeStaleRoutes int32 `json:"conflict_probe_stale_routes"`
	// ConflictProbeStaleAddrs counts leftover borrowed probe SOURCE
	// addresses reclaimed from the parent NIC (#723). Its sibling
	// above covers the leftover route; this one covers the address the
	// route was sourced from, which nothing recognised because it is
	// randomly chosen. NOT healthy-affecting: the probe went on to run.
	ConflictProbeStaleAddrs int32 `json:"conflict_probe_stale_addrs"`
	// AddressConflictProbes counts probes that reached a verdict. Read
	// it before believing address_conflicts=0: a zero here means the
	// detector did not run, not that the segment is clean.
	AddressConflictProbes int32 `json:"address_conflict_probes"`

	// SandboxNetnsVisible is how many sandbox netns entries the plugin
	// can currently see, or -1 when it cannot read the directory at all
	// (#567). Sampled at request time rather than accumulated — it
	// describes the plugin's view of the host right now, not something
	// that happened.
	//
	// It exists because the evidence sandboxGone depends on was
	// unreachable for the entire life of this plugin and nothing said
	// so. The directory is not part of the image; it is bind-mounted by
	// config.json, and before #567 it was not mounted at all, so
	// os.ReadDir failed on every call and sandboxGone answered "no
	// usable evidence" forever. A dead branch is invisible precisely
	// because it never does anything.
	//
	// READ IT AGAINST ACTIVE_ENDPOINTS, NOT ON ITS OWN. The two
	// failure modes are opposite and only the comparison separates
	// them:
	//
	//   -1  the directory is unreadable — the mount is missing. Every
	//       sandboxGone answer is "no evidence", which is safe but
	//       useless: the API 404 becomes the only source of truth.
	//    0  with endpoints attached, the directory is readable but
	//       WRONG — mounted from somewhere with no sandboxes in it.
	//       This is the dangerous one. sandboxGone finds no entry
	//       matching any key and concludes every container has
	//       vanished, which is worse than never answering.
	//
	// A plain zero with no endpoints attached is neither: there is
	// genuinely nothing to see.
	SandboxNetnsVisible int32 `json:"sandbox_netns_visible"`

	// DHCP-wire counters (T2-4). Naming intentionally drops the
	// Prometheus `_total` suffix to stay consistent with the
	// existing fields above; the issue's proposal listed them with
	// `_total` for documentation clarity but the wire field is the
	// shorter form.
	//
	// Each of these is the SUM of its *_v4 and *_v6 halves below, added
	// in healthSnapshot (#730). It is not a counter in its own right,
	// and nothing increments it. The meaning operators alert on is
	// unchanged — it was a v4+v6 total before and it is a v4+v6 total
	// now — but it is now derived from the halves rather than the
	// halves being derived from it.
	LeasesObtained int32 `json:"leases_obtained"`
	LeasesRenewed  int32 `json:"leases_renewed"`
	// DHCPServerTierFallbacks counts STEPS DOWN the dhcp_servers
	// ladder: one per preferred entry that did not answer inside its
	// slice of the budget and handed on to the next (#111). One
	// acquisition against three silent preferred servers adds 2, not
	// 1 — the counter measures how far down the list acquisition had
	// to walk, which is the number worth having and is what the code
	// has always produced. Three of the four places this was described
	// said "acquisitions" instead, and #731 is that drift.
	//
	// Not healthy-affecting — the endpoint still got an address; a
	// steady rise is how a silently-dead primary shows up.
	DHCPServerTierFallbacks int32 `json:"dhcp_server_tier_fallbacks"`
	// DHCPServerPolicyExhausted counts acquisitions abandoned because no
	// server listed in dhcp_servers answered (#111). Not Healthy-
	// affecting on its own: the acquisition failure it accompanies is
	// already counted and already fails the operation.
	DHCPServerPolicyExhausted int32 `json:"dhcp_server_policy_exhausted"`
	// DHCPServerPolicyTimeouts counts dhcp_timeouts on endpoints whose
	// renewal client is restricted to dhcp_servers (#731). A strict
	// subset of DHCPTimeouts and NOT Healthy-affecting: every tick it
	// counts is already counted there, and weighting one outage twice
	// would make a policy-restricted endpoint look worse than an
	// unrestricted one failing identically.
	DHCPServerPolicyTimeouts int32 `json:"dhcp_server_policy_timeouts"`
	DHCPTimeouts             int32 `json:"dhcp_timeouts"`
	// ClientStopFailures counts renewal clients that did not shut down
	// cleanly when the plugin signalled them at teardown. Not
	// Healthy-affecting: the endpoint is going away either way.
	//
	// It does NOT mean a lease was not handed back. Nothing this plugin
	// runs sends a DHCPRELEASE — a stopped container's lease expires on
	// the server's clock, like any other host's (#800). This counter
	// was called lease_release_failures until v1.9.0, when that stopped
	// being true.
	ClientStopFailures int32 `json:"client_stop_failures"`
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
	// ParentLinkWaits / ParentLinkWaitTimeouts cover contention on a
	// shared parent NIC. A parent is a macvlan port or an ipvlan port,
	// never both, so the validate_dhcp probe holding one across a DHCP
	// round trip can collide with an endpoint asking for the other
	// (#486/#549). The plugin queues them per parent instead.
	//
	// Waits counts the operations that had to queue; timeouts counts
	// those that gave up after parentGateBudget and went to the kernel
	// anyway. Neither is Healthy-affecting: queuing is the mechanism
	// working, and a timeout only restores the behaviour that existed
	// before the queue did. Timeouts climbing is the actionable one —
	// it means a reclaim is holding a parent far longer than its DORA
	// should take, and container starts on that NIC are failing with
	// "device or resource busy".
	ParentLinkWaits        int32 `json:"parent_link_waits"`
	ParentLinkWaitTimeouts int32 `json:"parent_link_wait_timeouts"`
	// LedgerWriteFailures counts failed appends to the audit_log
	// lease ledger (#109). Not Healthy-affecting — a lost audit line
	// degrades forensics, not networking; operators using audit_log
	// alert on this directly.
	LedgerWriteFailures int32 `json:"ledger_write_failures"`

	// DirectivesRefused / MountPrepFailures are the two places pkg/dhcp
	// declines to do what it was asked and carries on anyway (#780).
	// They are pulled from that package at snapshot time rather than
	// pushed into a sink, because one of them fires during config
	// rendering, which no caller watches.
	//
	// DirectivesRefused counts dhcpcd directives dropped for carrying a
	// control character in their value. dhcpcd.conf has no quoting, so a
	// value with a newline in it would become a second directive; the
	// drop is correct. What was missing is that an operator who set
	// hostname, vendor class or client ID then had it silently not
	// applied, and read a healthy plugin.
	//
	// MountPrepFailures counts individual commands in the per-client
	// mount-namespace preparation that failed. The chain is `;`-joined
	// deliberately, so dhcpcd starts regardless — but two containers
	// whose interface is the default eth0 then collide on dhcpcd's
	// control socket, and the second client silently never renews or
	// releases. It counts COMMANDS, so one client failing three of four
	// steps adds 3.
	//
	// Neither latches the healthy flag. Both describe an input that did
	// not take effect, not a container left without a renewal client,
	// and either can be non-zero on a plugin that is otherwise doing its
	// job. Alert on them moving, not on their absolute value.
	//
	// Both are process-global in pkg/dhcp and therefore do NOT reset
	// with a plugin restart of anything smaller than the process — which
	// is the same lifetime as every other counter here, since the
	// instance_id label changes with the process.
	DirectivesRefused int32 `json:"directives_refused"`
	MountPrepFailures int32 `json:"mount_prep_failures"`

	// Per-family breakdown of the wire counters (#212, #730). Both
	// halves are STORED; the un-suffixed field above is their sum,
	// computed in healthSnapshot from the same two values rendered
	// here. It is not a third counter, and neither half is a subset of
	// it. On a dual-stack host this isolates the v6-specific failure
	// signal (NAK/timeout) the aggregate hides.
	//
	// Until #730 the v4 share was not stored at all: the un-suffixed
	// field was the counter and the v4 number was recovered by
	// subtracting *_v6 from it at render time. Two independently
	// updated atomics combined by subtraction can produce a value lower
	// than the previous read, and a counter that decreases is a reset
	// to Prometheus. Storing both and adding for the total is
	// monotonic under every interleaving; subtracting is not.
	LeaseChangedV4   int32 `json:"lease_changed_v4"`
	LeasesObtainedV4 int32 `json:"leases_obtained_v4"`
	LeasesRenewedV4  int32 `json:"leases_renewed_v4"`
	DHCPTimeoutsV4   int32 `json:"dhcp_timeouts_v4"`
	NAKsReceivedV4   int32 `json:"naks_received_v4"`
	// ClientStopFailuresV4 is the v4 half of ClientStopFailures.
	ClientStopFailuresV4 int32 `json:"client_stop_failures_v4"`

	LeaseChangedV6   int32 `json:"lease_changed_v6"`
	LeasesObtainedV6 int32 `json:"leases_obtained_v6"`
	LeasesRenewedV6  int32 `json:"leases_renewed_v6"`
	DHCPTimeoutsV6   int32 `json:"dhcp_timeouts_v6"`
	NAKsReceivedV6   int32 `json:"naks_received_v6"`
	// ClientStopFailuresV6 is the v6 share of ClientStopFailures
	// (#608): the persistent DHCPv6 client held a binding and its
	// SIGTERM-driven RELEASE did not complete cleanly.
	ClientStopFailuresV6 int32 `json:"client_stop_failures_v6"`
}

func (p *Plugin) apiHealth(w http.ResponseWriter, r *http.Request) {
	util.JSONResponse(w, p.healthSnapshot(), http.StatusOK)
}

// healthSnapshot builds one consistent view of the plugin's counters.
//
// It exists so that /Plugin.Health and /metrics cannot disagree (#651).
// Both render from this and only this, which makes "two views, one
// source" a property of the code rather than something a reviewer has
// to keep noticing. The alternative — a metrics handler that reads the
// atomics itself — would be a second hand-maintained list of 45 fields,
// and this repo has watched that shape rot more than once (#542, #636).
//
// The counters are read without a lock and are therefore not a single
// atomic instant: two of them can be a few nanoseconds apart. For an
// individual monotonic counter read for rates and alerting that is
// harmless, and it is the behaviour /Plugin.Health has always had. Only
// the two map lengths need p.mu, because reading a map during a
// concurrent write is a data race rather than a stale number.
//
// It is NOT harmless for a value COMBINED from two of them, and #730 is
// what that costs. Each family pair is therefore loaded exactly once
// here, into a local, and the aggregate is the sum of those two locals
// — so the pair a caller sees is internally consistent even though the
// two loads are nanoseconds apart. Adding is what makes the skew
// tolerable: the sum of two monotonic counters is monotonic under every
// interleaving. The previous shape subtracted, and subtraction is not.
// Do not reintroduce a second .Load() of one of these halves; that is
// the defect, not the arithmetic.
func (p *Plugin) healthSnapshot() HealthResponse {
	p.mu.Lock()
	active := len(p.persistentDHCP)
	pending := len(p.joinHints)
	p.mu.Unlock()

	failed := p.recoveryFailed.Load()
	joinFails := p.joinStartFailures.Load()
	tsFails := p.tombstoneWriteFailures.Load()
	conflicts := p.addressConflicts.Load()
	tsQuarantines := p.tombstones.quarantines.Load()

	// Pulled from pkg/dhcp rather than held here: see DirectivesRefused.
	directivesRefused, mountPrepFailures := dhcp.RefusalCounts()

	// One load per half, used for both the half and the sum.
	leaseChangedV4 := p.leaseChangedV4.Load()
	leaseChangedV6 := p.leaseChangedV6.Load()
	leasesObtainedV4 := p.leasesObtainedV4.Load()
	leasesObtainedV6 := p.leasesObtainedV6.Load()
	leasesRenewedV4 := p.leasesRenewedV4.Load()
	leasesRenewedV6 := p.leasesRenewedV6.Load()
	dhcpTimeoutsV4 := p.dhcpTimeoutsV4.Load()
	dhcpTimeoutsV6 := p.dhcpTimeoutsV6.Load()
	naksReceivedV4 := p.naksReceivedV4.Load()
	naksReceivedV6 := p.naksReceivedV6.Load()
	clientStopFailuresV4 := p.clientStopFailuresV4.Load()
	clientStopFailuresV6 := p.clientStopFailuresV6.Load()

	return HealthResponse{
		// Healthy is false on any condition that means an operator
		// should look: a recovery or join-start failure means a running
		// container has no renewal goroutine; a tombstone-write failure
		// means the next restart of some container will pick a new
		// MAC/IP; an address conflict means a container is up and
		// reporting an address that belongs to someone else (#524); a
		// tombstone quarantine means the whole tombstone file was
		// unreadable, so EVERY container restarting in the next TTL
		// window picks a new MAC and address (#724).
		//
		// See HealthResponse's own comment for what this flag does and
		// does not say — in particular that it latches for the life of
		// the process.
		Healthy:           failed == 0 && joinFails == 0 && tsFails == 0 && conflicts == 0 && tsQuarantines == 0,
		InstanceID:        p.instanceID,
		UptimeSeconds:     time.Since(p.startTime).Seconds(),
		ActiveEndpoints:   active,
		PendingHints:      pending,
		RecoveredOK:       p.recoveredOK.Load(),
		RecoveryFailed:    failed,
		JoinStartFailures: joinFails,
		// The three below are deliberately absent from the Healthy
		// expression above, like JoinAbortedContainerGone. A daemon that
		// was still starting (#383), a container that had already
		// exited when recovery reached it (#376), and a network removed
		// out from under the recovery walk (#648) all leave nothing
		// behind to be unhealthy about.
		RecoveryDeferred:             p.recoveryDeferred.Load(),
		RecoveryAbortedContainerGone: p.recoveryAbortedContainerGone.Load(),
		RecoveryNetworkGone:          p.recoveryNetworkGone.Load(),
		RecoveryFingerprintsSkipped:  p.recoveryFingerprintsSkipped.Load(),
		RecoveryAlreadyManaged:       p.recoveryAlreadyManaged.Load(),
		JoinAbortedContainerGone:     p.joinAbortedContainerGone.Load(),
		JoinAbortedNoContainer:       p.joinAbortedNoContainer.Load(),
		JoinAttachSlow:               p.joinAttachSlow.Load(),
		RestartLinkUpWaited:          p.restartLinkUpWaited.Load(),
		RestartLinkUpTimeouts:        p.restartLinkUpTimeouts.Load(),
		JoinAbortedEndpointLeft:      p.joinAbortedEndpointLeft.Load(),
		TombstoneWriteFailures:       tsFails,
		TombstoneQuarantines:         tsQuarantines,
		UnsafeHostnamesRejected:      p.unsafeHostnamesRejected.Load(),
		UnsafeOptionValuesDropped:    p.unsafeOptionValuesDropped.Load(),
		NetworkOptionsRejected:       p.networkOptionsRejected.Load(),
		DNSPropagationPIDMismatches:  p.dnsPropagationPIDMismatches.Load(),
		NetnsPIDMismatches:           p.netnsPIDMismatches.Load(),
		DHCPRoutesApplied:            p.dhcpRoutesApplied.Load(),
		DHCPDefaultRouteSuperseded:   p.dhcpDefaultRouteSuperseded.Load(),
		LeaseTimeClamped:             p.leaseTimeClamped.Load(),
		MTURefused:                   p.mtuRefused.Load(),
		TombstonesConsumed:           p.tombstonesConsumed.Load(),
		LeaseChanged:                 leaseChangedV4 + leaseChangedV6,
		AddressConflicts:             conflicts,
		ConflictProbeFailures:        p.conflictProbeFailures.Load(),
		ConflictProbeStaleRoutes:     p.conflictProbeStaleRoutes.Load(),
		ConflictProbeStaleAddrs:      p.conflictProbeStaleAddrs.Load(),
		AddressConflictProbes:        p.addressConflictProbes.Load(),
		SandboxNetnsVisible:          sandboxNetnsVisibleIn(sandboxNetnsDirs),
		LeasesObtained:               leasesObtainedV4 + leasesObtainedV6,
		LeasesRenewed:                leasesRenewedV4 + leasesRenewedV6,
		DHCPServerTierFallbacks:      p.dhcpServerTierFallbacks.Load(),
		DHCPServerPolicyExhausted:    p.dhcpServerPolicyExhausted.Load(),
		DHCPServerPolicyTimeouts:     p.dhcpServerPolicyTimeouts.Load(),
		DHCPTimeouts:                 dhcpTimeoutsV4 + dhcpTimeoutsV6,
		ClientStopFailures:           clientStopFailuresV4 + clientStopFailuresV6,
		NAKsReceived:                 naksReceivedV4 + naksReceivedV6,
		DisplacedStops:               p.displacedStopsTotal.Load(),
		ParentLinkWaits:              p.parentLinkWaits.Load(),
		ParentLinkWaitTimeouts:       p.parentLinkWaitTimeouts.Load(),
		LedgerWriteFailures:          p.ledgerWriteFailures.Load(),
		DirectivesRefused:            directivesRefused,
		MountPrepFailures:            mountPrepFailures,
		LeaseChangedV4:               leaseChangedV4,
		LeasesObtainedV4:             leasesObtainedV4,
		LeasesRenewedV4:              leasesRenewedV4,
		DHCPTimeoutsV4:               dhcpTimeoutsV4,
		NAKsReceivedV4:               naksReceivedV4,
		ClientStopFailuresV4:         clientStopFailuresV4,
		LeaseChangedV6:               leaseChangedV6,
		LeasesObtainedV6:             leasesObtainedV6,
		LeasesRenewedV6:              leasesRenewedV6,
		DHCPTimeoutsV6:               dhcpTimeoutsV6,
		NAKsReceivedV6:               naksReceivedV6,
		ClientStopFailuresV6:         clientStopFailuresV6,
	}
}
