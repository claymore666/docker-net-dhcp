// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

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
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	// HostVeth is the veth end the plugin attaches MACVLAN children to
	// (driver-opt parent=HostVeth). Its peer, hostVethPeer, is a port
	// of DHCPSegment.
	HostVeth     = "dh-itest-host"
	hostVethPeer = "dh-itest-hostp"
	// IpvlanParent is the veth end the plugin attaches IPVLAN children
	// to, on the same L2 segment as HostVeth and deliberately NOT the
	// same netdev (#556).
	//
	// A parent NIC is a macvlan port or an ipvlan port, never both: the
	// two kinds contend for its single rx_handler and the second to ask
	// is refused with EBUSY. Plugin teardown is asynchronous relative
	// to test boundaries — an orphan-release reclaim can hold its
	// temporary child on the parent for seconds after the test that
	// caused it has returned — so with one shared parent a macvlan
	// test's tail could still own it when an ipvlan test's head asked,
	// and the suite went red on whichever test happened to be next.
	// Both directions are in the CI record. Two parents remove the
	// contention rather than racing it; CreateNetwork asserts the
	// invariant so a violation is named rather than surfacing as a
	// netlink error deep inside the plugin.
	IpvlanParent     = "dh-itest-ipv"
	ipvlanParentPeer = "dh-itest-ipvp"
	// DHCPSegment is the bridge that joins the two parents' peers into
	// one L2 segment. dnsmasq listens here and it carries the server
	// addresses; before #556 this was the plain veth peer of HostVeth
	// and kept its name so log lines (`DHCPACK(dh-itest-dhcp)`) and
	// captures read as before.
	DHCPSegment = "dh-itest-dhcp"

	// DHCPServerAddr is the static IP on DHCPSegment.
	DHCPServerAddr = "192.168.99.1/24"
	// HostVethAddr / IpvlanParentAddr are on-subnet addresses for the
	// macvlan and ipvlan PARENTS, and they are what let the
	// address-conflict detector work on this fixture at all (#549).
	//
	// A host answers an ARP request only if it can route a reply back
	// to the sender, so the conflict probe has to send from an address
	// on the leased subnet. Without one here the probe fell back to a
	// link-local source and — correctly, since #524 — reported every
	// result as *undetermined* rather than clean. The effect was that
	// the detector could not run on the macvlan and ipvlan fixtures,
	// i.e. on the two modes the feature exists for, while the bridge
	// fixture passed because its parent IS the addressed bridge.
	//
	// That is the exact failure this release keeps finding: "nothing
	// checked" and "nothing found" reading the same. The detector was
	// honest about it in the log; nothing failed.
	//
	// .2 and .3 are deliberate: outside [DHCPPoolStart, DHCPPoolEnd] so
	// dnsmasq can never hand them to a client, and not .1, which is the
	// server. They also mirror production, where a parent is a real
	// host NIC that carries an address.
	HostVethAddr     = "192.168.99.2/24"
	IpvlanParentAddr = "192.168.99.3/24"
	// DHCPPoolStart / DHCPPoolEnd / LeaseTime drive dnsmasq's
	// --dhcp-range. 2 minutes is dnsmasq's hard floor — anything
	// shorter is silently rounded up, which made an earlier "30s"
	// constant lie about the actual lease. T1 (renewal trigger
	// inside dhcpcd) lands at half-lease = 1m, so renewal-tests
	// have a ~1m floor on wait time.
	DHCPPoolStart = "192.168.99.10"
	DHCPPoolEnd   = "192.168.99.99"
	LeaseTime     = "2m"

	// StaticTestIP / StaticTestHostname reserve one pool address for
	// TestStaticIP_DriverOpt, which asks for it via `--driver-opt ip=`.
	//
	// The address MUST be reserved rather than merely "picked high in
	// the pool". dnsmasq allocates by hashing the client identity
	// across the whole range — not sequentially from the low end —
	// so every unreserved pool address is available to every container
	// at all times. The test used to rely on the sequential story and
	// was a coin flip: it passed three consecutive runs and then failed
	// twice on the same commit, once getting .89 and once .12.
	//
	// The odds got worse as the suite got faster. Leases live 2 minutes
	// (dnsmasq's floor, see LeaseTime) while the suite now finishes in
	// roughly half the time it used to, so far more leases are live
	// simultaneously and the chance any given address is occupied rose
	// with it. #356 is the real fix for the lease-timer floor.
	//
	// A --dhcp-host reservation takes the address out of the dynamic
	// pool entirely: dnsmasq will not hand it to any other client.
	//
	// The reservation keys on the MAC, deliberately, NOT on the
	// hostname. The plugin's hostname is best-effort at DISCOVER time:
	// initialDHCPHostname polls a 2s budget and returns "" when the
	// endpoint is not bound to a container yet, treating that as "not
	// yet known" and leaving it to renewal. A hostname-keyed
	// reservation would therefore inherit that race — and the run that
	// exposed all this shows the static test's container reaching
	// DHCPACK with no hostname logged at all, while its neighbours had
	// one. The MAC is set by the test and present in every packet.
	//
	// It must still fall INSIDE the pool range — dnsmasq NAKs a request
	// for an address outside every --dhcp-range. TestStaticReservation
	// enforces both halves.
	// HostStateDir is the plugin's STATE_DIR as seen from the host.
	//
	// Since #440 the manifest bind-mounts this path, so the plugin's
	// tombstones, per-network options and audit ledger live here rather
	// than inside the plugin rootfs — which is what makes them survive
	// `docker plugin rm`. Tests that read plugin state read it here; the
	// in-rootfs path is now just the mount point.
	HostStateDir = "/var/lib/net-dhcp"

	StaticTestIP = "192.168.99.95"
	// StaticTestMAC is locally administered (02: prefix) and unicast,
	// so it cannot collide with a real NIC or with Docker's own random
	// assignment.
	StaticTestMAC      = "02:00:00:00:99:95"
	StaticTestHostname = "dh-itest-staticip-ctr"

	// SubnetCIDR is what callers expect IP assertions to fall inside.
	SubnetCIDR = "192.168.99.0/24"

	// dnsmasqStaticReservation is the --dhcp-host flag that pins
	// StaticTestIP to StaticTestHostname. Built by StaticReservationArg
	// so the guard test can assert on the exact string the fixture
	// passes to dnsmasq rather than on a copy of it.
	dnsmasqStaticReservation = "--dhcp-host="

	// DHCPServerAddrV6 / DHCPv6PoolStart / DHCPv6PoolEnd dual-stack
	// the macvlan fixture (#103): the same dnsmasq serves stateful
	// DHCPv6 from a ULA prefix alongside the v4 pool. The prefix
	// spells "dhc"/"dhcp" in hex-ish (6470:6863) and is RFC 4193
	// private, so a leak onto a real LAN is both unroutable and
	// recognisable. Lease time shares LeaseTime, so DHCPv6 T1 (which
	// dnsmasq derives as lease/2) lands at 1m like v4.
	DHCPServerAddrV6 = "fd00:6470:6863::1/64"
	DHCPv6PoolStart  = "fd00:6470:6863::10"
	DHCPv6PoolEnd    = "fd00:6470:6863::99"
	SubnetV6CIDR     = "fd00:6470:6863::/64"

	// TestDNS6Server is advertised as DHCPv6 option 23 (DNS servers).
	// dhcpcd requests option 23 (via the config `option` list), so it
	// lands in the handler's `new_dhcp6_name_servers` env. Like
	// TestDNSServer, nothing actually serves DNS there — tests assert
	// propagation, not resolution.
	TestDNS6Server = "fd00:6470:6863::53"

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

	// TestWPAD / TestPosixTZ / TestTZDBTZ / TestTimeOffset are the
	// observe-only informational extras the fixture advertises (#262),
	// surfaced via the plugin log like NTP/TFTP:
	//   - 252 (WPAD URL)       — Info.WPAD
	//   - 100 (RFC 4833 PCode) — Info.PosixTimezone (dhcpcd: posix_timezone)
	//   - 101 (RFC 4833 TCode) — Info.TZDBTimezone  (dhcpcd: tzdb_timezone)
	//   - 2   (time offset, s) — Info.TimeOffset
	// Recognisably test-only values.
	// TestPosixTZ is deliberately comma-free: dnsmasq's --dhcp-option
	// treats commas as value separators, and a full POSIX TZ with DST
	// rules (CET-1CEST,M3.5.0,...) would be split. This is a valid
	// comma-free POSIX TZ; the test exercises option plumbing, not TZ
	// syntax.
	TestWPAD       = "http://wpad.corp.example/wpad.dat"
	TestPosixTZ    = "PST8PDT"
	TestTZDBTZ     = "Europe/Berlin"
	TestTimeOffset = "3600"

	// TestVendorClass / TestTaggedGateway drive the v0.9.0 / T2-3
	// vendor_class round-trip test. dnsmasq is configured to set tag
	// `dh-itest-vc` for clients sending option 60 = TestVendorClass,
	// then override option 3 (router) to TestTaggedGateway only for
	// tagged clients. Containers without the override get dnsmasq's
	// default gateway (its own listen address, .1).
	TestVendorClass    = "docker-net-dhcp-test-vc"
	TestTaggedGateway  = "192.168.99.250"
	dnsmasqVCTag       = "dh-itest-vc"
	defaultGatewayAddr = "192.168.99.1"

	// TestClasslessVendorClass / TestClasslessRoute drive the option-121
	// classless-static-routes test (#260). dnsmasq sets tag
	// `dh-itest-csr` for clients sending option 60 =
	// TestClasslessVendorClass, then pushes a non-default classless route
	// (TestClasslessRoute via TestClasslessRouteGW) only to tagged
	// clients — so default-config containers never see option 121 and
	// existing route assertions are unaffected.
	TestClasslessVendorClass = "docker-net-dhcp-test-csr"
	TestClasslessRoute       = "192.168.123.0/24"
	TestClasslessRouteGW     = "192.168.99.249"
	dnsmasqCSRTag            = "dh-itest-csr"
)

