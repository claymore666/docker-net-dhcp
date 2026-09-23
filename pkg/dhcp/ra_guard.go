// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
)

// DHCPv6 carries no router (RFC 9915 section 21), and RFC 5942 section 4 rule 1 and RFC 9915 section 18.2.10.1 forbid
// deriving an on-link prefix from an assigned address, so advertisements are processed on the managed path too; the
// library does it and the plugin applies the result (#875, #821).

// accept_ra=0 because the plugin now supplies the gateway, MTU, resolvers and routes; the kernel would be a second
// writer installing its own `proto ra` default route (#821). The write does not purge routes the kernel already
// installed; the caller purges. autoconf=0 keeps the kernel from forming an address the plugin does not know; measured
// on kernel 6.12.107+deb13-amd64, at accept_ra=0 and autoconf=0 a link formed no address and installed no route through
// twelve seconds of advertisements (#821).

// keep_addr_on_down: measured at 1.9.0, a link down/up flushed every global IPv6 address and kept IPv4; it keeps only a
// permanent address, so the finite DHCPv6 one returns at the next renewal (#911). addr_gen_mode is netlink-only; at
// 1.9.0 dhcpcd's -6 client moved it 0 -> 1, and with dhcpcd gone nothing sets it.

// No read-only /proc/sys shield as in 1.9.0 (D30 Q3, #911): nothing in the container writes these knobs unless
// something there chooses to, the knobs are reachable over netlink anyway, and a privileged process in the container
// can undo the guard, neither prevented nor counted.
const (
	// sysctlIPv6ConfDir is the per-interface IPv6 configuration tree, which is per network namespace.
	sysctlIPv6ConfDir = "/proc/sys/net/ipv6/conf"

	// raAcceptValue leaves advertisement processing to the plugin's own client (#821).
	raAcceptValue = "0"
	// raAutoconfValue keeps the kernel from forming an address beside the leased one (#821).
	raAutoconfValue = "0"
	// raKeepAddrValue keeps configured addresses across a carrier loss.
	raKeepAddrValue = "1"
)

// A table, so a knob's write and its read-back cannot drift apart; raGuardSteps derives every step from it (#911).

// raGuardKnob is one sysctl the guard writes and reads back.
type raGuardKnob struct {
	// name is the sysctl leaf under /proc/sys/net/ipv6/conf/<iface>/.
	name string
	// value is what the guard writes and then verifies.
	value string
}

func raGuardKnobs() []raGuardKnob {
	return []raGuardKnob{
		{name: "accept_ra", value: raAcceptValue},
		{name: "autoconf", value: raAutoconfValue},
		{name: "keep_addr_on_down", value: raKeepAddrValue},
	}
}

// Exported so the integration suite iterates this table instead of a hand copy that missed added knobs; a fresh map per
// call keeps the observed code from rewriting the observer's expectations (#875).

// RouterAdvertGuardContract is the guard's sysctl contract as data: knob name to the value the guard holds it at.
func RouterAdvertGuardContract() map[string]string {
	knobs := raGuardKnobs()
	out := make(map[string]string, len(knobs))
	for _, k := range knobs {
		out[k.name] = k.value
	}
	return out
}

// raGuardPath is one knob's sysctl path for iface.
func raGuardPath(dir, iface, knob string) string {
	return path.Join(dir, iface, knob)
}

// RouterAdvertGuardResult is what one application of the guard did, Failures counting write and read-back steps (#911).
type RouterAdvertGuardResult struct {
	Failures int
	Err      error
}

// The caller supplies both namespaces: the network one picks the link, the mount one makes the read-only managed-plugin
// /proc/sys writable; pkg/plugin/v6_link.go is the one production caller. The read-back is kept because a /proc/sys
// write can succeed for a value the kernel then ignores. Never fatal, but counted, since a guard that did not take
// looks healthy (#911).

// ApplyRouterAdvertGuard writes the three knobs on iface under dir and reads each one back.
func ApplyRouterAdvertGuard(dir, iface string) RouterAdvertGuardResult {
	var (
		res  RouterAdvertGuardResult
		errs []error
	)
	for _, k := range raGuardKnobs() {
		p := raGuardPath(dir, iface, k.name)
		if err := os.WriteFile(p, []byte(k.value+"\n"), 0o644); err != nil {
			res.Failures++
			errs = append(errs, fmt.Errorf("write %v=%v: %w", p, k.value, err))
			// A failed write on a knob that already holds the value is not a second failure (#911).
		}
		got, err := os.ReadFile(p)
		if err != nil {
			res.Failures++
			errs = append(errs, fmt.Errorf("read back %v: %w", p, err))
			continue
		}
		if strings.TrimSpace(string(got)) != k.value {
			res.Failures++
			errs = append(errs, fmt.Errorf("%v reads %q after a write of %q",
				p, strings.TrimSpace(string(got)), k.value))
		}
	}
	res.Err = errors.Join(errs...)
	return res
}
