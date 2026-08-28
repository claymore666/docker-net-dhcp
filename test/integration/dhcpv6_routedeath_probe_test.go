// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	docker "github.com/docker/docker/client"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// A PROBE, not a guard. It exists to answer one question in #875 and is
// expected to be DELETED once it has: what removes the container's IPv6
// default route?
//
// WHY IT IS NEEDED, AND WHY READING THE CODE WILL NOT DO. The
// measurement that opened #875 shows the route present at +3s and gone
// by +13s. `accept_ra=0` explains a route that is not REFRESHED; it does
// not explain one that DISAPPEARS. The advertised Router Lifetime at
// that measurement was around 1800s, so a route that should have lived
// half an hour was gone in thirteen seconds. Something deleted it, and
// until that something is named, a fix to the sysctls may repair nothing
// at all.
//
// That 1800 is not a constant and re-measuring will not reproduce it.
// The fixture now PINS the advertised lifetime with an explicit
// --ra-param; before the pin the server derived it from the
// advertisement interval, which is RFC 4861 6.2.1's 3x
// MaxRtrAdvInterval. A later reading of a different lifetime is the pin
// working, not a new finding and not a contradiction of this paragraph.
//
// FIVE candidates. The first four came from the protocol review on
// #875; the fifth was found only when this probe was re-derived, and it
// had been hiding inside candidate 2's signature:
//
//  1. the netns move flushing the link's v6 addresses and routes. The
//     one-shot client runs at CreateEndpoint while the container-side
//     veth is still HOST-side; Join moves it, and the +3s/+13s window
//     straddles that move.
//  2. libnetwork's gateway programming around Join with an empty
//     GatewayIPv6.
//  3. the engine disabling IPv6 on a link with no v6 address, which
//     flushes both.
//  4. forwarding enabled in the namespace, purging RA-learned routers.
//  5. an advertisement carrying Router Lifetime 0, which RFC 4861 6.3.4
//     requires a host to honour by removing the router. Correct host
//     behaviour reacting to a misconfigured segment -- and it looks
//     exactly like candidate 2 unless the advertisements themselves are
//     placed in time. The on-link prefix route is that witness: it is
//     reset by every advertisement carrying a prefix option, so a
//     countdown running unbroken across a deletion says none arrived.
//
// WHAT THE EARLIER MEASUREMENT ALREADY EXCLUDES. The +3s/+13s
// observation was taken in a SINGLE isolated network namespace with the
// client run directly: no container, no libnetwork, no engine, no veth
// move. Candidates 1, 2 and 3 were therefore all ABSENT while the route
// died, which promotes 4 and 5 for THAT observation. It does not clear
// 1-3 for the container path, which has never been measured. That gap is
// what this probe is for.
//
// WHAT THE KERNEL ACTUALLY DOES (MEASURED, read from torvalds/linux
// master at VERSION 7.2.0 on 2026-08-28; line numbers drift, the
// conditionals do not):
//
//   - net/ipv6/addrconf.c addrconf_fixup_forwarding ends in
//     `if (newf) rt6_purge_dflt_routers(net);`. The guard is the VALUE
//     WRITTEN, not a change. A redundant write of 1 over 1 still purges.
//   - the netconf notification beside it IS change-gated,
//     `(!newf) ^ (!old)`. So the 1-over-1 write purges and emits NO
//     event. That is the ambiguous cell this probe reports rather than
//     resolves.
//   - a write to conf.DEFAULT.forwarding returns early and never
//     reaches the purge. Only conf.all and conf.<if> do. conf.default is
//     still read, to show it was checked.
//   - the purge argument is `net` and rt6_purge_dflt_routers walks every
//     table in the namespace. It is namespace-wide, not per-device.
//   - __rt6_purge_dflt_routers deletes a route when
//     `rt->fib6_flags & (RTF_DEFAULT | RTF_ADDRCONF)` -- EITHER flag
//     suffices -- AND `(!idev || idev->cnf.accept_ra != 2)`.
//   - rtm_to_fib6_config builds a userspace route's flags from
//     `RTF_UP` plus REJECT/LOCAL/CACHE/ONLINK only. It never sets
//     RTF_DEFAULT or RTF_ADDRCONF, so a userspace-installed default
//     route is IMMUNE to the purge. `proto` is the readable proxy for
//     that; the flags are what actually decide.
//
// The accept_ra=2 exemption is the reason this probe samples accept_ra:
// it excludes candidate 4 outright and DOMINATES the forwarding verdict.
// It is also, note, a candidate REMEDY rather than a cause.
//
// HOW THE INSTRUMENT SEPARATES THEM -- AND WHERE IT CANNOT. Netlink says
// what happened and when, but route events carry NO ORIGINATOR: a
// userspace delete and the kernel's purge emit the same RTM_DELROUTE.
// So the probe is built to EXCLUDE candidates, not to spot one, and it
// reports AMBIGUOUS where exclusion is not available. Two monitors run,
// because the interesting events happen on both sides of a namespace
// move and neither side can see the other:
//
//	host monitor       started BEFORE the container exists, so it sees
//	                   the veth created, addressed and then MOVED AWAY
//	container monitor  started as soon as there is a namespace to enter,
//	                   so it sees RTM_DELROUTE and who follows it
//
// A route that is never in the container at all is candidate 1 -- but
// only if the monitor was already attached, which is why it attaches by
// SANDBOX KEY (present from Join, strictly before the container process)
// and logs its attach instant: candidate 1's signature is an ABSENCE,
// and an absence proves nothing if the observer arrived late. A
// disable_ipv6 flip beside the deletion is candidate 3. A
// NETCONFA_FORWARDING event immediately before the deletion is candidate
// 4; its absence WITH forwarding at 0 throughout is candidate 2 or 5.
// `expires` and `proto` on the route separate "expired when it was told
// to" from "deleted early" without a packet capture, which this runner
// image has no tcpdump for -- expires IS the advertised Router Lifetime,
// which turns candidate 5 from inference into arithmetic.
//
// It asserts NOTHING about the route, deliberately -- a probe that
// asserted the current behaviour would have to be rewritten by the fix
// that follows it. It DOES fail if it cannot collect the trace, because
// a probe that silently gathers nothing is worse than no probe: it
// reports absence as evidence.
func TestProbe_DHCPv6_WhatDeletesTheDefaultRoute(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatalf("this probe needs root (got uid=%d); it is meant for the integration runner", os.Geteuid())
	}
	if _, err := exec.LookPath("nsenter"); err != nil {
		t.Fatalf("nsenter not on PATH: %v — the container-side netlink monitor cannot be attached, "+
			"and without it this probe cannot distinguish a deleted route from an absent one", err)
	}

	cases := []struct {
		name string
		mode harness.V6Mode
		net  string
	}{
		// SLAAC: the address comes from the advertisement, so both the
		// address and the route are the kernel's and both are at risk.
		{"slaac", harness.V6SLAAC, "dh-itest-v6probe-sl"},
		// Managed: libnetwork applies the address itself. If the route
		// dies here too, the cause is independent of how the address
		// arrived -- which rules candidate 3 in or out on its own.
		{"managed", harness.V6Managed, "dh-itest-v6probe-mg"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := harness.NewV6Fixture(t, tc.mode)
			dumpOnFailure(t, f)

			// The host-side monitor goes up FIRST, before anything
			// creates a veth, because the event this candidate turns on
			// -- the link being moved out of the host namespace -- has
			// already happened by the time a container exists to enter.
			hostMon := startMonitor(t, "")
			defer hostMon.stop()

			logOff := harness.PluginLogSize(ctx)
			created := time.Now()

			// The host's own forwarding state, read before anything of
			// ours exists. A container namespace inherits nothing from
			// this, but if the host is a router the engine's own
			// plumbing is more likely to turn it on in the sandbox too,
			// and knowing that costs one read.
			t.Logf("host forwarding before the container exists: all=%s default=%s",
				strings.TrimSpace(readSysctl("/proc/sys/net/ipv6/conf/all/forwarding")),
				strings.TrimSpace(readSysctl("/proc/sys/net/ipv6/conf/default/forwarding")))

			id, err := startOnV6Segment(t, ctx, cli, f, tc.net)
			if err != nil {
				t.Fatalf("container did not start on the %s segment; the probe has nothing "+
					"to observe:\n%v", tc.mode, err)
			}

			insp, err := cli.ContainerInspect(ctx, id)
			if err != nil {
				t.Fatalf("ContainerInspect: %v", err)
			}
			pid := insp.State.Pid
			if pid <= 0 {
				t.Fatalf("container has no pid (state %+v); cannot enter its namespace", insp.State)
			}
			sandbox := insp.NetworkSettings.SandboxKey
			if sandbox == "" {
				t.Fatalf("container has no SandboxKey; cannot attach a monitor to its namespace")
			}
			ctrMon := startMonitor(t, sandbox)
			defer ctrMon.stop()

			// The ATTRIBUTION monitor. `ip monitor` prints the route
			// that changed and never prints WHO changed it, and the
			// netlink helper library this repo already depends on
			// decodes the broadcast into a struct that drops the field
			// on the floor. That field is the originating netlink port
			// id, and it is the whole question: the kernel sets it from
			// the requesting socket for a userspace-initiated change
			// and leaves it ZERO for one the kernel itself made.
			//
			// Not a novel technique and not an inference -- dhcpcd
			// reads the same field off the same broadcast to log "pid N
			// deleted" when something other than itself removes one of
			// its routes. This subscribes to the same two groups it
			// does.
			attrMon := startAttrMonitor(t, sandbox, time.Now())
			defer attrMon.stop()

			// The WIRE capture, and the reason it is here is that it
			// falsifies as cheaply as it confirms. The leading candidate
			// is RFC 4861 7.2.5: a Neighbour Advertisement with the R
			// bit CLEAR obliges a host to drop that router from its
			// Default Router List. If the advertisements on this segment
			// carry R=1, that candidate is dead on the spot and no
			// amount of correlation can revive it.
			//
			// A raw ICMPv6 socket rather than a packet capture: this
			// runner image has no tcpdump, and it does not need one. The
			// same socket also reads the Router Lifetime out of every
			// advertisement, which checks the lifetime-0 candidate
			// against the wire instead of against the kernel's copy of
			// the number.
			icmpMon := startICMP6Monitor(t, sandbox, time.Now())
			defer icmpMon.stop()
			t.Logf("container pid=%d sandbox=%s; namespace monitor attached at t+%s after "+
				"ContainerStart returned. An ABSENCE of events before that instant is not "+
				"evidence -- the host monitor is what covers the window before it.",
				pid, sandbox, time.Since(created).Round(time.Millisecond))

			// The interface name is DISCOVERED. Bridge endpoints are not
			// named eth0 -- the Join response's DstPrefix is the bridge
			// name -- and this probe used to read conf/eth0/forwarding,
			// get "cat: No such file", and score that as "not 1". With
			// conf.all also 0 the verdict then read "forwarding 0 on
			// every scope, candidate 4 EXCLUDED" having never read the
			// per-interface value at all. A false exclusion is worse
			// than an ambiguous one.
			nsLinks := nsenterOut(pid, "ip", "-o", "link", "show")
			nsIf, nsIfOK := harness.FirstNonLoopback(nsLinks)
			if !nsIfOK {
				t.Fatalf("no non-loopback interface in the sandbox namespace; there is "+
					"nothing to sample, and every per-interface reading below would be an "+
					"error string scored as a value:\n%s", nsLinks)
			}
			t.Logf("sandbox interface discovered as %q", nsIf)

			// THE WINDOW MUST COVER THE DEATH, NOT SIT INSIDE IT.
			//
			// This was 45s, chosen against the +3s/+13s observation
			// from the isolated-namespace measurement. On the CONTAINER
			// path the route does not die there: the managed interface
			// test sees it alive at +15s and gone at +150s, and the
			// first 45s of this very probe showed it healthy the whole
			// time, counting an 1800s lifetime down one second per
			// second. A probe whose window ends at 45s would have
			// reported "no deletion observed" forever, which reads as
			// exoneration and is really just a short look.
			//
			// 180s covers the whole interval in which the route is
			// known to die, and covers the DHCPv6 lease expiring: the
			// fixture's lease is dnsmasq's 2-minute floor, so t+120s is
			// the first moment a renewal can disturb anything. The
			// interval relaxes to 1s because the netlink monitors carry
			// the sub-second timing; this poll only samples sysctl and
			// route STATE.
			const (
				probeWindow   = 180 * time.Second
				probeInterval = time.Second
			)
			type row struct {
				at                            time.Duration
				route, addr                   string
				acceptRA, autoconf, disableV6 string
				fwdIf, fwdAll, fwdDef         string
				raRx                          string
				brFwdIf, brFwdAll             string
			}
			var rows []row
			start := time.Now()
			for time.Since(start) < probeWindow {
				rows = append(rows, row{
					at:        time.Since(start),
					route:     nsenterOut(pid, "ip", "-6", "route", "show"),
					addr:      nsenterOut(pid, "ip", "-6", "addr", "show", "dev", nsIf),
					acceptRA:  nsenterCat(pid, fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", nsIf)),
					autoconf:  nsenterCat(pid, fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/autoconf", nsIf)),
					disableV6: nsenterCat(pid, fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/disable_ipv6", nsIf)),
					fwdIf:     nsenterCat(pid, fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", nsIf)),
					fwdAll:    nsenterCat(pid, "/proc/sys/net/ipv6/conf/all/forwarding"),
					fwdDef:    nsenterCat(pid, "/proc/sys/net/ipv6/conf/default/forwarding"),
					raRx:      icmp6Counter(pid, "Icmp6InRouterAdvertisements"),
					// The ROUTER side, read in the HOST namespace. Every
					// forwarding reading this probe took until now was
					// taken inside the container, which is the wrong end
					// for RFC 4861 7.2.5: Linux fills the R bit of a
					// solicited neighbour advertisement from the
					// ADVERTISING interface's forwarding sysctl. So a
					// fixture bridge that advertises itself as a router
					// through dnsmasq while its kernel answers "I am not
					// a router" is a self-contradictory segment, and
					// these two files are where that shows up.
					brFwdIf: hostCat(fmt.Sprintf(
						"/proc/sys/net/ipv6/conf/%s/forwarding", harness.V6BridgeName)),
					brFwdAll: hostCat("/proc/sys/net/ipv6/conf/all/forwarding"),
				})
				time.Sleep(probeInterval)
			}

			// Only transitions are printed. Five hundred samples of an
			// unchanging string is not a trace, it is a haystack, and
			// the whole question here is WHEN something changed.
			t.Logf("--- %s: transitions in the container's v6 state ---", tc.mode)
			var prev *row
			for i := range rows {
				r := rows[i]
				if prev == nil || r.route != prev.route || r.addr != prev.addr ||
					r.acceptRA != prev.acceptRA || r.autoconf != prev.autoconf ||
					r.disableV6 != prev.disableV6 || r.fwdIf != prev.fwdIf ||
					r.fwdAll != prev.fwdAll || r.fwdDef != prev.fwdDef {
					t.Logf("t+%-7s accept_ra=%s autoconf=%s disable_ipv6=%s "+
						"forwarding[if/all/default]=%s/%s/%s\n"+
						"  ip -6 route show:\n%s\n  ip -6 addr show dev "+nsIf+":\n%s",
						r.at.Round(probeInterval),
						strings.TrimSpace(r.acceptRA), strings.TrimSpace(r.autoconf),
						strings.TrimSpace(r.disableV6),
						strings.TrimSpace(r.fwdIf), strings.TrimSpace(r.fwdAll),
						strings.TrimSpace(r.fwdDef),
						indent(r.route), indent(r.addr))
					prev = &rows[i]
				}
			}

			// The default route's `expires`, which is the Router
			// Lifetime the kernel took off the wire. This is what
			// separates "the route expired when it was told to" from
			// "something removed it early", and it needs no capture.
			firstDefault := ""
			for _, r := range rows {
				for _, line := range strings.Split(r.route, "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "default") {
						firstDefault = strings.TrimSpace(line)
						break
					}
				}
				if firstDefault != "" {
					break
				}
			}
			// The ON-LINK PREFIX ROUTE, which is a witness for RA
			// ARRIVAL and is strictly stronger than reading the
			// default route's own `expires`. Every accepted
			// advertisement carrying a Prefix Information option
			// resets this route's `expires` to the advertised Valid
			// Lifetime; between advertisements it counts down. So a
			// RESET marks an RA arriving, and an UNBROKEN countdown
			// across a default-route deletion says no advertisement
			// arrived at that instant -- which is what excludes
			// candidate 5, because candidate 5 requires an
			// advertisement to be the thing that did it.
			//
			// Identified as "carries expires and is not the default
			// route": the link-local fe80::/64 route is `proto
			// kernel` with no lifetime, so it cannot be confused for
			// this one.
			type onlinkPoint struct {
				at   time.Duration
				line string
			}
			var onlink []onlinkPoint
			for _, r := range rows {
				for _, line := range strings.Split(r.route, "\n") {
					l := strings.TrimSpace(line)
					if l == "" || strings.HasPrefix(l, "default") {
						continue
					}
					if !strings.Contains(l, "expires") {
						continue
					}
					onlink = append(onlink, onlinkPoint{at: r.at, line: l})
					break
				}
			}
			if len(onlink) == 0 {
				t.Logf("VERDICT INPUT (RA arrival): no on-link prefix route with a lifetime " +
					"was seen at any sample, so this run carries NO witness for advertisement " +
					"arrival and cannot exclude candidate 5. Read that as a missing " +
					"instrument, not as an absence of advertisements.")
			} else {
				var b strings.Builder
				for _, p := range onlink {
					fmt.Fprintf(&b, "  t+%s: %s\n", p.at.Round(time.Second), p.line)
				}
				t.Logf("VERDICT INPUT (RA arrival): on-link prefix route across the window.\n%s"+
					"  A RESET of `expires` marks an advertisement arriving. If the countdown "+
					"runs UNBROKEN across a default-route deletion in the trace below, no "+
					"advertisement arrived at that instant and candidate 5 is excluded for "+
					"that deletion.\n"+
					"  The one escape, named rather than closed over: an advertisement "+
					"carrying no Prefix Information option would not reset this route, and "+
					"would be invisible to this witness. Reading the advertisements off the "+
					"wire is what closes that, and this probe does not.",
					b.String())
			}

			// The ROUTER-SIDE forwarding reading, which is premise one
			// of the RFC 4861 7.2.5 candidate and is stated separately
			// from it so the premise can fail on its own.
			if len(rows) > 0 {
				first, last := rows[0], rows[len(rows)-1]
				t.Logf("VERDICT INPUT (router side, %s): forwarding on the fixture bridge %s "+
					"[if/all] = %s/%s at t+0 and %s/%s at the end of the window.\n"+
					"  Linux sets the R bit of a solicited neighbour advertisement from the "+
					"advertising interface's forwarding sysctl. A ZERO here means this "+
					"segment advertises a router via the DHCP server while the kernel behind "+
					"that address answers neighbour solicitations with R=0, which is the "+
					"precondition for RFC 4861 7.2.5 to remove it from the Default Router "+
					"List -- correct host behaviour on a self-contradictory segment.\n"+
					"  This is a reading of a PREMISE, not a finding. The wire capture above "+
					"is what says whether an R=0 advertisement actually arrived, and nothing "+
					"here licenses changing the fixture before it does.",
					tc.mode, harness.V6BridgeName,
					strings.TrimSpace(first.brFwdIf), strings.TrimSpace(first.brFwdAll),
					strings.TrimSpace(last.brFwdIf), strings.TrimSpace(last.brFwdAll))
			}

			// The RA ARRIVAL CLOCK, read straight out of the kernel's
			// own per-namespace ICMPv6 counters. This is the second
			// witness for advertisement arrival and it is better than
			// the on-link prefix route in one specific way: it counts
			// EVERY advertisement the kernel accepted, including one
			// carrying no Prefix Information option, which is exactly
			// the escape the prefix-route witness has to name and
			// cannot close.
			//
			// It exists to answer a question about the correlation
			// itself rather than about the route. The deletions track a
			// DHCPv6 REPLY at a suspiciously stable lag, and a stable
			// lag is equally consistent with the REPLY being the CAUSE
			// and with the REPLY being a COINCIDENT MARKER for
			// something else on a similar clock. There is a named
			// candidate for such a clock: this fixture pins the
			// advertisement interval, and RFC 4861 6.2.1 lets a router
			// send unsolicited advertisements as close together as a
			// third of its configured maximum, which for the pinned
			// value lands near ten seconds. INFERRED, and deliberately
			// not relied on -- the counter measures the arrivals
			// directly, so the probe does not need this guess to be
			// right.
			//
			// Reading it: if every deletion sits on an advertisement
			// arrival, the advertisement is the event and the REPLY is
			// a marker. If deletions fall where no advertisement
			// arrived, the REPLY correlation stands as a cause and
			// candidate 5 is dead.
			var raSeries []string
			var lastRA string
			for _, r := range rows {
				cur := strings.TrimSpace(r.raRx)
				if cur != lastRA {
					raSeries = append(raSeries,
						fmt.Sprintf("  t+%s: Icmp6InRouterAdvertisements=%s",
							r.at.Round(probeInterval), cur))
					lastRA = cur
				}
			}
			if len(raSeries) <= 1 {
				t.Logf("VERDICT INPUT (RA clock): the received-advertisement counter never "+
					"changed across %s (last read %q). Either no advertisement was accepted "+
					"in the window or the counter could not be read; the value above says "+
					"which. Treat this run as carrying NO advertisement clock rather than as "+
					"evidence that none arrived.", probeWindow, lastRA)
			} else {
				t.Logf("VERDICT INPUT (RA clock): every increment below is an advertisement "+
					"the kernel ACCEPTED in this namespace.\n%s\n"+
					"  Correlate these instants against the DELROUTE timestamps in the "+
					"attribution table and the trace. A deletion ON an increment makes the "+
					"advertisement the event and the DHCPv6 REPLY a coincident marker; a "+
					"deletion where the counter did NOT move rules the advertisement out for "+
					"that deletion.\n"+
					"  Bound: this counts advertisements ACCEPTED. One dropped before the "+
					"counter -- by a sysctl, or as malformed -- is invisible here, so a flat "+
					"counter is not proof that the wire was quiet.",
					strings.Join(raSeries, "\n"))
			}

			// The forwarding verdict, which is the one that can be
			// reached by elimination and therefore the one worth
			// stating explicitly.
			fwdEverOn := false
			fwdAlwaysOn := len(rows) > 0
			fwdReadable := len(rows) > 0
			scorable := func(s string) bool {
				s = strings.TrimSpace(s)
				return s == "0" || s == "1"
			}
			for _, r := range rows {
				on := strings.TrimSpace(r.fwdIf) == "1" || strings.TrimSpace(r.fwdAll) == "1"
				fwdEverOn = fwdEverOn || on
				fwdAlwaysOn = fwdAlwaysOn && on
				fwdReadable = fwdReadable && scorable(r.fwdIf) && scorable(r.fwdAll)
			}
			// The purge SKIPS any interface whose accept_ra is 2: in
			// __rt6_purge_dflt_routers the flag test is AND-ed with
			// `!idev || idev->cnf.accept_ra != 2`. accept_ra=2 therefore
			// excludes candidate 4 outright, whatever forwarding reads,
			// and it DOMINATES the forwarding verdict below -- including
			// the ambiguous 1-over-1 cell, which cannot arise on an
			// exempt interface. Read before forwarding for that reason.
			acceptRA2Always := len(rows) > 0
			for _, r := range rows {
				acceptRA2Always = acceptRA2Always && strings.TrimSpace(r.acceptRA) == "2"
			}
			switch {
			case acceptRA2Always:
				t.Logf("VERDICT INPUT (forwarding): accept_ra read 2 on the interface for " +
					"the WHOLE window. The kernel's purge skips interfaces with " +
					"accept_ra=2, so candidate 4 is EXCLUDED regardless of what " +
					"forwarding reads, and the ambiguous 1-over-1 cell does not apply. " +
					"The deleter is candidate 2 or 5 (or unenumerated).")
			case !fwdReadable:
				t.Logf("VERDICT INPUT (forwarding): a forwarding value was NOT READABLE as " +
					"0 or 1 in at least one sample, so it cannot be scored. Candidate 4 is " +
					"neither confirmed nor excluded: an unreadable value must never be " +
					"counted as 'not 1', which is how a false EXCLUSION gets reported. " +
					"Report AMBIGUOUS and repair the reading before trusting this cell.")
			case fwdAlwaysOn:
				t.Logf("VERDICT INPUT (forwarding): forwarding read 1 for the WHOLE window. " +
					"This is the ambiguous cell: a redundant 1-over-1 write still purges " +
					"RA-learned default routers, emits no netconf event and changes no " +
					"readable value, so candidate 4 can be neither confirmed nor excluded " +
					"from these observations. Report AMBIGUOUS rather than attributing to " +
					"candidate 2 by elimination.")
			case !fwdEverOn:
				t.Logf("VERDICT INPUT (forwarding): forwarding read 0 on every scope for the " +
					"whole window. The kernel reaches its purge only on a write of 1, so " +
					"candidate 4 is EXCLUDED and the deleter is candidate 2 or 5 (or " +
					"unenumerated).")
			default:
				t.Logf("VERDICT INPUT (forwarding): forwarding CHANGED during the window. " +
					"Correlate the transition above against the DELROUTE timestamp in the " +
					"namespace trace; a netconf FORWARDING event immediately before the " +
					"deletion is candidate 4.")
			}

			if firstDefault == "" {
				t.Logf("VERDICT INPUT (route): no IPv6 default route was EVER seen in the "+
					"container across %s. If the host trace below shows one on the veth "+
					"before the move, that is candidate 1; the namespace monitor's attach "+
					"time above bounds what its silence can mean.", probeWindow)
			} else {
				t.Logf("VERDICT INPUT (route): first default route seen was %q.\n"+
					"  proto: `proto static` or `proto boot` here EXCLUDES candidate 4 and "+
					"candidate 5 outright -- the kernel's purge selects RTF_ADDRCONF and "+
					"RFC 4861 6.3.4 governs RA-derived routers, so neither reaches a route "+
					"userspace programmed.\n"+
					"  expires: this IS the advertised Router Lifetime counting down, and "+
					"it bounds candidate 5 in ONE direction only. A small value here is "+
					"candidate 5. A value near the advertised lifetime excludes a "+
					"lifetime-0 FIRST advertisement and nothing more -- a later one could "+
					"still carry 0, and this reading is taken at the first sample. The RA "+
					"arrival witness above is the discriminator that covers every deletion "+
					"in the window; prefer it, and treat this line as arithmetic on the "+
					"opening state.\n"+
					"  Do not bank the NUMBER as a finding either way. This branch pins the "+
					"advertised lifetime with an explicit --ra-param; unpinned, the server "+
					"derives it from the advertisement interval as RFC 4861 6.2.1's 3x "+
					"MaxRtrAdvInterval, so measurements taken before and after the pin read "+
					"different lifetimes without disagreeing about anything.",
					firstDefault)
			}

			// The Router Lifetime could also be read straight out of
			// dhcpcd's ROUTERADVERT hook environment, which is a
			// decoded advertisement and is where nd1_flags already
			// comes from. Not taken here: dumping the complete hook
			// environment means changing cmd/dhcp-handler, which is
			// product code, and `expires` answers the same question
			// from the kernel's own copy of the number. If the two ever
			// disagree, the hook is the tie-breaker.

			attrMon.report(t, tc.mode)
			icmpMon.report(t, tc.mode)
			reportDockerdLog(t, tc.mode, probeWindow)

			t.Logf("--- %s: HOST netlink trace (veth before and during the move) ---\n%s",
				tc.mode, indent(hostMon.text()))
			t.Logf("--- %s: CONTAINER netlink trace (RTM_DELROUTE and its neighbours) ---\n%s",
				tc.mode, indent(ctrMon.text()))

			_, data, err := harness.PluginLog(ctx)
			if err != nil {
				t.Logf("plugin log unavailable: %v", err)
			} else {
				t.Logf("--- %s: plugin log for this endpoint (dhcpcd starts timestamped) ---\n%s",
					tc.mode, indent(string(harness.LogSince(data, logOff))))
			}
		})
	}
}

// monitor is a backgrounded `ip -ts monitor`, in the host namespace when
// pid is nil and in that pid's network namespace otherwise.
type monitor struct {
	cmd *exec.Cmd
	buf *os.File
	pth string
}

// monitorSupportsNetconf asks this `ip` whether it accepts the keyword,
// rather than trusting a version number.
func monitorSupportsNetconf() bool {
	out, _ := exec.Command("ip", "monitor", "help").CombinedOutput()
	return strings.Contains(string(out), "netconf")
}

func readSysctl(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Sprintf("(%v)", err)
	}
	return string(b)
}