// DefaultGateway is the gateway untagged clients receive — dnsmasq's
// own listen address. Exposed for tests that assert vendor-class
// tagging didn't accidentally fire on a default-config container.
const DefaultGateway = defaultGatewayAddr

// StaticReservationArg is the --dhcp-host flag pinning StaticTestIP to
// StaticTestHostname, so TestStaticIP_DriverOpt's address cannot be
// handed to any other container. It is a function rather than a const
// so TestStaticReservation asserts on the exact string the fixture
// passes to dnsmasq, not on a restatement of it that could drift.
func StaticReservationArg() string {
	return dnsmasqStaticReservation + StaticTestMAC + "," + StaticTestIP
}

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

	// Second DHCP server on the bridge segment, started on demand by
	// the server-policy tests only (see bridge_challenger.go).
	chal *bridgeChallenger
}

// New builds the parent-attached segment — the DHCPSegment bridge, one
// veth pair per parent kind with the far end enslaved to it — addresses
// the server end, and starts dnsmasq. Returns an error if any step
// fails; on failure, partial state is cleaned up before returning.
func New() (*Fixture, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("integration tests must run as root (got uid=%d). Use 'sudo make integration-test' or run the runner as root", os.Geteuid())
	}

	// Idempotent: kill any stragglers from a previous panic'd run.
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
	// Same FORWARD opening the bridge-mode fixture needs: the segment
	// is a bridge now, and br_netfilter would otherwise run frames
	// between its two ports through docker's DROP policy.
	if err := installBridgeForward(DHCPSegment); err != nil {
		return nil, wrapTeardown(err)
	}

	// One veth pair per parent kind, each with its peer on the segment,
	// so a macvlan child and an ipvlan child are on the same L2 domain
	// as the server without ever sharing a netdev (#556).
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
	// The ULA must be on the interface before dnsmasq starts, or it
	// refuses the v6 dhcp-range with "no address range available".
	addrV6, err := netlink.ParseAddr(DHCPServerAddrV6)
	if err != nil {
		return nil, wrapTeardown(fmt.Errorf("ParseAddr v6: %w", err))
	}
	if err := netlink.AddrAdd(dhcpLink, addrV6); err != nil {
		return nil, wrapTeardown(fmt.Errorf("AddrAdd dhcp v6: %w", err))
	}

	// The parent's own on-subnet address, so the conflict probe has a
	// source a responder can route back to. See HostVethAddr (#549).
	//
	// Added AFTER the server's, deliberately. Both ends of the pair now
	// hold an address in 192.168.99.0/24, so the host has two connected
	// routes for it with equal metric, and Linux resolves that tie by
	// insertion order. Installing the server's first leaves every
	// existing, non-source-pinned path selecting the interface it
	// selected before this change. The probe itself is unaffected
	// either way: it installs a /32 link-scope route for the address
	// under test, which is more specific than either /24.
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
	f.dnsmasq = withCLocale(exec.Command("/usr/sbin/dnsmasq",
		"--no-daemon",
		"--conf-file=/dev/null",
		"--port=0",                 // disable DNS
		"--interface="+DHCPSegment, // DHCP only on this interface
		"--bind-interfaces",        // don't open sockets on others
		"--except-interface=lo",    // belt + braces
		"--dhcp-range="+DHCPPoolStart+","+DHCPPoolEnd+","+LeaseTime,
		// Takes StaticTestIP out of the dynamic pool — see the constant.
		StaticReservationArg(),
		// Stateful DHCPv6 on the ULA prefix (#103). --enable-ra makes
		// dnsmasq emit Router Advertisements with the M (managed) flag
		// for this range — RA handling stays kernel-delegated in the
		// container netns; the plugin only drives dhcpcd.
		"--dhcp-range="+DHCPv6PoolStart+","+DHCPv6PoolEnd+","+LeaseTime,
		"--enable-ra",
		"--dhcp-option=option6:dns-server,["+TestDNS6Server+"]",
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
		// Observe-only informational extras (#262): surfaced in the plugin
		// log, never applied to the container. Sent when the client
		// requests them (dhcpcd's option list now does).
		"--dhcp-option=2,"+TestTimeOffset, // option 2: time offset (seconds)
		"--dhcp-option=100,"+TestPosixTZ,  // option 100: RFC 4833 PCode
		"--dhcp-option=101,"+TestTZDBTZ,   // option 101: RFC 4833 TCode
		"--dhcp-option=252,"+TestWPAD,     // option 252: WPAD URL
		// Vendor-class tagging for the v0.9.0 / T2-3 round-trip test.
		// dnsmasq sets tag `dh-itest-vc` on any DISCOVER carrying
		// option 60 = TestVendorClass; the matching tag:... rule then
		// overrides the gateway (option 3) only for tagged clients.
		// Untagged clients (default `vendor_class` not set) keep
		// dnsmasq's default gateway (its own listen IP), so existing
		// tests that don't opt-in are unaffected.
		"--dhcp-vendorclass=set:"+dnsmasqVCTag+","+TestVendorClass,
		"--dhcp-option=tag:"+dnsmasqVCTag+",3,"+TestTaggedGateway,
		// Option-121 classless static route, pushed only to clients
		// tagged via TestClasslessVendorClass (#260). Non-default
		// destination, so it never alters the default-route assertions
		// of other suites.
		"--dhcp-vendorclass=set:"+dnsmasqCSRTag+","+TestClasslessVendorClass,
		"--dhcp-option=tag:"+dnsmasqCSRTag+",121,"+TestClasslessRoute+","+TestClasslessRouteGW,
		// NOTE: this fixture deliberately does NOT pass --dhcp-broadcast.
		// dnsmasq honours the client's BROADCAST flag, so it broadcasts
		// OFFER/ACK only when the client asks. ipvlan-L2 needs that
		// (shared parent MAC ⇒ a unicast OFFER can't be demuxed to the
		// right slave during initial acquisition), and the plugin now
		// sets it via the dhcpcd `broadcast` directive for ipvlan (#243).
		// Forcing --dhcp-broadcast here would mask a regression in that
		// client-side flag — so the ipvlan lifecycle test exercises the
		// real path. bridge/macvlan use distinct MACs and get unicast
		// replies, which is fine.
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

// addParentVeth creates the veth pair name<->peer, enslaves peer to the
// segment bridge and brings both ends up. Returns the parent end.
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

// cleanupNetlink removes any leftover fixture interfaces from a
// previous run. Best-effort. Veths first (deleting one end removes
// both), then the segment bridge they were ports of.
func cleanupNetlink() {
	removeBridgeForward(DHCPSegment)
	for _, name := range []string{HostVeth, IpvlanParent, DHCPSegment, BridgeName} {
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

// CountLogLines counts dnsmasq log lines containing every one of the
// given substrings (case-insensitive), e.g. ("DHCPRELEASE", mac).
// Mirrors EphemeralFixture.CountLogLines so an assertion reads the same
// in either suite.
//
// Counts rather than booleans so callers can assert on a DELTA across a
// window: the shared fixture accumulates every test's traffic, so an
// absolute count says nothing about the endpoint under test.
func (f *Fixture) CountLogLines(substrings ...string) int {
	return countMatchingLines(f.dnsmasqLog, substrings...)
}

// withCLocale pins a fixture server's messages to English before it is
// started, and every DHCP server this harness launches goes through it.
//
// WHY. dnsmasq is translated. On a host with a German locale it writes
// "DHCP, Sockets exklusiv an die Schnittstelle … gebunden" where an
// English one writes "sockets bound exclusively to interface …", and
// waitChallengerReady — which has no port to poll, because the socket is
// in another namespace — matches on the English text. Five server-policy
// tests failed on exactly that, against a server that had started
// correctly and said so in the operator's language.
//
// The sharper reason is the one that does not announce itself. The
// #800 assertions read these same logs to prove an ABSENCE: zero
// DHCPRELEASE lines for an address. Protocol tokens happen not to be
// translated today, but nothing guarantees that, and a token that got
// translated would make every one of those assertions pass VACUOUSLY —
// the matcher would find nothing because it no longer recognised
// anything, and report the clean result the test is looking for. The
// canned log in releasematcher_test.go is English, so the control and
// its subject would have drifted apart by locale alone.
//
// LC_ALL beats LANG and LC_MESSAGES, so one variable settles it. The
// rest of the environment is inherited: os.Environ() first, override
// after.
func withCLocale(cmd *exec.Cmd) *exec.Cmd {
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	return cmd
}

// countMatchingLines is the one implementation behind both
// CountLogLines and CountBridgeLogLines.
//
// It is shared rather than duplicated because both are load-bearing for
// #800's absence assertions, and a matcher whose two copies can drift is
// a matcher only half of which is under test: the control in
// releasematcher_test.go would go on passing against the copy it drives
// while the other silently stopped recognising a release.
//
// A blank line matches every substring vacuously, so blank lines are
// skipped. That is only reachable through a call with no substrings,
// which no caller makes — but a matcher used to prove an absence should
// not have a way to over-count either.
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

// IsInPoolV6 returns whether ip is in the macvlan fixture's DHCPv6
// range [DHCPv6PoolStart, DHCPv6PoolEnd]. Mirrors IsInPool's
// stricter-than-subnet semantics: the ::1 server address is in subnet
// but not in pool.
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
