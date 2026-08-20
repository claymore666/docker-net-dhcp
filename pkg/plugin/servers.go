// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// serverPolicy is the validated form of the dhcp_servers (#111) and
// dhcp_deny_servers (#669) network options.
//
// The two options answer different questions and are deliberately not
// merged: Prefer is an ORDERING ("which of these wins when several
// answer"), Deny is a PERMISSION ("this one never answers for us").
//
// Both are enforced by dhcpcd's `whitelist` / `blacklist` directives,
// which carry two properties the rest of this file exists to respect:
//
//  1. They match the packet's IP SOURCE address, not the Server
//     Identifier (option 54) it advertises — dhcpcd 10.3.2
//     src/dhcp.c:3641 sets `from` from `ip->ip_src`, and :3181/:3190
//     test that. Behind a DHCP relay every offer is sourced from the
//     relay, so neither option can tell servers apart there.
//  2. A configured whitelist DISABLES the blacklist outright: in
//     src/dhcp.c:3181-3196 the blacklist is only consulted in the
//     WHTLST_NONE branch. Emitting both directives would therefore make
//     a deny-list silently inert on any network that also sets a
//     preference.
//
// (2) is why Deny is subtracted from Prefer here, at parse time, rather
// than left to dhcpcd to compose. After resolveServerPolicy there is one
// truth about what is allowed, and the renderer never emits both kinds
// of directive at once.
type serverPolicy struct {
	// Prefer is the operator's ordered preference list with denied
	// entries already removed. Empty means no preference.
	Prefer []netip.Addr
	// Deny is the deny-list. Empty means nothing is denied.
	Deny []netip.Addr
}

// IsZero reports whether the policy asks for nothing, which is the
// default for every network that sets neither option.
func (p serverPolicy) IsZero() bool { return len(p.Prefer) == 0 && len(p.Deny) == 0 }

// parseServerList parses one comma-separated option value into unique
// IPv4 addresses, preserving order.
//
// IPv6 is rejected rather than ignored. dhcpcd stores both lists as
// in_addr_t (src/if-options.c:1436-1457) and dhcp6.c never consults
// them, so a v6 entry would parse, apply to nothing, and leave the
// operator believing a server was ranked or denied when it was not.
// The same reasoning as the validate_dhcp carve-out in
// validateModeOptions: refuse loudly instead of no-op'ing quietly.
func parseServerList(option, value string) ([]netip.Addr, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}

	var out []netip.Addr
	seen := make(map[netip.Addr]struct{})
	for _, field := range strings.Split(value, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			return nil, fmt.Errorf("%w: %s has an empty entry", util.ErrInvalidServerList, option)
		}
		addr, err := netip.ParseAddr(field)
		if err != nil {
			return nil, fmt.Errorf("%w: %s entry %q is not an IP address", util.ErrInvalidServerList, option, field)
		}
		if !addr.Is4() {
			return nil, fmt.Errorf("%w: %s entry %q is not IPv4; both lists are DHCPv4-only",
				util.ErrInvalidServerList, option, field)
		}
		if _, dup := seen[addr]; dup {
			return nil, fmt.Errorf("%w: %s lists %q twice", util.ErrInvalidServerList, option, field)
		}
		seen[addr] = struct{}{}
		out = append(out, addr)
	}
	return out, nil
}

// resolveServerPolicy validates both options together and returns the
// policy the config renderer works from.
func resolveServerPolicy(opts DHCPNetworkOptions) (serverPolicy, error) {
	prefer, err := parseServerList("dhcp_servers", opts.DHCPServers)
	if err != nil {
		return serverPolicy{}, err
	}
	deny, err := parseServerList("dhcp_deny_servers", opts.DenyServers)
	if err != nil {
		return serverPolicy{}, err
	}

	denied := make(map[netip.Addr]struct{}, len(deny))
	for _, a := range deny {
		denied[a] = struct{}{}
	}

	// Subtract, so the whitelist dhcpcd sees never contains a denied
	// address — see the type comment for why this cannot be left to
	// dhcpcd's own precedence.
	kept := prefer[:0:0]
	for _, a := range prefer {
		if _, bad := denied[a]; bad {
			continue
		}
		kept = append(kept, a)
	}

	// A preference list that denies its way to empty is a contradiction,
	// and one that would otherwise degrade into "no preference at all" —
	// i.e. silently accept any server, which is the opposite of what
	// both options were set to achieve. Fail the network create.
	if len(prefer) > 0 && len(kept) == 0 {
		return serverPolicy{}, fmt.Errorf(
			"%w: every dhcp_servers entry is also in dhcp_deny_servers, leaving no server to lease from",
			util.ErrInvalidServerList)
	}

	return serverPolicy{Prefer: kept, Deny: deny}, nil
}

// allowList is the set of servers the client may accept from, as
// dhcpcd `whitelist` arguments. Empty means "impose no whitelist".
//
// The PERSISTENT client gets the whole preference list rather than one
// tier: it must be able to renew and rebind after the preferred server
// goes away, and a whitelist pinned to the tier that won acquisition
// would strand the endpoint with no lease instead of failing over.
// Ordering is not expressible to dhcpcd, so preference is enforced at
// acquisition (see tiers) and the lease then stays with whoever granted
// it — DHCP renewal is unicast to that server.
func (p serverPolicy) allowList() []string {
	return addrsToStrings(p.Prefer)
}

