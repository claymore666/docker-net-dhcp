// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import "regexp"

// ValidIfaceName accepts only a kernel-legal network interface name:
// 1–15 characters (IFNAMSIZ-1), starting with an alphanumeric and
// otherwise limited to alphanumerics, dot, dash and underscore.
//
// WHAT IT GUARDS HAS CHANGED, AND SAYING SO IS THE POINT. Until 2.0
// the interface name was interpolated into a dhcpcd argv, where
// getopt's permutation re-read a name shaped like `-c/out/evil.sh` as
// an option and ran that script as uid 0 (#706). There is no argv now:
// the name is passed to the library, which resolves it with
// net.InterfaceByName. The old mechanism is gone and the rule is kept
// on its own merits — a name the kernel would refuse should fail the
// Docker request loudly rather than at the socket — but it is no
// longer load-bearing against an injection, and a rule whose stated
// reason has expired is a rule someone relaxes for the wrong reason.
//
// Exported because pkg/plugin applies it at CreateNetwork and
// CreateEndpoint, one step earlier (#705).
var ValidIfaceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,14}$`).MatchString

// SafeValue reports whether a server-chosen string can be carried into
// the places the plugin puts one — a resolv.conf line, a log line, a
// hostname — without changing the structure of the sink.
//
// It used to be SafeDirectiveValue, and the name meant a dhcpcd
// configuration directive. There is no configuration file any more, so
// the directive half of the name described nothing; the rule it
// enforces is unchanged and still needed, because a control character
// in an option value reaches resolv.conf and the log either way.
//
// The rule is deliberately about control characters and not about
// well-formedness: an over-strict check rejects hostnames Docker and
// real deployments accept — underscores, for one — and breaks
// containers that are doing nothing wrong. Structure is what must be
// protected.
func SafeValue(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
