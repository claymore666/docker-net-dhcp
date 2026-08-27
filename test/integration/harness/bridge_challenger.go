// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// A second DHCP server on the bridge fixture's segment.
//
// Every other fixture in this harness goes out of its way to keep
// exactly one server per broadcast domain, because a race between two
// servers makes every address assertion in the suite ambiguous. The
// server-policy tests (#111 prefer-list, #669 deny-list) need the
// opposite: the plugin's whole job there is to pick one of several
// servers that all answer, and a fixture with one server cannot
// distinguish "the policy worked" from "there was nothing to choose".
//
// So this server exists, and it is started ON DEMAND — never as part
// of the shared fixture. A permanently-running second server would
// re-introduce exactly the race the rest of the harness avoids, and
// every existing IsInBridgePool assertion would become a coin flip.
//
// Two properties make the result readable as outside evidence rather
// than as the plugin's own opinion:
//
//   - The pools are DISJOINT (.10-.99 primary, .150-.199 challenger).
//     The leased address alone names the server that won; no counter
//     and no plugin log is consulted to answer "which one".
//   - The server runs in its own network namespace, reached over a
//     veth into the same Linux bridge. Same L2 segment, so both
//     genuinely receive every DHCPDISCOVER; separate netns, so it can
//     bind UDP/67 alongside the primary and so its address does not
//     add a second route for 192.168.100.0/24 in the host namespace.
//     This is the topology the design was validated against.
//
// It is IPv4-only on purpose. The server-selection directives dhcpcd
// exposes (whitelist/blacklist) are DHCPv4-only, and the plugin
// rejects v6 entries at network create; an RA-emitting second server
// here would only add noise the feature can never act on.
const (
	// BridgeChallengerNetns is the namespace the challenger runs in.
	BridgeChallengerNetns = "dh-itest-chal"
	// bridgeChallengerVeth is the bridge-side end of the veth pair
	// (enslaved to BridgeName); bridgeChallengerPeer is the end moved
	// into BridgeChallengerNetns and addressed. Both carry the
	// dh-itest- prefix the orphan-cleanup script keys on, and both fit
	// IFNAMSIZ.
	bridgeChallengerVeth = "dh-itest-chalbr"
	bridgeChallengerPeer = "dh-itest-chal"

	// BridgeChallengerAddr is the challenger's address on the bridge
	// segment — outside the primary's pool, so the primary can never
	// hand it to a container.
	BridgeChallengerAddr = "192.168.100.2/24"
	// BridgeChallengerIP is BridgeChallengerAddr without the prefix,
	// which is what a test passes as dhcp_servers / dhcp_deny_servers.
	BridgeChallengerIP = "192.168.100.2"

	// BridgeChallengerPoolStart / End are disjoint from
	// BridgeDHCPPoolStart / End. That disjointness is the assertion
	// surface: see the file comment.
	BridgeChallengerPoolStart = "192.168.100.150"
	BridgeChallengerPoolEnd   = "192.168.100.199"

	// BridgeAbsentServerIP is an address on the bridge subnet that
	// nothing answers on. Tests use it as a prefer-list entry that
	// must be tried and must fail, so a fallback or an exhaustion is
	// forced without taking a real server down.
	BridgeAbsentServerIP = "192.168.100.250"
)

// bridgeChallenger holds the on-demand second server's state.
type bridgeChallenger struct {
	cmd       *exec.Cmd
	tmpDir    string
	leaseFile string
	logFile   string
}

// StartBridgeChallenger brings up a second DHCP server on the bridge
// segment and registers its teardown. Call it from a test that needs
// two servers answering; every other test on the bridge fixture keeps
// seeing exactly one.
//
// Fails the test rather than returning an error: a policy test whose
// second server did not start would otherwise "pass" against a single
// server, which is the one outcome that must never look like success.
func (f *Fixture) StartBridgeChallenger(t *testing.T) {
	t.Helper()
	if f.chal != nil {
		t.Fatal("bridge challenger already running: start it once per test")
	}
	if err := f.startBridgeChallenger(); err != nil {
		f.stopBridgeChallenger()
		t.Fatalf("start bridge challenger: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			f.DumpBridgeChallengerLog(func(s string) { t.Log(s) })
		}
		f.stopBridgeChallenger()
	})
	t.Logf("bridge challenger up: server %s, pool %s-%s (primary %s, pool %s-%s)",
		BridgeChallengerIP, BridgeChallengerPoolStart, BridgeChallengerPoolEnd,
		strings.SplitN(BridgeAddr, "/", 2)[0], BridgeDHCPPoolStart, BridgeDHCPPoolEnd)
}

