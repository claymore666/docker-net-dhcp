// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
)

// metricPrefix namespaces every series this plugin exposes.
const metricPrefix = "net_dhcp_"

// metricDef describes one exposed metric family and — crucially — which
// HealthResponse field(s) it renders.
//
// The field names are not decoration. TestMetrics_EveryHealthFieldIsExposed
// walks HealthResponse by reflection and asserts every json tag is claimed
// here, so adding field 46 to HealthResponse without exposing it fails the
// unit suite rather than silently leaving a hole in someone's dashboard.
// That is the whole reason this is a table and not a series of Fprintf
// calls: a hand-kept list is the shape this repo has repeatedly watched rot
// (#542, #636), and the metrics surface is the one place where rot is
// invisible until an alert that should have fired does not.
type metricDef struct {
	// name is the series name without the net_dhcp_ prefix and without
	// the _total suffix that counters get.
	name string
	// counter marks a monotonic series. Counters are exposed as
	// <prefix><name>_total per Prometheus convention; gauges as
	// <prefix><name>.
	counter bool
	// help is the HELP line. It is operator-facing documentation and
	// should say what the number means, not restate the name.
	help string
	// healthy marks a counter that the Healthy expression reads: a
	// non-zero value makes the plugin report itself unhealthy.
	//
	// THIS IS THE DECLARATION. It used to be inferred by reading the
	// English in help, which is how #826 happened: the classifier saw
	// "Healthy-affecting." inside "Not healthy-affecting:" and called a
	// denial an assertion. The fix read the sentence better, and #854
	// found the same defect one axis over -- "Not a healthy-affecting
	// counter" puts a word between the negator and the term and is read
	// as asserting again. A heuristic over prose has now been wrong
	// twice in the same place, so the property stopped being prose.
	//
	// scripts/check-health-contract.sh reads this field. The sentence
	// in help stays for operators and is pinned to this field by
	// TestMetricHelpMatchesHealthyField, which is what stops the two
	// from drifting apart now that only one of them is authoritative.
	healthy bool
	// warn marks a counter the reference table tells an operator to
	// watch or alert on WITHOUT calling it a fault. It is the second
	// axis of the health document's check classification: `healthy`
	// counters become `fail` checks, these become `warn` checks, and a
	// counter with neither is informational and is not a check at all.
	//
	// The rule is the reference row's own words, not taste: the row
	// carries an imperative about THIS counter's value ("alert on it",
	// "Watch it", "worth investigating", "the actionable one"). The
	// classification is also a column of that table, and
	// scripts/check-health-contract.sh reads this declaration against
	// it -- the same reconciliation `healthy` already gets, because a
	// classification stated in two places is the #638 shape one column
	// over.
	//
	// healthy and warn are mutually exclusive; a check is one status.
	warn bool
	// unit is the check's observedUnit (draft section 4.4) and action
	// is its output (section 4.8) when the counter is non-zero. Both
	// are required on a check and meaningless without one, which
	// TestHealthChecks_EveryCheckIsAnnotated holds.
	unit   string
	action string
	// field is the HealthResponse json tag this renders.
	field string
	// v4field and v6field, when set, make this a family-split metric:
	// each names the stored half rendered under the matching family
	// label. field then names the v4+v6 aggregate, which this metric
	// CLAIMS (so the exposure guard counts it as covered) but does not
	// emit as a series of its own — the two labelled series carry it.
	//
	// Both halves are read, never derived. This file used to compute
	// the ipv4 series as aggregate-minus-v6 in a helper called
	// familySplit; #730 removed it. Two independently updated counters
	// combined by SUBTRACTION can yield a value below the previous
	// scrape, which Prometheus reads as a counter reset and repays as a
	// rate spike of the entire accumulated count. Do not reintroduce
	// the arithmetic: if a family series ever needs computing rather
	// than reading, the fix belongs in healthSnapshot, where both
	// halves are loaded once.
	v4field string
	v6field string
	// values maps a STRING field's value to its exposition number.
	// Prometheus has no string type, so a string health field is
	// exposed as an enumeration rather than not at all, and a value
	// this map does not carry is an error rather than a silent zero --
	// a status nobody enumerated must not render as `pass`.
	values map[string]string
}

