//go:build integration

// Package harness sets up the privileged fixture (veth pair, DHCP
// server) shared by every integration test. Per the v0.7.0 design
// (5c hybrid isolation), tests share the fixture but own their own
// plugin network and container — so the fixture lives for the whole
// `go test` invocation, set up in TestMain.
//
// Current scope: macvlan + ipvlan (parent-attached) modes. Bridge
// mode needs a separate fixture (Linux bridge + dnsmasq listening on
// the bridge interface, on a distinct subnet to avoid host routing
// conflicts) and is tracked as a follow-up — see test/integration/
// README.md.
package harness

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	// HostVeth is the veth end the plugin attaches macvlan/ipvlan
	// children to (driver-opt parent=HostVeth).
	HostVeth = "dh-itest-host"
	// DhcpVeth is the other end of the pair; dnsmasq listens here.
	DhcpVeth = "dh-itest-dhcp"

	// DHCPServerAddr is the static IP on DhcpVeth.
	DHCPServerAddr = "192.168.99.1/24"
	// DHCPPoolStart / DHCPPoolEnd / LeaseTime drive dnsmasq's
	// --dhcp-range. 2 minutes is dnsmasq's hard floor — anything
	// shorter is silently rounded up, which made an earlier "30s"
	// constant lie about the actual lease. T1 (renewal trigger
	// inside udhcpc) lands at half-lease = 1m, so renewal-tests
	// have a ~1m floor on wait time.
	DHCPPoolStart = "192.168.99.10"
	DHCPPoolEnd   = "192.168.99.99"
	LeaseTime     = "2m"

	// SubnetCIDR is what callers expect IP assertions to fall inside.
	SubnetCIDR = "192.168.99.0/24"

	// TestDNSServer / TestMTU are the values the macvlan-fixture
	// dnsmasq advertises via DHCP options 6 and 26 respectively.
	// Tests that exercise PropagateDNS / PropagateMTU assert these
	// land on the container; tests that don't opt-in should see
	// neither the DNS server in resolv.conf nor a non-1500 MTU.
	// .53 is a recognisable "this is a DNS server" address but
	// nothing on the test fixture actually serves DNS — the test
	// only asserts the address propagation, not query resolution.
	TestDNSServer = "192.168.99.53"
	// TestMTU is the value the fixture's dnsmasq advertises as DHCP
	// option 26. Chosen below the 1500 default so:
	//   - macvlan children can come up at this MTU regardless of
	//     parent (children can be ≤ parent, so 1400 ≤ 1500 holds);
	//   - "unchanged default" tests can assert MTU != 1400 because
	//     1500 is what the link inherits without propagation.
	// Operationally 1400 is the typical VPN-reduced MTU (WireGuard,
	// OpenVPN), so the test mirrors a real-world propagation case.
	TestMTU = "1400"

	// TestNTPServer / TestSearchList / TestTFTPServer / TestBootFile
	// are the values the macvlan-fixture dnsmasq advertises via the
	// extra DHCP options surfaced in v0.9.0 / T2-2:
	//   - 42  (NTP)        — captured into Info.NTPServers, surfaced via plugin log
	//   - 119 (search)     — written to resolv.conf when PropagateDNS=true
	//   - 66  (TFTP)       — captured, surfaced via plugin log
	//   - 67  (boot file)  — captured, surfaced via plugin log
	// Values are recognisable + obviously test-only so a real-LAN
	// leak would be immediately obvious.
	TestNTPServer  = "192.168.99.123"
	TestSearchList = "corp.example,internal.example"
	TestTFTPServer = "tftp.example.test"
	TestBootFile   = "pxelinux.0"
)

// Fixture owns the lifecycle of the shared integration-test environment.
// Use New() in TestMain; defer f.Teardown(). Re-running on a host with
// leftover state from a panicked previous run is safe — Teardown is
// idempotent and Setup tears down before creating.
//
// The bridge-mode fields (BridgeName, second dnsmasq, iptables rules)
// are set up alongside the macvlan veth pair so a single fixture
// covers every mode the suite exercises. Tests that don't touch
// bridge mode pay only the small one-time setup cost.
type Fixture struct {
	dnsmasq    *exec.Cmd
	leaseFile  string
	dnsmasqLog string

	// Bridge-mode fixture state (see bridge.go).
	bridgeDnsmasq     *exec.Cmd
	bridgeLeaseFile   string
	bridgeDnsmasqLog  string
	iptablesInstalled bool
}

