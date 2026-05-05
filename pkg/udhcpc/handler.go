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
}

type Event struct {
	Type string
	Data Info
}
