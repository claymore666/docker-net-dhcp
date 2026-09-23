// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"strings"

	"github.com/claymore666/dhcp-golib/proto"
)

// IPv6Modes is every `ipv6_mode` value, derived from proto.Mode6 so the option and the machine are one enumeration
// (#817).
func IPv6Modes() []string {
	all := proto.AllModes6()
	out := make([]string, 0, len(all))
	for _, m := range all {
		out = append(out, m.String())
	}
	return out
}

// Unset and `dhcp` are both proto.Mode6's zero value, but `dhcp` switches IPv6 on, so set is returned apart (#817); an
// unknown value is refused, since resolving it to zero would silently pick `dhcp`.

// ParseIPv6Mode turns an `ipv6_mode` value into the library's mode and reports whether the option was set.
func ParseIPv6Mode(v string) (mode proto.Mode6, set bool, err error) {
	if v == "" {
		return proto.Mode6Off, false, nil
	}
	for _, m := range proto.AllModes6() {
		if m.String() == v {
			return m, true, nil
		}
	}
	return proto.Mode6Off, false, fmt.Errorf("ipv6_mode %q is not one of %s", v, strings.Join(IPv6Modes(), ", "))
}

// Negative, not zero: proto.Params6.AutoFallback reads zero as DefaultAutoFallback, so a zero here would fall back
// anyway (#817).
const strictAutoFallback proto.Duration = -1

// A second spelling of the unexported proto.Mode6.formsAddresses; TestIPv6ModeFormsAddresses_AgreesWithTheLibrary pins
// the two together (#817).

// IPv6ModeFormsAddresses reports whether a mode can form an address from an advertised Prefix Information option.
func IPv6ModeFormsAddresses(m proto.Mode6) bool {
	return m == proto.Mode6SLAAC || m == proto.Mode6Auto
}