// denyList is the set of servers to reject, as dhcpcd `blacklist`
// arguments. It is empty whenever a preference list exists, because
// dhcpcd would ignore a blacklist in that case anyway (dhcp.c:3181) —
// the denial is already carried by the subtraction in
// resolveServerPolicy, and emitting a directive dhcpcd will not read
// would misrepresent what is enforced.
func (p serverPolicy) denyList() []string {
	if len(p.Prefer) > 0 {
		return nil
	}
	return addrsToStrings(p.Deny)
}

// tiers is the acquisition ladder: one tier per preferred server, in
// operator order, each a whitelist restricted to that single server.
// Empty when no preference is configured, meaning "one attempt, no
// restriction".
//
// The ladder subdivides the existing acquisition budget and never
// extends it — see acquisitionTiers' caller. #403 and #417 both concern
// how tight that budget already is.
func (p serverPolicy) tiers() [][]string {
	if len(p.Prefer) == 0 {
		return nil
	}
	out := make([][]string, 0, len(p.Prefer))
	for _, a := range p.Prefer {
		out = append(out, []string{a.String()})
	}
	return out
}

func addrsToStrings(in []netip.Addr) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, a := range in {
		out = append(out, a.String())
	}
	return out
}

// acquisitionAttempt is one pass of the initial DHCP exchange: which
// servers it will accept from, and how long it gets.
type acquisitionAttempt struct {
	Allow  []string
	Deny   []string
	Budget time.Duration
}

// acquisitionAttempts expands a policy into the ordered attempts the
// initial acquisition should make within total.
//
// The ladder DIVIDES total; it never extends it. A preference list must
// not make `docker run` slower than it is today — the one-shot
// acquisition at CreateEndpoint already runs against a tight ceiling
// (#403 asks whether a loaded host can hit it, #417 removes ~9.8s of
// dead time from the same path), so buying ordering with extra seconds
// there would trade a rare misconfiguration for a common regression.
//
// v6 always gets a single unrestricted attempt: dhcpcd's whitelist and
// blacklist are DHCPv4-only (dhcp6.c never reads them, and
// if-options.c stores both as in_addr_t), so applying them to a v6
// exchange would restrict nothing while implying it had.
func acquisitionAttempts(pol serverPolicy, v6 bool, total time.Duration) []acquisitionAttempt {
	if v6 || len(pol.Prefer) == 0 {
		return []acquisitionAttempt{{Deny: denyForFamily(pol, v6), Budget: total}}
	}

	tiers := pol.tiers()
	// Integer division deliberately: the remainder is dropped rather
	// than handed to the last tier, so the sum of the slices can only
	// be <= total.
	each := total / time.Duration(len(tiers))
	out := make([]acquisitionAttempt, 0, len(tiers))
	for _, tier := range tiers {
		out = append(out, acquisitionAttempt{Allow: tier, Budget: each})
	}
	return out
}

// denyForFamily returns the blacklist entries that apply to a family.
// v6 gets none, for the same reason acquisitionAttempts gives it no
// whitelist.
func denyForFamily(pol serverPolicy, v6 bool) []string {
	if v6 {
		return nil
	}
	return pol.denyList()
}

// acquireWithPolicy runs one initial DHCP acquisition through the
// network's server-preference ladder and returns the first lease won.
//
// Every acquisition path goes through here rather than looping over
// acquisitionAttempts itself. There are two of them (the bridge path in
// CreateEndpoint and the parent-attached path), and a preference list
// that silently applied to one of them would be worse than no feature
// at all — the operator would see it work and not know which half.
//
// base carries the per-endpoint identity and hints; this function owns
// only the per-attempt server restriction, the per-attempt deadline and
// the counters.
func (p *Plugin) acquireWithPolicy(
	ctx context.Context,
	iface string,
	pol serverPolicy,
	v6 bool,
	budget time.Duration,
	endpointID string,
	base dhcp.DHCPClientOptions,
) (dhcp.Info, error) {
	attempts := acquisitionAttempts(pol, v6, budget)

	var (
		info    dhcp.Info
		lastErr error
	)
	for i, attempt := range attempts {
		clientOpts := base
		clientOpts.V6 = v6
		// Never both — see serverPolicy for why dhcpcd cannot be handed
		// a whitelist and a blacklist together.
		clientOpts.AllowServers = attempt.Allow
		clientOpts.DenyServers = attempt.Deny

		attemptCtx, cancel := context.WithTimeout(ctx, attempt.Budget)
		info, lastErr = dhcp.GetIP(attemptCtx, iface, &clientOpts)
		cancel()
		if lastErr == nil {
			return info, nil
		}
		if i < len(attempts)-1 {
			p.dhcpServerTierFallbacks.Add(1)
			log.WithFields(log.Fields{
				"endpoint": endpointID,
				"server":   attempt.Allow,
				"next":     attempts[i+1].Allow,
			}).Warn("Preferred DHCP server did not answer; trying the next in dhcp_servers")
		}
	}

	// Distinguish "the servers you named are all silent" from "DHCP is
	// broken". They are identical in a timeout log and call for
	// different operator action.
	if len(attempts) > 1 {
		p.dhcpServerPolicyExhausted.Add(1)
	}
	return info, lastErr
}