// startMonitor attaches in the host namespace when netnsPath is empty,
// and in that netns otherwise.
//
// IT TAKES A NETNS PATH, NOT A PID, AND THAT IS THE POINT. A namespace
// move flushes on the way OUT of the SOURCE namespace, so it is
// invisible from inside the destination; and the sandbox namespace
// exists from Join, which precedes the container process, so a pid is
// available strictly later than the namespace is. Attaching by the
// sandbox key is the earliest this observer can exist. Absence of a
// DELROUTE is candidate 1's entire signature, and an absence only means
// something if the observer was already there -- so the attach time is
// logged beside it.
func startMonitor(t *testing.T, netnsPath string) *monitor {
	t.Helper()
	tmp, err := os.CreateTemp("", "dh-itest-v6mon-")
	if err != nil {
		t.Fatalf("CreateTemp for netlink monitor: %v", err)
	}
	// netconf is what separates a forwarding purge from a userspace
	// delete: the kernel emits NETCONFA_FORWARDING on a forwarding
	// CHANGE and `ip monitor` decodes it. Verified as an accepted
	// OBJECT on iproute2 6.15.0; probed here rather than assumed,
	// because the runner's build is Debian's and an `ip` that rejects
	// the keyword would otherwise take the whole monitor down and turn
	// a missing separator into a missing trace.
	// `neigh` is not padding. RFC 4861 7.2.5 removes a router from the
	// Default Router List when a Neighbour Advertisement arrives with
	// the R bit clear, and that path emits a NEIGH event and nothing
	// else: no route lifetime change, no addr event, no link event, no
	// netconf event. A monitor without `neigh` cannot see the only
	// remaining RFC 4861 path that removes a default router, which
	// would make its silence on that candidate an artefact of the
	// object list rather than a finding.
	objs := []string{"route", "addr", "link", "neigh"}
	if monitorSupportsNetconf() {
		objs = append(objs, "netconf")
	} else {
		t.Logf("WARNING: this iproute2 does not accept `ip monitor netconf`. The trace " +
			"cannot separate a forwarding purge from a userspace delete, and any " +
			"verdict between those two must be reported AMBIGUOUS.")
	}
	args := append([]string{"-ts", "-6", "monitor"}, objs...)

	var cmd *exec.Cmd
	if netnsPath == "" {
		cmd = exec.Command("ip", args...)
	} else {
		cmd = exec.Command("nsenter", append([]string{"--net=" + netnsPath, "ip"}, args...)...)
	}
	cmd.Stdout = tmp
	cmd.Stderr = tmp
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start netlink monitor (%v): %v", cmd.Args, err)
	}
	return &monitor{cmd: cmd, buf: tmp, pth: tmp.Name()}
}

