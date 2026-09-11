// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"strings"
	"time"
)

// The #875 observer reads two things out of the container's own `ip`
// output. Both parsers live here, as pure functions, for one reason:
// the first version of this observer was absence-driven against a
// probe image with full iproute2 and keyed on `proto ra` — a field
// BUSYBOX NEVER PRINTS. It could not have passed in alpine:3.20, which
// is the image the suite actually runs, and that was only discovered
// after a full integration round went red.
//
// Pure functions pinned to VERBATIM captured output are the fix: they
// run in the fast lane, against real strings from the real image,
// instead of being validated in a world the observer does not run in.

// V6IfaceFromAddrShow returns the interface carrying addr, given the
// output of `ip -6 -o addr show scope global`. Empty string when the
// address is not present.
//
// Busybox renders one interface per line as
// "2: NAME    inet6 ADDR/LEN scope global \  valid_lft ...", so the
// device is field 2. Full iproute2 renders "2: NAME    inet6 ..." too
// but some versions suffix the index differently, hence the TrimSuffix.
//
// The address is matched as a WHOLE FIELD, split at its prefix length,
// and not as a substring of the line (#875). A substring
// test answers yes for `fd00::3` on a line carrying `fd00::32/128`, and
// it would then return that OTHER interface — after which the observer
// reads its sysctls from the wrong path and reports whatever it finds
// there. The fixture prefixes make a collision impossible today, which
// is exactly why nothing would have caught it; and this PR is what
// makes a second global address on one link possible at all
// (`autoconf=1`), so the bound is closed here rather than written down.
// CountDHCPv6Binds keeps the substring behaviour deliberately and has
// it pinned as such.
//
// The remaining bound, named rather than claimed away: a caller passing
// an address WITH a prefix length ("fd00::42/64") now matches nothing,
// because the field is split before the comparison. Every caller passes
// a bare address, and the empty return is the safe direction — the
// observer reports "not found" rather than the wrong interface.
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

// HasLinkLocalDefaultRoute reports whether the output of
// `ip -6 route show default` contains a default route via a
// link-local next hop.
//
// Keyed on the VIA ADDRESS, not on a `proto ra` field, because busybox
// prints no proto field at all. The property is protocol-level rather
// than tool-level: DHCPv6 carries no router (RFC 9915 §21) and this
// plugin sets no IPv6 gateway, so a default route via fe80::/10 can
// only have been learned from a Router Advertisement.
func HasLinkLocalDefaultRoute(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "default") && strings.Contains(line, "via fe80:") {
			return true
		}
	}
	return false
}

// SysctlReadFailed reports whether out is a failed read rather than a
// value. A read that failed must never be scored as a wrong value or,
// worse, as a pass: it means the observer measured nothing.
func SysctlReadFailed(out string) bool {
	return strings.TrimSpace(out) == "" ||
		strings.Contains(out, "No such file") ||
		strings.Contains(out, "can't open")
}

// CountDHCPv6Binds counts dnsmasq DHCPREPLY lines in log that mention
// every one of needles (case-insensitively). dnsmasq logs one
// DHCPREPLY per blessed REQUEST/RENEW, so this counts binds and
// renewals, not solicits or advertisements.
//
// Pure, and unit-tested in the fast lane on purpose (#875). This
// predicate decides when the RA-guard assertions are allowed to run;
// if it matches too eagerly those assertions fire before the guard has
// executed and the whole gate goes quietly vacuous, which is the exact
// bug it was written to close. A matcher that lives only behind the
// `integration` build tag is one nothing can drive.
//
// Callers scope it to a single endpoint by passing the container's MAC
// alongside the address: dnsmasq puts the client DUID on the reply
// line, and the plugin pins that DUID as a DUID-LL over the MAC, so
// the MAC distinguishes this container's bind from a reply left in the
// shared fixture log by an earlier container that held the same pooled
// address.
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

// dnsmasqStamp is the fixed-width prefix dnsmasq writes on every line
// under --log-facility=- : "Aug 28 13:57:39". Syslog's format, so it
// carries no year and no zone, which is why LastDHCPv6BindAt takes the
// reference time rather than calling time.Now() itself.
const dnsmasqStamp = "Jan _2 15:04:05"

// LastDHCPv6BindAt returns the moment the SERVER stamped on the last
// DHCPREPLY line matching every needle, and whether such a line was
// found with a readable stamp.
//
// WHY A TEST WANTS THE SERVER'S CLOCK. A renewal timer is the server's:
// dnsmasq starts T1 when it sends the reply, not when the client's
// address becomes visible to `ip -6 addr`. A test that anchors its
// window on the address surfacing is anchored some unknown delay LATER
// than the timer it is measuring, and spends that delay out of its own
// margin. The stamp is already in the evidence the caller reads, so
// reading it is a re-derivation of the anchor, not a new instrument.
//
// Both clocks are the same host's -- dnsmasq runs in the fixture beside
// the test -- so the value is directly comparable to time.Now().
//
// Resolution is one second and the stamp carries no year, so `ref`
// supplies the year and the result is rolled back one year if that
// would put it in the future (the 31 December boundary). A line whose
// stamp cannot be parsed is reported as not found rather than as the
// zero time: an unreadable clock must not read as "long ago", which
// would make every window trivially satisfied.
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
