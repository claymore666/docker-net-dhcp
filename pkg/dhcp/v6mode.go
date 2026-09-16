// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"fmt"
	"strings"

	"github.com/claymore666/dhcp-golib/proto"
)

// IPv6Modes is every value `ipv6_mode` accepts, in the library's own
// order.
//
// Derived rather than written, for the reason ConflictModes is derived:
// the option a user writes and the value proto.Machine6 switches on are
// ONE enumeration. proto.Mode6's own comment states that contract from
// the other side -- "They are the chassis's four option values, so that
// the option a user writes and the value the machine switches on are one
// enumeration rather than two that must be kept in agreement" -- and a
// literal list here is the second one it warns about.
func IPv6Modes() []string {
	all := proto.AllModes6()
	out := make([]string, 0, len(all))
	for _, m := range all {
		out = append(out, m.String())
	}
	return out
}

// ParseIPv6Mode turns the operator's `ipv6_mode` value into the
// library's mode, and says whether the option was set at all.
//
// THE THIRD RESULT IS NOT A CONVENIENCE. proto.Mode6's zero value is
// Mode6DHCP, so an unset option and `ipv6_mode=dhcp` are the same
// number; they are not the same instruction, because `ipv6_mode=dhcp`
// switches IPv6 on and an unset option leaves that to `ipv6`. A parser
// that folded the two would make `ipv6_mode` unable to mean anything on
// a network that had not also set `ipv6`, which is the whole of what
// the option is for.
//
// A value outside the set is REFUSED rather than resolved to the zero
// value, on conflict_check's rule: the zero value is `dhcp`, and a typo
// would quietly buy the mode an operator was trying to move away from.
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

// strictAutoFallback is what proto.Params6.AutoFallback has to hold for
// `auto` to fail the endpoint instead of forming an address from an
// advertised prefix when the DHCPv6 server stays silent.
//
// IT IS NEGATIVE AND NOT ZERO, and that is the whole reason this
// constant exists rather than a literal at the assignment. Zero is the
// library's "the caller did not say", which resolves to
// proto.DefaultAutoFallback -- half the router-discovery window -- so a
// strict setting that shipped as a zero would read as honoured at every
// layer above and fall back anyway. proto.Params6.AutoFallback states
// it: "Zero is the default, DefaultAutoFallback. NEGATIVE IS STRICT".
const strictAutoFallback proto.Duration = -1

// IPv6ModeFormsAddresses reports whether a mode can form an address
// from a Router Advertisement's Prefix Information option rather than
// asking a server for one.
//
// IT IS A SECOND SPELLING OF proto.Mode6.formsAddresses, WHICH IS
// UNEXPORTED, and the duplication is bounded rather than hidden: the
// library's own validate() refuses a Params6 with no LinkAddr in
// exactly these modes, so TestIPv6ModeFormsAddresses_AgreesWithTheLibrary
// drives proto.New6 once per declared mode and fails if this function
// and the library ever disagree. The predicate is spelled here rather
// than derived at every call because the call sites are CreateNetwork
// and a log line, and building a machine to answer a question about an
// option value would be a state machine per `docker network create`.
func IPv6ModeFormsAddresses(m proto.Mode6) bool {
	return m == proto.Mode6SLAAC || m == proto.Mode6Auto
}