// New creates the veth pair, brings both ends up, addresses the DHCP
// end, and starts dnsmasq. Returns an error if any step fails; on
// failure, partial state is cleaned up before returning.
func New() (*Fixture, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("integration tests must run as root (got uid=%d). Use 'sudo make integration-test' or run the runner as root", os.Geteuid())
	}

	// Idempotent: kill any stragglers from a previous panic'd run.
	cleanupNetlink()

	la := netlink.NewLinkAttrs()
	la.Name = HostVeth
	veth := &netlink.Veth{LinkAttrs: la, PeerName: DhcpVeth}
	if err := netlink.LinkAdd(veth); err != nil {
		return nil, fmt.Errorf("LinkAdd veth: %w", err)
	}

	hostLink, err := netlink.LinkByName(HostVeth)
	if err != nil {
		return nil, wrapTeardown(fmt.Errorf("LinkByName host: %w", err))
	}
	dhcpLink, err := netlink.LinkByName(DhcpVeth)
	if err != nil {
		return nil, wrapTeardown(fmt.Errorf("LinkByName dhcp: %w", err))
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		return nil, wrapTeardown(fmt.Errorf("LinkSetUp host: %w", err))
	}
	if err := netlink.LinkSetUp(dhcpLink); err != nil {
		return nil, wrapTeardown(fmt.Errorf("LinkSetUp dhcp: %w", err))
	}

	addr, err := netlink.ParseAddr(DHCPServerAddr)
	if err != nil {
		return nil, wrapTeardown(fmt.Errorf("ParseAddr: %w", err))
	}
	if err := netlink.AddrAdd(dhcpLink, addr); err != nil {
		return nil, wrapTeardown(fmt.Errorf("AddrAdd dhcp: %w", err))
	}

	// Per-run temp dir for dnsmasq lease file + log.
	tmp, err := os.MkdirTemp("", "dh-itest-")
	if err != nil {
		return nil, wrapTeardown(fmt.Errorf("MkdirTemp: %w", err))
	}
	f := &Fixture{
		leaseFile:  filepath.Join(tmp, "leases"),
		dnsmasqLog: filepath.Join(tmp, "dnsmasq.log"),
	}

	if err := f.startDnsmasq(); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, wrapTeardown(err)
	}

	if err := waitDnsmasqReady(2 * time.Second); err != nil {
		_ = f.Teardown()
		return nil, err
	}

	// Bridge fixture comes after the veth/dnsmasq is healthy so a
	// failure here cleanly tears the partial state back down. We
	// log-and-skip if the bridge fixture itself fails so the whole
	// suite isn't lost when only bridge-mode tests need it — but in
	// practice bridge setup should be just as reliable as the
	// macvlan path.
	if err := f.startBridge(); err != nil {
		_ = f.Teardown()
		return nil, fmt.Errorf("startBridge: %w", err)
	}

	return f, nil
}

func (f *Fixture) startDnsmasq() error {
	logF, err := os.Create(f.dnsmasqLog)
	if err != nil {
		return fmt.Errorf("create dnsmasq log: %w", err)
	}
	f.dnsmasq = exec.Command("/usr/sbin/dnsmasq",
		"--no-daemon",
		"--conf-file=/dev/null",
		"--port=0",              // disable DNS
		"--interface="+DhcpVeth, // DHCP only on this interface
		"--bind-interfaces",     // don't open sockets on others
		"--except-interface=lo", // belt + braces
		"--dhcp-range="+DHCPPoolStart+","+DHCPPoolEnd+","+LeaseTime,
		"--dhcp-leasefile="+f.leaseFile,
		"--dhcp-no-override",
		// DHCP options every test gets to opt-into via PropagateDNS /
		// PropagateMTU on the network. Tests that don't opt-in see
		// the options on the wire (in the dnsmasq log) but the plugin
		// ignores them, so default behaviour is unchanged.
		"--dhcp-option=6,"+TestDNSServer,    // option 6: DNS servers
		"--dhcp-option=26,"+TestMTU,         // option 26: Interface MTU
		"--dhcp-option=42,"+TestNTPServer,   // option 42: NTP servers
		"--dhcp-option=66,"+TestTFTPServer,  // option 66: TFTP server name
		"--dhcp-option=67,"+TestBootFile,    // option 67: boot file
		"--dhcp-option=119,"+TestSearchList, // option 119: domain search list
		// dhcp-broadcast forces OFFER/ACK to be sent as L2 broadcast
		// regardless of the client's broadcast flag. Required for
		// ipvlan-L2 mode: the slave's IP isn't registered with the
		// parent's ipvlan until the lease is configured (chicken &
		// egg), so unicast OFFERs addressed to parent MAC can't be
		// routed to the slave during initial DHCP. Real LAN DHCP
		// servers typically broadcast anyway; this matches that.
		"--dhcp-broadcast",
		"--log-dhcp",
		"--log-facility=-",
	)
	f.dnsmasq.Stdout = logF
	f.dnsmasq.Stderr = logF
	f.dnsmasq.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return f.dnsmasq.Start()
}

