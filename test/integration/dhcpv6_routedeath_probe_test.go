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
// Four candidates, from the protocol review on #875:
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
//
// HOW ONE INSTRUMENT SEPARATES ALL FOUR. Netlink says who did what and
// when. Two monitors run, because the interesting events happen on both
// sides of a namespace move and neither side can see the other:
//
//	host monitor       started BEFORE the container exists, so it sees
//	                   the veth created, addressed and then MOVED AWAY
//	container monitor  started as soon as there is a namespace to enter,
//	                   so it sees RTM_DELROUTE and who follows it
//
// An RTM_DELROUTE in the container after the move, with no preceding
// link event, is candidate 2 or 4. A route that is never in the
// container at all is candidate 1. A disable_ipv6 flip beside the
// deletion is candidate 3. `expires` on the route separates "expired
// normally" from "deleted early" without a packet capture, which this
// runner image has no tcpdump for.
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
			hostMon := startMonitor(t, nil)
			defer hostMon.stop()

			logOff := harness.PluginLogSize(ctx)
			created := time.Now()

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
			ctrMon := startMonitor(t, &pid)
			defer ctrMon.stop()
			t.Logf("container pid=%d, netlink monitor attached at t+%s",
				pid, time.Since(created).Round(time.Millisecond))

			// Sample fast enough to bracket a ten-second window, and
			// long enough to cover well past it.
			const (
				probeWindow   = 45 * time.Second
				probeInterval = 500 * time.Millisecond
			)
			type row struct {
				at                            time.Duration
				route, addr                   string
				acceptRA, autoconf, disableV6 string
				fwd                           string
			}
			var rows []row
			start := time.Now()
			for time.Since(start) < probeWindow {
				rows = append(rows, row{
					at:        time.Since(start),
					route:     nsenterOut(pid, "ip", "-6", "route", "show"),
					addr:      nsenterOut(pid, "ip", "-6", "addr", "show", "dev", "eth0"),
					acceptRA:  nsenterCat(pid, "/proc/sys/net/ipv6/conf/eth0/accept_ra"),
					autoconf:  nsenterCat(pid, "/proc/sys/net/ipv6/conf/eth0/autoconf"),
					disableV6: nsenterCat(pid, "/proc/sys/net/ipv6/conf/eth0/disable_ipv6"),
					fwd:       nsenterCat(pid, "/proc/sys/net/ipv6/conf/eth0/forwarding"),
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
					r.disableV6 != prev.disableV6 || r.fwd != prev.fwd {
					t.Logf("t+%-7s accept_ra=%s autoconf=%s disable_ipv6=%s forwarding=%s\n"+
						"  ip -6 route show:\n%s\n  ip -6 addr show dev eth0:\n%s",
						r.at.Round(probeInterval),
						strings.TrimSpace(r.acceptRA), strings.TrimSpace(r.autoconf),
						strings.TrimSpace(r.disableV6), strings.TrimSpace(r.fwd),
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
			if firstDefault == "" {
				t.Logf("VERDICT INPUT: no IPv6 default route was EVER seen in the container "+
					"across %s. If the host monitor below shows one on the veth before the "+
					"move, that is candidate 1 (the namespace move), not a lifetime problem.",
					probeWindow)
			} else {
				t.Logf("VERDICT INPUT: first default route seen was %q. An `expires` near the "+
					"advertised Router Lifetime here, followed by a deletion long before it, "+
					"means the route was REMOVED rather than aged out.", firstDefault)
			}

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

func startMonitor(t *testing.T, pid *int) *monitor {
	t.Helper()
	tmp, err := os.CreateTemp("", "dh-itest-v6mon-")
	if err != nil {
		t.Fatalf("CreateTemp for netlink monitor: %v", err)
	}
	args := []string{"-ts", "-6", "monitor", "route", "addr", "link"}
	var cmd *exec.Cmd
	if pid == nil {
		cmd = exec.Command("ip", args...)
	} else {
		cmd = exec.Command("nsenter", append([]string{"-t", strconv.Itoa(*pid), "-n", "ip"}, args...)...)
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
