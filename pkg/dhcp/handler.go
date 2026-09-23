// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

type Info struct {
	IP      string
	Gateway string
	Domain  string

	// DNSServers is option 6 (v4) or 23 (v6); empty means leave the container's resolv.conf alone, not clear it (#100).
	DNSServers []string `json:",omitempty"`

	// MTU is option 26; 0 means leave the link MTU alone, and a renewal re-applies only a changed value (#101).
	MTU int `json:",omitempty"`

	// NTPServers is option 42, logged on bind and renew and never applied to the container (#105).
	NTPServers []string `json:",omitempty"`

	// SearchList is option 119, the resolv.conf `search` line under PropagateDNS, falling back to Domain (option 15)
	// (#105).
	SearchList []string `json:",omitempty"`

	// TFTPServer is option 66, logged and never applied to the container (#105).
	TFTPServer string `json:",omitempty"`

	// BootFile is option 67, logged like TFTPServer (#105).
	BootFile string `json:",omitempty"`

	// WPAD (option 252), the RFC 4833 timezones (options 100 and 101) and TimeOffset (option 2) are logged only, never
	// pushed into the container, to keep the no-plumbing bar (#262).
	WPAD          string `json:",omitempty"`
	PosixTimezone string `json:",omitempty"`
	TZDBTimezone  string `json:",omitempty"`
	TimeOffset    string `json:",omitempty"`

	// A default route is never here: RFC 3442 folds 0.0.0.0/0 into Gateway and RFC 4191 section 2.3's ::/0 means the
	// same, and a copy would race the default route Docker installs (#821).

	// Routes are option 121's routes (RFC 3442) on v4 and RFC 4191 Route Information Options on v6, applied at Join
	// (#821).
	Routes []Route `json:",omitempty"`

	// Applied once at Join because accept_ra is off (#821) and a DHCPv6 /128 yields no prefix (RFC 5942 section 4);
	// without it a Router Lifetime 0 segment leaves no IPv6 route. The library reports the most recent advertisement's
	// prefixes, not a union.

	// OnLinkPrefixes are RFC 4861 section 4.6.2 Prefix Information options with the L flag; v6 only.
	OnLinkPrefixes []string `json:",omitempty"`

	// RFC 9915 section 18.2.1's Solicit does not wait for router discovery, so an MTU of 0 before an advertisement is
	// silence while an MTU of 0 after one is a withdrawal; folding them flips the link MTU per event (#821).

	// RouterSeen says whether a Router Advertisement had been seen on the link when this Info was built; v6 only.
	RouterSeen bool `json:",omitempty"`

	// T1 is not carried: dhcpcd 10.3.2 under --noconfigure never renewed at T1 and rebound at T2 (a 120 s lease rebound
	// at t+105 s), so a T1 deadline would fire on healthy clients. LeaseSeconds backs a lapse no event reports (#353,
	// #800, #855).

	// LeaseSeconds is the granted lease lifetime, the IA_NA valid lifetime on v6, 0 when the server gave none.
	LeaseSeconds int `json:",omitempty"`

	// RFC 4862 section 5.5.4: a deprecated address serves existing connections but must not start new ones, so both
	// lifetimes are installed and the kernel enforces the gap (#911).

	// PreferredSeconds is RFC 9915 section 7.1's preferred lifetime of a v6 address, 0 for v4 or infinite.
	PreferredSeconds int `json:",omitempty"`

	// IPDeprecated is the Deprecated flag of the address in IP, since the two numbers cannot express it (#818).
	IPDeprecated bool `json:",omitempty"`

	// RFC 4862 section 5.5.3 forms one address per autonomous prefix, up to proto.MaxSLAACAddresses. Each carries its
	// own lifetimes, since the lease's are aggregates: Lease.Expire is the longest valid and Lease.Preferred the
	// shortest preferred (#818).

	// Addrs is every address a DHCPv6 or SLAAC lease holds, IP being the one reported to Docker; empty for v4.
	Addrs []V6Addr `json:",omitempty"`

	// SLAAC says the addresses were formed from an advertisement (RFC 4862 section 5.5.3), not granted by a server
	// (#818).
	SLAAC bool `json:",omitempty"`

	// MainAddrFallback says no lease address falls inside the `ipv6_main_prefix`, so IP is the lease's first address
	// (#818).
	MainAddrFallback bool `json:",omitempty"`
}

// A SLAAC address carries the advertised prefix length and a granted one /128, by the library's rule; with the kernel's
// RA processing off that length alone makes a formed prefix on-link (#818).

// V6Addr is one v6 lease address with its RFC 9915 section 7.1 lifetimes in seconds, 0 meaning no deadline.
type V6Addr struct {
	// IP is the address with its prefix length, e.g. "2001:db8::1c:42ff:fe00:2/64".
	IP string
	// ValidSeconds and PreferredSeconds are this address's own pair.
	ValidSeconds     int `json:",omitempty"`
	PreferredSeconds int `json:",omitempty"`
	// A router may withdraw a prefix gently with preferred 0 and valid unbounded (RFC 4861 section 4.6.2), which reads
	// (0, 0) like a permanent address, so the flag travels and v6AddrAttrs takes it (#819).

	// Deprecated says the preferred lifetime has elapsed, RFC 4862 section 5.5.4's "SHOULD NOT be used to initiate new
	// communications".
	Deprecated bool `json:",omitempty"`
}

// Route is a single classless static route from DHCP option 121.
type Route struct {
	// Destination is the canonical CIDR (e.g. "10.0.0.0/8").
	Destination string
	// Gateway is the next hop; empty means on-link.
	Gateway string `json:",omitempty"`
}

type Event struct {
	Type string
	Data Info
	// UnsafeValuesDropped is how many server-chosen string values sanitizeInfo refused for a control character (#703).
	UnsafeValuesDropped int `json:",omitempty"`
	// Nothing branches on it; dhcp.RAObservation is the machine-readable form. It rides every v6 event since the
	// library stamps its observation on each (#868).

	// RouterFlags is RFC 4861 section 4.2's M and O bits as log letters: "M", "O", "MO" or empty.
	RouterFlags string `json:",omitempty"`
}