func (m *monitor) stop() {
	if m.cmd != nil && m.cmd.Process != nil {
		_ = syscall.Kill(-m.cmd.Process.Pid, syscall.SIGTERM)
		_, _ = m.cmd.Process.Wait()
	}
	_ = m.buf.Close()
	_ = os.Remove(m.pth)
}

func (m *monitor) text() string {
	data, err := os.ReadFile(m.pth)
	if err != nil {
		return fmt.Sprintf("(could not read monitor output: %v)", err)
	}
	if len(data) == 0 {
		return "(no netlink events)"
	}
	return string(data)
}

func nsenterOut(pid int, args ...string) string {
	out, err := exec.Command("nsenter", append([]string{"-t", strconv.Itoa(pid), "-n"}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(%v: %v)", args, err)
	}
	return string(out)
}

// nsenterCat reads a sysctl through the MOUNT namespace as well as the
// network one: /proc/sys/net paths resolve per network namespace, but
// only when /proc is the one that namespace sees.
func nsenterCat(pid int, path string) string {
	out, err := exec.Command("nsenter",
		"-t", strconv.Itoa(pid), "-n", "-m", "cat", path).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(cat %s: %v)", path, err)
	}
	return string(out)
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// icmp6Counter reads one named row out of the container namespace's
// /proc/net/snmp6. Those counters are per network namespace, so this is
// the kernel's own tally of what it accepted on this link and not a
// figure anything in userspace can flatter.
func icmp6Counter(pid int, name string) string {
	out := nsenterCat(pid, "/proc/net/snmp6")
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == name {
			return f[1]
		}
	}
	return ""
}

