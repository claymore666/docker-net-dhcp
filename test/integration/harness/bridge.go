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
	"time"

	"github.com/vishvananda/netlink"
)

// The bridge-mode fixture runs a second dnsmasq on its own Linux bridge and subnet (#61): two servers on one broadcast
// domain would race, so it shares neither L2 nor L3 with the macvlan fixture.
const (
	// BridgeName is the Linux bridge passed as `bridge=`, within IFNAMSIZ.
	BridgeName = "dh-itest-br2"
	// BridgeAddr is the bridge's static IPv4, which dnsmasq binds to.
	BridgeAddr = "192.168.100.1/24"

	// BridgeDHCPPoolStart is the first address of the bridge fixture's pool.
	BridgeDHCPPoolStart = "192.168.100.10"
	BridgeDHCPPoolEnd   = "192.168.100.99"
	BridgeSubnetCIDR    = "192.168.100.0/24"

	// BridgeTestDNSServer is the bridge fixture's option 6, distinct from TestDNSServer so a container answered by the
	// wrong fixture is visible (#899); nothing serves DNS there and it reaches only propagate_dns networks.
	BridgeTestDNSServer = "192.168.100.53"

	// Dual-stack constants for the bridge fixture (#103), on a ULA prefix distinct from the macvlan fixture's.
	BridgeAddrV6          = "fd00:6470:6864::1/64"
	BridgeDHCPv6PoolStart = "fd00:6470:6864::10"
	BridgeDHCPv6PoolEnd   = "fd00:6470:6864::99"
	BridgeSubnetV6CIDR    = "fd00:6470:6864::/64"
)

// startBridge brings up the bridge, its FORWARD rules and the bridge dnsmasq.
func (f *Fixture) startBridge() error {
	la := netlink.NewLinkAttrs()
	la.Name = BridgeName
	br := &netlink.Bridge{LinkAttrs: la}
	if err := netlink.LinkAdd(br); err != nil {
		return fmt.Errorf("LinkAdd bridge %s: %w", BridgeName, err)
	}
	link, err := netlink.LinkByName(BridgeName)
	if err != nil {
		return fmt.Errorf("LinkByName bridge: %w", err)
	}

	// The kernel's 15 s STP forward delay blocks DHCP on a new port; one bridge has no loop to guard (#61).
	fdPath := filepath.Join("/sys/class/net", BridgeName, "bridge/forward_delay")
	if err := os.WriteFile(fdPath, []byte("0"), 0o644); err != nil {
		return fmt.Errorf("disable STP forward_delay: %w", err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("LinkSetUp bridge: %w", err)
	}

	addr, err := netlink.ParseAddr(BridgeAddr)
	if err != nil {
		return fmt.Errorf("ParseAddr bridge: %w", err)
	}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("AddrAdd bridge: %w", err)
	}
	addrV6, err := netlink.ParseAddr(BridgeAddrV6)
	if err != nil {
		return fmt.Errorf("ParseAddr bridge v6: %w", err)
	}
	if err := netlink.AddrAdd(link, addrV6); err != nil {
		return fmt.Errorf("AddrAdd bridge v6: %w", err)
	}

	if err := installBridgeForward(BridgeName); err != nil {
		return err
	}
	f.iptablesInstalled = true

	tmp, err := os.MkdirTemp("", "dh-itest-br-")
	if err != nil {
		return fmt.Errorf("MkdirTemp bridge: %w", err)
	}
	f.bridgeLeaseFile = filepath.Join(tmp, "leases")
	f.bridgeDnsmasqLog = filepath.Join(tmp, "dnsmasq.log")

	logF, err := os.Create(f.bridgeDnsmasqLog)
	if err != nil {
		return fmt.Errorf("create bridge dnsmasq log: %w", err)
	}
	f.bridgeDnsmasq = withCLocale(exec.Command("/usr/sbin/dnsmasq",
		"--no-daemon",
		"--conf-file=/dev/null",
		"--port=0",
		"--interface="+BridgeName,
		"--bind-interfaces",
		"--except-interface=lo",
		"--dhcp-range="+BridgeDHCPPoolStart+","+BridgeDHCPPoolEnd+","+LeaseTime,
		"--dhcp-range="+BridgeDHCPv6PoolStart+","+BridgeDHCPv6PoolEnd+","+LeaseTime,
		"--enable-ra",
		"--dhcp-option=option6:dns-server,["+TestDNS6Server+"]",
		"--dhcp-option=6,"+BridgeTestDNSServer,
		"--dhcp-leasefile="+f.bridgeLeaseFile,
		"--dhcp-no-override",
		"--dhcp-broadcast",
		"--log-dhcp",
		"--log-facility=-",
	))
	f.bridgeDnsmasq.Stdout = logF
	f.bridgeDnsmasq.Stderr = logF
	f.bridgeDnsmasq.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := f.bridgeDnsmasq.Start(); err != nil {
		return fmt.Errorf("start bridge dnsmasq: %w", err)
	}
	// dnsmasq runs with --bind-interfaces, so a probe on 0.0.0.0:67 succeeds even after the bind; dial the bridge IP.
	if err := waitBridgeDnsmasqReady(2 * time.Second); err != nil {
		return fmt.Errorf("bridge dnsmasq did not bind: %w", err)
	}
	return nil
}

