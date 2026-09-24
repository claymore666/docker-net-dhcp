// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

// Package harness sets up the privileged fixtures (veth pairs, bridges, DHCP servers) the integration tests share for
// one `go test` run, while each test owns its plugin network and container (#61).
package harness

import (
	"bytes"
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

const (
	// HostVeth is the macvlan parent; its peer hostVethPeer is a port of DHCPSegment.
	HostVeth     = "dh-itest-host"
	hostVethPeer = "dh-itest-hostp"
	// IpvlanParent is the ipvlan parent, on HostVeth's L2 segment but a separate netdev (#556). macvlan and ipvlan contend
	// for a NIC's single rx_handler and the second is refused with EBUSY; plugin teardown outlives test boundaries (the
	// #800 reclaim held its child on the parent for seconds), so one shared parent made the next test red.
	IpvlanParent     = "dh-itest-ipv"
	ipvlanParentPeer = "dh-itest-ipvp"
	// DHCPSegment is the bridge joining both parents' peers; it kept its pre-#556 name so logs read `DHCPACK(dh-itest-dhcp)`.
	DHCPSegment = "dh-itest-dhcp"

	// DHCPServerAddr is the static IP on DHCPSegment.
	DHCPServerAddr = "192.168.99.1/24"
	// HostVethAddr and IpvlanParentAddr give the parents on-subnet addresses (#549), outside the pool and not the server's
	// .1. RFC 5227 retired their original reason; the section 2.4 tests still ping them, and parentaddr_test.go re-derives
	// the constraints on their values.
	HostVethAddr     = "192.168.99.2/24"
	IpvlanParentAddr = "192.168.99.3/24"
	// dnsmasq's lease floor is 2 minutes and a shorter value is silently rounded up, so T1 lands at 1 minute (#61).
	DHCPPoolStart = "192.168.99.10"
	DHCPPoolEnd   = "192.168.99.99"
	LeaseTime     = "2m"

	// HostStateDir is the plugin's STATE_DIR on the host, bind-mounted since #440 so state survives `docker plugin rm`.
	HostStateDir = "/var/lib/net-dhcp"

	// StaticTestIP is reserved by MAC with --dhcp-host (#425): dnsmasq hashes clients across the whole range, so an
	// unreserved address was a coin flip (.89 and .12 on one commit). A hostname key would race initialDHCPHostname's
	// 2 s budget, and dnsmasq NAKs an address outside every --dhcp-range.
	StaticTestIP = "192.168.99.95"
	// StaticTestMAC is locally administered and unicast, so it collides with no real NIC or Docker-assigned MAC.
	StaticTestMAC      = "02:00:00:00:99:95"
	StaticTestHostname = "dh-itest-staticip-ctr"

	// SubnetCIDR is what callers expect IP assertions to fall inside.
	SubnetCIDR = "192.168.99.0/24"

	// dnsmasqStaticReservation is the --dhcp-host flag StaticReservationArg builds.
	dnsmasqStaticReservation = "--dhcp-host="

	// The macvlan fixture's dnsmasq also serves stateful DHCPv6 from an RFC 4193 ULA prefix (#103); dnsmasq derives the
	// DHCPv6 T1 as lease/2, so it lands at 1 minute like v4.
	DHCPServerAddrV6 = "fd00:6470:6863::1/64"
	DHCPv6PoolStart  = "fd00:6470:6863::10"
	DHCPv6PoolEnd    = "fd00:6470:6863::99"
	SubnetV6CIDR     = "fd00:6470:6863::/64"

	// TestDNS6Server is advertised as DHCPv6 option 23; nothing serves DNS there.
	TestDNS6Server = "fd00:6470:6863::53"

	// TestDNSServer is advertised as DHCP option 6 for the PropagateDNS tests; nothing serves DNS there.
	TestDNSServer = "192.168.99.53"
	// TestMTU is advertised as DHCP option 26, below the 1500 a link inherits so a missing propagation is visible.
	TestMTU = "1400"

	// Extra options advertised by the macvlan fixture: 42 NTP, 119 search list, 66 TFTP server and 67 boot file.
	TestNTPServer  = "192.168.99.123"
	TestSearchList = "corp.example,internal.example"
	TestTFTPServer = "tftp.example.test"
	TestBootFile   = "pxelinux.0"

	// Observe-only extras (#262): 252 WPAD, 100 and 101 RFC 4833 timezones, 2 time offset. TestPosixTZ is comma-free
	// because dnsmasq's --dhcp-option splits values on commas.
	TestWPAD       = "http://wpad.corp.example/wpad.dat"
	TestPosixTZ    = "PST8PDT"
	TestTZDBTZ     = "Europe/Berlin"
	TestTimeOffset = "3600"

	// dnsmasq tags clients sending option 60 = TestVendorClass and overrides option 3 to TestTaggedGateway for them only.
	TestVendorClass    = "docker-net-dhcp-test-vc"
	TestTaggedGateway  = "192.168.99.250"
	dnsmasqVCTag       = "dh-itest-vc"
	defaultGatewayAddr = "192.168.99.1"

	// Clients sending option 60 = TestClasslessVendorClass get an option 121 route to TestClasslessRoute (#260).
	TestClasslessVendorClass = "docker-net-dhcp-test-csr"
	TestClasslessRoute       = "192.168.123.0/24"
	TestClasslessRouteGW     = "192.168.99.249"
	dnsmasqCSRTag            = "dh-itest-csr"
)

// DefaultGateway is the gateway untagged clients receive, dnsmasq's own address.
const DefaultGateway = defaultGatewayAddr

// StaticReservationArg returns the --dhcp-host flag reserving StaticTestIP for StaticTestMAC (#425).
func StaticReservationArg() string {
	return dnsmasqStaticReservation + StaticTestMAC + "," + StaticTestIP
}

// Fixture owns the shared macvlan, ipvlan and bridge fixtures; New sets them up and Teardown is idempotent.
type Fixture struct {
	dnsmasq    *exec.Cmd
	leaseFile  string
	dnsmasqLog string

	bridgeDnsmasq     *exec.Cmd
	bridgeLeaseFile   string
	bridgeDnsmasqLog  string
	iptablesInstalled bool

	chal *bridgeChallenger
}

// acceptLocal sets accept_local on ifName. Every fixture link shares one netns, so a DHCPRELEASE bound to the parent
// arrives carrying a source address this host owns, and Linux drops that as a martian source without it. In
// dhcp-golib's dnsmasq fixture of the same shape, dnsmasq logged nothing for such a release; that explains #966's first
// lane seeing one DHCPRELEASE per endpoint (the multicast v6 Release arrived) instead of two.
func acceptLocal(ifName string) error {
	p := filepath.Join("/proc/sys/net/ipv4/conf", ifName, "accept_local")
	if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", p, err)
	}
	return nil
}

