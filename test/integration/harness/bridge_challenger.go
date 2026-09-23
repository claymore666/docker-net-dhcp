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

// A second DHCP server on the bridge segment, started on demand for the server-policy tests (#111 prefer-list,
// #669 deny-list); a permanent second server would make every IsInBridgePool assertion a race. The pools are
// disjoint (.10-.99 primary, .150-.199 challenger), so the leased address alone names the winning server. It runs in
// its own netns over a veth into the same bridge: both servers see every DHCPDISCOVER, it can bind UDP/67 beside the
// primary, and the host gains no second route for the subnet. IPv4 only: dhcpcd's whitelist/blacklist are DHCPv4-only.
const (
	// BridgeChallengerNetns is the namespace the challenger runs in.
	BridgeChallengerNetns = "dh-itest-chal"
	// bridgeChallengerVeth is the bridge-side veth end; both ends carry the dh-itest- prefix the orphan cleanup keys on.
	bridgeChallengerVeth = "dh-itest-chalbr"
	bridgeChallengerPeer = "dh-itest-chal"

	// BridgeChallengerAddr is the challenger's address, outside the primary's pool.
	BridgeChallengerAddr = "192.168.100.2/24"
	// BridgeChallengerIP is BridgeChallengerAddr without the prefix, as passed to dhcp_servers and dhcp_deny_servers.
	BridgeChallengerIP = "192.168.100.2"

	// BridgeChallengerPoolStart is the challenger pool's first address, disjoint from the primary's pool (#111).
	BridgeChallengerPoolStart = "192.168.100.150"
	BridgeChallengerPoolEnd   = "192.168.100.199"

	// BridgeAbsentServerIP is a bridge-subnet address nothing answers on, used to force a fallback or exhaustion.
	BridgeAbsentServerIP = "192.168.100.250"
)

// bridgeChallenger holds the on-demand second server's state.
type bridgeChallenger struct {
	cmd       *exec.Cmd
	tmpDir    string
	leaseFile string
	logFile   string
}

// StartBridgeChallenger starts the second DHCP server and registers its teardown, failing the test if it cannot start.
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

	// The challenger's socket is in another netns, invisible to the port poll, so this waits on its log.
	if err := f.waitChallengerReady(5 * time.Second); err != nil {
		return err
	}
	return nil
}

// waitChallengerReady polls the challenger's log for dnsmasq's sockets-bound line. dnsmasq logs the "DHCP, IP range"
// line before it binds, so waiting on that line would race the first DHCPDISCOVER (#111).
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

// stopBridgeChallenger tears down whatever startBridgeChallenger set up, best-effort and idempotent.
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

// cleanupBridgeChallenger removes both veth ends and then the netns; `ip netns del` alone leaves the peer (#111).
func cleanupBridgeChallenger() {
	if link, err := netlink.LinkByName(bridgeChallengerVeth); err == nil {
		_ = netlink.LinkDel(link)
	}
	_ = withCLocale(exec.Command("ip", "netns", "del", BridgeChallengerNetns)).Run()
}

// DumpBridgeChallengerLog prints the challenger's dnsmasq log.
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

// BridgeChallengerLog returns the challenger's dnsmasq log so far.
func (f *Fixture) BridgeChallengerLog() string {
	if f.chal == nil || f.chal.logFile == "" {
		return ""
	}
	data, _ := os.ReadFile(f.chal.logFile)
	return string(data)
}

// BridgeLog returns the primary bridge dnsmasq's log so far.
func (f *Fixture) BridgeLog() string {
	if f.bridgeDnsmasqLog == "" {
		return ""
	}
	data, _ := os.ReadFile(f.bridgeDnsmasqLog)
	return string(data)
}

// IsInBridgeChallengerPool reports whether ip came from the challenger's pool.
func IsInBridgeChallengerPool(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	start := net.ParseIP(BridgeChallengerPoolStart).To4()
	end := net.ParseIP(BridgeChallengerPoolEnd).To4()
	return bytesGE(v4, start) && bytesLE(v4, end)
}