// attrEvent is one route change as the kernel BROADCAST it, keeping the
// field `ip monitor` never prints: the netlink port id of whoever asked
// for the change.
type attrEvent struct {
	at     time.Duration
	del    bool
	pid    uint32
	family uint8
	dstLen uint8
	proto  uint8
	// peer is resolved AT EVENT TIME, not when the report is written.
	// A netlink port id is only a lead to a pid, and a pid is only
	// valid while that process lives: the report runs at the end of a
	// three-minute window, by which point a short-lived originator has
	// exited and its number may already have been reissued to something
	// unrelated. Resolving late would not merely lose the name, it
	// would confidently print the WRONG one. Resolving here narrows the
	// race to the microseconds between the kernel's broadcast and this
	// line -- it does not close it, and the text says so.
	peer string
}

func (e attrEvent) v6Default() bool { return e.family == unix.AF_INET6 && e.dstLen == 0 }

type attrMon struct {
	mu        sync.Mutex
	events    []attrEvent
	errs      []string
	closed    bool
	sock      *nl.NetlinkSocket
	start     time.Time
	netnsPath string
}

// startAttrMonitor subscribes to route notifications INSIDE the
// container's network namespace and records the originating port id of
// every route change.
//
// Why this and not eBPF or strace. strace on a named process can only
// exonerate the process you thought to name, which is the failure mode
// this whole investigation has already hit twice: the two userspace
// suspects were excluded by reading their source, and the remaining
// candidates include both the kernel itself and the container engine,
// which is a process nobody would have thought to attach to. A port id
// read off the broadcast names whoever it actually was, including "no
// one" -- and "no one" is a specific, checkable answer here rather than
// an absence of evidence, because the kernel writes zero in that field
// only when it made the change itself.
//
// The bound, stated rather than discovered later: a netlink port id is
// not guaranteed to equal a process id. It does by default, because the
// kernel autobinds a socket to the caller's pid, but a process holding
// more than one netlink socket gets something else for the later ones.
// So a NON-ZERO value resolves to a process only as a lead, and the
// report says so. ZERO is the reliable half, and zero is the reading
// this run is going in expecting.
func startAttrMonitor(t *testing.T, netnsPath string, start time.Time) *attrMon {
	t.Helper()
	ns, err := netns.GetFromPath(netnsPath)
	if err != nil {
		t.Fatalf("open netns %s for the attribution monitor: %v", netnsPath, err)
	}
	defer func() { _ = ns.Close() }()

	// Both families on purpose. The v4 client is CONFIGURING -- it is
	// the one process in this namespace whose route code is live -- so
	// its route changes are the positive control for this instrument.
	// If no event in the whole window carries a non-zero port id, the
	// instrument has not been shown to be able to report one, and a
	// zero on the v6 deletions proves nothing. That check is in
	// report(), and it is the reason this subscribes to v4 at all.
	sock, err := nl.SubscribeAt(ns, netns.None(), unix.NETLINK_ROUTE,
		unix.RTNLGRP_IPV4_ROUTE, unix.RTNLGRP_IPV6_ROUTE)
	if err != nil {
		t.Fatalf("subscribe to route notifications in the container namespace: %v", err)
	}
	m := &attrMon{sock: sock, start: start, netnsPath: netnsPath}
	go m.run()
	return m
}

