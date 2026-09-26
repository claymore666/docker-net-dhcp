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

const metricPrefix = "net_dhcp_"

// metricDef is one metric family and the HealthResponse fields it renders, which
// TestMetrics_EveryHealthFieldIsExposed checks (#542, #636).
type metricDef struct {
	name    string
	counter bool
	help    string
	// healthy marks a counter whose non-zero value makes the plugin unhealthy, declared here and no longer read from
	// help (#826, #854).
	healthy bool
	// warn marks a counter the reference table says to watch without calling it a fault; it excludes healthy (#638).
	warn   bool
	unit   string
	action string
	field  string
	// v4field and v6field name the stored halves of a family-split metric, both read: subtracting one from the
	// aggregate can go below the last scrape, which Prometheus reads as a counter reset (#730).
	v4field string
	v6field string
	values  map[string]string
}

func metricDefs() []metricDef {
	return []metricDef{
		{name: "health_status", help: "The health document's overall status, ordered so that worse is higher: 0 pass, 1 warn, 2 fail. `> 0` is the alerting expression; `>= 2` is the subset net_dhcp_healthy already carried. Like net_dhcp_healthy it LATCHES for the life of the process -- read net_dhcp_build_info's instance_id to tell a fault this process recorded earlier from a new one.", field: "status", values: map[string]string{statusPass: "0", statusWarn: "1", statusFail: "2"}},
		{name: "healthy", help: "1 when the plugin reports itself healthy, 0 when an operator should look. Mirrors the healthy field of /Plugin.Health.", field: "healthy"},
		{name: "uptime_seconds", help: "Seconds since this plugin process started.", field: "uptime_seconds"},
		{name: "active_endpoints", help: "Endpoints with a live DHCP renewal client.", field: "active_endpoints"},
		{name: "link_local_endpoints", help: "Endpoints on an RFC 3927 169.254/16 address because no DHCPv4 lease arrived in time; each moves to a lease when one does.", field: "link_local_endpoints"},
		{name: "pending_hints", help: "CreateEndpoint hints waiting for their Join.", field: "pending_hints"},
		{name: "sandbox_netns_visible", help: "Sandbox netns entries the plugin can see; -1 means the directory is unreadable and sandbox-liveness answers carry no evidence.", field: "sandbox_netns_visible"},
		{name: "sandbox_netns_propagation", help: "Whether a mount the daemon makes under the sandbox netns directory after this process started can reach it: 1 linked, 0 private, -1 unreadable or uncovered. Answered before the directory exists, from the mount covering its parent. A 0 means every attach takes the container PID route, a 1 means every attach takes the sandbox key route, and both are ordinary.", field: "sandbox_netns_propagation"},
		{name: "sandbox_netns_init_mounts", help: "Sandbox netns mounts in PID 1's mount table: -2 PID 1 shares this process's mount namespace, -1 unreadable, N otherwise. Read it against sandbox_netns_visible.", field: "sandbox_netns_init_mounts"},

		{name: "leases_obtained", counter: true, help: "Leases obtained from the DHCP server.", field: "leases_obtained", v4field: "leases_obtained_v4", v6field: "leases_obtained_v6"},
		{name: "leases_renewed", counter: true, help: "Lease renewals accepted by the DHCP server.", field: "leases_renewed", v4field: "leases_renewed_v4", v6field: "leases_renewed_v6"},
		{name: "lease_changed", counter: true, warn: true, unit: "renewals", action: "A renewal returned a different address, and docker inspect does not update on a lease change, so its reported address is stale for those containers.", help: "Renewals that came back with a different address than the client held.", field: "lease_changed", v4field: "lease_changed_v4", v6field: "lease_changed_v6"},
		{name: "renewals_unanswered", counter: true, help: "Renewal requests the server did not answer, one per request, counted while the client is still running. Read it against leases_renewed: renewals completing with this flat is a healthy lease, and this climbing with leases_renewed flat is a DHCP server that has gone quiet. It moves at the first retransmission, and RFC 2131 section 4.4.5 puts that wait at half the time left to T2 with 60 seconds as a floor under it, so it is a minute on a short lease and about 4.5 hours on the 24-hour lease #940 was reported from, where dhcp_timeouts first moves for a held lease only when that lease ends. The request currently in flight is not counted, so a client that has sent N requests into silence reports N-1.", field: "renewals_unanswered", v4field: "renewals_unanswered_v4", v6field: "renewals_unanswered_v6"},
		{name: "dhcp_timeouts", counter: true, help: "Acquisitions or renewals that expired without an answer.", field: "dhcp_timeouts", v4field: "dhcp_timeouts_v4", v6field: "dhcp_timeouts_v6"},
		{name: "naks_received", counter: true, help: "DHCPNAKs received from the server.", field: "naks_received", v4field: "naks_received_v4", v6field: "naks_received_v6"},
		{name: "client_stop_failures", counter: true, help: "Renewal clients that did not shut down cleanly when the plugin signalled them. Not a lease release: whether a lease goes back at teardown is release_lease's question, and this counter is about the client's own shutdown either way.", field: "client_stop_failures", v4field: "client_stop_failures_v4", v6field: "client_stop_failures_v6"},
		{name: "releases_sent", counter: true, help: "Leases handed back to the server: a DHCPRELEASE (RFC 2131 section 4.4.6) or a DHCPv6 Release (RFC 9915 section 18.2.7) that left this host. Zero on a network that does not set release_lease, which is the default. Counted from the send and not from the decision to release, so it does not move for a release the host could not put on the wire.", field: "releases_sent", v4field: "releases_sent_v4", v6field: "releases_sent_v6"},
		{name: "releases_reclaimed", counter: true, help: "Held addresses a running container is using again at the end of the restart window on a release_lease=on_remove network, so the record was closed and nothing was sent. Narrower than nothing-was-sent: an address stopped a second time and an acquisition in flight under the same key also send nothing and are not counted here. Zero on release_lease=never and on release_lease=on_stop, which have no window. It is the option's quiet half: with releases_sent climbing and this flat, nothing is restarting inside the window, and with this climbing the window is doing what it exists for.", field: "releases_reclaimed", v4field: "releases_reclaimed_v4", v6field: "releases_reclaimed_v6"},
		{name: "release_failures", counter: true, warn: true, unit: "releases", action: "A lease this network asked to hand back did not go on the wire, so that address stays leased until it expires on the server. Read it against releases_sent, and read the plugin log beside it for which of the reasons it was.", help: "Attempts to hand a lease back that put no message on the wire: no lease record for the endpoint, no address or no server on the record, a record the sender refuses, no address on the parent to send from, the send itself failing, or a DHCPv6 address that could not be taken off the link first (RFC 9915 section 18.2.7 requires that before the exchange). The address is left to expire on the server's clock, which is what a release_lease=never network does on every teardown.", field: "release_failures", v4field: "release_failures_v4", v6field: "release_failures_v6"},

		// No family label: there is no v4 counterpart, so a v4 series would be zero by construction (#815).
		{name: "dhcpv6_config_only", counter: true, help: "DHCPv6 information replies received: address-less configuration from a network advertising the RA other-config flag. Counts replies received, not configuration applied.", field: "dhcpv6_config_only"},
		{name: "dhcpv6_not_offered", counter: true, help: "Endpoints started on an IPv6 network whose router advertisement offered no DHCPv6 address (stateless or SLAAC). Not a fault: the network is working as configured and there is no DHCPv6 address on it to be had. The container comes up with IPv4, an IPv6 link-local and DHCPv6 configuration where the segment offers it, and no global IPv6 address from this plugin. Since v2.2.0 the interface is at accept_ra=0/autoconf=0, so the kernel forms no SLAAC address and installs no route from the advertisement either, and the daemon refuses an IPv6 route on a link carrying no IPv6 address, so the plugin cannot supply one here; the container has a link-local address and no IPv6 route until a global address is formed for it (#818). Kept apart from dhcpv6_no_router_advert because that one means no router answered at all.", field: "dhcpv6_not_offered"},
		{name: "dhcpv6_refused", counter: true, help: "Endpoints that failed because a DHCPv6 server answered and refused the client, carrying a Status Code other than Success (RFC 9915 section 21.13). The code's name is in the log line beside the endpoint. The server is reachable and configured and has no address for this client: an exhausted pool answers NoAddrsAvail, and a client asking for an address outside the range the server serves gets NotOnLink. Kept apart from dhcpv6_no_server, which is the ending where nothing answered at all.", field: "dhcpv6_refused"},
		{name: "dhcpv6_no_server", counter: true, help: "Endpoints that failed because the segment's router advertisement carried the managed-address flag and no DHCPv6 server answered inside the acquisition budget. The segment said addresses are available over DHCPv6 and none arrived. Kept apart from dhcpv6_refused, which is the ending where a server answered and said no.", field: "dhcpv6_no_server"},
		{name: "dhcpv6_slaac_no_prefix", counter: true, help: "Endpoints that failed on a network whose ipv6_mode forms the address from a router advertisement, because a router advertised and none of its prefixes formed one. RFC 4862 section 5.5.3 lists the reasons a Prefix Information option forms nothing: no Autonomous flag, a zero valid lifetime, a preferred lifetime past the valid one, a prefix length that with the interface identifier does not total 128 bits, or the link-local prefix. The router's prefix configuration is what to look at.", field: "dhcpv6_slaac_no_prefix"},
		{name: "dhcpv6_auto_fallbacks", counter: true, help: "Endpoints on an ipv6_mode=auto network whose IPv6 address was formed from a router's advertised prefix after the segment advertised DHCPv6 and no server answered inside the fallback window, which is half the router-discovery window by default. It counts addresses that really formed and never fallbacks attempted. Set ipv6_auto_strict=true on the network to fail those endpoints instead of forming an address.", field: "dhcpv6_auto_fallbacks"},
		{name: "dhcpv6_no_router_advert", counter: true, help: "Endpoints started on an IPv6 network where no router advertisement arrived inside the acquisition budget. The endpoint keeps running with no IPv6 address. Usually a missing or misconfigured router rather than a plugin fault, but unlike dhcpv6_not_offered it is not a configuration the operator chose.", field: "dhcpv6_no_router_advert"},
		{name: "dhcpv6_slaac_no_address", counter: true, help: "Endpoints that failed on a network whose ipv6_mode forms the address from a router advertisement, where a router advertised and no address formed inside the acquisition budget. Two things produce it: another node already holds the address this endpoint's prefix and MAC address form, and a modified EUI-64 interface identifier gets no second try after duplicate address detection fails (RFC 4862 section 5.4.5); or the advertisement arrived too late in the budget for detection to finish. Kept apart from dhcpv6_slaac_no_prefix, which is the ending where the advertised prefixes themselves formed nothing, and from dhcpv6_no_server, which needs a DHCPv6 exchange -- ipv6_mode=slaac sends no Solicit at all.", field: "dhcpv6_slaac_no_address"},
		{name: "ipv6_slaac_addresses", counter: true, help: "IPv6 addresses formed from a router's advertised prefix and installed on a container link. It counts ADDRESSES, not endpoints and not leases: RFC 4862 section 5.5.3 forms one address per autonomous prefix, so one container on a link advertising a unique-local prefix and a global one raises this by two. It is the counter that says ipv6_mode=slaac and ipv6_mode=auto are reaching containers, and it moves on the netlink call rather than on the client forming an address, so it cannot report addresses the container does not have.", field: "ipv6_slaac_addresses"},
		{name: "ipv6_addresses_withdrawn", counter: true, help: "IPv6 addresses removed from a container link because the lease stopped holding them: a valid lifetime that ran out, or a prefix the router stopped advertising. The other half of ipv6_slaac_addresses, and what makes a renumbering visible: an address arriving and an address leaving are two events, and counting only arrivals reads as a container collecting addresses forever. The audit ledger's withdrawn row names which address, when audit_log is on.", field: "ipv6_addresses_withdrawn"},
		{name: "ipv6_slaac_prefixes_ignored", counter: true, help: "Advertised Prefix Information options no address was formed from. RFC 4862 section 5.5.3 gives the reasons: no Autonomous flag, the link-local prefix, a preferred lifetime past the valid one, a prefix length that with the interface identifier does not total 128 bits, a zero valid lifetime on a prefix not already held, and an address already found in use. This client's own cap of eight addresses per endpoint is counted here too, which is what separates an endpoint holding eight addresses on a link advertising nine from one that found nothing to form from at all. It is counted only on a network whose ipv6_mode forms addresses: on ipv6_mode=dhcp the same rule refuses every autonomous prefix on every advertisement, correctly, since the address comes from the server, and a router readvertises every few seconds (RFC 4861 section 6.2.1).", field: "ipv6_slaac_prefixes_ignored"},
		{name: "ipv6_main_prefix_unmatched", counter: true, help: "Endpoints whose network named an ipv6_main_prefix that none of the endpoint's addresses fell inside, so the first advertised prefix was reported to Docker as the endpoint's address instead. Not a fault: the container has its addresses and docker inspect shows a different one than the option asked for, which is the router's prefix list to look at.", field: "ipv6_main_prefix_unmatched"},
		{name: "ipv6_link_enable_failures", counter: true, help: "Container links the plugin could not administratively enable IPv6 on before starting a DHCPv6 client. The engine disables IPv6 on a sandbox interface whose endpoint has no IPv6 address, so on such a link nothing IPv6 can arrive at all; without this counter that is indistinguishable from a segment that is merely quiet.", field: "ipv6_link_enable_failures"},
		{name: "router_advert_guard_failures", counter: true, help: "Steps of the DHCPv6 Router Advertisement guard that did not take: a sysctl write that failed, a read-back holding something other than what was written, or a route the kernel had already installed from an advertisement that could not be removed. Three knobs (accept_ra=0, autoconf=0, keep_addr_on_down=1), two steps each, plus the route purge. The plugin puts the IPv6 gateway, MTU, routes and DNS into the Join answer itself, so a container whose kernel is also acting on advertisements carries a second default route beside the plugin's and which one wins is a metric comparison nobody chose. It does not count a privileged process inside the container writing the settings back.", field: "router_advert_guard_failures"},
		{name: "ipv6_router_withdrawn", counter: true, help: "Container IPv6 default routes removed because the router that advertised itself set its Router Lifetime to 0 (RFC 4861 section 4.2). Counts routes removed, not advertisements received, so a router sending several on its way down moves this once per container. Not a fault: the containers on that segment are correctly left with no default route rather than one pointing at a router that is gone.", field: "ipv6_router_withdrawn"},
		{name: "router_solicits_sent", counter: true, help: "RFC 4861 section 6.3.7 Router Solicitations sent from container links by the DHCPv6 client. READ router_adverts_seen_total AGAINST THIS ONE: a zero sighting count beside a zero solicitation count is a client that never asked, which is not the same reading as a link whose routers are silent. Only IPv6 endpoints solicit, so it cannot move on an IPv4-only host.", field: "router_solicits_sent"},
		{name: "router_adverts_seen", counter: true, help: "Router Advertisements that decoded and reached the DHCPv6 state machine. This is the number that says an IPv6 segment is answering at all: the gateway, the MTU, the on-link prefixes, the more-specific routes and a stateless segment's resolvers all come out of these frames, and a container that came up with none of them on a host reading zero here was on a link nothing advertised on. Frames dropped before the client saw them are in none of these series.", field: "router_adverts_seen"},
		{name: "router_adverts_refused", counter: true, help: "Frames whose ICMPv6 type said Router Advertisement and which would not decode: a zero-length option, an option running past the end of the frame, a non-zero ICMP Code. Separate from router_adverts_seen_total because their difference is the diagnostic. A link with no router and a link whose router is advertising something this client refuses read the same in a total holding both, and one of them is a router to find while the other is a router to fix. Not healthy-affecting: the plugin cannot tell a broken router from a frame something on the path corrupted.", field: "router_adverts_refused"},
		{name: "router_advert_options_ignored", counter: true, help: "Options inside an advertisement refused by that option's own standard, while the rest of the advertisement was read. It counts OPTIONS and not frames, and it rises on advertisements that are otherwise fine, so it is not part of router_adverts_refused_total. Two things put entries in it: an option the decoder cannot read by its own rule, and a value the state machine read and may not use, which today is a link MTU outside the range RFC 4861 section 6.3.4 lets a host copy. Not healthy-affecting: a router offering one option nobody can use still supplies everything else.", field: "router_advert_options_ignored"},
		{name: "router_table_entries_dropped", counter: true, help: "Advertised entries a full list in the client's router table would not take: a ninth router, or a seventeenth more-specific route. Non-zero means the table's caps are in force, which on a segment carrying one link's worth of routers means something is advertising more than a link has. A refusal holds what was heard first, so what is lost is the newest arrival. Not healthy-affecting: the caps exist so that a link cannot spend this plugin's memory, and they are doing that.", field: "router_table_entries_dropped"},
		{name: "router_table_entries_evicted", counter: true, help: "Held entries a full resolver or search list threw out to take an arrival, which is what RFC 8106 section 6.2 (d) asks of those two lists. The pair of router_table_entries_dropped_total, and the other way round: an eviction holds what expires last, so what is lost is something the client already had. Read the two together; either above zero means the caps are in force. Not healthy-affecting: the caps exist so that a link cannot spend this plugin's memory, and an eviction is them working.", field: "router_table_entries_evicted"},

		{name: "dhcp_routes_applied", counter: true, help: "DHCP option-121 classless static routes handed to Docker. Counts routes, not Joins.", field: "dhcp_routes_applied"},
		{name: "dhcp_default_route_superseded", counter: true, help: "Joins whose option-121 routes cover 0.0.0.0/0 by union rather than by a literal default entry, so container egress follows those next hops even though the reported gateway still names the option-3 router. Legitimate in split-tunnel setups; the point is that it is now visible.", field: "dhcp_default_route_superseded"},
		{name: "mtu_refused", counter: true, help: "Option-26 MTUs outside the range the plugin will apply; the container link keeps the MTU it had.", field: "mtu_refused"},

		{name: "dhcp_server_tier_fallbacks", counter: true, help: "Steps down the dhcp_servers ladder: one per preferred entry that did not answer inside its slice of the budget and handed on to the next. One acquisition against three silent preferred servers adds 2, not 1. The only outside signal that a preferred server is silently dead.", field: "dhcp_server_tier_fallbacks"},
		{name: "dhcp_server_policy_exhausted", counter: true, help: "Acquisitions abandoned because no server listed in dhcp_servers answered.", field: "dhcp_server_policy_exhausted"},
		{name: "dhcp_server_policy_timeouts", counter: true, help: "dhcp_timeouts on endpoints whose renewal client is restricted to dhcp_servers.", field: "dhcp_server_policy_timeouts"},

		{name: "recovered_ok", counter: true, help: "Endpoints whose renewal client was rebuilt after a plugin restart.", field: "recovered_ok"},
		{name: "recovery_failed", counter: true, healthy: true, unit: "endpoints", action: "A container that is still running has no lease-renewal client and will lose its address at expiry. Restart it; the plugin log carries the cause.", help: "Post-restart rebuilds that failed for a container that is still running; it runs without lease renewal and loses its IP at expiry. Healthy-affecting.", field: "recovery_failed"},
		{name: "recovery_deferred", counter: true, help: "Recovery walks postponed because the daemon was still starting (#383). Not a fault.", field: "recovery_deferred"},
		{name: "recovery_aborted_container_gone", counter: true, help: "Endpoints skipped during recovery because their container had already exited. Not a fault.", field: "recovery_aborted_container_gone"},
		{name: "recovery_network_gone", counter: true, help: "Networks skipped during recovery because they were removed mid-walk. Not a fault.", field: "recovery_network_gone"},
		{name: "recovery_fingerprints_skipped", counter: true, help: "Endpoints recovery adopted but could not describe, because the container inspect gave no hostname. Not healthy-affecting: they keep their renewal client and lose only address stability across their next restart.", field: "recovery_fingerprints_skipped"},
		{name: "recovery_already_managed", counter: true, help: "Endpoints a recovery walk left alone because a Join had already claimed them. Not a fault; the only outward evidence of recovery racing a Join.", field: "recovery_already_managed"},

		{name: "join_start_failures", counter: true, healthy: true, unit: "endpoints", action: "A container that is still running got its initial lease but no renewal client. Restart it; the plugin log carries the cause.", help: "Joins whose DHCP client failed to start, leaving a running container without lease renewal. Healthy-affecting.", field: "join_start_failures"},
		{name: "join_aborted_container_gone", counter: true, help: "Joins abandoned because the container disappeared mid-attach. Not a fault.", field: "join_aborted_container_gone"},
		{name: "join_aborted_no_container", counter: true, help: "Joins abandoned because no container was ever found for the endpoint. Not a fault.", field: "join_aborted_no_container"},
		{name: "join_aborted_endpoint_left", counter: true, help: "Joins abandoned because a Leave arrived while the attach was in flight. Not a fault.", field: "join_aborted_endpoint_left"},
		{name: "join_attach_slow", counter: true, help: "Attaches that outran their expected window and needed the daemon-busy grace.", field: "join_attach_slow"},
		{name: "join_attach_completed", counter: true, help: "Successful attaches. The population the join_attach_* buckets partition.", field: "join_attach_completed"},
		{name: "join_attach_under_1s", counter: true, help: "Successful attaches that finished in under a second.", field: "join_attach_under_1s"},
		{name: "join_attach_1s_to_budget", counter: true, help: "Successful attaches that took a second or more but stayed inside AwaitTimeout.", field: "join_attach_1s_to_budget"},
		{name: "join_attach_ms_max", help: "The longest successful attach, in milliseconds. Read it against AwaitTimeout.", field: "join_attach_ms_max"},
		{name: "displaced_stops", counter: true, help: "DHCP managers stopped because a Join displaced them. Counts the intent to stop; it is not evidence the client went away.", field: "displaced_stops"},
		{name: "restart_link_up_waited", counter: true, help: "Container restarts that had to wait for the interface to come back up.", field: "restart_link_up_waited"},
		{name: "restart_link_up_timeouts", counter: true, warn: true, unit: "restarts", action: "A departing link held its address past the wait budget, so docker restart failed with \"address already in use\". Worth investigating: any non-zero value means a restart was refused.", help: "Container restarts where the interface never came up inside the wait.", field: "restart_link_up_timeouts"},

		{name: "address_conflicts", counter: true, healthy: true, unit: "addresses", action: "A leased address was found in use by another device on the segment. Look for a statically configured host inside the DHCP pool.", help: "Leased addresses found already in use by another host, over the whole life of the lease. The ipv4 series is RFC 5227: section 2.1's probes before the address is used and section 2.4's listener afterwards, and conflict_check governs it -- it moves in =wait and =async, and in =off the client neither probes nor listens, so it moves only for a conflict reported to the client from outside it, which no code path in this plugin does today. The ipv6 series is the kernel's Duplicate Address Detection (RFC 4862 section 5.4), declined under RFC 9915 section 18.2.8; it is not ARP, conflict_check does not govern it, and nothing ARP-shaped counts it. Healthy-affecting.", field: "address_conflicts", v4field: "address_conflicts_v4", v6field: "address_conflicts_v6"},
		{name: "acd_probes_sent", counter: true, help: "RFC 5227 section 2.1.1 ARP Probes sent. READ THIS BEFORE BELIEVING address_conflicts{family=\"ipv4\"} IS ZERO: zero here over a running plugin means no IPv4 address was ever checked, which is not the same reading as a clean segment (#524). It says nothing about the ipv6 series, which is Duplicate Address Detection and sends no ARP. Moves in conflict_check=wait and =async, never in =off.", field: "acd_probes_sent"},
		{name: "acd_announcements_sent", counter: true, help: "RFC 5227 section 2.3 ARP Announcements sent. Two go out per address that passed the probe, and a live scrape can be one behind: the first is sent at the bind and the second from a timer 2s later (section 2.3 ANNOUNCE_INTERVAL), while the plugin folds the library counter on client events, so a freshly bound address reads 1 until the next event on that endpoint. Moves in conflict_check=wait and =async, never in =off. Read against acd_probes_sent: probes climbing with no announcements means addresses are being checked and none is coming back clean.", field: "acd_announcements_sent"},
		{name: "acd_conflicts_detected", counter: true, help: "RFC 5227 conflicts the DHCP library itself counted. It is the same population as address_conflicts{family=\"ipv4\"} -- counted inside the ARP state machine rather than from the events it emitted -- and the two are expected to be equal; a difference is a defect in the plugin's event handling, not a property of the segment. It is NOT comparable to the address_conflicts total, which carries the ipv6 series too: DHCPv6 conflicts come from Duplicate Address Detection and this machine never sees them. Its =off rule is the ipv4 series's, not a different one: with no probe and no listener the only thing that can move either counter is a conflict reported to the client from outside it, which no code path in this plugin does today.", field: "acd_conflicts_detected"},
		{name: "acd_arp_send_failures", counter: true, warn: true, unit: "frames", action: "ARP Probes or Announcements the socket refused. A probe that never went out proves nothing about the address, so address_conflicts=0 stops meaning the segment is clean.", help: "ARP Probes and Announcements the socket refused. Not healthy-affecting: a refused send is not itself a conflict, but a probe that never went out proves nothing about the address, so a rise turns \"no conflict found\" into \"the question was not asked\". Moves in conflict_check=wait and =async, never in =off.", field: "acd_arp_send_failures"},
		{name: "acd_resumed_unchecked", counter: true, warn: true, unit: "endpoints", action: "An endpoint was resumed from a record whose RFC 5227 section 2.1 check had not finished, so it held its address with no completed check behind it until the resumed client re-checked it on the INIT-REBOOT acknowledgement.", help: "Endpoints picked up after a plugin restart from a durable record whose RFC 5227 section 2.1 check had not completed (D23). The resumed client re-runs section 2.1 on its INIT-REBOOT acknowledgement whatever the record said, so the window closes on its own; this counts how often it opened. Not healthy-affecting: the container keeps its address and the check is re-run.", field: "acd_resumed_unchecked"},

		{name: "parent_link_waits", counter: true, help: "Operations that queued for a parent interface another operation was using, and got it. Includes the ones that gave up waiting for a holder attaching the SAME kind of child (macvlan beside macvlan, ipvlan beside ipvlan), because a parent accepts those side by side and the wait protected nothing -- the operation proceeds and succeeds. Two containers starting together on one parent-attached network land here, since an address reservation holds the parent across its DHCP exchange. Not healthy-affecting: it is contention, not failure.", field: "parent_link_waits"},
		{name: "parent_link_wait_timeouts", counter: true, warn: true, unit: "operations", action: "An operation gave up waiting for a parent interface that was being used to attach the OTHER kind of child. A parent NIC is a macvlan port or an ipvlan port and never both, so the kernel may refuse what this operation went on to do, and a container start can fail with \"device or resource busy\". Look for a macvlan and an ipvlan network sharing one parent, or a validate_dhcp probe running beside container starts.", help: "Operations that gave up waiting for a parent interface held for the other kind of child, or held by something this plugin could no longer identify. They proceed anyway and the kernel is the authority; this counts the times that gamble was taken. Same-kind contention is NOT counted here -- see parent_link_waits -- because the kernel permits it and a warning nobody can act on is worse than none.", field: "parent_link_wait_timeouts"},

		{name: "tombstone_write_failures", counter: true, healthy: true, unit: "writes", action: "A tombstone could not be written or re-read, so some container will pick a fresh MAC and address on its next restart. Check STATE_DIR for space and for read errors.", help: "Tombstone writes that failed, so the next restart of that container picks a new MAC and address. Healthy-affecting.", field: "tombstone_write_failures"},
		{name: "tombstone_quarantines", counter: true, healthy: true, unit: "files", action: "The tombstone file was unparseable and was moved aside, taking every live tombstone on the host with it. Every container restarting in the next TTL window comes back with a new MAC and address.", help: "Times the tombstone file was found unparseable and moved aside as tombstones.json.corrupt-<ts>; every live tombstone on the host was lost with it, so containers restarting in the next TTL window come back with new MACs and addresses. Healthy-affecting.", field: "tombstone_quarantines"},
		{name: "tombstones_consumed", counter: true, help: "Tombstones read back to preserve a container's MAC and address across a restart.", field: "tombstones_consumed"},
		{name: "unsafe_hostnames_rejected", counter: true, help: "Container hostnames dropped before reaching the DHCP request because they carried a control character. A legitimate hostname never does, so any rise is deliberate (#692).", field: "unsafe_hostnames_rejected"},
		{name: "hostnames_applied_late", counter: true, help: "Container names given to a DHCP client that was already leasing, which is how an endpoint is named without the attach waiting for the daemon (#961). The mechanism working. It narrows the zeros on hostname_lookup_failures and hostname_apply_failures without deciding them: zero on all three is also a host whose containers were started without --hostname, or one where every attach took the name before the client started. Counts non-empty names only, and v4 only, because the late path runs only without register_dns, where the DHCPv6 client sends no name (#1029).", field: "hostnames_applied_late"},
		{name: "hostname_lookup_failures", counter: true, warn: true, unit: "endpoints", action: "The container inspect that supplies the DHCP name did not answer inside the attach window. The endpoint keeps its lease and its renewal client; what it lacks is a name in the DHCP server's table, until something re-attaches it. Watch it: a sustained rise is a daemon that is not answering, which affects far more than names.", help: "Attaches whose container inspect did not answer, so the endpoint leases with no name in the DHCP server's table.", field: "hostname_lookup_failures"},
		{name: "hostname_apply_failures", counter: true, warn: true, unit: "endpoints", action: "The running DHCP client would not take the container's name: an unsendable name, a full request queue, or no client left to give it to. The endpoint keeps its lease. Watch it beside hostnames_applied_late; a rise with no lookup failures beside it is the client side and not the daemon.", help: "Container names the running DHCP client refused, so the endpoint leases with no name in the DHCP server's table.", field: "hostname_apply_failures"},
		{name: "host_ifnames_applied", counter: true, help: "Host-side links renamed after the container they belong to, on a network that asked for it with host_ifname (#978). The mechanism working, and the denominator for host_ifname_conflicts and host_ifname_failures: a host with no such network has zeros everywhere. Bridge mode only, because it is the only mode that leaves a link on the host.", field: "host_ifnames_applied"},
		{name: "host_ifname_conflicts", counter: true, warn: true, unit: "endpoints", action: "The name a container asked for is already on this host. Interface names are one namespace shared with every network and every NIC on the box, so rename the container, the other interface, or switch that network's host_ifname to the other source. The endpoint keeps its lease and its generated link name.", help: "Renames refused because another interface on the host already had the name, so the endpoint's host-side link kept its generated name.", field: "host_ifname_conflicts"},
		{name: "host_ifname_failures", counter: true, warn: true, unit: "endpoints", action: "The rename was refused and it was not a name that was taken: a container name with no character an interface name may carry, a host-side link that was not there, or a kernel that would not rename a running link. Read the plugin log, which names which. The endpoint keeps its lease and its generated link name.", help: "Renames that did not happen for any reason other than the name being taken, so the endpoint's host-side link kept its generated name.", field: "host_ifname_failures"},
		{name: "unsafe_option_values_dropped", counter: true, help: "Server-chosen DHCP string values refused before use because they carried a control character, plus option-15 domains truncated at their first space. The DHCP library validates domain-typed options; string-typed ones can carry anything the server put on the wire (#703, #704).", field: "unsafe_option_values_dropped"},
		{name: "network_options_rejected", counter: true, help: "Endpoint operations that met a network's stored options and would not act on them as written: an interface name the kernel would not accept, or a mode this plugin does not implement. DeleteEndpoint counts without refusing, so a rise does not mean nothing was torn down. Not healthy-affecting: refusing is the safe outcome and the operation already fails visibly to Docker. A rise means options persisted before name validation existed, or a hand-edited state directory (#727).", field: "network_options_rejected"},
		// The IPAM driver's counters stay zero on a network created with --ipam-driver null (#110).
		{name: "ipam_replay_hits", counter: true, help: "Stored endpoint addresses confirmed at a daemon restart from this plugin's own lease record. The mechanism working: it is how an IPAM-mode endpoint keeps its address across a restart. Read it as the denominator for ipam_replay_miss.", field: "ipam_replay_hits"},
		{name: "ipam_replay_miss", counter: true, warn: true, unit: "addresses", action: "A stored endpoint's address matched no lease record in its network, so the plugin refused to confirm it and the network driver's recovery adopts the endpoint from Docker's view instead. Worth investigating: the lease record and Docker's store have drifted apart, which is a lost or hand-edited record file.", help: "Stored endpoint addresses this plugin would not confirm at a daemon restart because no lease record in that network holds them.", field: "ipam_replay_miss"},
		{name: "ipam_rebind_ambiguous", counter: true, warn: true, unit: "requests", action: "Several containers on one network restarted together, and an address request carries no hostname and no endpoint id, so nothing said which previous lease it belonged to and the DHCP server decided. Watch it: addresses on this host moved, and pinning with --ip or --mac-address, or using --ipam-driver null, is the remedy.", help: "Address requests that met more than one recently-removed endpoint on the network and so could not tell which address to ask for.", field: "ipam_rebind_ambiguous"},
		{name: "ipam_reserve_duplicate_mac", counter: true, warn: true, unit: "requests", action: "Two endpoints on one network were pinned to one --mac-address, so the second container did not start. Worth investigating: each move is a failed container start, and the remedy is the operator's -- give each container its own --mac-address, or leave it unset so Docker generates one per endpoint. It is not the daemon's re-send after a plugin-call timeout, which carries no body and is refused before any handler runs, so raising --timeout does not affect it.", help: "Address requests refused because the network was already leasing an address for that hardware address.", field: "ipam_reserve_duplicate_mac"},
		{name: "ipam_stranded_records", counter: true, warn: true, unit: "records", action: "A previous plugin process ended between an address request and the endpoint being created, and this process gave the address back so a container restarting can claim it. The move is the repair, not the fault; if it rises steadily, look at why the plugin or the daemon under it keeps restarting while containers start.", help: "Lease records a previous plugin process left with no endpoint behind them, given up at start-up so their addresses can be claimed again.", field: "ipam_stranded_records"},
		{name: "ipam_release_unknown", counter: true, help: "Addresses libnetwork released that no lease record of ours holds. Not a fault: a release for an address whose record is already retained or closed is the normal ordering.", field: "ipam_release_unknown"},
		{name: "dns_propagation_pid_mismatches", counter: true, help: "DNS propagations refused because the container PID resolved through Docker no longer belonged to that container. The plugin shares the host PID namespace, so each one is a resolv.conf write that would otherwise have landed in an unrelated host process (#688).", field: "dns_propagation_pid_mismatches"},
		{name: "netns_pid_mismatches", counter: true, help: "Sandbox network-namespace opens refused because the container PID resolved through Docker no longer belonged to that container. Each one is a netlink handle, and a root DHCP client, that would otherwise have been bound to an unrelated host process's network namespace.", field: "netns_pid_mismatches"},
		{name: "sandbox_key_entries", counter: true, help: "Container network namespaces entered through the sandbox key the daemon publishes at /var/run/docker/netns/<key>. Every attach counts here on a host whose sandbox_netns_propagation is 1, and every recovery after a plugin restart counts here on any host. This is the denominator for sandbox_pid_fallbacks: zero fallbacks with zero entries here means nothing was opened, not that the key route works.", field: "sandbox_key_entries"},
		{name: "sandbox_key_entry_failures", counter: true, help: "Attempts to enter a container network namespace through the sandbox key that were refused. EXPECTED, once per container attach, on a host whose sandbox_netns_propagation is 0: the daemon bind-mounts each sandbox netns after the plugin's own /var/run/docker mount was taken and that mount is private, so the key resolves to the placeholder file underneath and the container PID route carries the attach. Nothing is degraded and no action is indicated. On a host answering 1 this stays flat and sandbox_key_entries rises instead. Read sandbox_key_absent, sandbox_key_not_permitted, sandbox_key_not_a_namespace, sandbox_key_wrong_ns_type and sandbox_key_unavailable to see which refusal this was; they sum to this counter.", field: "sandbox_key_entry_failures"},
		{name: "sandbox_key_absent", counter: true, help: "Key-route refusals because neither the Join request nor the container inspect carried a sandbox key, so the key route was never attempted and the container PID route carries the attach. Not observed on any measured host; split out of sandbox_key_not_permitted in 2.0-alpha.1, where an absent key was indistinguishable from the --exec-root case whose remedy is a change to this plugin. An arm of sandbox_key_entry_failures.", field: "sandbox_key_absent"},
		{name: "sandbox_key_not_permitted", counter: true, help: "Key-route refusals because a non-empty sandbox key did not name an entry of a directory this plugin accepts (/var/run/docker/netns, /run/docker/netns). This is the arm that is NOT expected: a daemon started with a non-default --exec-root publishes keys elsewhere, and the remedy is a change to this plugin rather than to the host. An arm of sandbox_key_entry_failures.", field: "sandbox_key_not_permitted"},
		{name: "sandbox_key_not_a_namespace", counter: true, help: "Key-route refusals because the entry opened and was not a namespace — the placeholder file libnetwork creates before it bind-mounts the sandbox netns over it. EXPECTED, once per container attach, on a host whose sandbox_netns_propagation is 0, and it is the evidence for SECURITY.md's reason that the host PID namespace and CAP_SYS_PTRACE stay on such a host. An arm of sandbox_key_entry_failures.", field: "sandbox_key_not_a_namespace"},
		{name: "sandbox_key_wrong_ns_type", counter: true, help: "Key-route refusals because the entry was a namespace of some other type. Not observed on any measured host; published so that \"not observed\" stays a statement a reader can check. An arm of sandbox_key_entry_failures.", field: "sandbox_key_wrong_ns_type"},
		{name: "sandbox_key_unavailable", counter: true, help: "Key-route refusals that were none of the three named arms — the entry never became openable inside the attach budget, or its directory could not be read. The residual arm, so that the four always sum to sandbox_key_entry_failures rather than nearly doing so.", field: "sandbox_key_unavailable"},
		{name: "sandbox_pid_fallbacks", counter: true, help: "Endpoints whose network namespace was entered through /proc/<pid>/ns/net after the sandbox key route was refused. Every attach counts here on a host whose sandbox_netns_propagation is 0. That route is why the manifest asks for the host PID namespace and CAP_SYS_PTRACE; a zero here across a host's whole uptime, with sandbox_key_entries non-zero, is the evidence that it was not needed.", field: "sandbox_pid_fallbacks"},
		{name: "docker_api_non_get_refusals", counter: true, help: "Requests to the Docker API refused before they were sent because their method was not GET. The plugin's whole Docker surface is four read calls, so this stays zero unless code in this process tried to write to the daemon; the socket mount is the grant that makes such a write equivalent to root on the host (#691).", field: "docker_api_non_get_refusals"},
		{name: "ledger_write_failures", counter: true, warn: true, unit: "writes", action: "Lease-ledger appends are failing, so the audit_log record of who held which address is incomplete. Forensics only; networking is unaffected.", help: "Lease-ledger writes that failed.", field: "ledger_write_failures"},
		{name: "ifname_unsupported", counter: true, warn: true, unit: "endpoints", action: "A container was asked to name its interface on an engine older than the first that applies a remote driver's name, so the interface carries the driver's prefix and index instead. Read the container's own `ip link` to see which name it got. The remedy is an engine at " + MinEngineIfnameVersion + " or newer, or a configuration that does not depend on the name.", help: "Endpoints created with a custom interface name (Compose interface_name, endpoint option com.docker.network.endpoint.ifname) on an engine below the first version that applies one. The test is the engine's reported version and not what it does: 28.5.2 and 29.7.2 were measured ignoring the name and " + MinEngineIfnameVersion + ".0 applying it, so a vendor build that departs from its version is counted the wrong way. Not healthy-affecting: the container comes up on a working network and only the interface name differs from the request. It counts nothing on an engine whose version the plugin could not read.", field: "ifname_unsupported"},
		{name: "state_file_chmod_failures", counter: true, warn: true, unit: "files", action: "A file under STATE_DIR was left with access outside 0600 by the startup sweep. The plugin log names the path; chmod 0600 it by hand. A value of 1 with no file named means STATE_DIR itself could not be read, so no file was examined at all.", help: "Files the startup sweep could not tighten, plus one if STATE_DIR could not be read at all, in which case no file was examined (#804). Not healthy-affecting: nothing the plugin does is degraded by a loose mode on a state file, and the writer is root either way. Zero is the normal reading on every host, including one that has never been upgraded, because a sweep with nothing to tighten does not move it.", field: "state_file_chmod_failures"},
	}
}