func (f *Fixture) startBridgeChallenger() error {
	// Idempotent against a previous panicked run: both the link and
	// the namespace outlive a killed test binary.
	cleanupBridgeChallenger()

	la := netlink.NewLinkAttrs()
	la.Name = bridgeChallengerVeth
	veth := &netlink.Veth{LinkAttrs: la, PeerName: bridgeChallengerPeer}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("LinkAdd challenger veth: %w", err)
	}
	f.chal = &bridgeChallenger{}

	br, err := netlink.LinkByName(BridgeName)
	if err != nil {
		return fmt.Errorf("LinkByName %s (bridge fixture not started?): %w", BridgeName, err)
	}
	hostEnd, err := netlink.LinkByName(bridgeChallengerVeth)
	if err != nil {
		return fmt.Errorf("LinkByName %s: %w", bridgeChallengerVeth, err)
	}
	if err := netlink.LinkSetMaster(hostEnd, br); err != nil {
		return fmt.Errorf("enslave %s to %s: %w", bridgeChallengerVeth, BridgeName, err)
	}
	if err := netlink.LinkSetUp(hostEnd); err != nil {
		return fmt.Errorf("LinkSetUp %s: %w", bridgeChallengerVeth, err)
	}

	for _, args := range [][]string{
		{"netns", "add", BridgeChallengerNetns},
		{"link", "set", bridgeChallengerPeer, "netns", BridgeChallengerNetns},
		{"netns", "exec", BridgeChallengerNetns, "ip", "link", "set", "lo", "up"},
		{"netns", "exec", BridgeChallengerNetns, "ip", "link", "set", bridgeChallengerPeer, "up"},
		{"netns", "exec", BridgeChallengerNetns, "ip", "addr", "add", BridgeChallengerAddr, "dev", bridgeChallengerPeer},
	} {
		if out, err := withCLocale(exec.Command("ip", args...)).CombinedOutput(); err != nil {
			return fmt.Errorf("ip %s: %w (%s)", strings.Join(args, " "), err, out)
		}
	}

	tmp, err := os.MkdirTemp("", "dh-itest-chal-")
	if err != nil {
		return fmt.Errorf("MkdirTemp challenger: %w", err)
	}
	f.chal.tmpDir = tmp
	f.chal.leaseFile = filepath.Join(tmp, "leases")
	f.chal.logFile = filepath.Join(tmp, "dnsmasq.log")

	logF, err := os.Create(f.chal.logFile)
	if err != nil {
		return fmt.Errorf("create challenger log: %w", err)
	}
	defer logF.Close()

	f.chal.cmd = withCLocale(exec.Command("ip", "netns", "exec", BridgeChallengerNetns,
		"/usr/sbin/dnsmasq",
		"--no-daemon",
		"--conf-file=/dev/null",
		"--port=0",
		"--interface="+bridgeChallengerPeer,
		"--bind-interfaces",
		"--except-interface=lo",
		"--dhcp-range="+BridgeChallengerPoolStart+","+BridgeChallengerPoolEnd+","+LeaseTime,
		"--dhcp-leasefile="+f.chal.leaseFile,
		"--dhcp-no-override",
		"--dhcp-broadcast",
		"--log-dhcp",
		"--log-facility=-",
	))
	f.chal.cmd.Stdout = logF
	f.chal.cmd.Stderr = logF
	f.chal.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := f.chal.cmd.Start(); err != nil {
		return fmt.Errorf("start challenger dnsmasq: %w", err)
	}

	// The challenger's socket lives in another namespace, so the
	// port-poll waitBridgeDnsmasqReady uses cannot see it from here.
	// Wait on the server's own "sockets bound" line instead.
	if err := f.waitChallengerReady(5 * time.Second); err != nil {
		return err
	}
	return nil
}