func (m *attrMon) run() {
	for {
		msgs, _, err := m.sock.Receive()
		if err != nil {
			m.mu.Lock()
			if !m.closed {
				m.errs = append(m.errs, err.Error())
			}
			m.mu.Unlock()
			return
		}
		at := time.Since(m.start)
		for _, msg := range msgs {
			del := msg.Header.Type == unix.RTM_DELROUTE
			if !del && msg.Header.Type != unix.RTM_NEWROUTE {
				continue
			}
			// struct rtmsg is eight single bytes then a u32, so the
			// fields read here need no endianness handling and no
			// unsafe cast: family, dst_len, src_len, tos, table,
			// protocol, scope, type.
			if len(msg.Data) < 12 {
				continue
			}
			ev := attrEvent{
				at:     at,
				del:    del,
				pid:    msg.Header.Pid,
				family: msg.Data[0],
				dstLen: msg.Data[1],
				proto:  msg.Data[5],
			}
			ev.peer = describeNetlinkPeer(ev.pid, m.netnsPath)
			m.mu.Lock()
			m.events = append(m.events, ev)
			m.mu.Unlock()
		}
	}
}

func (m *attrMon) stop() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	if m.sock != nil {
		m.sock.Close()
	}
}

func (m *attrMon) report(t *testing.T, mode harness.V6Mode) {
	m.mu.Lock()
	events := append([]attrEvent(nil), m.events...)
	errs := append([]string(nil), m.errs...)
	m.mu.Unlock()

	for _, e := range errs {
		t.Logf("WARNING: attribution monitor (%s) read error: %s. Any silence below is "+
			"bounded by this, not by the absence of route changes.", mode, e)
	}
	if len(events) == 0 {
		t.Logf("VERDICT INPUT (attribution, %s): the attribution monitor recorded NO route "+
			"change of either family. That is an instrument reporting nothing, not a "+
			"namespace in which nothing happened -- the traces below are the check on "+
			"which of those it was.", mode)
		return
	}

	// The positive control comes first, because every reading below is
	// worthless without it.
	sawNonZero := false
	for _, e := range events {
		if e.pid != 0 {
			sawNonZero = true
			break
		}
	}

	var b strings.Builder
	for _, e := range events {
		if !e.v6Default() {
			continue
		}
		verb := "installed"
		if e.del {
			verb = "DELETED  "
		}
		fmt.Fprintf(&b, "  t+%-8s %s  proto=%-3d  by %s\n",
			e.at.Round(time.Millisecond), verb, e.proto, e.peer)
	}
	if b.Len() == 0 {
		fmt.Fprintf(&b, "  (no IPv6 default-route change was broadcast in this window)\n")
	}

	t.Logf("VERDICT INPUT (attribution, %s): IPv6 default-route changes and who asked for "+
		"them.\n%s", mode, b.String())

	if !sawNonZero {
		t.Logf("VERDICT INPUT (attribution control, %s): NOT ONE route change of EITHER "+
			"family carried a non-zero port id across %d events. The control failed: this "+
			"run has not shown that the instrument can report a userspace originator at "+
			"all, so a zero above is UNINTERPRETABLE and must not be read as "+
			"'the kernel did it'. The v4 client configures routes in this namespace, so "+
			"the expected reading here is at least one non-zero.", mode, len(events))
		return
	}
	t.Logf("VERDICT INPUT (attribution control, %s): the instrument DID report a non-zero "+
		"port id somewhere in the window, so it is able to name a userspace originator and "+
		"a zero above is meaningful: the kernel made that change with no userspace caller. "+
		"That excludes every userspace candidate for that specific deletion at once -- "+
		"dhcpcd, the plugin and the container engine alike -- and points the remaining "+
		"question at which kernel path did it.", mode)
}