// New builds the parent-attached segment and starts dnsmasq, cleaning up partial state on failure.
func New() (*Fixture, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("integration tests must run as root (got uid=%d). Use 'sudo make integration-test' or run the runner as root", os.Geteuid())
	}

	cleanupNetlink()

	segAttrs := netlink.NewLinkAttrs()
	segAttrs.Name = DHCPSegment
	if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: segAttrs}); err != nil {
		return nil, fmt.Errorf("LinkAdd segment bridge: %w", err)
	}
	dhcpLink, err := netlink.LinkByName(DHCPSegment)
	if err != nil {
		return nil, wrapTeardown(fmt.Errorf("LinkByName segment: %w", err))
	}
	if err := netlink.LinkSetUp(dhcpLink); err != nil {
		return nil, wrapTeardown(fmt.Errorf("LinkSetUp segment: %w", err))
	}
	// The segment is a bridge, so br_netfilter runs its frames through Docker's DROP policy without this.
	if err := installBridgeForward(DHCPSegment); err != nil {
		return nil, wrapTeardown(err)
	}
	if err := acceptLocal(DHCPSegment); err != nil {
		return nil, wrapTeardown(err)
	}

	// One veth pair per parent kind, each peer on the segment, so macvlan and ipvlan never share a netdev (#556).
	hostLink, err := addParentVeth(dhcpLink, HostVeth, hostVethPeer)
	if err != nil {
		return nil, wrapTeardown(err)
	}
	ipvlanLink, err := addParentVeth(dhcpLink, IpvlanParent, ipvlanParentPeer)
	if err != nil {
		return nil, wrapTeardown(err)
	}

	addr, err := netlink.ParseAddr(DHCPServerAddr)
	if err != nil {
		return nil, wrapTeardown(fmt.Errorf("ParseAddr: %w", err))
	}
	if err := netlink.AddrAdd(dhcpLink, addr); err != nil {
		return nil, wrapTeardown(fmt.Errorf("AddrAdd dhcp: %w", err))
	}
	// dnsmasq refuses the v6 dhcp-range with "no address range available" unless the ULA is already on the interface.
	addrV6, err := netlink.ParseAddr(DHCPServerAddrV6)
	if err != nil {
		return nil, wrapTeardown(fmt.Errorf("ParseAddr v6: %w", err))
	}
	if err := netlink.AddrAdd(dhcpLink, addrV6); err != nil {
		return nil, wrapTeardown(fmt.Errorf("AddrAdd dhcp v6: %w", err))
	}

	// Added after the server's address: with two equal-metric connected /24 routes Linux picks by insertion order, so
	// existing paths keep their interface (#549).
	for _, pa := range []struct {
		link netlink.Link
		addr string
	}{{hostLink, HostVethAddr}, {ipvlanLink, IpvlanParentAddr}} {
		a, err := netlink.ParseAddr(pa.addr)
		if err != nil {
			return nil, wrapTeardown(fmt.Errorf("ParseAddr %s: %w", pa.addr, err))
		}
		if err := netlink.AddrAdd(pa.link, a); err != nil {
			return nil, wrapTeardown(fmt.Errorf("AddrAdd %s on %s: %w", pa.addr, pa.link.Attrs().Name, err))
		}
	}

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

	// A failed bridge fixture is logged and skipped so only the bridge-mode tests are lost.
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
	f.dnsmasq = withCLocale(exec.Command("/usr/sbin/dnsmasq",
		"--no-daemon",
		"--conf-file=/dev/null",
		"--port=0",
		"--interface="+DHCPSegment,
		"--bind-interfaces",
		"--except-interface=lo",
		"--dhcp-range="+DHCPPoolStart+","+DHCPPoolEnd+","+LeaseTime,
		StaticReservationArg(),
		// Stateful DHCPv6 on the ULA prefix (#103); --enable-ra sends RAs with the M flag.
		"--dhcp-range="+DHCPv6PoolStart+","+DHCPv6PoolEnd+","+LeaseTime,
		"--enable-ra",
		"--dhcp-option=option6:dns-server,["+TestDNS6Server+"]",
		"--dhcp-leasefile="+f.leaseFile,
		"--dhcp-no-override",
		// Tests opt into options 6 and 26 with PropagateDNS and PropagateMTU; without it the plugin ignores them.
		"--dhcp-option=6,"+TestDNSServer,
		"--dhcp-option=26,"+TestMTU,
		"--dhcp-option=42,"+TestNTPServer,
		"--dhcp-option=66,"+TestTFTPServer,
		"--dhcp-option=67,"+TestBootFile,
		"--dhcp-option=119,"+TestSearchList,
		// Observe-only extras (#262), logged by the plugin and never applied.
		"--dhcp-option=2,"+TestTimeOffset,
		"--dhcp-option=100,"+TestPosixTZ,
		"--dhcp-option=101,"+TestTZDBTZ,
		"--dhcp-option=252,"+TestWPAD,
		// Vendor-class tagging: the tag overrides option 3 only for clients sending TestVendorClass.
		"--dhcp-vendorclass=set:"+dnsmasqVCTag+","+TestVendorClass,
		"--dhcp-option=tag:"+dnsmasqVCTag+",3,"+TestTaggedGateway,
		// Option 121 only for clients tagged via TestClasslessVendorClass (#260), to a non-default destination.
		"--dhcp-vendorclass=set:"+dnsmasqCSRTag+","+TestClasslessVendorClass,
		"--dhcp-option=tag:"+dnsmasqCSRTag+",121,"+TestClasslessRoute+","+TestClasslessRouteGW,
		// No --dhcp-broadcast: dnsmasq honours the client's BROADCAST flag, which ipvlan L2 needs because children share the
		// parent's MAC (#243). Forcing it here would mask a regression in the client-side flag.
		"--log-dhcp",
		"--log-facility=-",
	))
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

