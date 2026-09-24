// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: a pure parser driven against verbatim output from the suite's container image.
package harness

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// V6AddrFlags is the container kernel's NODAD, tentative and dadfailed bits for one address (D30 Q1: the library ran
// DAD, so the kernel is told not to repeat it); Found tells an absent address from a clean one (#819).
type V6AddrFlags struct {
	Found     bool
	NoDAD     bool
	Tentative bool
	DADFailed bool
	// Deprecated is IFA_F_DEPRECATED, the kernel's word for an elapsed preferred lifetime (RFC 4862 section 5.5.4), which #819's deprecation arm reads.
	Deprecated bool
	// Valid and Preferred are the kernel's two lifetimes; Lifetimes says whether a pair was read, since unprinted is not zero.
	Lifetimes bool
	Valid     V6Lifetime
	Preferred V6Lifetime
	// Line is the verbatim line the flags were read from, empty when Found is false.
	Line string
}

// V6AddrFlagsFromAddrShow reads addr's flags from `ip -6 -o addr show` output. Measured 2026-09-06: iproute2 6.x
// prints `nodad`, alpine:3.20's busybox 1.36.1 prints `flags 02`, so the residual `flags <hex>` word is read for every
// flag, with bit values from x/sys/unix. The address is a whole field (#875). Only fields before the backslash are read;
// both tools put the lifetimes after it (#819).
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
				f.Valid, f.Preferred, f.Lifetimes = v6LifetimesFrom(fields[i+1:])
				i = len(fields)
			case "deprecated":
				f.Deprecated = true
			case "nodad":
				f.NoDAD = true
			case "tentative":
				f.Tentative = true
			case "dadfailed":
				f.DADFailed = true
			case "scope", "proto":
				// In `ip addr` output the value of these two keywords is the next field.
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
		f.Deprecated = f.Deprecated || residual&unix.IFA_F_DEPRECATED != 0
		return f
	}
	return V6AddrFlags{}
}

// V6Lifetime is one lifetime as `ip addr` prints it, seconds or forever; on the wire 0xFFFFFFFF is infinity and 0 is expiry.
type V6Lifetime struct {
	Seconds int
	Forever bool
}

func (l V6Lifetime) String() string {
	if l.Forever {
		return "forever"
	}
	return fmt.Sprintf("%dsec", l.Seconds)
}

// v6LifetimesFrom reads the `valid_lft X preferred_lft Y` pair after the backslash by keyword; ok needs both values (#819).
func v6LifetimesFrom(tail []string) (valid, preferred V6Lifetime, ok bool) {
	var gotValid, gotPreferred bool
	for i := 0; i+1 < len(tail); i++ {
		switch tail[i] {
		case "valid_lft":
			valid, gotValid = parseV6Lifetime(tail[i+1])
		case "preferred_lft":
			preferred, gotPreferred = parseV6Lifetime(tail[i+1])
		}
	}
	return valid, preferred, gotValid && gotPreferred
}

func parseV6Lifetime(s string) (V6Lifetime, bool) {
	if s == "forever" {
		return V6Lifetime{Forever: true}, true
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "sec"))
	if err != nil || n < 0 {
		return V6Lifetime{}, false
	}
	return V6Lifetime{Seconds: n}, true
}