// describeNetlinkPeer turns a netlink port id into something a reader
// can act on, and is careful to say which half of the answer is solid.
//
// Called from the receive loop rather than from the report, because a
// pid is a perishable identifier: the kernel reissues it once the
// process is reaped, so a lookup taken minutes later can name a process
// that had nothing to do with the event. This is the same hazard #695
// removed from the product when it stopped opening the container
// namespace through a /proc path built from a pid, and the reason that
// rule is worth respecting in a probe is that a probe's output is read
// as evidence.
func describeNetlinkPeer(pid uint32, netnsPath string) string {
	if pid == 0 {
		return "the KERNEL (no userspace originator)"
	}
	comm := "?"
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		comm = strings.TrimSpace(string(b))
	}
	where := "OUTSIDE the container namespace"
	if sameNetNS(int(pid), netnsPath) {
		where = "INSIDE the container namespace"
	}
	return fmt.Sprintf("port id %d (a LEAD, not an identification: likely pid %d, comm %q, %s)",
		pid, pid, comm, where)
}

// sameNetNS compares namespaces by inode. The container's netns is a
// bind mount rather than a symlink, so this cannot be done by comparing
// readlink strings.
func sameNetNS(pid int, netnsPath string) bool {
	a, err := os.Stat(fmt.Sprintf("/proc/%d/ns/net", pid))
	if err != nil {
		return false
	}
	b, err := os.Stat(netnsPath)
	if err != nil {
		return false
	}
	as, ok1 := a.Sys().(*syscall.Stat_t)
	bs, ok2 := b.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && as.Ino == bs.Ino && as.Dev == bs.Dev
}