// Teardown stops both dnsmasq processes and removes every interface, rule and temp directory; it is idempotent.
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

// addParentVeth creates the veth pair name<->peer, enslaves peer to the segment and returns the parent end.
func addParentVeth(segment netlink.Link, name, peer string) (netlink.Link, error) {
	la := netlink.NewLinkAttrs()
	la.Name = name
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: la, PeerName: peer}); err != nil {
		return nil, fmt.Errorf("LinkAdd veth %s: %w", name, err)
	}
	parent, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("LinkByName %s: %w", name, err)
	}
	peerLink, err := netlink.LinkByName(peer)
	if err != nil {
		return nil, fmt.Errorf("LinkByName %s: %w", peer, err)
	}
	if err := netlink.LinkSetMaster(peerLink, segment); err != nil {
		return nil, fmt.Errorf("enslave %s to %s: %w", peer, segment.Attrs().Name, err)
	}
	if err := netlink.LinkSetUp(peerLink); err != nil {
		return nil, fmt.Errorf("LinkSetUp %s: %w", peer, err)
	}
	if err := netlink.LinkSetUp(parent); err != nil {
		return nil, fmt.Errorf("LinkSetUp %s: %w", name, err)
	}
	return parent, nil
}

// cleanupNetlink removes a previous run's leftover interfaces, best-effort.
func cleanupNetlink() {
	removeBridgeForward(DHCPSegment)
	for _, name := range []string{HostVeth, IpvlanParent, DHCPSegment, BridgeName} {
		if link, err := netlink.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	}
}