// metricDefs is the complete exposition table.
//
// Order here is the order on the wire, which makes the golden file a
// readable document rather than a hash. Related metrics sit together for
// the same reason.
func metricDefs() []metricDef {
	return []metricDef{
		// Identity and liveness.
		{name: "health_status", help: "The health document's overall status, ordered so that worse is higher: 0 pass, 1 warn, 2 fail. `> 0` is the alerting expression; `>= 2` is the subset net_dhcp_healthy already carried. Like net_dhcp_healthy it LATCHES for the life of the process -- read net_dhcp_build_info's instance_id to tell a fault this process recorded earlier from a new one.", field: "status", values: map[string]string{statusPass: "0", statusWarn: "1", statusFail: "2"}},
		{name: "healthy", help: "1 when the plugin reports itself healthy, 0 when an operator should look. Mirrors the healthy field of /Plugin.Health.", field: "healthy"},
		{name: "uptime_seconds", help: "Seconds since this plugin process started.", field: "uptime_seconds"},
		{name: "active_endpoints", help: "Endpoints with a live DHCP renewal client.", field: "active_endpoints"},
		{name: "pending_hints", help: "CreateEndpoint hints waiting for their Join.", field: "pending_hints"},
		{name: "sandbox_netns_visible", help: "Sandbox netns entries the plugin can see; -1 means the directory is unreadable and sandbox-liveness answers carry no evidence.", field: "sandbox_netns_visible"},

		// Lease lifecycle. These six carry a family label.
		{name: "leases_obtained", counter: true, help: "Leases obtained from the DHCP server.", field: "leases_obtained", v4field: "leases_obtained_v4", v6field: "leases_obtained_v6"},
		{name: "leases_renewed", counter: true, help: "Lease renewals accepted by the DHCP server.", field: "leases_renewed", v4field: "leases_renewed_v4", v6field: "leases_renewed_v6"},
		{name: "lease_changed", counter: true, warn: true, unit: "renewals", action: "A renewal returned a different address, and docker inspect does not update on a lease change, so its reported address is stale for those containers.", help: "Renewals that came back with a different address than the client held.", field: "lease_changed", v4field: "lease_changed_v4", v6field: "lease_changed_v6"},
		{name: "dhcp_timeouts", counter: true, help: "Acquisitions or renewals that expired without an answer.", field: "dhcp_timeouts", v4field: "dhcp_timeouts_v4", v6field: "dhcp_timeouts_v6"},
		{name: "naks_received", counter: true, help: "DHCPNAKs received from the server.", field: "naks_received", v4field: "naks_received_v4", v6field: "naks_received_v6"},
		{name: "client_stop_failures", counter: true, help: "Renewal clients that did not shut down cleanly when the plugin signalled them. Not a lease release: nothing this plugin runs sends a DHCPRELEASE.", field: "client_stop_failures", v4field: "client_stop_failures_v4", v6field: "client_stop_failures_v6"},

		// No family label: there is no v4 counterpart to measure, so a
		// v4field here would expose a series that is zero by
		// construction rather than by observation (#815).
		{name: "dhcpv6_config_only", counter: true, help: "DHCPv6 information replies received: address-less configuration from a network advertising the RA other-config flag. Counts replies received, not configuration applied.", field: "dhcpv6_config_only"},
		{name: "dhcpv6_not_offered", counter: true, help: "Endpoints started on an IPv6 network whose router advertisement offered no DHCPv6 address (stateless or SLAAC). Not a fault: the network is working as configured and there is no DHCPv6 address on it to be had. The container comes up with IPv4, an IPv6 link-local and DHCPv6 configuration where the segment offers it, and no global IPv6 address from this plugin. Since v1.9.0 the interface is left at accept_ra=2/autoconf=1, so the kernel may still form a SLAAC address if the advertised prefix sets the A flag; that address is not a lease and is not reported in docker inspect. Kept apart from dhcpv6_no_router_advert because that one means no router answered at all.", field: "dhcpv6_not_offered"},
		{name: "dhcpv6_no_router_advert", counter: true, help: "Endpoints started on an IPv6 network where no router advertisement arrived inside the acquisition budget. The endpoint keeps running with no IPv6 address. Usually a missing or misconfigured router rather than a plugin fault, but unlike dhcpv6_not_offered it is not a configuration the operator chose.", field: "dhcpv6_no_router_advert"},
		{name: "ipv6_link_enable_failures", counter: true, help: "Container links the plugin could not administratively enable IPv6 on before starting a DHCPv6 client. The engine disables IPv6 on a sandbox interface whose endpoint has no IPv6 address, so on such a link nothing IPv6 can arrive at all; without this counter that is indistinguishable from a segment that is merely quiet.", field: "ipv6_link_enable_failures"},

		// Server-supplied values the plugin bounds or must evidence (#699).
		{name: "dhcp_routes_applied", counter: true, help: "DHCP option-121 classless static routes handed to Docker. Counts routes, not Joins.", field: "dhcp_routes_applied"},
		{name: "dhcp_default_route_superseded", counter: true, help: "Joins whose option-121 routes cover 0.0.0.0/0 by union rather than by a literal default entry, so container egress follows those next hops even though the reported gateway still names the option-3 router. Legitimate in split-tunnel setups; the point is that it is now visible.", field: "dhcp_default_route_superseded"},
		{name: "mtu_refused", counter: true, help: "Option-26 MTUs outside the range the plugin will apply; the container link keeps the MTU it had.", field: "mtu_refused"},

		// Server selection (#111).
		{name: "dhcp_server_tier_fallbacks", counter: true, help: "Steps down the dhcp_servers ladder: one per preferred entry that did not answer inside its slice of the budget and handed on to the next. One acquisition against three silent preferred servers adds 2, not 1. The only outside signal that a preferred server is silently dead.", field: "dhcp_server_tier_fallbacks"},
		{name: "dhcp_server_policy_exhausted", counter: true, help: "Acquisitions abandoned because no server listed in dhcp_servers answered.", field: "dhcp_server_policy_exhausted"},
		{name: "dhcp_server_policy_timeouts", counter: true, help: "dhcp_timeouts on endpoints whose renewal client is restricted to dhcp_servers.", field: "dhcp_server_policy_timeouts"},

		// Post-restart recovery.
		{name: "recovered_ok", counter: true, help: "Endpoints whose renewal client was rebuilt after a plugin restart.", field: "recovered_ok"},
		{name: "recovery_failed", counter: true, healthy: true, unit: "endpoints", action: "A container that is still running has no lease-renewal client and will lose its address at expiry. Restart it; the plugin log carries the cause.", help: "Post-restart rebuilds that failed for a container that is still running; it runs without lease renewal and loses its IP at expiry. Healthy-affecting.", field: "recovery_failed"},
		{name: "recovery_deferred", counter: true, help: "Recovery walks postponed because the daemon was still starting (#383). Not a fault.", field: "recovery_deferred"},
		{name: "recovery_aborted_container_gone", counter: true, help: "Endpoints skipped during recovery because their container had already exited. Not a fault.", field: "recovery_aborted_container_gone"},
		{name: "recovery_network_gone", counter: true, help: "Networks skipped during recovery because they were removed mid-walk. Not a fault.", field: "recovery_network_gone"},
		{name: "recovery_fingerprints_skipped", counter: true, help: "Endpoints recovery adopted but could not describe, because the container inspect gave no hostname. Not healthy-affecting: they keep their renewal client and lose only address stability across their next restart.", field: "recovery_fingerprints_skipped"},
		{name: "recovery_already_managed", counter: true, help: "Endpoints a recovery walk left alone because a Join had already claimed them. Not a fault; the only outward evidence of recovery racing a Join.", field: "recovery_already_managed"},

		// Join / attach.
		{name: "join_start_failures", counter: true, healthy: true, unit: "endpoints", action: "A container that is still running got its initial lease but no renewal client. Restart it; the plugin log carries the cause.", help: "Joins whose DHCP client failed to start, leaving a running container without lease renewal. Healthy-affecting.", field: "join_start_failures"},
		{name: "join_aborted_container_gone", counter: true, help: "Joins abandoned because the container disappeared mid-attach. Not a fault.", field: "join_aborted_container_gone"},
		{name: "join_aborted_no_container", counter: true, help: "Joins abandoned because no container was ever found for the endpoint. Not a fault.", field: "join_aborted_no_container"},
		{name: "join_aborted_endpoint_left", counter: true, help: "Joins abandoned because a Leave arrived while the attach was in flight. Not a fault.", field: "join_aborted_endpoint_left"},
		{name: "join_attach_slow", counter: true, help: "Attaches that outran their expected window and needed the daemon-busy grace.", field: "join_attach_slow"},
		{name: "displaced_stops", counter: true, help: "DHCP managers stopped because a Join displaced them. Counts the intent to stop; it is not evidence the client went away.", field: "displaced_stops"},
		{name: "restart_link_up_waited", counter: true, help: "Container restarts that had to wait for the interface to come back up.", field: "restart_link_up_waited"},
		{name: "restart_link_up_timeouts", counter: true, warn: true, unit: "restarts", action: "A departing link held its address past the wait budget, so docker restart failed with \"address already in use\". Worth investigating: any non-zero value means a restart was refused.", help: "Container restarts where the interface never came up inside the wait.", field: "restart_link_up_timeouts"},

		// RFC 5227 address conflict detection (#524, D12, D23).
		{name: "address_conflicts", counter: true, healthy: true, unit: "addresses", action: "A leased address was found in use by another device on the segment. Look for a statically configured host inside the DHCP pool.", help: "Leased addresses RFC 5227 found already in use by another host, over the whole life of the lease: section 2.1's probes before the address is used and section 2.4's listener afterwards. Healthy-affecting. Moves in conflict_check=wait and =async. In =off the client neither probes nor listens, so this moves only for a conflict reported to the client from outside it, which no code path in this plugin does today.", field: "address_conflicts"},
		{name: "acd_probes_sent", counter: true, help: "RFC 5227 section 2.1.1 ARP Probes sent. READ THIS BEFORE BELIEVING address_conflicts IS ZERO: zero here over a running plugin means no address was ever checked, which is not the same reading as a clean segment (#524). Moves in conflict_check=wait and =async, never in =off.", field: "acd_probes_sent"},
		{name: "acd_announcements_sent", counter: true, help: "RFC 5227 section 2.3 ARP Announcements sent, two per address that passed the probe. Moves in conflict_check=wait and =async, never in =off. Read against acd_probes_sent: probes climbing with no announcements means addresses are being checked and none is coming back clean.", field: "acd_announcements_sent"},
		{name: "acd_conflicts_detected", counter: true, help: "Conflicts the DHCP library itself counted. It is the same population as address_conflicts, counted inside the state machine rather than from the events it emitted, and the two are expected to be equal; a difference is a defect in the plugin's event handling, not a property of the segment. Its =off rule is address_conflicts's, not a different one: with no probe and no listener the only thing that can move either counter is a conflict reported to the client from outside it, which no code path in this plugin does today.", field: "acd_conflicts_detected"},
		{name: "acd_arp_send_failures", counter: true, warn: true, unit: "frames", action: "ARP Probes or Announcements the socket refused. A probe that never went out proves nothing about the address, so address_conflicts=0 stops meaning the segment is clean.", help: "ARP Probes and Announcements the socket refused. Not healthy-affecting: a refused send is not itself a conflict, but a probe that never went out proves nothing about the address, so a rise turns \"no conflict found\" into \"the question was not asked\". Moves in conflict_check=wait and =async, never in =off.", field: "acd_arp_send_failures"},
		{name: "acd_resumed_unchecked", counter: true, warn: true, unit: "endpoints", action: "An endpoint was resumed from a record whose RFC 5227 section 2.1 check had not finished, so it held its address with no completed check behind it until the resumed client re-checked it on the INIT-REBOOT acknowledgement.", help: "Endpoints picked up after a plugin restart from a durable record whose RFC 5227 section 2.1 check had not completed (D23). The resumed client re-runs section 2.1 on its INIT-REBOOT acknowledgement whatever the record said, so the window closes on its own; this counts how often it opened. Not healthy-affecting: the container keeps its address and the check is re-run.", field: "acd_resumed_unchecked"},

		// Orphaned leases (#370).

		// Parent link waits.
		{name: "parent_link_waits", counter: true, help: "Endpoint creations that waited for their parent interface to appear.", field: "parent_link_waits"},
		{name: "parent_link_wait_timeouts", counter: true, warn: true, unit: "operations", action: "Something held a parent interface far longer than a DHCP round trip, and container starts on that NIC were refused as a result.", help: "Endpoint creations where the parent interface never appeared inside the wait.", field: "parent_link_wait_timeouts"},

		// Persistence.
		{name: "tombstone_write_failures", counter: true, healthy: true, unit: "writes", action: "A tombstone could not be written or re-read, so some container will pick a fresh MAC and address on its next restart. Check STATE_DIR for space and for read errors.", help: "Tombstone writes that failed, so the next restart of that container picks a new MAC and address. Healthy-affecting.", field: "tombstone_write_failures"},
		{name: "tombstone_quarantines", counter: true, healthy: true, unit: "files", action: "The tombstone file was unparseable and was moved aside, taking every live tombstone on the host with it. Every container restarting in the next TTL window comes back with a new MAC and address.", help: "Times the tombstone file was found unparseable and moved aside as tombstones.json.corrupt-<ts>; every live tombstone on the host was lost with it, so containers restarting in the next TTL window come back with new MACs and addresses. Healthy-affecting.", field: "tombstone_quarantines"},
		{name: "tombstones_consumed", counter: true, help: "Tombstones read back to preserve a container's MAC and address across a restart.", field: "tombstones_consumed"},
		{name: "unsafe_hostnames_rejected", counter: true, help: "Container hostnames dropped before reaching the DHCP request because they carried a control character. A legitimate hostname never does, so any rise is deliberate (#692).", field: "unsafe_hostnames_rejected"},
		{name: "unsafe_option_values_dropped", counter: true, help: "Server-chosen DHCP string values refused before use because they carried a control character, plus option-15 domains truncated at their first space. The DHCP library validates domain-typed options; string-typed ones can carry anything the server put on the wire (#703, #704).", field: "unsafe_option_values_dropped"},
		{name: "network_options_rejected", counter: true, help: "Endpoint operations that met a network's stored options and would not act on them as written: an interface name the kernel would not accept, or a mode this plugin does not implement. DeleteEndpoint counts without refusing, so a rise does not mean nothing was torn down. Not healthy-affecting: refusing is the safe outcome and the operation already fails visibly to Docker. A rise means options persisted before name validation existed, or a hand-edited state directory (#727).", field: "network_options_rejected"},
		{name: "dns_propagation_pid_mismatches", counter: true, help: "DNS propagations refused because the container PID resolved through Docker no longer belonged to that container. The plugin shares the host PID namespace, so each one is a resolv.conf write that would otherwise have landed in an unrelated host process (#688).", field: "dns_propagation_pid_mismatches"},
		{name: "netns_pid_mismatches", counter: true, help: "Sandbox network-namespace opens refused because the container PID resolved through Docker no longer belonged to that container. Each one is a netlink handle, and a root DHCP client, that would otherwise have been bound to an unrelated host process's network namespace.", field: "netns_pid_mismatches"},
		{name: "sandbox_key_entries", counter: true, help: "Container network namespaces entered through the sandbox key the daemon publishes at /var/run/docker/netns/<key>. This is the denominator for sandbox_pid_fallbacks: zero fallbacks with zero entries here means nothing was opened, not that the key route works.", field: "sandbox_key_entries"},
		{name: "sandbox_key_entry_failures", counter: true, help: "Attempts to enter a container network namespace through the sandbox key that were refused. EXPECTED, once per container attach, on a stock engine: the daemon bind-mounts each sandbox netns after the plugin's own /var/run/docker mount was taken, so the key resolves to the placeholder file underneath and the container PID route carries the attach. Nothing is degraded and no action is indicated. Read sandbox_key_absent, sandbox_key_not_permitted, sandbox_key_not_a_namespace, sandbox_key_wrong_ns_type and sandbox_key_unavailable to see which refusal this was; they sum to this counter.", field: "sandbox_key_entry_failures"},
		{name: "sandbox_key_absent", counter: true, help: "Key-route refusals because neither the Join request nor the container inspect carried a sandbox key, so the key route was never attempted and the container PID route carries the attach. Not observed on any measured host; split out of sandbox_key_not_permitted in 2.0-alpha.1, where an absent key was indistinguishable from the --exec-root case whose remedy is a change to this plugin. An arm of sandbox_key_entry_failures.", field: "sandbox_key_absent"},
		{name: "sandbox_key_not_permitted", counter: true, help: "Key-route refusals because a non-empty sandbox key did not name an entry of a directory this plugin accepts (/var/run/docker/netns, /run/docker/netns). This is the arm that is NOT expected: a daemon started with a non-default --exec-root publishes keys elsewhere, and the remedy is a change to this plugin rather than to the host. An arm of sandbox_key_entry_failures.", field: "sandbox_key_not_permitted"},
		{name: "sandbox_key_not_a_namespace", counter: true, help: "Key-route refusals because the entry opened and was not a namespace — the placeholder file libnetwork creates before it bind-mounts the sandbox netns over it. EXPECTED, once per container attach, on a stock engine, and it is the evidence for SECURITY.md's reason that the host PID namespace and CAP_SYS_PTRACE stay. An arm of sandbox_key_entry_failures.", field: "sandbox_key_not_a_namespace"},
		{name: "sandbox_key_wrong_ns_type", counter: true, help: "Key-route refusals because the entry was a namespace of some other type. Not observed on any measured host; published so that \"not observed\" stays a statement a reader can check. An arm of sandbox_key_entry_failures.", field: "sandbox_key_wrong_ns_type"},
		{name: "sandbox_key_unavailable", counter: true, help: "Key-route refusals that were none of the three named arms — the entry never became openable inside the attach budget, or its directory could not be read. The residual arm, so that the four always sum to sandbox_key_entry_failures rather than nearly doing so.", field: "sandbox_key_unavailable"},
		{name: "sandbox_pid_fallbacks", counter: true, help: "Endpoints whose network namespace was entered through /proc/<pid>/ns/net after the sandbox key route was refused. That route is why the manifest asks for the host PID namespace and CAP_SYS_PTRACE; a zero here across a host's whole uptime, with sandbox_key_entries non-zero, is the evidence that it was not needed.", field: "sandbox_pid_fallbacks"},
		{name: "docker_api_non_get_refusals", counter: true, help: "Requests to the Docker API refused before they were sent because their method was not GET. The plugin's whole Docker surface is three read calls, so this stays zero unless code in this process tried to write to the daemon; the socket mount is the grant that makes such a write equivalent to root on the host (#691).", field: "docker_api_non_get_refusals"},
		{name: "ledger_write_failures", counter: true, warn: true, unit: "writes", action: "Lease-ledger appends are failing, so the audit_log record of who held which address is incomplete. Forensics only; networking is unaffected.", help: "Lease-ledger writes that failed.", field: "ledger_write_failures"},
	}
}

