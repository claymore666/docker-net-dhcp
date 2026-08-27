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
}

// metricDefs is the complete exposition table.
//
// Order here is the order on the wire, which makes the golden file a
// readable document rather than a hash. Related metrics sit together for
// the same reason.
func metricDefs() []metricDef {
	return []metricDef{
		// Identity and liveness.
		{name: "healthy", help: "1 when the plugin reports itself healthy, 0 when an operator should look. Mirrors the healthy field of /Plugin.Health.", field: "healthy"},
		{name: "uptime_seconds", help: "Seconds since this plugin process started.", field: "uptime_seconds"},
		{name: "active_endpoints", help: "Endpoints with a live DHCP renewal client.", field: "active_endpoints"},
		{name: "pending_hints", help: "CreateEndpoint hints waiting for their Join.", field: "pending_hints"},
		{name: "sandbox_netns_visible", help: "Sandbox netns entries the plugin can see; -1 means the directory is unreadable and sandbox-liveness answers carry no evidence.", field: "sandbox_netns_visible"},

		// Lease lifecycle. These six carry a family label.
		{name: "leases_obtained", counter: true, help: "Leases obtained from the DHCP server.", field: "leases_obtained", v4field: "leases_obtained_v4", v6field: "leases_obtained_v6"},
		{name: "leases_renewed", counter: true, help: "Lease renewals accepted by the DHCP server.", field: "leases_renewed", v4field: "leases_renewed_v4", v6field: "leases_renewed_v6"},
		{name: "lease_changed", counter: true, help: "Renewals that came back with a different address than the client held.", field: "lease_changed", v4field: "lease_changed_v4", v6field: "lease_changed_v6"},
		{name: "dhcp_timeouts", counter: true, help: "Acquisitions or renewals that expired without an answer.", field: "dhcp_timeouts", v4field: "dhcp_timeouts_v4", v6field: "dhcp_timeouts_v6"},
		{name: "naks_received", counter: true, help: "DHCPNAKs received from the server.", field: "naks_received", v4field: "naks_received_v4", v6field: "naks_received_v6"},
		{name: "lease_release_failures", counter: true, help: "Leases whose release did not complete cleanly at shutdown, leaving the address held until it expires.", field: "lease_release_failures", v4field: "lease_release_failures_v4", v6field: "lease_release_failures_v6"},

		// Server-supplied values the plugin bounds or must evidence (#699).
		{name: "dhcp_routes_applied", counter: true, help: "DHCP option-121 classless static routes handed to Docker. Counts routes, not Joins.", field: "dhcp_routes_applied"},
		{name: "dhcp_default_route_superseded", counter: true, help: "Joins whose option-121 routes cover 0.0.0.0/0 by union rather than by a literal default entry, so container egress follows those next hops even though the reported gateway still names the option-3 router. Legitimate in split-tunnel setups; the point is that it is now visible.", field: "dhcp_default_route_superseded"},
		{name: "lease_time_clamped", counter: true, help: "Option-51 lease lifetimes cut down before use as the outage watchdog's deadline. The reported lease time is unchanged. Non-zero means a server granted a container a lease long enough to switch silent-lapse detection off.", field: "lease_time_clamped"},
		{name: "mtu_refused", counter: true, help: "Option-26 MTUs outside the range the plugin will apply; the container link keeps the MTU it had.", field: "mtu_refused"},

		// Server selection (#111).
		{name: "dhcp_server_tier_fallbacks", counter: true, help: "Steps down the dhcp_servers ladder: one per preferred entry that did not answer inside its slice of the budget and handed on to the next. One acquisition against three silent preferred servers adds 2, not 1. The only outside signal that a preferred server is silently dead.", field: "dhcp_server_tier_fallbacks"},
		{name: "dhcp_server_policy_exhausted", counter: true, help: "Acquisitions abandoned because no server listed in dhcp_servers answered.", field: "dhcp_server_policy_exhausted"},
		{name: "dhcp_server_policy_timeouts", counter: true, help: "dhcp_timeouts on endpoints whose renewal client is restricted to dhcp_servers.", field: "dhcp_server_policy_timeouts"},

		// Post-restart recovery.
		{name: "recovered_ok", counter: true, help: "Endpoints whose renewal client was rebuilt after a plugin restart.", field: "recovered_ok"},
		{name: "recovery_failed", counter: true, help: "Post-restart rebuilds that failed for a container that is still running; it runs without lease renewal and loses its IP at expiry. Healthy-affecting.", field: "recovery_failed"},
		{name: "recovery_deferred", counter: true, help: "Recovery walks postponed because the daemon was still starting (#383). Not a fault.", field: "recovery_deferred"},
		{name: "recovery_aborted_container_gone", counter: true, help: "Endpoints skipped during recovery because their container had already exited. Not a fault.", field: "recovery_aborted_container_gone"},
		{name: "recovery_network_gone", counter: true, help: "Networks skipped during recovery because they were removed mid-walk. Not a fault.", field: "recovery_network_gone"},
		{name: "recovery_fingerprints_skipped", counter: true, help: "Endpoints recovery adopted but could not describe, because the container inspect gave no hostname. Not healthy-affecting: they keep their renewal client and lose only address stability across their next restart.", field: "recovery_fingerprints_skipped"},
		{name: "recovery_already_managed", counter: true, help: "Endpoints a recovery walk left alone because a Join had already claimed them. Not a fault; the only outward evidence of recovery racing a Join.", field: "recovery_already_managed"},

		// Join / attach.
		{name: "join_start_failures", counter: true, help: "Joins whose DHCP client failed to start, leaving a running container without lease renewal. Healthy-affecting.", field: "join_start_failures"},
		{name: "join_aborted_container_gone", counter: true, help: "Joins abandoned because the container disappeared mid-attach. Not a fault.", field: "join_aborted_container_gone"},
		{name: "join_aborted_no_container", counter: true, help: "Joins abandoned because no container was ever found for the endpoint. Not a fault.", field: "join_aborted_no_container"},
		{name: "join_aborted_endpoint_left", counter: true, help: "Joins abandoned because a Leave arrived while the attach was in flight. Not a fault.", field: "join_aborted_endpoint_left"},
		{name: "join_attach_slow", counter: true, help: "Attaches that outran their expected window and needed the daemon-busy grace.", field: "join_attach_slow"},
		{name: "displaced_stops", counter: true, help: "DHCP managers stopped because a Join displaced them. Counts the intent to stop; it is not evidence the client went away.", field: "displaced_stops"},
		{name: "restart_link_up_waited", counter: true, help: "Container restarts that had to wait for the interface to come back up.", field: "restart_link_up_waited"},
		{name: "restart_link_up_timeouts", counter: true, help: "Container restarts where the interface never came up inside the wait.", field: "restart_link_up_timeouts"},

		// Address conflict detection (#524).
		{name: "address_conflict_probes", counter: true, help: "Conflict probes that reached a verdict. Read this before believing address_conflicts is zero: zero here means the detector never ran.", field: "address_conflict_probes"},
		{name: "address_conflicts", counter: true, help: "Leased addresses found already in use by another host. Healthy-affecting.", field: "address_conflicts"},
		{name: "conflict_probe_failures", counter: true, help: "Conflict probes that could not reach a verdict.", field: "conflict_probe_failures"},
		{name: "conflict_probe_stale_routes", counter: true, help: "Leftover probe routes reclaimed from a probe cut short before it cleaned up.", field: "conflict_probe_stale_routes"},
		{name: "conflict_probe_stale_addrs", counter: true, help: "Leftover borrowed probe SOURCE addresses reclaimed from the parent NIC. The sibling conflict_probe_stale_routes covers the leftover route, which is recognisable because its destination is the probed address; the source address has random octets by design (#575) so nothing ever collided with it and it accumulated on the operator's NIC, one per stop-inside-the-probe-window. Not healthy-affecting: the probe went on to run (#723).", field: "conflict_probe_stale_addrs"},

		// Orphaned leases (#370).
		{name: "orphaned_leases_released", counter: true, help: "Addresses reclaimed for a container that exited before its renewal client could attach. One per address.", field: "orphaned_leases_released"},
		{name: "orphaned_lease_release_failures", counter: true, help: "Orphaned-lease reclaims that failed, leaving the address held until it expires.", field: "orphaned_lease_release_failures"},
		{name: "orphan_releases_suppressed", counter: true, help: "Orphaned-lease reclaims skipped because a tombstone already reserved the address for a restart, so the lease is kept rather than handed back. Tracks the container-restart rate on this host.", field: "orphan_releases_suppressed"},
		{name: "tombstones_suppressed", counter: true, help: "Tombstones not written because the orphaned-lease reclaim had already handed the lease back, so that container restarts onto a new MAC and address instead of its own. Restart stability was lost for that container.", field: "tombstones_suppressed"},

		// Parent link waits.
		{name: "parent_link_waits", counter: true, help: "Endpoint creations that waited for their parent interface to appear.", field: "parent_link_waits"},
		{name: "parent_link_wait_timeouts", counter: true, help: "Endpoint creations where the parent interface never appeared inside the wait.", field: "parent_link_wait_timeouts"},

		// Persistence.
		{name: "tombstone_write_failures", counter: true, help: "Tombstone writes that failed, so the next restart of that container picks a new MAC and address. Healthy-affecting.", field: "tombstone_write_failures"},
		{name: "tombstone_quarantines", counter: true, help: "Times the tombstone file was found unparseable and moved aside as tombstones.json.corrupt-<ts>; every live tombstone on the host was lost with it, so containers restarting in the next TTL window come back with new MACs and addresses. Healthy-affecting.", field: "tombstone_quarantines"},
		{name: "tombstones_consumed", counter: true, help: "Tombstones read back to preserve a container's MAC and address across a restart.", field: "tombstones_consumed"},
		{name: "unsafe_hostnames_rejected", counter: true, help: "Container hostnames dropped before reaching the DHCP client config because they carried a control character. A legitimate hostname never does, so any rise is deliberate (#692).", field: "unsafe_hostnames_rejected"},
		{name: "unsafe_option_values_dropped", counter: true, help: "Server-chosen DHCP string values refused before use because they carried a control character, plus option-15 domains truncated at their first space. dhcpcd validates only its dname-typed options; the string-typed ones pass newlines through (#703, #704).", field: "unsafe_option_values_dropped"},
		{name: "network_options_rejected", counter: true, help: "Endpoint operations that met a network's stored options and would not act on them as written: an interface name the kernel would not accept, or a mode this plugin does not implement. DeleteEndpoint counts without refusing, so a rise does not mean nothing was torn down. Not healthy-affecting: refusing is the safe outcome and the operation already fails visibly to Docker. A rise means options persisted before name validation existed, or a hand-edited state directory (#727).", field: "network_options_rejected"},
		{name: "dns_propagation_pid_mismatches", counter: true, help: "DNS propagations refused because the container PID resolved through Docker no longer belonged to that container. The plugin shares the host PID namespace, so each one is a resolv.conf write that would otherwise have landed in an unrelated host process (#688).", field: "dns_propagation_pid_mismatches"},
		{name: "netns_pid_mismatches", counter: true, help: "Sandbox network-namespace opens refused because the container PID resolved through Docker no longer belonged to that container. Each one is a netlink handle, and a root dhcpcd, that would otherwise have been bound to an unrelated host process's network namespace.", field: "netns_pid_mismatches"},
		{name: "directives_refused", counter: true, help: "dhcpcd directives dropped before being written because their value carried a control character. dhcpcd.conf has no quoting, so such a value would become a second directive; dropping is correct, but the operator hostname, vendor class or client ID it came from then never reaches the DHCP server and nothing else says so (#780).", field: "directives_refused"},
		{name: "mount_prep_failures", counter: true, help: "Commands in a DHCP client per-client mount-namespace preparation that failed. The chain continues deliberately and dhcpcd starts anyway, so a rise is invisible in behaviour until two containers whose interface is the default eth0 collide on the dhcpcd control socket and the second client stops renewing. Counts commands, not clients: one client failing three of four steps adds 3 (#780).", field: "mount_prep_failures"},
		{name: "ledger_write_failures", counter: true, help: "Lease-ledger writes that failed.", field: "ledger_write_failures"},
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
	b.WriteString("# HELP " + metricPrefix + "build_info Plugin instance identity. The instance_id label changes on every plugin restart, so a counter reset appears as a new series rather than as a rewind.\n")
	b.WriteString("# TYPE " + metricPrefix + "build_info gauge\n")
	b.WriteString(metricPrefix + `build_info{instance_id="` + escapeLabelValue(h.InstanceID) + "\"} 1\n")

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
