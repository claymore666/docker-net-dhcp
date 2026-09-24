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

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// serverPolicy is dhcp_servers (#111) and dhcp_deny_servers (#669), matched by proto.ServerPolicy on option 54,
// the Server Identifier; behind a relay the source address is the relay's for every offer. Deny is subtracted from
// Prefer at parse time, and an Allow list fails closed on a message with no option 54.
type serverPolicy struct {
	// Prefer is the operator's ordered preference list with denied entries already removed.
	Prefer []netip.Addr
	// Deny is the deny-list.
	Deny []netip.Addr
}

// IsZero reports whether the policy asks for nothing, the default for a network that sets neither option.
func (p serverPolicy) IsZero() bool { return len(p.Prefer) == 0 && len(p.Deny) == 0 }

// parseServerList refuses IPv6: option 54 is an IPv4 address and proto.Params6 has no policy field (#669).
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

	kept := prefer[:0:0]
	for _, a := range prefer {
		if _, bad := denied[a]; bad {
			continue
		}
		kept = append(kept, a)
	}

	// A preference list denied to empty would silently accept any server, so the create fails (#669).
	if len(prefer) > 0 && len(kept) == 0 {
		return serverPolicy{}, fmt.Errorf(
			"%w: every dhcp_servers entry is also in dhcp_deny_servers, leaving no server to lease from",
			util.ErrInvalidServerList)
	}

	return serverPolicy{Prefer: kept, Deny: deny}, nil
}

// allowList gives the persistent client the whole preference list: renewal is unicast to the granting server, and
// a whitelist pinned to one tier would strand the endpoint when that server goes away (#111).
func (p serverPolicy) allowList() []string {
	return addrsToStrings(p.Prefer)
}

func (p serverPolicy) denyList() []string {
	if len(p.Prefer) > 0 {
		return nil
	}
	return addrsToStrings(p.Deny)
}

// tiers is one single-server whitelist per preferred server, dividing the acquisition budget (#111, #403, #417).
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

type acquisitionAttempt struct {
	Allow  []string
	Deny   []string
	Budget time.Duration
}

// minAttemptBudget is a policy choice, not a measurement: the smallest slice that holds a netns entry, a raw
// socket, a DHCP round trip and RFC 5227's check (#731). TestAcquisitionAttempts_NoAttemptIsStarved pins the
// guarantee; a budget below it gets one attempt.
const minAttemptBudget = 3 * time.Second

// packTiers merges the tail, so the entries the operator ranked lowest lose their own attempt (#731).
func packTiers(tiers [][]string, n int) [][]string {
	if n < 1 || len(tiers) <= n {
		return tiers
	}
	out := make([][]string, 0, n)
	out = append(out, tiers[:n-1]...)
	tail := make([]string, 0, len(tiers)-n+1)
	for _, t := range tiers[n-1:] {
		tail = append(tail, t...)
	}
	return append(out, tail)
}

// acquisitionAttempts divides total and never extends it (#403, #417); v6 gets one unrestricted attempt, since
// proto.Params6 has no policy field (#669).
func acquisitionAttempts(pol serverPolicy, v6 bool, total time.Duration) []acquisitionAttempt {
	return acquisitionAttemptsWithFloor(pol, v6, total, minAttemptBudget)
}

// acquisitionAttemptsWithFloor takes the floor as an argument so the guarantee is tested at several floors (#731).
func acquisitionAttemptsWithFloor(pol serverPolicy, v6 bool, total, floor time.Duration) []acquisitionAttempt {
	if v6 || len(pol.Prefer) == 0 {
		return []acquisitionAttempt{{Deny: denyForFamily(pol, v6), Budget: total}}
	}

	// Dividing total by the list length made six servers 1.66 s each and failed every attempt (#731); the tail shares
	// one whitelist attempt, so the total never grows (#403, #417).
	tiers := pol.tiers()
	// Below the floor the whole budget goes to one attempt: twenty servers on 1.5 s once got 75 ms each (#731).
	maxAttempts := int(total / floor)
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if len(tiers) > maxAttempts {
		tiers = packTiers(tiers, maxAttempts)
	}
	each := total / time.Duration(len(tiers))
	out := make([]acquisitionAttempt, 0, len(tiers))
	for _, tier := range tiers {
		out = append(out, acquisitionAttempt{Allow: tier, Budget: each})
	}
	return out
}

func denyForFamily(pol serverPolicy, v6 bool) []string {
	if v6 {
		return nil
	}
	return pol.denyList()
}

// policyRestricted reads the attempts, not the policy, so the counter follows what was sent (#731).
func policyRestricted(attempts []acquisitionAttempt) bool {
	for _, a := range attempts {
		if len(a.Allow) > 0 || len(a.Deny) > 0 {
			return true
		}
	}
	return false
}

// dhcpGetIP is a seam for the ladder test, which pins one fallback count per step (#731).
var dhcpGetIP = dhcp.GetIP

func (p *Plugin) acquireWithPolicy(
	ctx context.Context,
	iface string,
	pol serverPolicy,
	v6 bool,
	budget time.Duration,
	endpointID string,
	base dhcp.DHCPClientOptions,
) (dhcp.Info, dhcp.RAObservation, error) {
	attempts := acquisitionAttempts(pol, v6, budget)

	var (
		info    dhcp.Info
		ra      dhcp.RAObservation
		lastErr error
	)
	for i, attempt := range attempts {
		clientOpts := base
		clientOpts.V6 = v6
		clientOpts.AllowServers = attempt.Allow
		clientOpts.DenyServers = attempt.Deny

		attemptCtx, cancel := context.WithTimeout(ctx, attempt.Budget)
		var attemptRA dhcp.RAObservation
		info, attemptRA, lastErr = dhcpGetIP(attemptCtx, iface, &clientOpts)
		ra = ra.Merge(attemptRA)
		cancel()
		if lastErr == nil {
			p.noteMainPrefixFallback(v6, info, base.MainPrefix6, endpointID)
			return info, ra, nil
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

	// A single-entry dhcp_servers that goes quiet counts as an exhausted policy too (#111).
	if policyRestricted(attempts) {
		p.dhcpServerPolicyExhausted.Add(1)
	}
	return info, ra, lastErr
}

// noteMainPrefixFallback counts once per endpoint here; the lease seam runs on every renewal (#818).
func (p *Plugin) noteMainPrefixFallback(v6 bool, info dhcp.Info, main netip.Prefix, endpointID string) {
	if p == nil || !v6 || !info.MainAddrFallback {
		return
	}
	p.ipv6MainPrefixUnmatched.Add(1)
	log.WithFields(log.Fields{
		"endpoint":         shortID(endpointID),
		"ipv6_main_prefix": main,
		"address":          info.IP,
	}).Warn("No address of this endpoint falls inside the network's ipv6_main_prefix; " +
		"Docker is being told the first advertised prefix's address instead")
}
