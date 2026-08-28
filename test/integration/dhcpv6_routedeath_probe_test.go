// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// A PROBE, not a guard. It exists to answer one question in #875 and is
// expected to be DELETED once it has: what removes the container's IPv6
// default route?
//
// WHY IT IS NEEDED, AND WHY READING THE CODE WILL NOT DO. The
// measurement that opened #875 shows the route present at +3s and gone
// by +13s. `accept_ra=0` explains a route that is not REFRESHED; it does
// not explain one that DISAPPEARS. The advertised Router Lifetime is
// around 1800s, so a route that should have lived half an hour was gone
// in thirteen seconds. Something deleted it, and until that something is
// named, a fix to the sysctls may repair nothing at all.
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
//     exactly like candidate 2 unless `expires` and `proto` are read.
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
			nsIf, nsIfOK := firstNonLoopback(nsLinks)
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
					"  expires: this IS the advertised Router Lifetime counting down. A "+
					"small value here is candidate 5 -- an advertisement that asked for the "+
					"router to be removed -- and a value near the advertised lifetime "+
					"excludes candidate 5 by arithmetic, leaving a deleter.",
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
	objs := []string{"route", "addr", "link"}
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
