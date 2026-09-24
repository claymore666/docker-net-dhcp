// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// Untagged so healthfloor_test.go drives the floor in the unit job; HealthResponse lives here for that reason (#377).

package harness

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// HealthCheck mirrors pkg/plugin.HealthCheck, with the health-check draft's camelCase names (#910).
type HealthCheck struct {
	Status        string `json:"status"`
	ObservedValue int64  `json:"observedValue"`
	ObservedUnit  string `json:"observedUnit"`
	Time          string `json:"time"`
	Output        string `json:"output,omitempty"`
}

// EndpointHealth mirrors pkg/plugin.EndpointHealth.
type EndpointHealth struct {
	Endpoint      string `json:"endpoint"`
	Network       string `json:"network"`
	Mode          string `json:"mode"`
	Address       string `json:"address,omitempty"`
	LeaseState    string `json:"lease_state"`
	RenewAt       string `json:"renew_at,omitempty"`
	RebindAt      string `json:"rebind_at,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	Server        string `json:"server,omitempty"`
	LastEvent     string `json:"last_event,omitempty"`
	LastEventAt   string `json:"last_event_at,omitempty"`
	ConflictCheck string `json:"conflict_check"`
	ACDPhase      string `json:"acd_phase"`
}

// HealthFieldSeries names the series for health fields not exposed as net_dhcp_<tag> or net_dhcp_<tag>_total.
var HealthFieldSeries = map[string]string{
	"status": "health_status",
}

// HealthFieldsAsLabels maps label-only fields to their identity series, mirroring pkg/plugin without importing it (#670).
var HealthFieldsAsLabels = map[string]string{
	"instance_id":    "build_info",
	"version":        "build_info",
	"commit":         "build_info",
	"library":        "build_info",
	"engine_version": "engine_info",
	"api_version":    "engine_info",
}

var HealthFieldsNotExposed = map[string]string{
	"checks":    "each check's observedValue is the counter's own series, already exposed",
	"endpoints": "a series per container is a cardinality decision O-5 takes, not this one",
}

// HealthResponse mirrors pkg/plugin.HealthResponse without importing it.
type HealthResponse struct {
	Healthy bool `json:"healthy"`
	// Status is draft-inadarei-api-health-check-06's pass/warn/fail, nil on a plugin that publishes none (#910).
	Status *string `json:"status"`
	// Checks is the document's named checks, nil on a plugin that publishes none.
	Checks map[string][]HealthCheck `json:"checks"`
	// Endpoints is one entry per registered manager.
	Endpoints []EndpointHealth `json:"endpoints"`
	// Version, Commit and Library identify the serving binary; nil means not published.
	Version *string `json:"version"`
	Commit  *string `json:"commit"`
	Library *string `json:"library"`
	// InstanceID identifies the serving plugin process, so deltas compare only within one instance (#405).
	InstanceID      string  `json:"instance_id"`
	UptimeSeconds   float64 `json:"uptime_seconds"`
	ActiveEndpoints int     `json:"active_endpoints"`
	PendingHints    int     `json:"pending_hints"`
	RecoveredOK     int32   `json:"recovered_ok"`
	// DisplacedStops counts managers a Join stopped because one was already registered (#338).
	DisplacedStops int32 `json:"displaced_stops"`
	RecoveryFailed int32 `json:"recovery_failed"`
	// RecoveryDeferred counts recoveries retried because the daemon was not serving yet (#383).
	RecoveryDeferred int32 `json:"recovery_deferred"`
	// RecoveryAbortedContainerGone counts endpoints whose container exited before recovery reached them (#376).
	RecoveryAbortedContainerGone int32 `json:"recovery_aborted_container_gone"`
	// RecoveryNetworkGone counts networks removed between recovery's listing and its detail read (#648).
	RecoveryNetworkGone int32 `json:"recovery_network_gone"`
	// RecoveryFingerprintsSkipped counts adopted endpoints with no hostname, so no tombstone is laid (#721).
	RecoveryFingerprintsSkipped int32 `json:"recovery_fingerprints_skipped"`
	// RecoveryAlreadyManaged counts endpoints a Join reached before recovery did (#480).
	RecoveryAlreadyManaged int32 `json:"recovery_already_managed"`
	JoinStartFailures      int32 `json:"join_start_failures"`
	// JoinAbortedContainerGone counts containers that exited before the persistent client was up (#373).
	JoinAbortedContainerGone int32 `json:"join_aborted_container_gone"`
	// JoinAbortedNoContainer counts endpoints no container claimed, whose address was released (#566).
	JoinAbortedNoContainer int32 `json:"join_aborted_no_container"`
	JoinAttachSlow         int32 `json:"join_attach_slow"`
	// HostnamesAppliedLate counts names applied after the client was already leasing, v4 only (#961).
	HostnamesAppliedLate   int32 `json:"hostnames_applied_late"`
	HostnameLookupFailures int32 `json:"hostname_lookup_failures"`
	HostnameApplyFailures  int32 `json:"hostname_apply_failures"`
	// HostIfnamesApplied counts host-side links named after their container, bridge mode only (#978).
	HostIfnamesApplied  int32 `json:"host_ifnames_applied"`
	HostIfnameConflicts int32 `json:"host_ifname_conflicts"`
	HostIfnameFailures  int32 `json:"host_ifname_failures"`
	// JoinAttachCompleted counts completed attaches, the domain join_attach_slow is read against (#403).
	JoinAttachCompleted  int32 `json:"join_attach_completed"`
	JoinAttachUnder1s    int32 `json:"join_attach_under_1s"`
	JoinAttach1sToBudget int32 `json:"join_attach_1s_to_budget"`
	JoinAttachMsMax      int32 `json:"join_attach_ms_max"`

	// RestartLinkUpWaited counts child links that came up only after the departing link released the address (#408, #422).
	RestartLinkUpWaited     int32 `json:"restart_link_up_waited"`
	RestartLinkUpTimeouts   int32 `json:"restart_link_up_timeouts"`
	JoinAbortedEndpointLeft int32 `json:"join_aborted_endpoint_left"`
	TombstoneWriteFailures  int32 `json:"tombstone_write_failures"`
	// TombstoneQuarantines counts unparseable tombstone files moved aside, which is healthy-affecting (#724).
	TombstoneQuarantines int32 `json:"tombstone_quarantines"`
	// TombstonesConsumed counts addresses preserved by replaying a tombstone (#386).
	TombstonesConsumed int32 `json:"tombstones_consumed"`
	// AddressConflicts is healthy-affecting (#524).
	AddressConflicts int32 `json:"address_conflicts"`
	// AddressConflictsV4 is the RFC 5227 half; the v6 half is DAD (RFC 4862 section 5.4), declined under RFC 9915
	// section 18.2.8, and moves no ACD counter (#881).
	AddressConflictsV4 int32 `json:"address_conflicts_v4"`
	AddressConflictsV6 int32 `json:"address_conflicts_v6"`
	// ACDProbesSent counts RFC 5227 section 2.1.1 ARP Probes.
	ACDProbesSent int32 `json:"acd_probes_sent"`
	// ACDAnnouncementsSent counts RFC 5227 section 2.3 announcements, two per address that passed.
	ACDAnnouncementsSent int32 `json:"acd_announcements_sent"`
	// ACDConflictsDetected is the library's own count of AddressConflicts' population (#882).
	ACDConflictsDetected int32 `json:"acd_conflicts_detected"`
	// ACDARPSendFailures counts probes and announcements the socket refused (#882).
	ACDARPSendFailures int32 `json:"acd_arp_send_failures"`
	// ACDResumedUnchecked counts endpoints resumed before their RFC 5227 check finished, a warn check (#910).
	ACDResumedUnchecked int32 `json:"acd_resumed_unchecked"`
	// SandboxNetnsVisible is the number of sandbox netns entries the plugin sees, or -1 if unreadable (#567).
	SandboxNetnsVisible *int32 `json:"sandbox_netns_visible"`
	// SandboxNetnsPropagation is 1 linked, 0 private or -1 with no covering mount, for later sandbox netns mounts.
	SandboxNetnsPropagation *int32 `json:"sandbox_netns_propagation"`
	// SandboxNetnsInitMounts is PID 1's sandbox netns mount count: -2 shared namespace, -1 unreadable.
	SandboxNetnsInitMounts *int32 `json:"sandbox_netns_init_mounts"`
	// SandboxKeyEntries counts attaches that entered the container's namespace through the sandbox key.
	SandboxKeyEntries       *int32 `json:"sandbox_key_entries"`
	SandboxKeyEntryFailures *int32 `json:"sandbox_key_entry_failures"`
	SandboxPIDFallbacks     *int32 `json:"sandbox_pid_fallbacks"`
	// SandboxKeyAbsent counts endpoints with no published key from either source.
	SandboxKeyAbsent        *int32 `json:"sandbox_key_absent"`
	SandboxKeyNotPermitted  *int32 `json:"sandbox_key_not_permitted"`
	SandboxKeyNotANamespace *int32 `json:"sandbox_key_not_a_namespace"`
	SandboxKeyWrongNSType   *int32 `json:"sandbox_key_wrong_ns_type"`
	SandboxKeyUnavailable   *int32 `json:"sandbox_key_unavailable"`
	// DockerAPINonGETRefusals counts Docker API requests refused for an unsafe method (#691).
	DockerAPINonGETRefusals *int32 `json:"docker_api_non_get_refusals"`
	LeaseChanged            int32  `json:"lease_changed"`
	LeasesObtained          int32  `json:"leases_obtained"`
	// LeasesObtainedV4 is the v4 half of LeasesObtained, the half RFC 5227 covers (#881).
	LeasesObtainedV4 int32 `json:"leases_obtained_v4"`
	LeasesRenewed    int32 `json:"leases_renewed"`
	// RenewalsUnanswered counts renewals the server did not answer while the lease was still held (#940).
	RenewalsUnanswered   int32 `json:"renewals_unanswered"`
	RenewalsUnansweredV4 int32 `json:"renewals_unanswered_v4"`
	RenewalsUnansweredV6 int32 `json:"renewals_unanswered_v6"`
	DHCPTimeouts         int32 `json:"dhcp_timeouts"`
	// ClientStopFailures counts renewal clients that did not stop cleanly, lease_release_failures before #800 (#962).
	ClientStopFailures int32 `json:"client_stop_failures"`
	// ReleasesSent counts release_lease releases that left the host (#962).
	ReleasesSent      int32 `json:"releases_sent"`
	ReleasesSentV4    int32 `json:"releases_sent_v4"`
	ReleasesSentV6    int32 `json:"releases_sent_v6"`
	ReleaseFailures   int32 `json:"release_failures"`
	ReleaseFailuresV4 int32 `json:"release_failures_v4"`
	ReleaseFailuresV6 int32 `json:"release_failures_v6"`
	// ReleasesReclaimed counts on_remove addresses a running container reused within the window (#984).
	ReleasesReclaimed   int32 `json:"releases_reclaimed"`
	ReleasesReclaimedV4 int32 `json:"releases_reclaimed_v4"`
	ReleasesReclaimedV6 int32 `json:"releases_reclaimed_v6"`
	NAKsReceived        int32 `json:"naks_received"`
	LedgerWriteFailures int32 `json:"ledger_write_failures"`
	// StateFileChmodFailures counts STATE_DIR files the startup sweep could not tighten (#804).
	StateFileChmodFailures int32 `json:"state_file_chmod_failures"`
	// IfnameUnsupported counts custom interface names on an engine that does not apply them (#125, #670).
	IfnameUnsupported int32 `json:"ifname_unsupported"`
	// EngineVersion is the engine version the daemon reported at startup; nil means not published.
	EngineVersion *string `json:"engine_version"`
	APIVersion    *string `json:"api_version"`
	// ParentLinkWaits counts operations that queued on a shared parent NIC (#486, #549).
	ParentLinkWaits        int32 `json:"parent_link_waits"`
	ParentLinkWaitTimeouts int32 `json:"parent_link_wait_timeouts"`
	// DHCPServerTierFallbacks counts a preferred dhcp_servers entry silent and the next one answering (#111, #669).
	DHCPServerTierFallbacks   int32 `json:"dhcp_server_tier_fallbacks"`
	DHCPServerPolicyExhausted int32 `json:"dhcp_server_policy_exhausted"`

	// DHCPv6ConfigOnly counts DHCPv6 replies with configuration and no address (#815).
	DHCPv6ConfigOnly int32 `json:"dhcpv6_config_only"`

	// DHCPv6NotOffered counts IPv6 endpoints whose router advertised no managed address (#868).
	DHCPv6NotOffered       int32 `json:"dhcpv6_not_offered"`
	DHCPv6NoRouterAdvert   int32 `json:"dhcpv6_no_router_advert"`
	IPv6LinkEnableFailures int32 `json:"ipv6_link_enable_failures"`

	// DHCPv6NoServer and the SLAAC counters are read through CounterWindow, which checks the instance (#405).
	DHCPv6NoServer           int32 `json:"dhcpv6_no_server"`
	DHCPv6SLAACNoPrefix      int32 `json:"dhcpv6_slaac_no_prefix"`
	DHCPv6SLAACNoAddress     int32 `json:"dhcpv6_slaac_no_address"`
	DHCPv6AutoFallbacks      int32 `json:"dhcpv6_auto_fallbacks"`
	IPv6SLAACAddresses       int32 `json:"ipv6_slaac_addresses"`
	IPv6AddressesWithdrawn   int32 `json:"ipv6_addresses_withdrawn"`
	IPv6SLAACPrefixesIgnored int32 `json:"ipv6_slaac_prefixes_ignored"`
	IPv6MainPrefixUnmatched  int32 `json:"ipv6_main_prefix_unmatched"`

	// RouterAdvertGuardFailures counts RA guard steps that did not take (#911).
	//
	// DHCPv6 carries no next hop (RFC 9915 section 21) and RFC 5942 section 4 rule 1 forbids an on-link prefix, so the
	// container kernel must accept RAs for a route.
	RouterAdvertGuardFailures int32 `json:"router_advert_guard_failures"`

	// IPv6RouterWithdrawn counts default routes removed for a Router Lifetime of 0 (RFC 4861 section 4.2, #821).
	IPv6RouterWithdrawn int32 `json:"ipv6_router_withdrawn"`

	// RouterSolicitsSent and RouterAdvertsSeen are the library's RFC 4861 router-discovery counters (#814).
	RouterSolicitsSent         int32 `json:"router_solicits_sent"`
	RouterAdvertsSeen          int32 `json:"router_adverts_seen"`
	RouterAdvertsRefused       int32 `json:"router_adverts_refused"`
	RouterAdvertOptionsIgnored int32 `json:"router_advert_options_ignored"`
	RouterTableEntriesDropped  int32 `json:"router_table_entries_dropped"`
	RouterTableEntriesEvicted  int32 `json:"router_table_entries_evicted"`

	// IPAMReplayHits and the other IPAM counters describe the server or the daemon, not a plugin fault (#110).
	IPAMReplayHits          int32 `json:"ipam_replay_hits"`
	IPAMReplayMiss          int32 `json:"ipam_replay_miss"`
	IPAMRebindAmbiguous     int32 `json:"ipam_rebind_ambiguous"`
	IPAMReserveDuplicateMAC int32 `json:"ipam_reserve_duplicate_mac"`
	IPAMReleaseUnknown      int32 `json:"ipam_release_unknown"`

	// published is the decoded key set, since an absent field decodes to zero (#377); nil means built by hand.
	published map[string]json.RawMessage
}

// UnmarshalJSON decodes as usual and records which keys the payload carried.
func (h *HealthResponse) UnmarshalJSON(b []byte) error {
	type plain HealthResponse
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	if keys == nil {
		// A literal null body decodes to a nil map; it published nothing.
		keys = map[string]json.RawMessage{}
	}
	*h = HealthResponse(p)
	h.published = keys
	return nil
}

// floorCounter is one counter the floor reads; name equals the field's JSON tag.
type floorCounter struct {
	name  string
	read  func(*HealthResponse) int32
	fatal bool
	why   string
}

var floorCounters = []floorCounter{
	{
		name:  "join_start_failures",
		read:  func(h *HealthResponse) int32 { return h.JoinStartFailures },
		fatal: true,
		why:   "a running container was left without a renewal client; since #373 the benign container-exited case is counted separately as join_aborted_container_gone, so this counter now means only a real fault",
	},
	{
		name:  "tombstone_write_failures",
		read:  func(h *HealthResponse) int32 { return h.TombstoneWriteFailures },
		fatal: true,
		why:   "the plugin could not persist its tombstone state to disk; an endpoint will not keep its address across a restart",
	},
	{
		name:  "tombstone_quarantines",
		read:  func(h *HealthResponse) int32 { return h.TombstoneQuarantines },
		fatal: true,
		why:   "the tombstone file was unparseable and was quarantined as tombstones.json.corrupt-<ts>; every live tombstone on the host went with it, so any container that restarts inside the TTL window comes back with a different MAC and a different address. Strictly worse than tombstone_write_failures, which costs one container the same thing (#724). The quarantined file is still on disk under STATE_DIR and nothing reaps it — read it before deleting it, it is the only evidence of what was lost",
	},
	{
		name:  "recovery_failed",
		read:  func(h *HealthResponse) int32 { return h.RecoveryFailed },
		fatal: true,
		why:   "recovery could not rebuild a RUNNING container's renewal client, so its lease will not renew until it is restarted. Fatal since #421: the benign paths that used to land here are counted separately — recovery_deferred for a daemon that was not serving yet (#383), recovery_aborted_container_gone for a container that had already exited (#376), and recovery_network_gone for a network removed out from under the walk (#648) — and the probation runs this counter was left non-fatal for came back clean",
	},
	{
		name:  "address_conflicts",
		read:  func(h *HealthResponse) int32 { return h.AddressConflicts },
		fatal: true,
		why:   "an endpoint was leased an address another device on the segment already holds, so traffic for it is wrong for both hosts (#524). Fatal from the start, unlike recovery_failed: there is no benign path into this counter — it moves only when a probe got an ARP reply from a MAC that is not the endpoint's. A run that trips this has a container up on somebody else's address, which is the exact production fault the counter was added for",
	},
}

// absentWhy explains a finding for a counter the plugin did not publish, fatal whatever its verdict (#377).
const absentWhy = "the plugin did not publish this counter, so this run proves nothing about it — an absent JSON field decodes as zero and would otherwise read as clean. Either the plugin under test is an older build than the suite (rebuild and reinstall it), or the counter was renamed in pkg/plugin/endpoints.go without updating floorCounters in this file"

// FloorFinding is one healthy-affecting counter that moved off zero or was not reported.
type FloorFinding struct {
	Counter string
	Value   int32
	// Absent marks a counter the plugin never published.
	Absent bool
	// Flag marks a finding about the plugin's healthy flag.
	Flag bool
	// Fatal marks a counter that only ever means a plugin fault.
	Fatal bool
	// Why explains the verdict in the failure output.
	Why string
}

// healthyKey is the plugin's own summary verdict on the payload.
const healthyKey = "healthy"

// healthyWhy explains a floor failure raised by the flag.
const healthyWhy = "the plugin reports itself unhealthy while every counter this suite checks is at zero. That means pkg/plugin's Healthy expression covers a condition floorCounters does not — a new healthy-affecting counter was added there without being mirrored here. The plugin's own verdict wins: it is the surface operators page on"

// CheckHealthFloor returns findings for every healthy-affecting counter that is non-zero or unreported (#377).
//
// Values are absolute since plugin start. An old build without join_start_failures read as clean before #377.
func CheckHealthFloor(h *HealthResponse) []FloorFinding {
	if h == nil {
		return nil
	}
	var out []FloorFinding
	for _, c := range floorCounters {
		if h.published != nil {
			if _, ok := h.published[c.name]; !ok {
				out = append(out, FloorFinding{
					Counter: c.name,
					Absent:  true,
					Fatal:   true,
					Why:     absentWhy,
				})
				continue
			}
		}
		if v := c.read(h); v > 0 {
			out = append(out, FloorFinding{
				Counter: c.name,
				Value:   v,
				Fatal:   c.fatal,
				Why:     c.why,
			})
		}
	}

	// The plugin's own verdict, checked independently of the mirrored table (#421), and only on a decoded payload.
	if h.published == nil {
		return out
	}
	if _, ok := h.published[healthyKey]; !ok {
		return append(out, FloorFinding{
			Counter: healthyKey,
			Absent:  true,
			Flag:    true,
			Fatal:   true,
			Why:     absentWhy,
		})
	}
	if !h.Healthy {
		// Only when no named finding already explains it.
		if len(out) == 0 {
			out = append(out, FloorFinding{
				Counter: healthyKey,
				Flag:    true,
				Fatal:   true,
				Why:     healthyWhy,
			})
		}
	}
	return out
}

// FloorFailed reports whether any finding is fatal.
func FloorFailed(findings []FloorFinding) bool {
	for _, f := range findings {
		if f.Fatal {
			return true
		}
	}
	return false
}

// floorEvidenceMaxFaultLines bounds the fault section.
const floorEvidenceMaxFaultLines = 200

// FloorEvidence returns every error and warning line plus the last tailLines lines of a plugin log (#385).
func FloorEvidence(logData []byte, tailLines int) string {
	lines := strings.Split(strings.TrimRight(string(logData), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return "  (plugin log is empty)\n"
	}

	var faults []string
	for _, l := range lines {
		if strings.Contains(l, "level=error") || strings.Contains(l, "level=warning") {
			faults = append(faults, l)
		}
	}

	var b strings.Builder
	if len(faults) == 0 {
		// A moved counter with no error or warning line means the counter and its log line drifted apart.
		b.WriteString("  no error- or warning-level lines in the plugin log — " +
			"a counter moved without logging, which is itself a defect\n")
	} else {
		shown := faults
		if len(shown) > floorEvidenceMaxFaultLines {
			shown = shown[len(shown)-floorEvidenceMaxFaultLines:]
			fmt.Fprintf(&b, "  --- last %d of %d error/warning lines ---\n",
				len(shown), len(faults))
		} else {
			fmt.Fprintf(&b, "  --- %d error/warning lines ---\n", len(faults))
		}
		for _, l := range shown {
			b.WriteString("  " + l + "\n")
		}
	}

	if tailLines > 0 {
		tail := lines
		if len(tail) > tailLines {
			tail = tail[len(tail)-tailLines:]
		}
		fmt.Fprintf(&b, "  --- last %d lines ---\n", len(tail))
		for _, l := range tail {
			b.WriteString("  " + l + "\n")
		}
	}
	return b.String()
}

// floorFullCoverageRatio is the share of the suite the plugin's uptime must span to cover the run.
const floorFullCoverageRatio = 0.98

// floorPredatesRunSeconds is how far the uptime must exceed the suite before the line notes history.
const floorPredatesRunSeconds = 300

// FloorCleanLine renders a clean verdict with what it covered (#385).
//
// Run #379 printed "clean" for a run holding a real fault erased by a respawn (#383); uptime once printed as "the
// whole 15479s run" for a 61 s suite (#474).
func FloorCleanLine(h *HealthResponse, suiteSeconds float64) string {
	if h == nil {
		return ""
	}
	if suiteSeconds <= 0 {
		return fmt.Sprintf(
			"HEALTH FLOOR: clean — no healthy-affecting counter moved over the plugin's %.0fs uptime.\n"+
				"  The suite's own duration was not measured, so how much of the run that spans is\n"+
				"  unknown (healthy=%v).\n",
			h.UptimeSeconds, h.Healthy)
	}
	if h.UptimeSeconds >= suiteSeconds*floorFullCoverageRatio {
		line := fmt.Sprintf(
			"HEALTH FLOOR: clean — no healthy-affecting counter moved over the whole %.0fs run\n"+
				"  (plugin up %.0fs, so its counters span it; healthy=%v)\n",
			suiteSeconds, h.UptimeSeconds, h.Healthy)
		if h.UptimeSeconds-suiteSeconds > floorPredatesRunSeconds {
			line += fmt.Sprintf(
				"  The plugin predates this run by %.0fs, so these counters also carry history\n"+
					"  from before it. Clean is therefore stronger than this run needed — but had\n"+
					"  anything moved, it could not have been pinned on this run either.\n",
				h.UptimeSeconds-suiteSeconds)
		}
		return line
	}
	return fmt.Sprintf(
		"HEALTH FLOOR: clean over the last %.0fs of a %.0fs run — %.0f%% of it (healthy=%v).\n"+
			"  The plugin restarted mid-suite and its counters reset with it, so this verdict\n"+
			"  says nothing about the earlier %.0fs. The per-test deltas cover that stretch.\n",
		h.UptimeSeconds, suiteSeconds, 100*h.UptimeSeconds/suiteSeconds, h.Healthy,
		suiteSeconds-h.UptimeSeconds)
}

// AttachGraceLine reports how many attaches finished only through the daemon-busy grace (#406).
//
// These failures are intermittent: runs scored 6, 5, 3 and 0 against unchanged code.
func AttachGraceLine(h *HealthResponse, joinFailures int) string {
	if h == nil {
		return ""
	}
	switch {
	case h.JoinAttachSlow > 0 && joinFailures == 0:
		return fmt.Sprintf(
			"ATTACH GRACE: %d attach(es) finished only after outlasting AWAIT_TIMEOUT, and none\n"+
				"  were abandoned. Before #406 each of those was a running container left with no\n"+
				"  renewal client. This is the fix working, observed rather than inferred.\n",
			h.JoinAttachSlow)
	case h.JoinAttachSlow > 0:
		return fmt.Sprintf(
			"ATTACH GRACE: %d attach(es) needed the grace, and %d still failed. The grace is not\n"+
				"  sufficient for every case (#406).\n", h.JoinAttachSlow, joinFailures)
	case joinFailures == 0:
		return "ATTACH GRACE: no attach needed the grace this run. The daemon-busy window did not\n" +
			"  arise, so this run is not evidence either way about #406.\n"
	default:
		return fmt.Sprintf(
			"ATTACH GRACE: %d Join failure(s) and no attach used the grace. The failures are not\n"+
				"  the daemon-busy mechanism #406 describes — look elsewhere.\n", joinFailures)
	}
}

// ACDCensusLine reports whether the address-conflict check ran, which zero conflicts cannot say (#524).
func ACDCensusLine(h *HealthResponse) string {
	if h == nil {
		return ""
	}
	switch {
	case h.AddressConflicts > 0:
		return fmt.Sprintf(
			"ACD CENSUS: %d leased address(es) were already held by another device on the\n"+
				"  segment, out of %d ARP Probe(s) sent. The floor fails on this — see above (#524).\n",
			h.AddressConflicts, h.ACDProbesSent)
	case h.ACDProbesSent == 0:
		return "ACD CENSUS: no ARP Probe was sent this run, so address_conflicts=0 is not\n" +
			"  evidence the segment was clean — it is the absence of a measurement. Either no\n" +
			"  endpoint was leased a v4 address, every network ran conflict_check=off, or the\n" +
			"  check did not run (#524).\n"
	case h.ACDARPSendFailures > 0:
		return fmt.Sprintf(
			"ACD CENSUS: %d ARP Probe(s)/Announcement(s) went out and found no conflict, but %d\n"+
				"  send(s) were refused. The clean verdict covers only what was actually asked (#524).\n",
			h.ACDProbesSent, h.ACDARPSendFailures)
	default:
		return fmt.Sprintf(
			"ACD CENSUS: %d ARP Probe(s) sent and %d Announcement(s); no conflict, no refused\n"+
				"  send. The check ran and the segment was clean — observed, not inferred (#524).\n",
			h.ACDProbesSent, h.ACDAnnouncementsSent)
	}
}

// joinStartFailureMsg is logged at every real join_start_failures increment, not by its benign twin.
const joinStartFailureMsg = "Failed to start persistent DHCP client"

// fatalFaultSignature ties a healthy-affecting counter to the log line the plugin writes when it bumps it (#385).
//
// The counters reset with the plugin; they covered 10% and 12% of two measured runs.
type fatalFaultSignature struct {
	counter string
	msg     string
	why     string
}

var fatalFaultSignatures = []fatalFaultSignature{
	{
		counter: "tombstone_write_failures",
		msg:     "Failed to persist tombstone",
		why:     "a container restart may come back with a different MAC and IP",
	},
	{
		counter: "recovery_failed",
		msg:     "recovery: NetworkInspect failed",
		why:     "a whole network was skipped during post-restart recovery",
	},
	{
		counter: "recovery_failed",
		msg:     "recovery: failed to load network options",
		why:     "a whole network was skipped during post-restart recovery",
	},
	{
		counter: "recovery_failed",
		msg:     "recovery: endpoint recovery failed",
		why:     "an endpoint was not rebuilt after a plugin restart",
	},
	{
		counter: "recovery_failed",
		msg:     "recovery: daemon never became reachable",
		why:     "recovery gave up entirely; every previously-attached endpoint is running without renewal",
	},
	{
		counter: "recovery_failed",
		msg:     "recovery: persistent DHCP client Start failed",
		why:     "a running container's lease will not renew until it is restarted",
	},
}

// FaultCensus counts healthy-affecting faults other than Join failures across the whole plugin log.
func FaultCensus(logData []byte) (int, string) {
	lines := strings.Split(string(logData), "\n")
	counts := map[string]int{}
	whys := map[string]string{}
	total := 0
	for _, sig := range fatalFaultSignatures {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, sig.msg) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		total += n
		counts[sig.counter+": "+sig.msg] = n
		whys[sig.counter+": "+sig.msg] = sig.why
	}
	if total == 0 {
		return 0, ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PLUGIN FAULTS: %d across the whole run "+
		"(the log spans the run; the counters only span the last plugin restart)\n", total)
	for _, k := range sortedKeys(counts) {
		fmt.Fprintf(&b, "  %3d  %s\n       %s\n", counts[k], k, whys[k])
	}
	return total, b.String()
}

// JoinFailureCount counts the Join-start failures JoinFailureCensus summarises.
func JoinFailureCount(logData []byte) int {
	n := 0
	for _, l := range strings.Split(string(logData), "\n") {
		if strings.Contains(l, joinStartFailureMsg) {
			n++
		}
	}
	return n
}

// JoinFailureCensus summarises Join-start failures across the whole plugin log (#385, #401).
//
// One run logged twelve of these while the counter read 1.
func JoinFailureCensus(logData []byte) string {
	reasons := map[string]int{}
	total := 0
	for _, l := range strings.Split(string(logData), "\n") {
		if !strings.Contains(l, joinStartFailureMsg) {
			continue
		}
		total++
		reasons[joinFailureReason(l)]++
	}
	if total == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "JOIN FAILURES: %d across the whole run "+
		"(the log spans the run; join_start_failures only spans the last plugin restart)\n", total)
	for _, r := range sortedKeys(reasons) {
		fmt.Fprintf(&b, "  %3d  %s\n", reasons[r], r)
	}
	return b.String()
}

// joinFailureReason pulls the error= field out of a logrus text line.
func joinFailureReason(line string) string {
	const key = `error="`
	i := strings.Index(line, key)
	if i < 0 {
		return "(no error field)"
	}
	rest := line[i+len(key):]
	// logrus escapes inner quotes, so the first unescaped quote ends the value.
	for j := 0; j < len(rest); j++ {
		if rest[j] == '\\' {
			j++
			continue
		}
		if rest[j] == '"' {
			return rest[:j]
		}
	}
	return rest
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The ACD census gate (#551): from #527 until #550 every run printed "2 could not run" and stayed green.
//
// A test that degrades the ARP socket declares it, and a lease no probe covers (conflict_check=off, or a resumed
// endpoint whose probe is asynchronous) is declared per test. Measured in integration run 34600486961 main-3: a shard
// whose last test recycled the plugin read leases_obtained_v4=1 and acd_probes_sent=0 (#110). recovered_ok counts
// both families, so subtracting it would let a v6 endpoint cancel a v4 miss. leases_obtained is the v4 and v6 sum,
// so the gate reads leases_obtained_v4 (#881).

// acdAllowance accumulates what the tests in this shard declared, read by the floor in TestMain.
var acdAllowance struct {
	mu        sync.Mutex
	sendFail  int32
	unprobed  int32
	conflicts int32
}

// AllowARPSendFailures declares n expected refused ARP sends in this shard.
func AllowARPSendFailures(n int32) {
	acdAllowance.mu.Lock()
	defer acdAllowance.mu.Unlock()
	acdAllowance.sendFail += n
}

// AllowedARPSendFailures reports the total declared so far.
func AllowedARPSendFailures() int32 {
	acdAllowance.mu.Lock()
	defer acdAllowance.mu.Unlock()
	return acdAllowance.sendFail
}

// AllowUnprobedLeases declares n v4 leases in this shard that no ARP Probe covers, once per lease (#882, #110).
func AllowUnprobedLeases(n int32) {
	acdAllowance.mu.Lock()
	defer acdAllowance.mu.Unlock()
	acdAllowance.unprobed += n
}

// AllowedUnprobedLeases reports the total declared so far.
func AllowedUnprobedLeases() int32 {
	acdAllowance.mu.Lock()
	defer acdAllowance.mu.Unlock()
	return acdAllowance.unprobed
}

// AllowStagedConflicts declares n conflicts in this shard staged by a test's own squatter (#882).
//
// Inside the declaration the floor cannot tell a counter lost to a restart from a dropped event: declared=1, log=1,
// counter=0 yields no fatal finding. The conflict cases assert address_conflicts moved across their own squatter.
func AllowStagedConflicts(n int32) {
	acdAllowance.mu.Lock()
	defer acdAllowance.mu.Unlock()
	acdAllowance.conflicts += n
}

// AllowedStagedConflicts reports the total declared so far.
func AllowedStagedConflicts() int32 {
	acdAllowance.mu.Lock()
	defer acdAllowance.mu.Unlock()
	return acdAllowance.conflicts
}

// base returns the baseline, or zeroes when there is none, which judges the whole plugin life.
func base(b *HealthResponse) *HealthResponse {
	if b == nil {
		return &HealthResponse{}
	}
	return b
}

// deltaSincePluginStart returns now-was, or now when the plugin restarted below the baseline (#385, #584).
func deltaSincePluginStart(now, was int32) int32 {
	if now < was {
		return now
	}
	return now - was
}

// The log lines the plugin writes at an address_conflicts increment, copied from pkg/plugin/conflict.go.
const (
	conflictProbeMsg = "The address this endpoint was offered is already in use on the segment"
	conflictHeldMsg  = "The address this endpoint HOLDS was found in use by another device on the segment"
	// The DHCPv6 pair, written when DAD finds a conflict (#881).
	conflictProbeMsg6 = "The IPv6 address this endpoint was offered is already in use on the link"
	conflictHeldMsg6  = "The IPv6 address this endpoint HOLDS was found in use by another node on the link"
)

// conflictMsgs lists every conflict line.
var conflictMsgs = []string{
	conflictHeldMsg,
	conflictProbeMsg,
	conflictHeldMsg6,
	conflictProbeMsg6,
}

// ConflictsInLog counts conflicts across the whole run, since the counters reset with the plugin (#385).
func ConflictsInLog(logData []byte) int {
	if len(logData) == 0 {
		return 0
	}
	n := 0
	for _, l := range strings.Split(string(logData), "\n") {
		for _, msg := range conflictMsgs {
			if strings.Contains(l, msg) {
				n++
				break
			}
		}
	}
	return n
}

// ACDCensusFindings judges the census against the shard's declarations; nil means nothing to say.
func ACDCensusFindings(h *HealthResponse, allowedSendFailures, allowedUnprobed, allowedConflicts int32, conflictsInLog int, baseline *HealthResponse) []FloorFinding {
	if h == nil {
		return nil
	}
	// An absent counter is not a zero.
	if h.published != nil {
		// leases_obtained_v4 is the gate's domain operand (#881).
		for _, k := range []string{"acd_probes_sent", "acd_arp_send_failures", "leases_obtained_v4"} {
			if _, ok := h.published[k]; !ok {
				return []FloorFinding{{
					Counter: k,
					Absent:  true,
					Fatal:   true,
					Why: "the plugin did not publish this, so whether RFC 5227's check " +
						"ran cannot be established. address_conflicts=0 is not evidence " +
						"without it (#551).",
				}}
			}
		}
	}

	var out []FloorFinding

	// Counters are per plugin life and the allowance per process; the coverage lane runs one plugin through both
	// suites, so compare deltas (#584).
	sendFailures := deltaSincePluginStart(h.ACDARPSendFailures, base(baseline).ACDARPSendFailures)
	probes := deltaSincePluginStart(h.ACDProbesSent, base(baseline).ACDProbesSent)
	// The v4 half: ARP covers only IPv4 (#881).
	leases := deltaSincePluginStart(h.LeasesObtainedV4, base(baseline).LeasesObtainedV4)
	conflicts := deltaSincePluginStart(h.AddressConflicts, base(baseline).AddressConflicts)

	if excess := sendFailures - allowedSendFailures; excess > 0 {
		out = append(out, FloorFinding{
			Counter: "acd_arp_send_failures",
			Value:   sendFailures,
			Fatal:   true,
			Why: fmt.Sprintf(
				"%d ARP Probe(s)/Announcement(s) were refused by the socket; %d is the number "+
					"this shard declared as deliberate, so %d went unexplained. A probe that never "+
					"went out proves nothing about the address, and address_conflicts=0 does not "+
					"cover it — that reading is exactly how #524 stayed invisible in production "+
					"for months (#551).",
				sendFailures, allowedSendFailures, excess),
		})
	}

	// Nothing was attempted on a shard with leases that should have probed.
	if checked := leases - allowedUnprobed; probes == 0 && sendFailures == 0 && checked > 0 {
		out = append(out, FloorFinding{
			Counter: "acd_probes_sent",
			Value:   0,
			Fatal:   true,
			Why: fmt.Sprintf(
				"%d v4 lease(s) (leases_obtained_v4, not the v4+v6 sum) were obtained by endpoints "+
					"that run RFC 5227's check before the address is used (%d more were declared "+
					"with AllowUnprobedLeases and are not counted here) and not one ARP Probe "+
					"was sent. With the declared population subtracted this is the check having "+
					"stopped working rather than a shard with nothing to look at (#551).",
				checked, allowedUnprobed),
		})
	}

	// A conflict in the log and not the counter is a restart or a dropped event (#524). Measured on the 2.x lane
	// 2026-09-04: two staged conflicts and a counter reset made two red shards, so staged conflicts are subtracted (#882).
	if int32(conflictsInLog) > conflicts+allowedConflicts {
		why := fmt.Sprintf(
			"the log records %d address conflict(s) across the run and the counter shows %d. "+
				"A conflict is a container up on somebody else's address; the log is the record "+
				"that survives a plugin restart, so the higher number is the one to believe "+
				"(#385, #524).",
			conflictsInLog, conflicts)
		if allowedConflicts > 0 {
			why += fmt.Sprintf(
				" %d of them were staged by this shard and declared with AllowStagedConflicts, "+
					"which the counter is allowed to have lost to a plugin restart; the excess is not.",
				allowedConflicts)
		}
		out = append(out, FloorFinding{
			Counter: "address_conflicts",
			Value:   int32(conflictsInLog),
			Fatal:   true,
			Why:     why,
		})
	}

	return out
}
