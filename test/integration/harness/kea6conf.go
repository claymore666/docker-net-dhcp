// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// Untagged so the config renderer, the lease-file reader and the router-address verdict run in the fast lane; the
// tagged fixture in kea6.go starts the server (#214).

package harness

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Kea as the DHCPv6 server of the lane (#214). Its Ubuntu AppArmor profile lets kea-dhcp6 read /etc/kea/**, own
// /var/lib/kea/kea-leases6.csv*, /var/lib/kea/kea-dhcp6-serverid, one PID file, one lock file and
// /var/log/kea/kea-dhcp6.log*. The fixture keeps every file Kea or the readiness wait touches there, the log included:
// an fd inherited from a temp directory is denied on an enforcing host. Debian ships no profile, so a path outside the
// list passes on the pool and fails only there (dhcp-golib runtime/kea6_linux_test.go measured the same).
const (
	// Kea6ConfDir is a subdirectory of /etc/kea so the packaged kea-dhcp6.conf is never touched and a leftover server
	// is found by its command line; the file keeps the basename the profile's PID-file rule is written for (#214).
	Kea6ConfDir  = "/etc/kea/dh-itest"
	Kea6ConfFile = "kea-dhcp6.conf"
	Kea6LeaseDir = "/var/lib/kea"
	// Kea6LeaseFile is the only lease-file name the profile allows (suffixes allowed: the lease-file-cleanup rotation).
	Kea6LeaseFile    = "kea-leases6.csv"
	Kea6ServerIDFile = "kea-dhcp6-serverid"
	Kea6PidDir       = "/run/kea"
	Kea6PidFile      = "kea-dhcp6.kea-dhcp6.pid"
	Kea6LockDir      = "/run/lock/kea"
	Kea6LockFile     = "logger_lockfile"
	Kea6LogDir       = "/var/log/kea"
	Kea6LogFile      = "kea-dhcp6.log"
)

// Kea6 pools, inside the V6 fixture's segment and clear of its dnsmasq pool (::10 to ::99) and the bridge's ::1.
const (
	Kea6PoolStart = "fd00:6470:6865::1000"
	Kea6PoolEnd   = "fd00:6470:6865::10ff"

	// Kea6PDPrefix is a /48 of unique-local space no fixture advertises, delegated as /64 (the library's tests use the
	// same pool shape, #214).
	Kea6PDPrefix       = "fd00:98::"
	Kea6PDPoolLen      = 48
	Kea6PDDelegatedLen = 64
)

// Kea6 defaults are the library's Kea test values, so a timer the caller does not set is the one the library measured.
const (
	Kea6ValidSec     = 600
	Kea6PreferredSec = 400
	Kea6RenewSec     = 300
	Kea6RebindSec    = 480
)

// Kea's lease_type column: 0 is IA_NA, 1 is IA_TA and 2 is IA_PD (dhcp-golib runtime keaLeasePD = 2). The #214 brief
// says IA_PD is 1, which is IA_TA.
const (
	Kea6LeaseNA = 0
	Kea6LeaseTA = 1
	Kea6LeasePD = 2
)

// What the packaged AppArmor profile of kea-dhcp6 allows, rule by rule: a directory tree, and files by name pattern
// (#214). The PID and lock files are single names, not trees.
var kea6AllowedPrefixes = []string{"/etc/kea/"}
var kea6AllowedGlobs = []string{
	"/var/lib/kea/kea-leases6.csv*", "/var/lib/kea/kea-dhcp6-serverid",
	"/run/kea/kea-dhcp6.kea-dhcp6.pid", "/run/lock/kea/logger_lockfile", "/var/log/kea/kea-dhcp6.log*",
}

func Kea6PathAllowed(p string) bool {
	for _, pre := range kea6AllowedPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	for _, g := range kea6AllowedGlobs {
		if ok, _ := path.Match(g, p); ok {
			return true
		}
	}
	return false
}

type Kea6Config struct {
	Interface                                   string
	Subnet, PoolStart, PoolEnd                  string
	PD                                          *Kea6PDPool
	ValidSec, PreferredSec, RenewSec, RebindSec int
}

type Kea6Option func(*Kea6Config)

func WithKea6PD() Kea6Option { return func(c *Kea6Config) { c.PD = DefaultKea6PD() } }

func WithKea6Timers(renew, rebind int) Kea6Option {
	return func(c *Kea6Config) { c.RenewSec, c.RebindSec = renew, rebind }
}

func WithKea6Lifetimes(valid, preferred int) Kea6Option {
	return func(c *Kea6Config) { c.ValidSec, c.PreferredSec = valid, preferred }
}

type Kea6PDPool struct {
	Prefix       string
	PoolLen      int
	DelegatedLen int
}

// DefaultKea6PD is the delegation pool the fixture offers when asked: a /48 of ULA space split into /64.
func DefaultKea6PD() *Kea6PDPool {
	return &Kea6PDPool{Prefix: Kea6PDPrefix, PoolLen: Kea6PDPoolLen, DelegatedLen: Kea6PDDelegatedLen}
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func (c Kea6Config) Timers() (valid, preferred, renew, rebind int) {
	return orDefault(c.ValidSec, Kea6ValidSec), orDefault(c.PreferredSec, Kea6PreferredSec),
		orDefault(c.RenewSec, Kea6RenewSec), orDefault(c.RebindSec, Kea6RebindSec)
}

type keaJSON struct {
	Dhcp6 keaDhcp6 `json:"Dhcp6"`
}

type keaDhcp6 struct {
	Interfaces keaInterfaces `json:"interfaces-config"`
	Lease      keaLeaseDB    `json:"lease-database"`
	Valid      int           `json:"valid-lifetime"`
	Renew      int           `json:"renew-timer"`
	Rebind     int           `json:"rebind-timer"`
	Preferred  int           `json:"preferred-lifetime"`
	Subnets    []keaSubnet6  `json:"subnet6"`
	Loggers    []keaLogger   `json:"loggers"`
}

type keaInterfaces struct {
	Interfaces []string `json:"interfaces"`
}

type keaLeaseDB struct {
	Type        string `json:"type"`
	Persist     bool   `json:"persist"`
	Name        string `json:"name"`
	LFCInterval int    `json:"lfc-interval"`
}

type keaSubnet6 struct {
	ID        int          `json:"id"`
	Subnet    string       `json:"subnet"`
	Interface string       `json:"interface"`
	Pools     []keaPool    `json:"pools"`
	PDPools   []keaPDPool6 `json:"pd-pools,omitempty"`
}

type keaPool struct {
	Pool string `json:"pool"`
}

type keaPDPool6 struct {
	Prefix       string `json:"prefix"`
	PrefixLen    int    `json:"prefix-len"`
	DelegatedLen int    `json:"delegated-len"`
}

type keaLogger struct {
	Name          string      `json:"name"`
	OutputOptions []keaLogOut `json:"output_options"`
	Severity      string      `json:"severity"`
	DebugLevel    int         `json:"debuglevel"`
}

type keaLogOut struct {
	Output string `json:"output"`
	Flush  bool   `json:"flush"`
}

// JSON renders the configuration both Kea 2.4 and 2.6 accept: the logger key is output_options, which 2.6 still takes.
func (c Kea6Config) JSON() string {
	valid, pref, renew, rebind := c.Timers()
	s := keaSubnet6{
		ID: 1, Subnet: c.Subnet, Interface: c.Interface,
		Pools: []keaPool{{Pool: c.PoolStart + " - " + c.PoolEnd}},
	}
	if c.PD != nil {
		s.PDPools = []keaPDPool6{{Prefix: c.PD.Prefix, PrefixLen: c.PD.PoolLen, DelegatedLen: c.PD.DelegatedLen}}
	}
	conf := keaJSON{Dhcp6: keaDhcp6{
		Interfaces: keaInterfaces{Interfaces: []string{c.Interface}},
		Lease: keaLeaseDB{Type: "memfile", Persist: true, LFCInterval: 0,
			Name: path.Join(Kea6LeaseDir, Kea6LeaseFile)},
		Valid: valid, Renew: renew, Rebind: rebind, Preferred: pref,
		Subnets: []keaSubnet6{s},
		Loggers: []keaLogger{{Name: "kea-dhcp6", Severity: "INFO",
			OutputOptions: []keaLogOut{{Output: path.Join(Kea6LogDir, Kea6LogFile), Flush: true}}}},
	}}
	b, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("kea6 config: %v", err))
	}
	return string(b) + "\n"
}