// metricLabelOnlyFields are HealthResponse fields deliberately exposed as
// a LABEL rather than as a series of their own.
//
// instance_id is the whole point of build_info: a counter reset is
// invisible in a time series unless something in the series identity
// changes with the process. Carrying the id as a label means a plugin
// restart appears as a new series, which Prometheus already knows how to
// handle, instead of as a counter that silently rewound. That is the same
// failure #405 found inside our own integration suite, where counters
// reset three times per run and nothing noticed.
var metricLabelOnlyFields = map[string]string{
	"instance_id": "build_info",
	"version":     "build_info",
	"commit":      "build_info",
	"library":     "build_info",
}

// metricNotExposedFields are HealthResponse fields deliberately absent
// from /metrics, each with the reason.
//
// AN ESCAPE HATCH WITH A COST, and it is here because the alternative
// is worse. healthFieldsByTag renders scalars; a map or a slice falls
// through its default arm, which means a structured field added to
// HealthResponse would be missing from that map, unclaimed by
// metricDefs, and — before this list existed —
// TestMetrics_EveryHealthFieldIsExposed would have failed with no way
// to say "deliberately". Saying it in a table with a reason is a
// decision a reviewer can read; skipping non-scalars in
// healthFieldsByTag would have been the same decision, taken silently,
// for every future field at once.
//
// The reason is length-checked by the same test, so an entry cannot be
// added with an empty one.
var metricNotExposedFields = map[string]string{
	"checks":    "one series per check would restate net_dhcp_health_status and the healthy-affecting counters, which are already exposed; the check's observedValue IS the counter's series",
	"endpoints": "a series per container is a cardinality decision this row does not take; the per-endpoint lease gauge is its own piece of work (O-5)",
}

