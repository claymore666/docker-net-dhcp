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
}

type Event struct {
	Type string
	Data Info
}