// icmp6Event is one ICMPv6 message as it arrived on the wire in the
// container's namespace, decoded only as far as the two fields this
// investigation turns on.
type icmp6Event struct {
	at     time.Duration
	typ    uint8
	src    string
	naR    bool
	naS    bool
	naO    bool
	naTgt  string
	raLife uint16
}

type icmp6Mon struct {
	mu     sync.Mutex
	events []icmp6Event
	errs   []string
	closed bool
	fd     int
	start  time.Time
	dead   bool
}

// startICMP6Monitor opens a raw ICMPv6 socket INSIDE the container's
// network namespace.
//
// Why a raw socket and not a capture: the runner image has no tcpdump,
// which is the reason the earlier round of this probe reasoned about
// Router Lifetime from the kernel's `expires` instead of reading it off
// the wire. A raw ICMPv6 socket needs no extra package, and the probe
// already requires root for everything else it does.
//
// What it is FOR, stated as a falsification rather than a hunt: if the
// Neighbour Advertisements on this segment carry R=1, then RFC 4861
// 7.2.5 cannot be removing the default router and the candidate dies
// here regardless of how well it fits every other measurement. A
// candidate that explains everything is exactly the one to point an
// instrument at that can kill it.
//
// Bounds, both real. It sees what the KERNEL delivers to a raw socket in
// this namespace, so a message dropped before that point is invisible;
// and it starts when the probe starts, so anything before the container
// was running is out of its reach by construction.
func startICMP6Monitor(t *testing.T, netnsPath string, start time.Time) *icmp6Mon {
	t.Helper()
	m := &icmp6Mon{fd: -1, start: start}
	fd, err := rawICMP6InNetNS(netnsPath)
	if err != nil {
		m.dead = true
		t.Logf("WARNING: could not open a raw ICMPv6 socket in the container namespace: %v. "+
			"This run carries NO wire capture: the R bit of the neighbour advertisements "+
			"and the advertised Router Lifetime are both UNMEASURED, and neither "+
			"RFC 4861 7.2.5 nor a lifetime-0 advertisement can be confirmed OR ruled out "+
			"from it. Read any conclusion below accordingly.", err)
		return m
	}
	m.fd = fd
	go m.run()
	return m
}

// rawICMP6InNetNS creates the socket in the target namespace and returns
// to the caller's, on a locked thread so the namespace switch cannot
// leak to another goroutine.
func rawICMP6InNetNS(netnsPath string) (int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cur, err := netns.Get()
	if err != nil {
		return -1, fmt.Errorf("read current netns: %w", err)
	}
	defer func() {
		_ = netns.Set(cur)
		_ = cur.Close()
	}()

	target, err := netns.GetFromPath(netnsPath)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", netnsPath, err)
	}
	defer func() { _ = target.Close() }()

	if err := netns.Set(target); err != nil {
		return -1, fmt.Errorf("enter %s: %w", netnsPath, err)
	}
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMPV6)
	if err != nil {
		return -1, fmt.Errorf("raw ICMPv6 socket: %w", err)
	}
	// A receive timeout rather than closing the fd under a blocked
	// read: the loop then notices the stop flag on its own, and the
	// probe never races a close against a reader.
	tv := unix.Timeval{Sec: 1}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("set receive timeout: %w", err)
	}
	return fd, nil
}