// Kea6SocketFailures are Kea's socket-open failures, each meaning the server will not answer.
var Kea6SocketFailures = []string{
	"DHCPSRV_NO_SOCKETS_OPEN",
	"DHCPSRV_OPEN_SOCKET_FAIL",
	"DHCP6_OPEN_SOCKETS_FAILED",
	"DHCP6_OPEN_SOCKETS_NO_RECONNECT_CTL",
}

func Kea6SocketFailure(window string) string {
	for _, m := range Kea6SocketFailures {
		if strings.Contains(window, m) {
			return m
		}
	}
	return ""
}

// Kea6Row is one line of Kea's memfile lease file.
type Kea6Row struct {
	Addr      string
	DUID      string
	Valid     int64
	Expire    int64
	LeaseType int
	PrefixLen int
	State     int
}

// ParseKea6Leases reads every row of the lease file, oldest first, by column name: the 2.4 and 2.6 headers differ. Kea
// appends a row for each renewal and release, so the same address repeats; use Latest or Held to read the state (#214).
func ParseKea6Leases(data string) []Kea6Row {
	var col map[string]int
	var rows []Kea6Row
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if col == nil {
			col = map[string]int{}
			for i, n := range fields {
				col[n] = i
			}
			continue
		}
		get := func(n string) string {
			if i, ok := col[n]; ok && i < len(fields) {
				return fields[i]
			}
			return ""
		}
		atoi := func(n string) int64 {
			v, _ := strconv.ParseInt(get(n), 10, 64)
			return v
		}
		rows = append(rows, Kea6Row{
			Addr: get("address"), DUID: strings.ToLower(get("duid")),
			Valid: atoi("valid_lifetime"), Expire: atoi("expire"),
			LeaseType: int(atoi("lease_type")), PrefixLen: int(atoi("prefix_len")), State: int(atoi("state")),
		})
	}
	return rows
}

