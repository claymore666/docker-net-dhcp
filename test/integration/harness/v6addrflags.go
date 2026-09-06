// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No `//go:build integration` tag, for the reason raguard_parse.go
// gives: this is a pure function over bytes, so it is driven in the
// fast lane against VERBATIM strings captured from the image the suite
// actually runs containers in.
package harness

import (
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// V6AddrFlags is what the container's kernel says about one address:
// the three bits that decide whether the DHCPv6 lease was installed the
// way the chassis claims it installs it (D30 Q1 -- the library ran
// duplicate-address detection, so the kernel is told not to repeat it).
//
// Found is separate from the three booleans on purpose. An address that
// is not on the link at all reads as "no NODAD, not tentative, not
// dadfailed", which is indistinguishable from a healthy address by any
// caller that only looks at the bits -- and it is the exact shape a
// mis-derived interface name or a typo'd address produces.
type V6AddrFlags struct {
	Found     bool
	NoDAD     bool
	Tentative bool
	DADFailed bool
	// Line is the verbatim line the flags were read from, for the
	// failure message. Empty when Found is false.
	Line string
}

// V6AddrFlagsFromAddrShow reads the flags of addr out of the output of
// `ip -6 -o addr show`.
//
// # WHY THIS IS NOT A strings.Contains ON "nodad"
//
// MEASURED 2026-09-06 on this box, the same address installed with
// IFA_F_NODAD, read by the two `ip` implementations this suite meets:
//
//	iproute2 6.x:      ... scope global nodad dynamic \ valid_lft 300sec ...
//	busybox 1.36.1:    ... scope global dynamic flags 02 \ valid_lft 300sec ...
//
// alpine:3.20 -- the image the suite runs containers in -- ships the
// busybox one, and it has no name for IFA_F_NODAD: it prints the bits
// it cannot name as a residual `flags <hex>`. An observer keyed on the
// word "nodad" is therefore an observer that can only ever fail inside
// the shipped image, which is the same defect raguard_parse.go's header
// records against `proto ra` and the reason that file exists.
//
// Busybox DOES name tentative, dadfailed, deprecated, secondary and
// dynamic (MEASURED the same way), so those arrive as words from both
// tools -- but the residual `flags` word is read for them too, because
// "this tool names it today" is not a property to build an observer on.
//
// The bit values come from the kernel headers via x/sys/unix rather
// than being spelled here, so this cannot drift from what the plugin
// sets.
//
// The address is matched as a WHOLE FIELD split at its prefix length,
// the same rule V6IfaceFromAddrShow uses and for the same #875 reason:
// a substring match answers yes for `fd00::3` on a line carrying
// `fd00::32/128`.
//
// The bound, named rather than claimed away: only the fields BEFORE the
// backslash are read. Both tools put the lifetimes after it, and
// `valid_lft`/`preferred_lft` are not flags; a tool that moved a flag
// behind the backslash would read here as an absent flag, which is the
// safe direction for every caller (they assert a flag is PRESENT).
func V6AddrFlagsFromAddrShow(out, addr string) V6AddrFlags {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		at := -1
		for i, field := range fields {
			if a, _, ok := strings.Cut(field, "/"); ok && a == addr {
				at = i
				break
			}
		}
		if at < 0 {
			continue
		}

		f := V6AddrFlags{Found: true, Line: strings.TrimSpace(line)}
		var residual uint64
		for i := at + 1; i < len(fields); i++ {
			switch fields[i] {
			case `\`:
				i = len(fields)
			case "nodad":
				f.NoDAD = true
			case "tentative":
				f.Tentative = true
			case "dadfailed":
				f.DADFailed = true
			case "scope", "proto":
				// The two keywords in `ip addr` output whose VALUE is
				// the next field. Skipping the value keeps a scope or
				// protocol that happens to spell a flag name from
				// reading as that flag.
				i++
			case "flags":
				if i+1 < len(fields) {
					if v, err := strconv.ParseUint(fields[i+1], 16, 32); err == nil {
						residual |= v
					}
					i++
				}
			}
		}
		f.NoDAD = f.NoDAD || residual&unix.IFA_F_NODAD != 0
		f.Tentative = f.Tentative || residual&unix.IFA_F_TENTATIVE != 0
		f.DADFailed = f.DADFailed || residual&unix.IFA_F_DADFAILED != 0
		return f
	}
	return V6AddrFlags{}
}