// wrapTeardown cleans up partial setup state when New fails.
func wrapTeardown(err error) error {
	cleanupNetlink()
	return err
}

// LeaseFile returns the path of dnsmasq's lease file, in dnsmasq(8)'s "expiration_epoch MAC IP hostname client-id" form.
func (f *Fixture) LeaseFile() string { return f.leaseFile }

// DnsmasqLog returns the path of the macvlan fixture's dnsmasq log.
func (f *Fixture) DnsmasqLog() string { return f.dnsmasqLog }

// CountLogLines counts dnsmasq log lines containing every substring, case-insensitively.
func (f *Fixture) CountLogLines(substrings ...string) int {
	return countMatchingLines(f.dnsmasqLog, substrings...)
}

// countMatchingLines backs both CountLogLines and CountBridgeLogLines, so #800's absence assertions share one matcher.
// Blank lines are skipped, since they match every substring vacuously.
func countMatchingLines(path string, substrings ...string) int {
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		l := strings.ToLower(line)
		all := true
		for _, s := range substrings {
			if !strings.Contains(l, strings.ToLower(s)) {
				all = false
				break
			}
		}
		if all {
			count++
		}
	}
	return count
}

// DumpLogs writes the captured dnsmasq log through write, for a failing test.
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

// IsInPool reports whether ip is in [DHCPPoolStart, DHCPPoolEnd].
func IsInPool(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	start := net.ParseIP(DHCPPoolStart).To4()
	end := net.ParseIP(DHCPPoolEnd).To4()
	return bytesGE(v4, start) && bytesLE(v4, end)
}

// IsInEphemeralPool reports whether ip is in [EphemeralPoolStart, EphemeralPoolEnd].
func IsInEphemeralPool(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	start := net.ParseIP(EphemeralPoolStart).To4()
	end := net.ParseIP(EphemeralPoolEnd).To4()
	return bytesGE(v4, start) && bytesLE(v4, end)
}

func bytesGE(a, b net.IP) bool { return bytes.Compare(a, b) >= 0 }
func bytesLE(a, b net.IP) bool { return bytes.Compare(a, b) <= 0 }

// IsInPoolV6 reports whether ip is in [DHCPv6PoolStart, DHCPv6PoolEnd].
func IsInPoolV6(ip net.IP) bool {
	return inV6Range(ip, DHCPv6PoolStart, DHCPv6PoolEnd)
}

// IsInBridgePoolV6 is IsInPoolV6 for the bridge fixture's v6 range.
func IsInBridgePoolV6(ip net.IP) bool {
	return inV6Range(ip, BridgeDHCPv6PoolStart, BridgeDHCPv6PoolEnd)
}

func inV6Range(ip net.IP, start, end string) bool {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil {
		return false
	}
	s := net.ParseIP(start).To16()
	e := net.ParseIP(end).To16()
	return bytesGE(v6, s) && bytesLE(v6, e)
}
