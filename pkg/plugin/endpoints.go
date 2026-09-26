// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"net/http"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/buildinfo"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// Payloads are based on https://github.com/docker/go-plugins-helpers/blob/master/network/api.go

// CapabilitiesResponse returns whether or not this network is global or local
type CapabilitiesResponse struct {
	Scope             string
	ConnectivityScope string
	// GwAllocChecker makes libnetwork call /NetworkDriver.GwAllocCheck, whose 404 is then a real error (#110).
	GwAllocChecker bool
}

func (p *Plugin) apiGetCapabilities(w http.ResponseWriter, r *http.Request) {
	util.JSONResponse(w, CapabilitiesResponse{
		Scope:             "local",
		ConnectivityScope: "global",
		GwAllocChecker:    true,
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

	// replay marks reacquireEndpoint's rebuild of an endpoint Docker already created; JSON cannot set it (#1036).
	replay bool
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

// ifnameOption carries Compose's `interface_name` (engine 28+, API 1.48+) or a `--driver-opt` key (#125).
const ifnameOption = "com.docker.network.endpoint.ifname"

// InterfaceName is the Join interface name, whose DstName remote drivers get from engine 29.8.0 (moby/moby#52866);
// 28.5.2 and 29.7.2 were measured naming the interface by DstPrefix and index (#125, #670).
type InterfaceName struct {
	SrcName   string
	DstPrefix string
	DstName   string
}

// libnetwork's StaticRoute.RouteType values (moby/libnetwork docs/remote.md): 0 has a NextHop, 1 is on-link.
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

// HealthResponse is the /Plugin.Health payload, whose Healthy latches false for the process's life once a counter
// marked Healthy-affecting below moves, and only a plugin restart clears it (#638, #724).
type HealthResponse struct {
	// Status and Checks follow draft-inadarei-api-health-check-06 sections 3.1 and 3.6; `fail` is `healthy: false`.
	Status string `json:"status"`
	// Version, Commit and Library are the build identity (pkg/buildinfo), also the net_dhcp_build_info labels.
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Library string `json:"library"`
	// EngineVersion and APIVersion are what the daemon answered at startup, or `unknown` when its socket was not yet
	// serving (#383, #670).
	EngineVersion string `json:"engine_version"`
	APIVersion    string `json:"api_version"`
	Healthy       bool   `json:"healthy"`
	// InstanceID names the serving process, so two reads compare as a delta only when it matches (#405).
	InstanceID      string  `json:"instance_id"`
	UptimeSeconds   float64 `json:"uptime_seconds"`
	ActiveEndpoints int     `json:"active_endpoints"`
	// LinkLocalEndpoints counts the endpoints on the RFC 3927 fallback now, from the array's snapshot (#904).
	LinkLocalEndpoints int   `json:"link_local_endpoints"`
	PendingHints       int   `json:"pending_hints"`
	RecoveredOK        int32 `json:"recovered_ok"`
	// RecoveryFailed counts failed recoveries of a still-running container, which then has no renewal client; Healthy-affecting.
	RecoveryFailed int32 `json:"recovery_failed"`
	// RecoveryDeferred counts recoveries retried once the daemon socket came up (#383); not Healthy-affecting.
	RecoveryDeferred int32 `json:"recovery_deferred"`
	// RecoveryAbortedContainerGone counts recoveries abandoned because the container had exited (#376); not
	// Healthy-affecting.
	RecoveryAbortedContainerGone int32 `json:"recovery_aborted_container_gone"`
	// RecoveryNetworkGone counts networks removed between NetworkList and NetworkInspect (#648); not Healthy-affecting.
	RecoveryNetworkGone int32 `json:"recovery_network_gone"`
	// RecoveryFingerprintsSkipped counts adopted endpoints with no hostname to fingerprint, so no tombstone (#721);
	// not Healthy-affecting.
	RecoveryFingerprintsSkipped int32 `json:"recovery_fingerprints_skipped"`
	// RecoveryAlreadyManaged counts endpoints a Join registered before the recovery walk (#480); not Healthy-affecting.
	RecoveryAlreadyManaged int32 `json:"recovery_already_managed"`
	// JoinStartFailures counts renewal client Start failures at Join (#317); Healthy-affecting.
	JoinStartFailures int32 `json:"join_start_failures"`
	// JoinAbortedContainerGone counts attaches abandoned because the container exited first (#373); not
	// Healthy-affecting.
	JoinAbortedContainerGone int32 `json:"join_aborted_container_gone"`
	// JoinAbortedNoContainer counts attaches no container claimed, whose address is left to expire (#566, #800,
	// #962); not Healthy-affecting.
	JoinAbortedNoContainer int32 `json:"join_aborted_no_container"`

	// JoinAttachSlow counts attaches that succeeded after outlasting AwaitTimeout (#406); not healthy-affecting.
	JoinAttachSlow int32 `json:"join_attach_slow"`

	// JoinAttachCompleted counts successful attaches, the population the three duration buckets partition.
	JoinAttachCompleted int32 `json:"join_attach_completed"`
	// JoinAttachUnder1s and JoinAttach1sToBudget with JoinAttachSlow sum to JoinAttachCompleted (#403).
	JoinAttachUnder1s    int32 `json:"join_attach_under_1s"`
	JoinAttach1sToBudget int32 `json:"join_attach_1s_to_budget"`
	// JoinAttachMsMax is the longest successful attach in milliseconds, saturating at MaxInt32 (#403).
	JoinAttachMsMax int32 `json:"join_attach_ms_max"`

	// RestartLinkUpWaited counts child links brought up after the departing link released the address (#408); not
	// healthy-affecting.
	RestartLinkUpWaited int32 `json:"restart_link_up_waited"`
	// RestartLinkUpTimeouts counts that wait outlasting its budget, failing with `address already in use` (#422); not
	// healthy-affecting.
	RestartLinkUpTimeouts int32 `json:"restart_link_up_timeouts"`

	// JoinAbortedEndpointLeft counts attaches cancelled because the endpoint left first; not healthy-affecting.
	JoinAbortedEndpointLeft int32 `json:"join_aborted_endpoint_left"`
	// TombstoneWriteFailures counts failed tombstone writes and reads refused since #724; Healthy-affecting.
	TombstoneWriteFailures int32 `json:"tombstone_write_failures"`
	// TombstoneQuarantines counts unparseable tombstone files moved aside as tombstones.json.corrupt-<ts>, since
	// something wrote garbage into stateDir (#724); Healthy-affecting.
	TombstoneQuarantines int32 `json:"tombstone_quarantines"`
	// UnsafeHostnamesRejected counts container hostnames dropped for a control character (#692); not healthy-affecting.
	UnsafeHostnamesRejected int32 `json:"unsafe_hostnames_rejected"`

	// HostnamesAppliedLate, HostnameLookupFailures and HostnameApplyFailures are the outcomes of a late v4 hostname
	// (#961).
	HostnamesAppliedLate   int32 `json:"hostnames_applied_late"`
	HostnameLookupFailures int32 `json:"hostname_lookup_failures"`
	HostnameApplyFailures  int32 `json:"hostname_apply_failures"`

	// HostIfnamesApplied, HostIfnameConflicts and HostIfnameFailures are the bridge-mode host link naming outcomes
	// (#978).
	HostIfnamesApplied  int32 `json:"host_ifnames_applied"`
	HostIfnameConflicts int32 `json:"host_ifname_conflicts"`
	HostIfnameFailures  int32 `json:"host_ifname_failures"`
	// UnsafeOptionValuesDropped counts server-chosen DHCP strings refused or truncated (#703, #704); not
	// healthy-affecting.
	UnsafeOptionValuesDropped int32 `json:"unsafe_option_values_dropped"`
	// NetworkOptionsRejected counts endpoint operations refusing a network's stored options (#705, #727); not
	// healthy-affecting.
	NetworkOptionsRejected int32 `json:"network_options_rejected"`
	// IPAMReplayHits counts stored endpoint addresses confirmed from a lease record at a daemon restart (#110).
	IPAMReplayHits int32 `json:"ipam_replay_hits"`
	// IPAMReplayMiss counts stored endpoint addresses refused because no lease record holds them (#110).
	IPAMReplayMiss int32 `json:"ipam_replay_miss"`
	// IPAMRebindAmbiguous counts address requests that met more than one removed endpoint, so the server chose (#110).
	IPAMRebindAmbiguous int32 `json:"ipam_rebind_ambiguous"`
	// IPAMReserveDuplicateMAC counts address requests refused for a hardware address already leasing (#110).
	IPAMReserveDuplicateMAC int32 `json:"ipam_reserve_duplicate_mac"`
	// IPAMStrandedRecords counts CREATED records a previous process left without an endpoint, given up at start-up
	// (#1047).
	IPAMStrandedRecords int32 `json:"ipam_stranded_records"`
	// IPAMReleaseUnknown counts released addresses no lease record holds, the normal order after a retain (#110).
	IPAMReleaseUnknown int32 `json:"ipam_release_unknown"`
	// DNSPropagationPIDMismatches counts DNS writes refused because the container PID was recycled (#688).
	DNSPropagationPIDMismatches int32 `json:"dns_propagation_pid_mismatches"`
	// NetnsPIDMismatches counts sandbox netns opens refused because the container PID was recycled (#695).
	NetnsPIDMismatches int32 `json:"netns_pid_mismatches"`
	// SandboxKeyEntries, SandboxKeyEntryFailures and SandboxPIDFallbacks count the sandbox key route, its refusals and
	// the /proc/<pid>/ns/net fallback, the evidence for whether the PID route is still needed (#725).
	SandboxKeyEntries       int32 `json:"sandbox_key_entries"`
	SandboxKeyEntryFailures int32 `json:"sandbox_key_entry_failures"`
	SandboxPIDFallbacks     int32 `json:"sandbox_pid_fallbacks"`

	// SandboxKeyNotANamespace (the expected arm under sandbox_netns_propagation=0), SandboxKeyNotPermitted (a
	// non-default `dockerd --exec-root`), SandboxKeyWrongNSType, SandboxKeyUnavailable and SandboxKeyAbsent sum to
	// SandboxKeyEntryFailures (#725).
	SandboxKeyAbsent        int32 `json:"sandbox_key_absent"`
	SandboxKeyNotPermitted  int32 `json:"sandbox_key_not_permitted"`
	SandboxKeyNotANamespace int32 `json:"sandbox_key_not_a_namespace"`
	SandboxKeyWrongNSType   int32 `json:"sandbox_key_wrong_ns_type"`
	SandboxKeyUnavailable   int32 `json:"sandbox_key_unavailable"`

	// DockerAPINonGETRefusals counts Docker API requests refused for a method other than GET (#691).
	DockerAPINonGETRefusals int32 `json:"docker_api_non_get_refusals"`
	// DHCPRoutesApplied counts option-121 routes handed to Docker, and DHCPDefaultRouteSuperseded the Joins whose
	// routes cover 0.0.0.0/0 by union, so egress bypasses the option-3 router (#700).
	DHCPRoutesApplied          int32 `json:"dhcp_routes_applied"`
	DHCPDefaultRouteSuperseded int32 `json:"dhcp_default_route_superseded"`
	// MTURefused counts option-26 MTUs outside the applied range, which leave the link MTU unchanged (#702).
	MTURefused int32 `json:"mtu_refused"`
	// TombstonesConsumed counts CreateEndpoints that gave a recreated container its previous MAC and IP (#386).
	TombstonesConsumed int32 `json:"tombstones_consumed"`
	// LeaseChanged counts renewals where the server returned a different IP than last recorded (#100).
	LeaseChanged int32 `json:"lease_changed"`
	// AddressConflicts is the sum of AddressConflictsV4 and AddressConflictsV6 over the lease's life, RFC 5227
	// sections 2.1 and 2.4 (#524); Healthy-affecting.
	AddressConflicts int32 `json:"address_conflicts"`
	// ACDProbesSent, ACDAnnouncementsSent, ACDConflictsDetected and ACDARPSendFailures are the library's RFC 5227
	// counters, which show whether a zero AddressConflicts had any probe behind it (#524, #882).
	ACDProbesSent        int32 `json:"acd_probes_sent"`
	ACDAnnouncementsSent int32 `json:"acd_announcements_sent"`
	ACDConflictsDetected int32 `json:"acd_conflicts_detected"`
	ACDARPSendFailures   int32 `json:"acd_arp_send_failures"`
	// ACDResumedUnchecked counts endpoints resumed before their RFC 5227 section 2.1 check finished, re-run on the
	// INIT-REBOOT ACK (#882); a `warn` check.
	ACDResumedUnchecked int32 `json:"acd_resumed_unchecked"`

	// SandboxNetnsVisible is the sandbox netns entries visible at request time, -1 when the directory is unreadable
	// (a missing mount) and 0 with endpoints attached when it is mounted from the wrong place (#567).
	SandboxNetnsVisible int32 `json:"sandbox_netns_visible"`

	// SandboxNetnsPropagation is 1 when a later daemon mount under the sandbox netns directory reaches this process, 0
	// when the mount is private so attaches take the PID route, and -1 when mountinfo is unreadable (#417).
	SandboxNetnsPropagation int32 `json:"sandbox_netns_propagation"`

	// SandboxNetnsInitMounts is the sandbox netns mounts in PID 1's table: -2 shared namespace, -1 unreadable, else
	// the count, and under a nested engine PID 1 is that engine's init (#417).
	SandboxNetnsInitMounts int32 `json:"sandbox_netns_init_mounts"`

	// LeasesObtained and the wire counters below are the sums of their _v4 and _v6 halves, without `_total` (#730).
	LeasesObtained int32 `json:"leases_obtained"`
	LeasesRenewed  int32 `json:"leases_renewed"`
	// RenewalsUnanswered counts renewal requests that got no answer, first moving at the RFC 2131 section 4.4.5
	// retransmission (half the time to T2, floor 60 s), measured 4h30m after T1 on a 24 h lease (#940).
	RenewalsUnanswered int32 `json:"renewals_unanswered"`
	// DHCPServerTierFallbacks counts steps down the dhcp_servers ladder, one per silent preferred entry (#111, #731).
	DHCPServerTierFallbacks int32 `json:"dhcp_server_tier_fallbacks"`
	// DHCPServerPolicyExhausted counts acquisitions no dhcp_servers entry answered (#111); not Healthy-affecting.
	DHCPServerPolicyExhausted int32 `json:"dhcp_server_policy_exhausted"`
	// DHCPServerPolicyTimeouts counts the subset of dhcp_timeouts on dhcp_servers-restricted endpoints (#731).
	DHCPServerPolicyTimeouts int32 `json:"dhcp_server_policy_timeouts"`
	DHCPTimeouts             int32 `json:"dhcp_timeouts"`
	// ClientStopFailures counts renewal clients that did not stop cleanly, not a missing release (#800, #962).
	ClientStopFailures int32 `json:"client_stop_failures"`
	// ReleasesSent and ReleaseFailures are the sums of their per-family halves, ReleaseFailures a `warn` check (#962).
	ReleasesSent    int32 `json:"releases_sent"`
	ReleaseFailures int32 `json:"release_failures"`
	// ReleasesReclaimed counts on_remove addresses a running container reused inside the restart window, so the
	// record closed without a datagram (#984).
	ReleasesReclaimed int32 `json:"releases_reclaimed"`
	// NAKsReceived counts server NAKs on renewal or rebind, each widening the lease_changed divergence (#128).
	NAKsReceived int32 `json:"naks_received"`
	// DisplacedStops counts recovery-registered managers displaced by a Join for the same endpoint.
	DisplacedStops int32 `json:"displaced_stops"`
	// ParentLinkWaits and ParentLinkWaitTimeouts count operations queued on a shared parent NIC and those that gave up
	// after parentGateBudget, since a parent is a macvlan or an ipvlan port, never both (#486, #549).
	ParentLinkWaits        int32 `json:"parent_link_waits"`
	ParentLinkWaitTimeouts int32 `json:"parent_link_wait_timeouts"`
	// LedgerWriteFailures counts failed appends to the audit_log lease ledger (#109); not Healthy-affecting.
	LedgerWriteFailures int32 `json:"ledger_write_failures"`
	// IfnameUnsupported counts custom interface names on an engine that does not apply them (#125, #670).
	IfnameUnsupported int32 `json:"ifname_unsupported"`
	// StateFileChmodFailures counts state files the startup sweep could not tighten, plus an unreadable STATE_DIR
	// (#804).
	StateFileChmodFailures int32 `json:"state_file_chmod_failures"`

	// LeaseChangedV4 and the per-family counters below are stored, and the un-suffixed field is their sum, since a
	// total recovered by subtraction can decrease and reads as a reset to Prometheus (#212, #730).
	LeaseChangedV4   int32 `json:"lease_changed_v4"`
	LeasesObtainedV4 int32 `json:"leases_obtained_v4"`
	LeasesRenewedV4  int32 `json:"leases_renewed_v4"`
	// RenewalsUnansweredV4 is the IPv4 half of RenewalsUnanswered.
	RenewalsUnansweredV4 int32 `json:"renewals_unanswered_v4"`
	DHCPTimeoutsV4       int32 `json:"dhcp_timeouts_v4"`
	NAKsReceivedV4       int32 `json:"naks_received_v4"`
	// ClientStopFailuresV4 is the v4 half of ClientStopFailures.
	ClientStopFailuresV4 int32 `json:"client_stop_failures_v4"`
	// ReleasesSentV4 and ReleaseFailuresV4 are the IPv4 `release_lease` pair (#962).
	ReleasesSentV4    int32 `json:"releases_sent_v4"`
	ReleaseFailuresV4 int32 `json:"release_failures_v4"`
	// ReleasesReclaimedV4 is the IPv4 half of ReleasesReclaimed (#984).
	ReleasesReclaimedV4 int32 `json:"releases_reclaimed_v4"`
	// AddressConflictsV4 is the RFC 5227 half, the only one comparable with the ARP-based ACD counters.
	AddressConflictsV4 int32 `json:"address_conflicts_v4"`

	// LeaseChangedV6 and the v6 fields below are written by the DHCPv6 client, so a zero means it did not happen
	// (#911).
	LeaseChangedV6   int32 `json:"lease_changed_v6"`
	LeasesObtainedV6 int32 `json:"leases_obtained_v6"`
	LeasesRenewedV6  int32 `json:"leases_renewed_v6"`
	// RenewalsUnansweredV6 counts unanswered Renew and Rebind messages (RFC 9915 sections 18.2.4 and 18.2.5).
	RenewalsUnansweredV6 int32 `json:"renewals_unanswered_v6"`
	DHCPTimeoutsV6       int32 `json:"dhcp_timeouts_v6"`
	NAKsReceivedV6       int32 `json:"naks_received_v6"`
	// AddressConflictsV6 counts addresses the kernel's DAD (RFC 4862 section 5.4) found in use, declined under RFC 9915
	// section 18.2.8; Docker's record is not updated for the replacement, as for v4 (#104).
	AddressConflictsV6 int32 `json:"address_conflicts_v6"`
	// ClientStopFailuresV6 is the v6 share of ClientStopFailures, with any release counted apart (#608, #962).
	ClientStopFailuresV6 int32 `json:"client_stop_failures_v6"`
	// ReleasesSentV6 and ReleaseFailuresV6 count DHCPv6 Release messages (RFC 9915 section 18.2.7), read per family.
	ReleasesSentV6    int32 `json:"releases_sent_v6"`
	ReleaseFailuresV6 int32 `json:"release_failures_v6"`
	// ReleasesReclaimedV6 is the DHCPv6 half of ReleasesReclaimed, a second record with its own deadline (#984).
	ReleasesReclaimedV6 int32 `json:"releases_reclaimed_v6"`
	// DHCPv6ConfigOnly counts DHCPv6 information replies on a network with the RA "other config" flag (#815).
	DHCPv6ConfigOnly int32 `json:"dhcpv6_config_only"`
	// DHCPv6NotOffered counts endpoints with no DHCPv6 address because the RA advertised no managed DHCPv6, and with
	// autoconf=0 no SLAAC address either (#821, #868).
	DHCPv6NotOffered int32 `json:"dhcpv6_not_offered"`
	// DHCPv6NoRouterAdvert counts endpoints with no DHCPv6 address because no router advertisement arrived (#868).
	DHCPv6NoRouterAdvert int32 `json:"dhcpv6_no_router_advert"`
	// DHCPv6Refused counts endpoints failed by a server Status Code other than Success (RFC 9915 21.13, #816).
	DHCPv6Refused int32 `json:"dhcpv6_refused"`
	// DHCPv6NoServer counts endpoints failed because a managed segment's DHCPv6 server never answered (#816).
	DHCPv6NoServer int32 `json:"dhcpv6_no_server"`
	// DHCPv6SLAACNoPrefix counts SLAAC endpoints failed because no advertised prefix could form an address (RFC 4862
	// 5.5.3, #816, #817).
	DHCPv6SLAACNoPrefix int32 `json:"dhcpv6_slaac_no_prefix"`

	// DHCPv6SLAACNoAddress counts SLAAC endpoints where no address formed inside the acquisition budget (#818).
	DHCPv6SLAACNoAddress int32 `json:"dhcpv6_slaac_no_address"`

	// IPv6SLAACAddresses and IPv6AddressesWithdrawn count addresses installed and removed, not endpoints (#818, #819).
	IPv6SLAACAddresses     int32 `json:"ipv6_slaac_addresses"`
	IPv6AddressesWithdrawn int32 `json:"ipv6_addresses_withdrawn"`

	// IPv6SLAACPrefixesIgnored counts prefixes skipped under RFC 4862 section 5.5.3 or the eight-address cap.
	IPv6SLAACPrefixesIgnored int32 `json:"ipv6_slaac_prefixes_ignored"`

	// IPv6MainPrefixUnmatched counts endpoints where no address fell inside ipv6_main_prefix (#819).
	IPv6MainPrefixUnmatched int32 `json:"ipv6_main_prefix_unmatched"`
	// DHCPv6AutoFallbacks counts `ipv6_mode=auto` endpoints that formed a SLAAC address after DHCPv6 went silent
	// (#817).
	DHCPv6AutoFallbacks int32 `json:"dhcpv6_auto_fallbacks"`
	// IPv6LinkEnableFailures counts container links IPv6 could not be enabled on before the DHCPv6 client started.
	IPv6LinkEnableFailures int32 `json:"ipv6_link_enable_failures"`
	// RouterAdvertGuardFailures counts RA guard sysctl steps that failed or read back wrong on a container link (#875).
	RouterAdvertGuardFailures int32 `json:"router_advert_guard_failures"`
	// IPv6RouterWithdrawn counts container v6 default routes removed for a Router Lifetime of 0 (#821).
	IPv6RouterWithdrawn int32 `json:"ipv6_router_withdrawn"`

	// RouterSolicitsSent and the router counters below are the library's RFC 4861 counters folded across every
	// DHCPv6 manager, solicitations under section 6.3.7 (#814).
	RouterSolicitsSent int32 `json:"router_solicits_sent"`
	// RouterAdvertsSeen counts decoded advertisements and RouterAdvertsRefused undecodable ones (#814).
	RouterAdvertsSeen    int32 `json:"router_adverts_seen"`
	RouterAdvertsRefused int32 `json:"router_adverts_refused"`
	// RouterAdvertOptionsIgnored counts refused options, not frames, in otherwise read advertisements (#814).
	RouterAdvertOptionsIgnored int32 `json:"router_advert_options_ignored"`
	// RouterTableEntriesDropped and RouterTableEntriesEvicted count the router table caps in force (RFC 8106 6.2 (d)).
	RouterTableEntriesDropped int32 `json:"router_table_entries_dropped"`
	RouterTableEntriesEvicted int32 `json:"router_table_entries_evicted"`

	// Checks is one single-element array per named check, as the health-check draft's section 4 asks.
	Checks map[string][]HealthCheck `json:"checks"`
	// Endpoints is one entry per registered manager, kept out of /metrics as a cardinality decision.
	Endpoints []EndpointHealth `json:"endpoints"`
}

func (p *Plugin) apiHealth(w http.ResponseWriter, r *http.Request) {
	util.JSONResponse(w, p.healthSnapshot(), http.StatusOK)
}

func (p *Plugin) checkStamps() map[string]time.Time {
	return map[string]time.Time{
		"recovery_failed":            p.recoveryFailed.LastMoved(),
		"join_start_failures":        p.joinStartFailures.LastMoved(),
		"tombstone_write_failures":   p.tombstoneWriteFailures.LastMoved(),
		"tombstone_quarantines":      p.tombstones.quarantines.LastMoved(),
		"address_conflicts":          laterOf(p.addressConflictsV4.LastMoved(), p.addressConflictsV6.LastMoved()),
		"lease_changed":              laterOf(p.leaseChangedV4.LastMoved(), p.leaseChangedV6.LastMoved()),
		"acd_arp_send_failures":      p.acdARPSendFailures.LastMoved(),
		"acd_resumed_unchecked":      p.acdResumedUnchecked.LastMoved(),
		"restart_link_up_timeouts":   p.restartLinkUpTimeouts.LastMoved(),
		"parent_link_wait_timeouts":  p.parentLinkWaitTimeouts.LastMoved(),
		"ledger_write_failures":      p.ledgerWriteFailures.LastMoved(),
		"state_file_chmod_failures":  p.stateFileChmodFailures.LastMoved(),
		"ifname_unsupported":         p.ifnameUnsupported.LastMoved(),
		"ipam_replay_miss":           p.ipamReplayMiss.LastMoved(),
		"ipam_rebind_ambiguous":      p.ipamRebindAmbiguous.LastMoved(),
		"ipam_reserve_duplicate_mac": p.ipamReserveDuplicateMAC.LastMoved(),
		"ipam_stranded_records":      p.ipamStrandedRecords.LastMoved(),
		"hostname_lookup_failures":   p.hostnameLookupFailures.LastMoved(),
		"hostname_apply_failures":    p.hostnameApplyFailures.LastMoved(),
		"host_ifname_conflicts":      p.hostIfnameConflicts.LastMoved(),
		"host_ifname_failures":       p.hostIfnameFailures.LastMoved(),
		"release_failures":           laterOf(p.releaseFailuresV4.LastMoved(), p.releaseFailuresV6.LastMoved()),
	}
}

// healthSnapshot is the one source for /Plugin.Health and /metrics (#651). Counters load without a lock, and each
// family pair loads once and is summed, since a sum of monotonic counters stays monotonic (#730).
func (p *Plugin) healthSnapshot() HealthResponse {
	managers, pending := p.managerSnapshot()
	endpoints := endpointViewsOf(managers)

	engine := p.engineSnapshot()

	netns := p.netnsReadingSources()

	failed := p.recoveryFailed.Load()
	joinFails := p.joinStartFailures.Load()
	tsFails := p.tombstoneWriteFailures.Load()
	conflictsV4 := p.addressConflictsV4.Load()
	conflictsV6 := p.addressConflictsV6.Load()
	conflicts := conflictsV4 + conflictsV6
	tsQuarantines := p.tombstones.quarantines.Load()

	leaseChangedV4 := p.leaseChangedV4.Load()
	leaseChangedV6 := p.leaseChangedV6.Load()
	leasesObtainedV4 := p.leasesObtainedV4.Load()
	leasesObtainedV6 := p.leasesObtainedV6.Load()
	leasesRenewedV4 := p.leasesRenewedV4.Load()
	leasesRenewedV6 := p.leasesRenewedV6.Load()
	renewalsUnansweredV4 := p.renewalsUnansweredV4.Load()
	renewalsUnansweredV6 := p.renewalsUnansweredV6.Load()
	dhcpTimeoutsV4 := p.dhcpTimeoutsV4.Load()
	dhcpTimeoutsV6 := p.dhcpTimeoutsV6.Load()
	naksReceivedV4 := p.naksReceivedV4.Load()
	naksReceivedV6 := p.naksReceivedV6.Load()
	clientStopFailuresV4 := p.clientStopFailuresV4.Load()
	clientStopFailuresV6 := p.clientStopFailuresV6.Load()
	releasesSentV4 := p.releasesSentV4.Load()
	releasesSentV6 := p.releasesSentV6.Load()
	releaseFailuresV4 := p.releaseFailuresV4.Load()
	releaseFailuresV6 := p.releaseFailuresV6.Load()
	releasesReclaimedV4 := p.releasesReclaimedV4.Load()
	releasesReclaimedV6 := p.releasesReclaimedV6.Load()

	now := time.Now()
	h := HealthResponse{
		// Healthy is false on the five latched conditions (#524, #724); see HealthResponse.
		Healthy:            failed == 0 && joinFails == 0 && tsFails == 0 && conflicts == 0 && tsQuarantines == 0,
		EngineVersion:      engine.Version,
		APIVersion:         engine.APIVersion,
		InstanceID:         p.instanceID,
		UptimeSeconds:      time.Since(p.startTime).Seconds(),
		ActiveEndpoints:    len(endpoints),
		LinkLocalEndpoints: countLinkLocal(endpoints),
		Endpoints:          endpoints,
		PendingHints:       pending,
		RecoveredOK:        p.recoveredOK.Load(),
		RecoveryFailed:     failed,
		JoinStartFailures:  joinFails,
		// Deliberately absent from Healthy: a starting daemon (#383), an exited container (#376), a removed network
		// (#648).
		RecoveryDeferred:             p.recoveryDeferred.Load(),
		RecoveryAbortedContainerGone: p.recoveryAbortedContainerGone.Load(),
		RecoveryNetworkGone:          p.recoveryNetworkGone.Load(),
		RecoveryFingerprintsSkipped:  p.recoveryFingerprintsSkipped.Load(),
		RecoveryAlreadyManaged:       p.recoveryAlreadyManaged.Load(),
		JoinAbortedContainerGone:     p.joinAbortedContainerGone.Load(),
		JoinAbortedNoContainer:       p.joinAbortedNoContainer.Load(),
		JoinAttachSlow:               p.joinAttachSlow.Load(),
		JoinAttachCompleted:          p.joinAttachCompleted.Load(),
		JoinAttachUnder1s:            p.joinAttachUnder1s.Load(),
		JoinAttach1sToBudget:         p.joinAttach1sToBudget.Load(),
		JoinAttachMsMax:              p.joinAttachMsMax.Load(),
		RestartLinkUpWaited:          p.restartLinkUpWaited.Load(),
		RestartLinkUpTimeouts:        p.restartLinkUpTimeouts.Load(),
		JoinAbortedEndpointLeft:      p.joinAbortedEndpointLeft.Load(),
		TombstoneWriteFailures:       tsFails,
		TombstoneQuarantines:         tsQuarantines,
		UnsafeHostnamesRejected:      p.unsafeHostnamesRejected.Load(),
		HostnamesAppliedLate:         p.hostnamesAppliedLate.Load(),
		HostnameLookupFailures:       p.hostnameLookupFailures.Load(),
		HostnameApplyFailures:        p.hostnameApplyFailures.Load(),
		HostIfnamesApplied:           p.hostIfnamesApplied.Load(),
		HostIfnameConflicts:          p.hostIfnameConflicts.Load(),
		HostIfnameFailures:           p.hostIfnameFailures.Load(),
		UnsafeOptionValuesDropped:    p.unsafeOptionValuesDropped.Load(),
		NetworkOptionsRejected:       p.networkOptionsRejected.Load(),
		IPAMReplayHits:               p.ipamReplayHits.Load(),
		IPAMReplayMiss:               p.ipamReplayMiss.Load(),
		IPAMRebindAmbiguous:          p.ipamRebindAmbiguous.Load(),
		IPAMReserveDuplicateMAC:      p.ipamReserveDuplicateMAC.Load(),
		IPAMStrandedRecords:          p.ipamStrandedRecords.Load(),
		IPAMReleaseUnknown:           p.ipamReleaseUnknown.Load(),
		DNSPropagationPIDMismatches:  p.dnsPropagationPIDMismatches.Load(),
		NetnsPIDMismatches:           p.netnsPIDMismatches.Load(),
		SandboxKeyEntries:            p.sandboxKeyEntries.Load(),
		SandboxKeyEntryFailures:      p.sandboxKeyEntryFailures.Load(),
		SandboxPIDFallbacks:          p.sandboxPIDFallbacks.Load(),
		SandboxKeyAbsent:             p.sandboxKeyAbsent.Load(),
		SandboxKeyNotPermitted:       p.sandboxKeyNotPermitted.Load(),
		SandboxKeyNotANamespace:      p.sandboxKeyNotANamespace.Load(),
		SandboxKeyWrongNSType:        p.sandboxKeyWrongNSType.Load(),
		SandboxKeyUnavailable:        p.sandboxKeyUnavailable.Load(),
		DockerAPINonGETRefusals:      p.dockerAPINonGETRefusals.Load(),
		DHCPRoutesApplied:            p.dhcpRoutesApplied.Load(),
		DHCPDefaultRouteSuperseded:   p.dhcpDefaultRouteSuperseded.Load(),
		MTURefused:                   p.mtuRefused.Load(),
		TombstonesConsumed:           p.tombstonesConsumed.Load(),
		LeaseChanged:                 leaseChangedV4 + leaseChangedV6,
		AddressConflicts:             conflicts,
		AddressConflictsV4:           conflictsV4,
		AddressConflictsV6:           conflictsV6,
		ACDProbesSent:                p.acdProbesSent.Load(),
		ACDAnnouncementsSent:         p.acdAnnouncementsSent.Load(),
		ACDConflictsDetected:         p.acdConflictsDetected.Load(),
		ACDARPSendFailures:           p.acdARPSendFailures.Load(),
		ACDResumedUnchecked:          p.acdResumedUnchecked.Load(),
		SandboxNetnsVisible:          sandboxNetnsVisibleIn(netns.dirs),
		SandboxNetnsPropagation:      sandboxNetnsPropagationIn(netns.dirs, netns.mountinfo),
		SandboxNetnsInitMounts:       sandboxNetnsInitMountsIn(netns.dirs, netns.selfNS, netns.initNS, netns.initMountinfo),
		LeasesObtained:               leasesObtainedV4 + leasesObtainedV6,
		LeasesRenewed:                leasesRenewedV4 + leasesRenewedV6,
		RenewalsUnanswered:           renewalsUnansweredV4 + renewalsUnansweredV6,
		DHCPServerTierFallbacks:      p.dhcpServerTierFallbacks.Load(),
		DHCPServerPolicyExhausted:    p.dhcpServerPolicyExhausted.Load(),
		DHCPServerPolicyTimeouts:     p.dhcpServerPolicyTimeouts.Load(),
		DHCPTimeouts:                 dhcpTimeoutsV4 + dhcpTimeoutsV6,
		ClientStopFailures:           clientStopFailuresV4 + clientStopFailuresV6,
		ReleasesSent:                 releasesSentV4 + releasesSentV6,
		ReleaseFailures:              releaseFailuresV4 + releaseFailuresV6,
		ReleasesReclaimed:            releasesReclaimedV4 + releasesReclaimedV6,
		NAKsReceived:                 naksReceivedV4 + naksReceivedV6,
		DisplacedStops:               p.displacedStopsTotal.Load(),
		ParentLinkWaits:              p.parentLinkWaits.Load(),
		ParentLinkWaitTimeouts:       p.parentLinkWaitTimeouts.Load(),
		LedgerWriteFailures:          p.ledgerWriteFailures.Load(),
		IfnameUnsupported:            p.ifnameUnsupported.Load(),
		StateFileChmodFailures:       p.stateFileChmodFailures.Load(),
		LeaseChangedV4:               leaseChangedV4,
		LeasesObtainedV4:             leasesObtainedV4,
		LeasesRenewedV4:              leasesRenewedV4,
		RenewalsUnansweredV4:         renewalsUnansweredV4,
		DHCPTimeoutsV4:               dhcpTimeoutsV4,
		NAKsReceivedV4:               naksReceivedV4,
		ClientStopFailuresV4:         clientStopFailuresV4,
		ReleasesSentV4:               releasesSentV4,
		ReleaseFailuresV4:            releaseFailuresV4,
		ReleasesReclaimedV4:          releasesReclaimedV4,
		LeaseChangedV6:               leaseChangedV6,
		LeasesObtainedV6:             leasesObtainedV6,
		LeasesRenewedV6:              leasesRenewedV6,
		RenewalsUnansweredV6:         renewalsUnansweredV6,
		DHCPTimeoutsV6:               dhcpTimeoutsV6,
		NAKsReceivedV6:               naksReceivedV6,
		ClientStopFailuresV6:         clientStopFailuresV6,
		ReleasesSentV6:               releasesSentV6,
		ReleaseFailuresV6:            releaseFailuresV6,
		ReleasesReclaimedV6:          releasesReclaimedV6,
		DHCPv6ConfigOnly:             p.dhcpv6ConfigOnly.Load(),
		DHCPv6NotOffered:             p.dhcpv6NotOffered.Load(),
		DHCPv6NoRouterAdvert:         p.dhcpv6NoRouterAdvert.Load(),
		DHCPv6Refused:                p.dhcpv6Refused.Load(),
		DHCPv6NoServer:               p.dhcpv6NoServer.Load(),
		DHCPv6SLAACNoPrefix:          p.dhcpv6SLAACNoPrefix.Load(),
		DHCPv6SLAACNoAddress:         p.dhcpv6SLAACNoAddress.Load(),
		IPv6SLAACAddresses:           p.ipv6SLAACAddresses.Load(),
		IPv6AddressesWithdrawn:       p.ipv6AddressesWithdrawn.Load(),
		IPv6SLAACPrefixesIgnored:     p.ipv6SLAACPrefixesIgnored.Load(),
		IPv6MainPrefixUnmatched:      p.ipv6MainPrefixUnmatched.Load(),
		DHCPv6AutoFallbacks:          p.dhcpv6AutoFallbacks.Load(),
		IPv6LinkEnableFailures:       p.ipv6LinkEnableFailures.Load(),
		RouterAdvertGuardFailures:    p.routerAdvertGuardFailures.Load(),
		IPv6RouterWithdrawn:          p.ipv6RouterWithdrawn.Load(),
		RouterSolicitsSent:           p.routerSolicitsSent.Load(),
		RouterAdvertsSeen:            p.routerAdvertsSeen.Load(),
		RouterAdvertsRefused:         p.routerAdvertsRefused.Load(),
		RouterAdvertOptionsIgnored:   p.routerAdvertOptionsIgnored.Load(),
		RouterTableEntriesDropped:    p.routerTableEntriesDropped.Load(),
		RouterTableEntriesEvicted:    p.routerTableEntriesEvicted.Load(),
		Version:                      buildinfo.Version,
		Commit:                       buildinfo.Commit,
		Library:                      buildinfo.Library,
	}

	h.Status, h.Checks = healthChecks(h, p.checkStamps(), now)
	return h
}
