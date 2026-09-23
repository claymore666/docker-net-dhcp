// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"strings"
	"time"
)

// The #875 observer's parsers are pure functions over verbatim alpine:3.20 output: busybox never prints `proto ra`.

// V6IfaceFromAddrShow returns the interface carrying addr in `ip -6 -o addr show scope global` output, "" when absent.
// The address is a whole field split at its prefix length, never a substring (#875); a caller passing an address with
// a prefix length matches nothing.
func V6IfaceFromAddrShow(out, addr string) string {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		for _, field := range f[2:] {
			if a, _, ok := strings.Cut(field, "/"); ok && a == addr {
				return strings.TrimSuffix(f[1], ":")
			}
		}
	}
	return ""
}

// HasLinkLocalDefaultRoute reports a default route via a link-local next hop in `ip -6 route show default` output.
// busybox prints no proto field; DHCPv6 carries no router (RFC 9915 section 21) and the plugin sets no IPv6 gateway,
// so such a route came from a Router Advertisement.
func HasLinkLocalDefaultRoute(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "default") && strings.Contains(line, "via fe80:") {
			return true
		}
	}
	return false
}

// CountDefaultRoutes counts default routes in `ip -6 route show default` output, ignoring busybox's trailing blank line.
// The plugin owns the IPv6 gateway and writes accept_ra=0, so one route is the claim and two means the kernel added one (#821).
func CountDefaultRoutes(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "default") {
			n++
		}
	}
	return n
}

// ResolvNameservers returns resolv.conf nameservers in order, keeping the RFC 4007 section 11 scope zone the plugin writes.
func ResolvNameservers(out string) []string {
	var got []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			got = append(got, f[1])
		}
	}
	return got
}

// SysctlReadFailed reports whether out is a failed read rather than a value.
func SysctlReadFailed(out string) bool {
	return strings.TrimSpace(out) == "" ||
		strings.Contains(out, "No such file") ||
		strings.Contains(out, "can't open")
}

// CountDHCPv6Binds counts dnsmasq DHCPREPLY lines, one per bind or renewal, that mention every needle case-insensitively.
// The plugin's DUID is a DUID-LL over the MAC and dnsmasq logs it on the line, so a MAC needle scopes it to one
// container (#875).
func CountDHCPv6Binds(log string, needles ...string) int {
	lowered := make([]string, 0, len(needles))
	for _, n := range needles {
		lowered = append(lowered, strings.ToLower(n))
	}
	count := 0
	for _, line := range strings.Split(log, "\n") {
		l := strings.ToLower(line)
		if !strings.Contains(l, "dhcpreply") {
			continue
		}
		all := true
		for _, n := range lowered {
			if !strings.Contains(l, n) {
				all = false
				break
			}
		}
		if all {
			count++
		}
	}
	return count
}

// dnsmasqStamp is the syslog-format prefix dnsmasq writes under --log-facility=-, with no year and no zone.
const dnsmasqStamp = "Jan _2 15:04:05"

// LastDHCPv6BindAt returns the server's stamp on the last DHCPREPLY matching every needle, the renewal timer's start.
// dnsmasq runs on the test's host, so the stamp compares with time.Now(). ref supplies the year, rolled back one if
// the result is in the future; an unparseable stamp is not found, never the zero time (#103).
func LastDHCPv6BindAt(log string, ref time.Time, needles ...string) (time.Time, bool) {
	lowered := make([]string, 0, len(needles))
	for _, n := range needles {
		lowered = append(lowered, strings.ToLower(n))
	}
	var found time.Time
	ok := false
	for _, line := range strings.Split(log, "\n") {
		l := strings.ToLower(line)
		if !strings.Contains(l, "dhcpreply") {
			continue
		}
		all := true
		for _, n := range lowered {
			if !strings.Contains(l, n) {
				all = false
				break
			}
		}
		if !all || len(line) < len(dnsmasqStamp) {
			continue
		}
		ts, err := time.ParseInLocation(dnsmasqStamp, line[:len(dnsmasqStamp)], ref.Location())
		if err != nil {
			continue
		}
		ts = ts.AddDate(ref.Year(), 0, 0)
		if ts.After(ref.Add(24 * time.Hour)) {
			ts = ts.AddDate(-1, 0, 0)
		}
		found, ok = ts, true
	}
	return found, ok
}