// waitBridgeDnsmasqReady polls UDP/67 on the bridge IP until dnsmasq has bound it.
func waitBridgeDnsmasqReady(budget time.Duration) error {
	bridgeIP := net.ParseIP(strings.SplitN(BridgeAddr, "/", 2)[0])
	if bridgeIP == nil {
		return fmt.Errorf("invalid BridgeAddr %q", BridgeAddr)
	}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: bridgeIP, Port: 67})
		if err != nil {
			return nil
		}
		_ = conn.Close()
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("bridge dnsmasq did not bind UDP/67 on %s within %v", bridgeIP, budget)
}

// stopBridge tears down whatever startBridge set up, best-effort, including a previous run's leftovers.
func (f *Fixture) stopBridge() {
	if f.bridgeDnsmasq != nil && f.bridgeDnsmasq.Process != nil {
		_ = f.bridgeDnsmasq.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = f.bridgeDnsmasq.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = f.bridgeDnsmasq.Process.Kill()
			<-done
		}
	}
	if f.bridgeLeaseFile != "" {
		_ = os.RemoveAll(filepath.Dir(f.bridgeLeaseFile))
	}
	if f.iptablesInstalled {
		removeBridgeForward(BridgeName)
		f.iptablesInstalled = false
	}
	if link, err := netlink.LinkByName(BridgeName); err == nil {
		_ = netlink.LinkDel(link)
	}
}

// DumpBridgeLogs prints the bridge fixture's dnsmasq log.
func (f *Fixture) DumpBridgeLogs(write func(string)) {
	if f.bridgeDnsmasqLog == "" {
		write("(bridge fixture not started)")
		return
	}
	data, err := os.ReadFile(f.bridgeDnsmasqLog)
	if err != nil {
		write(fmt.Sprintf("(could not read bridge dnsmasq log: %v)", err))
		return
	}
	write("--- bridge dnsmasq log ---\n" + string(data))
}

// BridgeDnsmasqLogPath returns the path of the bridge fixture's dnsmasq log, empty if it never started (#875).
func (f *Fixture) BridgeDnsmasqLogPath() string { return f.bridgeDnsmasqLog }

// CountBridgeLogLines counts bridge dnsmasq log lines containing every substring, case-insensitively.
func (f *Fixture) CountBridgeLogLines(substrings ...string) int {
	return countMatchingLines(f.bridgeDnsmasqLog, substrings...)
}

// IsInBridgePool reports whether ip is in the bridge fixture's pool.
func IsInBridgePool(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	start := net.ParseIP(BridgeDHCPPoolStart).To4()
	end := net.ParseIP(BridgeDHCPPoolEnd).To4()
	return bytesGE(v4, start) && bytesLE(v4, end)
}

// installBridgeForward opens iptables and ip6tables FORWARD for a fixture bridge. Docker's FORWARD policy is DROP and
// br_netfilter sends bridged DHCP (UDP 67/68) and DHCPv6 (UDP 546/547) through it, so without these rules DHCPDISCOVER
// never reaches dnsmasq (#103). Since #556 the parent-attached segment uses it too.
func installBridgeForward(bridge string) error {
	for _, args := range [][]string{
		{"-I", "FORWARD", "-i", bridge, "-j", "ACCEPT"},
		{"-I", "FORWARD", "-o", bridge, "-j", "ACCEPT"},
	} {
		if out, err := withCLocale(exec.Command("iptables", args...)).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %v: %w (%s)", args, err, out)
		}
		if out, err := withCLocale(exec.Command("ip6tables", args...)).CombinedOutput(); err != nil {
			return fmt.Errorf("ip6tables %v: %w (%s)", args, err, out)
		}
	}
	return nil
}

// removeBridgeForward undoes installBridgeForward. Best-effort.
func removeBridgeForward(bridge string) {
	for _, args := range [][]string{
		{"-D", "FORWARD", "-i", bridge, "-j", "ACCEPT"},
		{"-D", "FORWARD", "-o", bridge, "-j", "ACCEPT"},
	} {
		_ = withCLocale(exec.Command("iptables", args...)).Run()
		_ = withCLocale(exec.Command("ip6tables", args...)).Run()
	}
}
