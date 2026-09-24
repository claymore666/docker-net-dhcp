// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import "regexp"

// The name reaches no argv since 2.0, so this no longer guards the #706 injection; it stays so a name the kernel
// refuses fails the Docker request, and pkg/plugin applies it at CreateNetwork and CreateEndpoint (#705).

// ValidIfaceName accepts a kernel-legal interface name: 1-15 (IFNAMSIZ-1) alphanumerics, dots, dashes, underscores.
var ValidIfaceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,14}$`).MatchString

// Control characters only, not well-formedness: a stricter rule refuses hostnames Docker accepts, like underscores
// (#699).

// SafeValue reports whether a server-chosen string can enter resolv.conf, a log line or a hostname safely.
func SafeValue(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