func waitDnsmasqReady(budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 67})
		if err != nil {
			return nil
		}
		_ = conn.Close()
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("dnsmasq did not bind UDP/67 within %v", budget)
}

// Teardown stops both dnsmasq processes, removes the veth pair and
// the bridge, drops the iptables FORWARD rules, and cleans up the
// per-run temp directories. Idempotent — safe to call twice or after
// a partial setup.
func (f *Fixture) Teardown() error {
	var firstErr error
	f.stopBridge()
	if f.dnsmasq != nil && f.dnsmasq.Process != nil {
		_ = f.dnsmasq.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = f.dnsmasq.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = f.dnsmasq.Process.Kill()
			<-done
		}
	}
	if f.leaseFile != "" {
		_ = os.RemoveAll(filepath.Dir(f.leaseFile))
	}
	cleanupNetlink()
	return firstErr
}

// cleanupNetlink removes any leftover fixture interfaces from a
// previous run. Best-effort.
func cleanupNetlink() {
	for _, name := range []string{HostVeth, DhcpVeth, BridgeName} {
		if link, err := netlink.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	}
}

// wrapTeardown ensures partial setup state is cleaned up if New
// fails midway, so the next test run starts fresh.
func wrapTeardown(err error) error {
	cleanupNetlink()
	return err
}

// LeaseFile returns the path to dnsmasq's lease file for tests that
// want to assert on lease state directly. Format documented in
// dnsmasq(8): "expiration_epoch MAC IP hostname client-id".
func (f *Fixture) LeaseFile() string { return f.leaseFile }

// DnsmasqLog returns the path of the macvlan-fixture dnsmasq log
// file. Tests that need to assert on the wire conversation (e.g.
// "did a renewal DHCPACK arrive?") can grep this file directly
// during the test rather than waiting for the failure-path dump.
func (f *Fixture) DnsmasqLog() string { return f.dnsmasqLog }

// DumpLogs prints captured dnsmasq stderr to a writer (usually
// t.Log) so failed tests have the wire-level conversation. Tests
// should call this from a t.Cleanup with a check for t.Failed().
func (f *Fixture) DumpLogs(write func(string)) {
	data, err := os.ReadFile(f.dnsmasqLog)
	if err != nil {
		write(fmt.Sprintf("(could not read dnsmasq log: %v)", err))
		return
	}
	write("--- dnsmasq log ---\n" + string(data))
}

// Subnet returns the /24 CIDR of the DHCP-managed subnet, parsed.
func Subnet() *net.IPNet {
	_, ipnet, _ := net.ParseCIDR(SubnetCIDR)
	return ipnet
}

// IsInPool returns whether ip is in the DHCP-handed range
// [DHCPPoolStart, DHCPPoolEnd]. Stricter than just "in subnet" — a
// container that grabbed the .1 server address would be in subnet
// but not in pool, and that's a bug worth flagging.
func IsInPool(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	start := net.ParseIP(DHCPPoolStart).To4()
	end := net.ParseIP(DHCPPoolEnd).To4()
	return bytesGE(v4, start) && bytesLE(v4, end)
}

func bytesGE(a, b net.IP) bool { return bytes.Compare(a, b) >= 0 }
func bytesLE(a, b net.IP) bool { return bytes.Compare(a, b) <= 0 }