// writeExposition renders one health snapshot as Prometheus text format
// (version 0.0.4) to w.
//
// Deliberately hand-rolled rather than pulling in prometheus/client_golang.
// go.mod carries 8 direct dependencies and this plugin runs with
// CAP_NET_ADMIN, CAP_SYS_ADMIN and CAP_SYS_PTRACE on the host network
// namespace, so the bar for a new direct dependency is high and the
// surface being bought here is one text renderer over 45 integers. The
// cost of that choice is that conformance is ours to hold, which is what
// the golden file and the escaping test are for.
func writeExposition(w io.Writer, h HealthResponse) error {
	return writeExpositionWith(w, h, metricDefs())
}

// writeExpositionWith is writeExposition with the table injected, so the
// error paths below are reachable from a test without a broken table
// having to be committed to reach them.
func writeExpositionWith(w io.Writer, h HealthResponse, defs []metricDef) error {
	byTag := healthFieldsByTag(h)
	var b strings.Builder

	// build_info first: it is the series a reader needs to interpret
	// every counter below it.
	b.WriteString("# HELP " + metricPrefix + "build_info Plugin build and instance identity. version is the release tag (dev outside a release), commit the git revision it was built from, library the revision of the in-tree DHCP library; none of the three is ever empty, and `unknown` means the build did not carry it. The instance_id label changes on every plugin restart, so a counter reset appears as a new series rather than as a rewind.\n")
	b.WriteString("# TYPE " + metricPrefix + "build_info gauge\n")
	b.WriteString(metricPrefix + `build_info{instance_id="` + escapeLabelValue(h.InstanceID) +
		`",version="` + escapeLabelValue(h.Version) +
		`",commit="` + escapeLabelValue(h.Commit) +
		`",library="` + escapeLabelValue(h.Library) + "\"} 1\n")

	for _, d := range defs {
		name := metricPrefix + d.name
		kind := "gauge"
		if d.counter {
			name += "_total"
			kind = "counter"
		}
		b.WriteString("\n# HELP " + name + " " + escapeHelp(d.help) + "\n")
		b.WriteString("# TYPE " + name + " " + kind + "\n")

		if d.v6field == "" {
			v, ok := byTag[d.field]
			if !ok {
				return fmt.Errorf("metric %q names unknown health field %q", d.name, d.field)
			}
			if d.values != nil {
				n, known := d.values[v]
				if !known {
					return fmt.Errorf("metric %q has no number for health field %q value %q", d.name, d.field, v)
				}
				v = n
			}
			b.WriteString(name + " " + v + "\n")
			continue
		}

		if _, ok := byTag[d.field]; !ok {
			return fmt.Errorf("metric %q names unknown health field %q", d.name, d.field)
		}
		v4, ok := byTag[d.v4field]
		if !ok {
			return fmt.Errorf("metric %q names unknown health field %q", d.name, d.v4field)
		}
		v6, ok := byTag[d.v6field]
		if !ok {
			return fmt.Errorf("metric %q names unknown health field %q", d.name, d.v6field)
		}
		b.WriteString(name + `{family="ipv4"} ` + v4 + "\n")
		b.WriteString(name + `{family="ipv6"} ` + v6 + "\n")
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// healthFieldsByTag renders every HealthResponse field to its exposition
// value, keyed by json tag.
//
// Reflection rather than a switch over 45 named fields, for the same
// reason metricDefs is a table: the alternative is a second list that has
// to be edited in lockstep with the struct, and nothing would fail when
// somebody forgot. A field whose type is not exposable is an error rather
// than a skip — silently dropping it would produce exactly the invisible
// hole this design exists to prevent.
//
// Booleans render as 1/0 because Prometheus has no boolean type; floats
// use 'g' with full precision so a value round-trips rather than being
// truncated to a scrape-time approximation.
func healthFieldsByTag(h HealthResponse) map[string]string {
	out := make(map[string]string)
	v := reflect.ValueOf(h)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		tag = strings.Split(tag, ",")[0]
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Bool:
			if f.Bool() {
				out[tag] = "1"
			} else {
				out[tag] = "0"
			}
		case reflect.String:
			out[tag] = f.String()
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			out[tag] = strconv.FormatInt(f.Int(), 10)
		case reflect.Float32, reflect.Float64:
			out[tag] = strconv.FormatFloat(f.Float(), 'g', -1, 64)
		default:
			// Unreachable for the current struct, and a compile-time
			// impossibility to assert. TestMetrics_EveryHealthFieldIsExposed
			// fails loudly if a field ever lands here, because the tag
			// will be missing from this map.
			continue
		}
	}
	return out
}

// escapeLabelValue applies the Prometheus text-format escaping rules for
// label values: backslash, double quote and newline.
func escapeLabelValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// escapeHelp applies the (different, smaller) escaping rules for HELP
// text: backslash and newline only. A double quote is legal there and
// must NOT be escaped, which is why this is not escapeLabelValue.
func escapeHelp(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(s)
}

// apiMetrics serves the exposition over HTTP.
func (p *Plugin) apiMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := writeExposition(w, p.healthSnapshot()); err != nil {
		http.Error(w, "failed to render metrics: "+err.Error(), http.StatusInternalServerError)
	}
}
