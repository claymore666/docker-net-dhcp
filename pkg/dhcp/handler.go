// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

type Info struct {
	IP      string
	Gateway string
	Domain  string

	// DNSServers is the DNS server list from DHCP option 6 (v4) or
	// option 23 (v6). Empty when the server didn't supply the option.
	// Consumers MUST treat empty as "do not change container resolv.conf"
	// — overwriting with empty would silently drop name resolution.
	DNSServers []string `json:",omitempty"`

	// MTU is the Interface MTU from DHCP option 26. 0 when the server
	// didn't supply the option. Consumers MUST treat 0 as "do not change
	// link MTU" — applying 0 would set a useless link state. Renewals
	// can include a different MTU; consumers should compare and only
	// re-apply on change.
	MTU int `json:",omitempty"`

	// NTPServers is the NTP server list from DHCP option 42 (dhcpcd
	// env var `new_ntp_servers`). Empty when the server didn't supply the
	// option. Surfaced to operators via plugin logs at info level on
	// bind/renew; not auto-applied to the container — workloads
	// needing NTP should consume the value themselves (typically via
	// a sidecar that reads docker logs or polls Plugin.Health).
	NTPServers []string `json:",omitempty"`

	// SearchList is the DNS Domain Search List from DHCP option 119
	// (dhcpcd env var `new_domain_search`). Empty when the server didn't supply
	// the option. When PropagateDNS=true the plugin emits this as the
	// `search` line in the container's /etc/resolv.conf; falls back
	// to the single-domain `Domain` (option 15) when SearchList is
	// empty.
	SearchList []string `json:",omitempty"`

	// TFTPServer is the TFTP server hostname from DHCP option 66
	// (dhcpcd env var `new_tftp_server_name`). Empty when not supplied. Used for
	// PXE-boot-style scenarios; surfaced to operators via plugin
	// logs, not auto-applied to the container.
	TFTPServer string `json:",omitempty"`

	// BootFile is the boot file name from DHCP option 67 (dhcpcd env
	// var `new_bootfile_name`). Same surfacing semantics as TFTPServer.
	BootFile string `json:",omitempty"`

	// WPAD is the Web Proxy Auto-Discovery URL from DHCP option 252
	// (dhcpcd env var `new_wpad`; option 252 is non-standard, so the
	// config `define`s it). PosixTimezone / TZDBTimezone come from the
	// RFC 4833 timezone options 100 (PCode, `new_posix_timezone`) and
	// 101 (TCode, `new_tzdb_timezone`); TimeOffset is the legacy option 2
	// (seconds from UTC, `new_time_offset`). All observe-only, like
	// TFTPServer/BootFile: surfaced to operators via plugin logs, never
	// pushed into the container (the no-plumbing bar, #262).
	WPAD          string `json:",omitempty"`
	PosixTimezone string `json:",omitempty"`
	TZDBTimezone  string `json:",omitempty"`
	TimeOffset    string `json:",omitempty"`

	// Routes are the classless static routes from DHCP option 121
	// (RFC 3442, dhcpcd env var `new_classless_static_routes`). v4 only —
	// DHCPv6 carries no route option (routes come from RAs). Empty when
	// the server didn't supply the option. A 0.0.0.0/0 entry is NOT
	// included here: per RFC 3442 its gateway supersedes option 3 and is
	// folded into Gateway during parsing. Applied at Join as additional
	// container StaticRoutes; `skip_routes=true` opts out.
	Routes []Route `json:",omitempty"`

	// LeaseSeconds is the lease lifetime the server granted, in seconds
	// (v4 `new_dhcp_lease_time`; v6 the IA_NA valid lifetime
	// `new_dhcp6_ia_na1_ia_addr1_vltime`). 0 when the server didn't
	// supply it.
	//
	// It exists so the plugin can tell "healthy client, quietly holding a
	// long lease" apart from "client that stopped getting service"
	// WITHOUT depending on a lease-loss hook (#353). dhcpcd does not
	// reliably deliver one: under `--noconfigure`, which this plugin
	// always ran, a lapsed lease fired the hook as RELEASE rather than
	// EXPIRE, and up to v1.8.x a graceful stop produced the same reason,
	// so it could not be counted as a failure.
	//
	// #800 changed that. The RELEASE-on-lapse behaviour needed the
	// `release` directive as well as `--noconfigure`, and #800 removed
	// the directive: this build's clients fire EXPIRE on a lapse, which
	// mapReason already counts. Measured four ways and confirmed by the
	// failure suite across both trees — see pkg/dhcp.mapReason and #855.
	//
	// LeaseSeconds is kept regardless. It is the backstop for a lapse
	// dhcpcd does not report at all, and it is what #353 was actually
	// about; it does not depend on which hook fires.
	//
	// The renewal time (T1, option 58) is deliberately NOT carried here
	// even though dhcpcd exports it, because under `--noconfigure` it is
	// not a deadline anything meets: with no address configured on the
	// link, dhcpcd's T1 unicast renewal always fails ("failed to renew
	// DHCP, rebinding") and the lease is actually renewed at T2 by
	// broadcast rebind. Verified against dhcpcd 10.3.2 with a healthy
	// server: on a 120s lease the only post-bind hook was REBIND at
	// t+105s. A T1-derived deadline would therefore fire on every
	// healthy client.
	LeaseSeconds int `json:",omitempty"`
}

// Route is a single classless static route from DHCP option 121.
type Route struct {
	// Destination is the canonical CIDR (e.g. "10.0.0.0/8").
	Destination string
	// Gateway is the next hop. Empty means the route is on-link (dhcpcd
	// reported the gateway as 0.0.0.0).
	Gateway string `json:",omitempty"`
}

type Event struct {
	Type string
	Data Info
	// UnsafeValuesDropped is how many server-chosen string values
	// BuildEvent refused because they carried a control character
	// (#703).
	//
	// It rides the event because the filter runs in the dhcpcd hook
	// process and the health counter lives in the plugin, which is a
	// different process on the other side of the FIFO. Without it the
	// drop would be invisible to operators, and a filter whose work
	// leaves no trace is indistinguishable from an attack that was
	// never attempted.
	UnsafeValuesDropped int `json:",omitempty"`
	// RouterFlags is the raw nd1_flags string from a ROUTERADVERT hook
	// event -- the flag letters dhcpcd recognised in the advertisement
	// ("MO", "O", or empty). Only ever set on a "routeradvert" event,
	// which only the one-shot acquisition client receives (#868).
	//
	// The string is dhcpcd's spelling, not a protocol name, and it is
	// carried verbatim rather than pre-interpreted so a later consumer
	// that cares about the O flag (stateless configuration, #815) does
	// not need the hook to change.
	RouterFlags string `json:",omitempty"`
}