// instance_id is a build_info label so a plugin restart shows as a new series, not a rewound counter (#405).
// Engine labels go on a separate series, since the engine changes on a Docker upgrade (#670).
var metricLabelOnlyFields = map[string]string{
	"instance_id":    "build_info",
	"version":        "build_info",
	"commit":         "build_info",
	"library":        "build_info",
	"engine_version": "engine_info",
	"api_version":    "engine_info",
}

// metricNotExposedFields are HealthResponse fields left out of /metrics, each with a reason the exposure test checks
// (#651).
var metricNotExposedFields = map[string]string{
	"checks":    "one series per check would restate net_dhcp_health_status and the healthy-affecting counters, which are already exposed; the check's observedValue IS the counter's series",
	"endpoints": "a series per container is a cardinality decision this row does not take; the per-endpoint lease gauge is its own piece of work (O-5)",
}

// writeExposition renders Prometheus text format 0.0.4 by hand: a new direct dependency in a process holding
// CAP_NET_ADMIN, CAP_SYS_ADMIN and CAP_SYS_PTRACE was not worth one renderer (#651).
func writeExposition(w io.Writer, h HealthResponse) error {
	return writeExpositionWith(w, h, metricDefs())
}

func writeExpositionWith(w io.Writer, h HealthResponse, defs []metricDef) error {
	byTag := healthFieldsByTag(h)
	var b strings.Builder

	b.WriteString("# HELP " + metricPrefix + "build_info Plugin build and instance identity. version is the release tag (dev outside a release), commit the git revision it was built from, library the revision of the in-tree DHCP library; none of the three is ever empty, and `unknown` means the build did not carry it. The instance_id label changes on every plugin restart, so a counter reset appears as a new series rather than as a rewind.\n")
	b.WriteString("# TYPE " + metricPrefix + "build_info gauge\n")
	b.WriteString(metricPrefix + `build_info{instance_id="` + escapeLabelValue(h.InstanceID) +
		`",version="` + escapeLabelValue(h.Version) +
		`",commit="` + escapeLabelValue(h.Commit) +
		`",library="` + escapeLabelValue(h.Library) + "\"} 1\n")

	// api_version is the negotiated min(client, daemon) version; both engine fields read `unknown` when the daemon
	// did not answer at startup (#383, #670).
	b.WriteString("\n# HELP " + metricPrefix + "engine_info The Docker Engine this plugin process is talking to, as the daemon reported it at startup. engine_version is what the minimum supported engine is compared against; api_version is the API version this client negotiated with it, which is the lower of the two maximums. Both read `unknown` when the daemon did not answer at startup.\n")
	b.WriteString("# TYPE " + metricPrefix + "engine_info gauge\n")
	b.WriteString(metricPrefix + `engine_info{engine_version="` + escapeLabelValue(h.EngineVersion) +
		`",api_version="` + escapeLabelValue(h.APIVersion) + "\"} 1\n")

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
			continue
		}
	}
	return out
}

func escapeLabelValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

// escapeHelp escapes backslash and newline only: a double quote is legal in HELP text.
func escapeHelp(s string) string {
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	return r.Replace(s)
}

func (p *Plugin) apiMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := writeExposition(w, p.healthSnapshot()); err != nil {
		http.Error(w, "failed to render metrics: "+err.Error(), http.StatusInternalServerError)
	}
}