func (m *icmp6Mon) run() {
	buf := make([]byte, 1500)
	for {
		m.mu.Lock()
		stop := m.closed
		m.mu.Unlock()
		if stop {
			return
		}
		n, from, err := unix.Recvfrom(m.fd, buf, 0)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK || err == unix.EINTR {
				continue
			}
			m.mu.Lock()
			if !m.closed {
				m.errs = append(m.errs, err.Error())
			}
			m.mu.Unlock()
			return
		}
		if n < 8 {
			continue
		}
		ev := icmp6Event{at: time.Since(m.start), typ: buf[0]}
		if sa, ok := from.(*unix.SockaddrInet6); ok {
			ev.src = net.IP(sa.Addr[:]).String()
		}
		switch buf[0] {
		case 134: // Router Advertisement
			// cur hop limit, flags, then the Router Lifetime as a
			// big-endian 16-bit count of seconds.
			ev.raLife = uint16(buf[6])<<8 | uint16(buf[7])
		case 136: // Neighbour Advertisement
			// R, S and O are the top three bits of the first byte
			// after the checksum; the target address follows the
			// four flag/reserved bytes.
			ev.naR = buf[4]&0x80 != 0
			ev.naS = buf[4]&0x40 != 0
			ev.naO = buf[4]&0x20 != 0
			if n >= 24 {
				ev.naTgt = net.IP(buf[8:24]).String()
			}
		default:
			continue
		}
		m.mu.Lock()
		m.events = append(m.events, ev)
		m.mu.Unlock()
	}
}

func (m *icmp6Mon) stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	if m.fd >= 0 {
		// Given the receive timeout, the reader is at most one second
		// from noticing the flag; closing after that is safe.
		time.Sleep(1200 * time.Millisecond)
		_ = unix.Close(m.fd)
		m.fd = -1
	}
}

func (m *icmp6Mon) report(t *testing.T, mode harness.V6Mode) {
	if m == nil || m.dead {
		return
	}
	m.mu.Lock()
	events := append([]icmp6Event(nil), m.events...)
	errs := append([]string(nil), m.errs...)
	m.mu.Unlock()

	for _, e := range errs {
		t.Logf("WARNING: wire capture (%s) read error: %s. Silence below is bounded by "+
			"this rather than by a quiet segment.", mode, e)
	}
	if len(events) == 0 {
		t.Logf("VERDICT INPUT (wire, %s): the raw ICMPv6 socket opened and received NOTHING "+
			"-- no advertisements of either kind. That is an instrument that collected "+
			"nothing, and it leaves BOTH RFC 4861 7.2.5 and a lifetime-0 advertisement "+
			"unmeasured. It is not evidence that the segment was quiet.", mode)
		return
	}

	var nas, ras strings.Builder
	naCount, naRClear, raCount, raLife0 := 0, 0, 0, 0
	for _, e := range events {
		switch e.typ {
		case 134:
			raCount++
			if e.raLife == 0 {
				raLife0++
			}
			fmt.Fprintf(&ras, "  t+%-8s RA from %s  RouterLifetime=%ds\n",
				e.at.Round(time.Millisecond), e.src, e.raLife)
		case 136:
			naCount++
			if !e.naR {
				naRClear++
			}
			fmt.Fprintf(&nas, "  t+%-8s NA from %s  R=%v S=%v O=%v  target=%s\n",
				e.at.Round(time.Millisecond), e.src, b2i(e.naR), b2i(e.naS), b2i(e.naO), e.naTgt)
		}
	}

	if naCount == 0 {
		t.Logf("VERDICT INPUT (wire NA, %s): no neighbour advertisement was received at all. "+
			"RFC 4861 7.2.5 requires one to fire, so this run does not support that "+
			"candidate -- but read it as 'not observed', not as 'excluded': the container "+
			"only receives a solicited advertisement if something in it solicited.", mode)
	} else {
		t.Logf("VERDICT INPUT (wire NA, %s): %d neighbour advertisement(s), %d with the R bit "+
			"CLEAR.\n%s"+
			"  R=0 from the router is the RFC 4861 7.2.5 precondition: a host MUST remove a "+
			"router from the Default Router List when its IsRouter flag goes true->false. "+
			"Correlate any R=0 instant against the DELROUTE timestamps.\n"+
			"  If every R is 1, the candidate is DEAD for this run no matter how well it "+
			"fits the other measurements.",
			mode, naCount, naRClear, nas.String())
	}

	t.Logf("VERDICT INPUT (wire RA, %s): %d advertisement(s), %d carrying Router Lifetime 0.\n%s"+
		"  This is the advertised lifetime read off the WIRE rather than off the kernel's "+
		"copy, so it settles the lifetime-0 candidate directly and at every arrival rather "+
		"than at the first sample only.",
		mode, raCount, raLife0, ras.String())
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// reportDockerdLog collects the container engine's own log for the probe
// window. The engine is the one remaining candidate that is USERSPACE,
// which means the attribution monitor would catch it -- so this is
// corroboration rather than the primary instrument, and it is here
// because collecting it costs one command and arguing about it costs
// more.
func reportDockerdLog(t *testing.T, mode harness.V6Mode, window time.Duration) {
	since := fmt.Sprintf("%d seconds ago", int(window.Seconds())+30)
	sources := [][]string{
		{"journalctl", "-u", "docker.service", "--no-pager", "--since", since},
		{"journalctl", "-u", "docker", "--no-pager", "--since", since},
	}
	for _, args := range sources {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			t.Logf("--- %s: container engine log for the probe window (%v) ---\n%s",
				mode, args, indent(string(out)))
			return
		}
	}
	if b, err := os.ReadFile("/var/log/docker.log"); err == nil {
		t.Logf("--- %s: container engine log (/var/log/docker.log) ---\n%s",
			mode, indent(string(b)))
		return
	}
	t.Logf("VERDICT INPUT (engine log, %s): the container engine's log could NOT be "+
		"collected from this runner -- neither journalctl nor /var/log/docker.log "+
		"produced anything. The engine is therefore uncorroborated here; the attribution "+
		"monitor is what actually covers it, since an engine-issued deletion is a "+
		"userspace one and would carry a non-zero port id.", mode)
}

// hostCat reads a file in the HOST namespace. Every other sysctl reader
// in this probe deliberately enters the container's namespaces; this one
// deliberately does not, because the router side of the segment is where
// the R bit comes from.
func hostCat(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(cat %s: %v)", path, err)
	}
	return string(b)
}