// waitChallengerReady polls the challenger's log for the line dnsmasq
// prints once its DHCP sockets are open.
//
// It is specifically the sockets-bound line and NOT the "DHCP, IP
// range" line above it: dnsmasq logs the configured range first and
// binds afterwards, so waiting on the range would return before the
// server can answer anything and leave the first DHCPDISCOVER of the
// test racing the bind.
//
// Matching log text is a weaker contract than the port poll the
// primary uses, and it is used only because the socket is in another
// namespace. If a future dnsmasq reworded this, the fixture fails
// loudly here with the whole log attached — which is the right
// failure, rather than a policy test quietly running against one
// server.
func (f *Fixture) waitChallengerReady(budget time.Duration) error {
	want := "sockets bound exclusively to interface " + bridgeChallengerPeer
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(f.chal.logFile)
		if err == nil && strings.Contains(string(data), want) {
			return nil
		}
		if f.chal.cmd.ProcessState != nil {
			return fmt.Errorf("challenger dnsmasq exited during startup (%s):\n%s",
				f.chal.cmd.ProcessState, data)
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, _ := os.ReadFile(f.chal.logFile)
	return fmt.Errorf("challenger dnsmasq did not announce %q within %v:\n%s", want, budget, data)
}

// stopBridgeChallenger tears down whatever startBridgeChallenger got
// as far as. Idempotent and best-effort, like stopBridge.
func (f *Fixture) stopBridgeChallenger() {
	if f.chal == nil {
		return
	}
	if f.chal.cmd != nil && f.chal.cmd.Process != nil {
		_ = f.chal.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = f.chal.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = f.chal.cmd.Process.Kill()
			<-done
		}
	}
	if f.chal.tmpDir != "" {
		_ = os.RemoveAll(f.chal.tmpDir)
	}
	f.chal = nil
	cleanupBridgeChallenger()
}

// cleanupBridgeChallenger removes the namespace and the veth pair.
// Deleting the bridge-side end takes the peer with it, but the peer
// lives in the namespace and `ip netns del` alone would leave it, so
// both are attempted in that order.
func cleanupBridgeChallenger() {
	if link, err := netlink.LinkByName(bridgeChallengerVeth); err == nil {
		_ = netlink.LinkDel(link)
	}
	_ = withCLocale(exec.Command("ip", "netns", "del", BridgeChallengerNetns)).Run()
}

// DumpBridgeChallengerLog prints the challenger's dnsmasq log.
// Symmetric with DumpBridgeLogs.
func (f *Fixture) DumpBridgeChallengerLog(write func(string)) {
	if f.chal == nil || f.chal.logFile == "" {
		write("(bridge challenger not started)")
		return
	}
	data, err := os.ReadFile(f.chal.logFile)
	if err != nil {
		write(fmt.Sprintf("(could not read challenger dnsmasq log: %v)", err))
		return
	}
	write("--- bridge challenger dnsmasq log ---\n" + string(data))
}

// BridgeChallengerLog returns the challenger's dnsmasq log so far, for
// tests that assert on what the SERVER did rather than on what the
// plugin says it did.
func (f *Fixture) BridgeChallengerLog() string {
	if f.chal == nil || f.chal.logFile == "" {
		return ""
	}
	data, _ := os.ReadFile(f.chal.logFile)
	return string(data)
}

// BridgeLog returns the primary bridge dnsmasq's log so far. Same
// purpose as BridgeChallengerLog, for the other side of the segment.
func (f *Fixture) BridgeLog() string {
	if f.bridgeDnsmasqLog == "" {
		return ""
	}
	data, _ := os.ReadFile(f.bridgeDnsmasqLog)
	return string(data)
}

// IsInBridgeChallengerPool reports whether ip was handed out by the
// challenger. Because the two pools are disjoint this is the answer to
// "which server leased this", not a heuristic.
func IsInBridgeChallengerPool(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	start := net.ParseIP(BridgeChallengerPoolStart).To4()
	end := net.ParseIP(BridgeChallengerPoolEnd).To4()
	return bytesGE(v4, start) && bytesLE(v4, end)
}