type kea6Key struct {
	addr      string
	prefixLen int
	leaseType int
}

// Kea6Latest keeps the last row of each address, prefix length and lease type: the file is append-only.
func Kea6Latest(rows []Kea6Row) []Kea6Row {
	last := map[kea6Key]int{}
	for i, r := range rows {
		last[kea6Key{r.Addr, r.PrefixLen, r.LeaseType}] = i
	}
	var out []Kea6Row
	for i, r := range rows {
		if last[kea6Key{r.Addr, r.PrefixLen, r.LeaseType}] == i {
			out = append(out, r)
		}
	}
	return out
}

// Kea6Held is the leases of one client that Kea holds at nowUnix: the latest row of each lease has a lifetime, is in the
// default state and has not expired. A release writes a row with a zero lifetime, which this drops (#214).
func Kea6Held(rows []Kea6Row, duid string, leaseType int, nowUnix int64) []Kea6Row {
	duid = strings.ToLower(duid)
	var out []Kea6Row
	for _, r := range Kea6Latest(rows) {
		if r.DUID == duid && r.LeaseType == leaseType && r.Valid > 0 && r.State == 0 && r.Expire > nowUnix {
			out = append(out, r)
		}
	}
	return out
}

func DUIDText(d []byte) string {
	parts := make([]string, len(d))
	for i, b := range d {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, ":")
}

// DUIDLLFromMAC is the DUID-LL (RFC 8415 section 11.4) of an Ethernet link-layer address: type 3, hardware type 1.
func DUIDLLFromMAC(mac net.HardwareAddr) string {
	return DUIDText(append([]byte{0x00, 0x03, 0x00, 0x01}, mac...))
}

func RASourceIP(f RAFrame) (netip.Addr, bool) {
	const srcOff = ethHeaderLen + 8
	if len(f.Raw) < srcOff+net.IPv6len {
		return netip.Addr{}, false
	}
	return netip.AddrFromSlice(f.Raw[srcOff : srcOff+net.IPv6len])
}

// RouterAddressFindings checks that only the harness sender is a router on the segment and that Kea's link carried the
// sender's frames, the control that a missing Kea advertisement was observed on a live link (#214).
func RouterAddressFindings(segment, server []RAFrame, senderAddrs, serverAddrs []netip.Addr) []string {
	var out []string
	in := func(set []netip.Addr, a netip.Addr) bool {
		for _, s := range set {
			if s == a {
				return true
			}
		}
		return false
	}
	senderOnServer := 0
	for name, frames := range map[string][]RAFrame{"segment": segment, "server link": server} {
		for _, f := range frames {
			src, ok := RASourceIP(f)
			switch {
			case !ok:
				out = append(out, fmt.Sprintf("%s: an advertisement with no readable source address (%d bytes)", name, len(f.Raw)))
			case in(serverAddrs, src):
				out = append(out, fmt.Sprintf("%s: Kea's namespace sent an advertisement from %s (lifetime %s)", name, src, f.RouterLifetime))
			case f.RouterLifetime > 0 && !in(senderAddrs, src):
				out = append(out, fmt.Sprintf("%s: %s advertises itself as a router (lifetime %s) and is not the sender %v", name, src, f.RouterLifetime, senderAddrs))
			}
			if name == "server link" && ok && in(senderAddrs, src) {
				senderOnServer++
			}
		}
	}
	if senderOnServer == 0 {
		out = append(out, fmt.Sprintf("positive control: no advertisement from the sender %v reached the capture on the "+
			"server's link, so the missing Kea advertisement proves nothing", senderAddrs))
	}
	sort.Strings(out)
	return out
}
