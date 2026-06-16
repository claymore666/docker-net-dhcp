package udhcpc

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
}
