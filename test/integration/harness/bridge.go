//go:build integration

package harness

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

// Bridge-mode fixture state. The plugin's bridge mode is structurally
// different from the parent-attached modes — it expects a Linux bridge
// the user already created and assigns each container a per-endpoint
// veth into that bridge. We therefore run a *second* dnsmasq on a
// distinct subnet so the bridge-mode tests don't collide with the
// macvlan/ipvlan path's pool.
//
// Why on a separate subnet? Two DHCP servers on the same broadcast
// domain would race and the plugin's containers would bind whichever
// answered first. Distinct L2 (this is a separate Linux bridge in the
// host netns, no link to the macvlan veth pair) plus distinct L3
// (192.168.100/24 vs 192.168.99/24) keeps them cleanly isolated.
const (
	// BridgeName is the Linux bridge the plugin's bridge-mode tests
	// pass as `bridge=`. Chosen to fit IFNAMSIZ (15 chars + NUL).
	BridgeName = "dh-itest-br2"
	// BridgeAddr is the static IPv4 the harness puts on the bridge so
	// dnsmasq has something to bind to.
	BridgeAddr = "192.168.100.1/24"

	// BridgeDHCPPoolStart / End / SubnetCIDR mirror the macvlan
	// fixture's constants but on the bridge subnet.
	BridgeDHCPPoolStart = "192.168.100.10"
	BridgeDHCPPoolEnd   = "192.168.100.99"
	BridgeSubnetCIDR    = "192.168.100.0/24"
)

// startBridge brings up the bridge fixture: a Linux bridge with a
// static IP, iptables FORWARD ACCEPT rules so DHCP isn't dropped by
// docker's default-deny FORWARD policy (br_netfilter routes bridged
// traffic through iptables when loaded), and a second dnsmasq bound
// to the bridge.
//
// The bridge is named distinctly from the veth pair to avoid any
// confusion at teardown — both use the same dh-itest-* prefix the
// orphan-cleanup script keys on, but distinct names so removal
// order doesn't matter.
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

	// Disable STP forward-delay: the kernel default is 15s, during
	// which the bridge port is in LISTENING/LEARNING and won't pass
	// DHCP. With a single bridge and no loop risk in the test setup
	// it's safe to set forward_delay=0.
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

	// docker's default FORWARD policy is DROP. With br_netfilter
	// loaded, even pure-bridge DHCP traffic (UDP 67/68 broadcast on
	// the same bridge) is run through iptables FORWARD. Without these
	// inserts the DHCPDISCOVER never reaches dnsmasq.
	for _, args := range [][]string{
		{"-I", "FORWARD", "-i", BridgeName, "-j", "ACCEPT"},
		{"-I", "FORWARD", "-o", BridgeName, "-j", "ACCEPT"},
	} {
		if out, err := exec.Command("iptables", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %v: %w (%s)", args, err, out)
		}
	}
	f.iptablesInstalled = true

	// Per-run temp dir for the second dnsmasq's lease file + log.
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
	f.bridgeDnsmasq = exec.Command("/usr/sbin/dnsmasq",
		"--no-daemon",
		"--conf-file=/dev/null",
		"--port=0",
		"--interface="+BridgeName,
		"--bind-interfaces",
		"--except-interface=lo",
		"--dhcp-range="+BridgeDHCPPoolStart+","+BridgeDHCPPoolEnd+","+LeaseTime,
		"--dhcp-leasefile="+f.bridgeLeaseFile,
		"--dhcp-no-override",
		"--dhcp-broadcast",
		"--log-dhcp",
		"--log-facility=-",
	)
	f.bridgeDnsmasq.Stdout = logF
	f.bridgeDnsmasq.Stderr = logF
	f.bridgeDnsmasq.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := f.bridgeDnsmasq.Start(); err != nil {
		return fmt.Errorf("start bridge dnsmasq: %w", err)
	}
	// Bind takes <50ms in practice but allow a beat for the listen
	// socket to come up.
	time.Sleep(200 * time.Millisecond)
	return nil
}

// stopBridge tears down whatever startBridge brought up. Idempotent
// and best-effort: each step swallows errors so a partial setup can
// still be cleaned, and a leftover from a previous panic'd run is
// removed too (the LinkByName/LinkDel pair handles that).
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
		for _, args := range [][]string{
			{"-D", "FORWARD", "-i", BridgeName, "-j", "ACCEPT"},
			{"-D", "FORWARD", "-o", BridgeName, "-j", "ACCEPT"},
		} {
			_ = exec.Command("iptables", args...).Run()
		}
		f.iptablesInstalled = false
	}
	if link, err := netlink.LinkByName(BridgeName); err == nil {
		_ = netlink.LinkDel(link)
	}
}

// DumpBridgeLogs prints the bridge-fixture dnsmasq log. Symmetric
// with DumpLogs for the macvlan side.
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

// IsInBridgePool reports whether ip falls in the bridge fixture's
// DHCP-handed range. Symmetric with IsInPool for the macvlan side.
func IsInBridgePool(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	start := net.ParseIP(BridgeDHCPPoolStart).To4()
	end := net.ParseIP(BridgeDHCPPoolEnd).To4()
	return bytesGE(v4, start) && bytesLE(v4, end)
}
