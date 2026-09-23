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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

const (
	// EphemeralHostVeth is the host end of the failure-injection veth pair, apart from the suite-static fixture (#128).
	EphemeralHostVeth = "dh-itest-ehost"
	ephemeralDhcpVeth = "dh-itest-edhcp"

	// Kea's server end lives in its own namespace (#356): dnsmasq binds 0.0.0.0%<iface>:67 with SO_REUSEADDR and Kea's
	// fallback socket bind is then refused, so beside the suite-static dnsmasq Kea opens no sockets. Measured both ways.
	// The dnsmasq backend stays in the host namespace so the FQDN test can query its resolver.
	ephemeralNetns = "dh-itest-eph"

	EphemeralServerAddr = "192.168.101.1/24"
	// EphemeralAltServerAddr is a different subnet for RestartOnSubnet, where a renewal is a wrong-network refusal.
	EphemeralAltServerAddr = "192.168.102.1/24"
	EphemeralAltPoolStart  = "192.168.102.10"
	EphemeralAltPoolEnd    = "192.168.102.99"
	EphemeralPoolStart     = "192.168.101.10"
	EphemeralPoolEnd       = "192.168.101.99"
	// EphemeralParentAddr is the host's own segment address, outside the pool.
	EphemeralParentAddr = "192.168.101.2/24"
	// EphemeralShiftedPoolStart is a disjoint range, so an authoritative server NAKs a renewal from the old pool.
	EphemeralShiftedPoolStart = "192.168.101.150"
	EphemeralShiftedPoolEnd   = "192.168.101.199"

	// EphemeralDefaultLeaseSeconds is 120 s, the floor dnsmasq imposed, so older tests keep their timing (#356).
	EphemeralDefaultLeaseSeconds = 120

	// EphemeralOutageLeaseSeconds is 20 s, the floor #356's probe saw dhcpcd renew cleanly at, four renewals in a row.
	EphemeralOutageLeaseSeconds = 20
)

// ephemeralBackend selects which DHCP server the fixture runs.
type ephemeralBackend int

const (
	// backendKea is the default: Kea honours valid-lifetime, renew-timer and rebind-timer verbatim (#356).
	backendKea ephemeralBackend = iota
	// backendDnsmasq serves WithDNS only, since Kea has no integrated resolver (#356).
	backendDnsmasq
)

func (b ephemeralBackend) String() string {
	if b == backendDnsmasq {
		return "dnsmasq"
	}
	return "kea"
}

// EphemeralFixture is a per-test authoritative DHCP server on its own veth pair for tests that break it (#128).
type EphemeralFixture struct {
	t *testing.T

	backend ephemeralBackend

	cmd            *exec.Cmd
	tmpDir         string
	leaseFile      string
	configFile     string
	renderedConfig string
	logFile        string

	poolStart, poolEnd string
	serverCIDR         string

	// parentCIDR is the address on the host end of the veth pair, empty for a bare parent.
	parentCIDR string

	// ignoreClientID keys lease bindings on the hardware address alone, dnsmasq's --dhcp-ignore-clid.
	ignoreClientID bool

	leaseSeconds int

	// renewT1 and renewT2 advertise options 58 and 59 in seconds; zero leaves them to the client (#253).
	renewT1, renewT2 int

	// dnsDomain selects dnsmasq with its resolver and --dhcp-fqdn, which registers only option-81 clients (#261).
	dnsDomain string
	dnsPort   int

	started bool
}

// EphemeralOption configures an EphemeralFixture before its server starts.
type EphemeralOption func(*EphemeralFixture)

// WithPool narrows the pool; one address lets a test know the lease in advance, since dnsmasq hashes clients (#524).
func WithPool(start, end string) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.poolStart = start
		ef.poolEnd = end
	}
}

// WithParentAddress sets the CIDR address on the host end of the veth pair, which is the default.
//
// An RFC 5227 section 2.1.1 probe has an all-zero sender address and Linux answers it without a route. Measured on
// 6.12 over a veth pair (#524): with no route an ARP request from a link-local sender stayed INCOMPLETE and one from
// an on-subnet sender was answered; with a link-local or default route both were answered. The fixture namespace
// has no default route.
func WithParentAddress(addr string) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.parentCIDR = addr
	}
}

// WithBareParent leaves the parent with no address.
func WithBareParent() EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.parentCIDR = ""
	}
}

// The server holds .1 and the pools start at .10, so the host takes .2.
func defaultParentAddr(serverCIDR string) string {
	ip, ipnet, err := net.ParseCIDR(serverCIDR)
	if err != nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	parent := net.IPv4(v4[0], v4[1], v4[2], 2)
	if parent.Equal(ip) {
		parent = net.IPv4(v4[0], v4[1], v4[2], 3)
	}
	ones, _ := ipnet.Mask.Size()
	return fmt.Sprintf("%s/%d", parent.String(), ones)
}

// WithRenewTimes advertises options 58 and 59 at t1 and t2 seconds and leaves the lease alone (#253).
func WithRenewTimes(t1, t2 int) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.renewT1 = t1
		ef.renewT2 = t2
	}
}

// WithLeaseSeconds sets the granted lease lifetime (#356).
func WithLeaseSeconds(seconds int) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.leaseSeconds = seconds
	}
}

// WithDnsmasqBackend selects dnsmasq with no other change, as the control for WithIgnoreClientID.
func WithDnsmasqBackend() EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.backend = backendDnsmasq
	}
}

// WithIgnoreClientID runs dnsmasq with --dhcp-ignore-clid, binding leases to the hardware address alone.
//
// IPAM mode keeps an address across restarts through the resent client identifier (RFC 2131 section 4.2), since
// libnetwork gives each endpoint a new MAC; against this server that property is unavailable (#110).
func WithIgnoreClientID() EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.backend = backendDnsmasq
		ef.ignoreClientID = true
	}
}

// WithDNS runs dnsmasq's resolver for domain with --dhcp-fqdn, so only option-81 clients resolve (#261, #356).
func WithDNS(domain string) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.backend = backendDnsmasq
		ef.dnsDomain = domain
		ef.dnsPort = 15353
	}
}

// NewEphemeralFixture creates the veth pair and starts the authoritative DHCP server.
func NewEphemeralFixture(t *testing.T, opts ...EphemeralOption) *EphemeralFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("EphemeralFixture needs root (got uid=%d)", os.Geteuid())
	}

	cleanupEphemeralLinks()

	la := netlink.NewLinkAttrs()
	la.Name = EphemeralHostVeth
	veth := &netlink.Veth{LinkAttrs: la, PeerName: ephemeralDhcpVeth}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("LinkAdd ephemeral veth: %v", err)
	}

	ef := &EphemeralFixture{
		t:            t,
		backend:      backendKea,
		poolStart:    EphemeralPoolStart,
		poolEnd:      EphemeralPoolEnd,
		serverCIDR:   EphemeralServerAddr,
		leaseSeconds: EphemeralDefaultLeaseSeconds,
	}
	ef.parentCIDR = defaultParentAddr(ef.serverCIDR)
	for _, opt := range opts {
		opt(ef)
	}
	t.Cleanup(ef.teardown)

	hostLink, err := netlink.LinkByName(EphemeralHostVeth)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", EphemeralHostVeth, err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		t.Fatalf("LinkSetUp %s: %v", EphemeralHostVeth, err)
	}

	if ef.parentCIDR != "" {
		parentAddr, err := netlink.ParseAddr(ef.parentCIDR)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", ef.parentCIDR, err)
		}
		if err := netlink.AddrAdd(hostLink, parentAddr); err != nil {
			t.Fatalf("AddrAdd %s on %s: %v", ef.parentCIDR, EphemeralHostVeth, err)
		}
	}

	if ef.isolated() {
		ef.run("ip", "netns", "add", ephemeralNetns)
		ef.run("ip", "link", "set", ephemeralDhcpVeth, "netns", ephemeralNetns)
		ef.runNetns("ip", "link", "set", "lo", "up")
		ef.runNetns("ip", "link", "set", ephemeralDhcpVeth, "up")
		ef.runNetns("ip", "addr", "add", ef.serverCIDR, "dev", ephemeralDhcpVeth)
	} else {
		dhcpLink, err := netlink.LinkByName(ephemeralDhcpVeth)
		if err != nil {
			t.Fatalf("LinkByName %s: %v", ephemeralDhcpVeth, err)
		}
		if err := netlink.LinkSetUp(dhcpLink); err != nil {
			t.Fatalf("LinkSetUp %s: %v", ephemeralDhcpVeth, err)
		}
		addr, err := netlink.ParseAddr(ef.serverCIDR)
		if err != nil {
			t.Fatalf("ParseAddr: %v", err)
		}
		if err := netlink.AddrAdd(dhcpLink, addr); err != nil {
			t.Fatalf("AddrAdd %s: %v", ephemeralDhcpVeth, err)
		}
	}

	tmp, err := os.MkdirTemp("", "dh-itest-ephemeral-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	ef.tmpDir = tmp
	ef.logFile = filepath.Join(tmp, "dhcp-server.log")
	if ef.backend == backendKea {
		ef.leaseFile = filepath.Join(tmp, "leases4.csv")
		ef.configFile = filepath.Join(tmp, "kea-dhcp4.json")
	} else {
		ef.leaseFile = filepath.Join(tmp, "leases")
	}

	t.Logf("ephemeral fixture: backend=%s lease=%ds pool=%s-%s server=%s",
		ef.backend, ef.leaseSeconds, ef.poolStart, ef.poolEnd, ef.serverCIDR)

	ef.start()
	return ef
}

// LeaseSeconds is the lease lifetime this fixture grants.
func (ef *EphemeralFixture) LeaseSeconds() int { return ef.leaseSeconds }

func (ef *EphemeralFixture) isolated() bool { return ef.backend == backendKea }

func (ef *EphemeralFixture) run(name string, args ...string) {
	ef.t.Helper()
	if out, err := withCLocale(exec.Command(name, args...)).CombinedOutput(); err != nil {
		ef.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func (ef *EphemeralFixture) runNetns(name string, args ...string) {
	ef.t.Helper()
	ef.run("ip", append([]string{"netns", "exec", ephemeralNetns, name}, args...)...)
}

// Squat puts addr on the server end inside the namespace and returns the squatter's MAC (#524).
//
// A host-side address never reaches the wire, and a macvlan sibling is unreachable from its parent (#528).
func (ef *EphemeralFixture) Squat(addr string) string {
	ef.t.Helper()
	if !ef.isolated() {
		ef.t.Fatal("Squat needs the namespaced fixture: in a shared namespace the kernel answers for the address locally and never ARPs, so the probe under test would never see it")
	}
	ef.runNetns("ip", "addr", "add", addr+"/24", "dev", ephemeralDhcpVeth)
	ef.t.Cleanup(func() {
		_ = withCLocale(exec.Command("ip", "netns", "exec", ephemeralNetns,
			"ip", "addr", "del", addr+"/24", "dev", ephemeralDhcpVeth)).Run()
	})

	out, err := withCLocale(exec.Command("ip", "netns", "exec", ephemeralNetns,
		"cat", "/sys/class/net/"+ephemeralDhcpVeth+"/address")).Output()
	if err != nil {
		ef.t.Fatalf("read squatter MAC: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// netnsCommand builds a command that runs in the fixture's namespace when it is isolated.
func (ef *EphemeralFixture) netnsCommand(name string, args ...string) *exec.Cmd {
	if !ef.isolated() {
		return withCLocale(exec.Command(name, args...))
	}
	return withCLocale(exec.Command("ip", append([]string{"netns", "exec", ephemeralNetns, name}, args...)...))
}

// PingFromServer pings ip from the server end of the veth pair, which may sit in a namespace the test cannot reach.
func (ef *EphemeralFixture) PingFromServer(ip string) ([]byte, error) {
	ef.t.Helper()
	return ef.netnsCommand("ping", "-c", "1", "-W", "2", "-I", ef.ServerIP(), ip).CombinedOutput()
}

// AnnounceSquatter pings dst from the squatter so its ARP request claims the squatted address (RFC 5227 section 2.4).
//
// The kernel announces a new address only under arp_notify, off by default (#882).
func (ef *EphemeralFixture) AnnounceSquatter(squattedAddr, dst string) {
	ef.t.Helper()
	out, err := ef.netnsCommand("ping", "-c", "1", "-W", "1", "-I", squattedAddr, dst).CombinedOutput()
	ef.t.Logf("squatter %s announced itself towards %s (ping err=%v): %s",
		squattedAddr, dst, err, strings.TrimSpace(string(out)))
}

func (ef *EphemeralFixture) start() {
	ef.t.Helper()
	if ef.backend == backendKea {
		ef.startKea()
	} else {
		ef.startDnsmasq()
	}
	ef.started = true
}

// Kea path rules (#356): the lease file must sit under /var/lib/kea unless KEA_DHCP_DATA_DIR says otherwise, logger
// output is validated the same way, and the PID directory /run/kea must exist. INFO severity: DEBUG repeats DHCPACK
// in DHCP4_RESPONSE_DATA. The logger key is output-options from Kea 2.5.4, output_options on Debian's 2.4.x (#615).
var (
	keaLoggerKeyOnce sync.Once
	keaLoggerKey     string
)

// resolveKeaLoggerKey tests the real config with `kea -t`, falling back only on the parse error naming the key (#615).
func (ef *EphemeralFixture) resolveKeaLoggerKey(keaPath string) string {
	ef.t.Helper()
	keaLoggerKeyOnce.Do(func() {
		keaLoggerKey = keaLoggerOutputModern
		probeDir, err := os.MkdirTemp("", "kea-logger-probe-")
		if err != nil {
			return
		}
		defer os.RemoveAll(probeDir)

		probe := filepath.Join(probeDir, "kea-dhcp4.json")
		if err := os.WriteFile(probe, []byte(ef.keaConfig(keaLoggerOutputModern)), 0o644); err != nil {
			return
		}
		out, err := withCLocale(exec.Command(keaPath, "-t", probe)).CombinedOutput()
		if err != nil && strings.Contains(string(out), keaLoggerOutputModern) {
			keaLoggerKey = keaLoggerOutputLegacy
			ef.t.Logf("kea rejects %q, falling back to %q (pre-2.5.4 server)",
				keaLoggerOutputModern, keaLoggerOutputLegacy)
		}
	})
	return keaLoggerKey
}

const (
	keaLoggerOutputModern = "output-options"
	keaLoggerOutputLegacy = "output_options"
)

func (ef *EphemeralFixture) keaConfig(loggerOutputKey string) string {
	timers := ""
	if ef.renewT1 > 0 {
		timers += fmt.Sprintf("    \"renew-timer\": %d,\n", ef.renewT1)
	}
	if ef.renewT2 > 0 {
		timers += fmt.Sprintf("    \"rebind-timer\": %d,\n", ef.renewT2)
	}
	return fmt.Sprintf(`{
  "Dhcp4": {
    "interfaces-config": { "interfaces": [ %q ] },
    "lease-database": {
      "type": "memfile",
      "persist": true,
      "name": %q,
      "lfc-interval": 0
    },
    "valid-lifetime": %d,
%s    "authoritative": true,
    "subnet4": [ {
      "id": 1,
      "subnet": %q,
      "pools": [ { "pool": "%s - %s" } ]
    } ],
    "loggers": [ {
      "name": "kea-dhcp4",
      %q: [ { "output": "stdout", "flush": true } ],
      "severity": "INFO"
    } ]
  }
}
`, ephemeralDhcpVeth, ef.leaseFile, ef.leaseSeconds, timers, ef.subnet(),
		ef.poolStart, ef.poolEnd, loggerOutputKey)
}

func (ef *EphemeralFixture) subnet() string {
	_, ipNet, err := net.ParseCIDR(ef.serverCIDR)
	if err != nil {
		ef.t.Fatalf("ParseCIDR %s: %v", ef.serverCIDR, err)
	}
	return ipNet.String()
}

// keaBinary is resolved through PATH; a missing binary otherwise surfaced as a readiness timeout (#356).
const keaBinary = "kea-dhcp4"

// requireKea fails the test when kea-dhcp4 is not installed.
func (ef *EphemeralFixture) requireKea() string {
	ef.t.Helper()
	path, err := exec.LookPath(keaBinary)
	if err != nil {
		ef.t.Fatalf("%s not found in PATH: %v\n"+
			"The ephemeral fixture needs it (#356). On the CI runner it comes from the "+
			"dhcp-ci-runner image (ci/runner-image/Dockerfile installs kea-dhcp4-server); "+
			"if this fires in CI the runner is on an image built before that landed. "+
			"Locally: install kea-dhcp4-server, see test/integration/README.md.",
			keaBinary, err)
	}
	return path
}

func (ef *EphemeralFixture) startKea() {
	ef.t.Helper()
	keaPath := ef.requireKea()
	// Kea will not create its PID directory and dies before reporting any config error (#356).
	if err := os.MkdirAll("/run/kea", 0o755); err != nil {
		ef.t.Fatalf("mkdir /run/kea: %v", err)
	}
	ef.renderedConfig = ef.keaConfig(ef.resolveKeaLoggerKey(keaPath))
	if err := os.WriteFile(ef.configFile, []byte(ef.renderedConfig), 0o644); err != nil {
		ef.t.Fatalf("write kea config: %v", err)
	}

	logF, err := os.OpenFile(ef.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		ef.t.Fatalf("open ephemeral kea log: %v", err)
	}
	defer logF.Close()

	startMark := ef.logSize()
	ef.cmd = ef.netnsCommand(keaPath, "-c", ef.configFile)
	// KEA_DHCP_DATA_DIR and KEA_LOCKFILE_DIR keep every file Kea writes in the fixture's temp dir (#356).
	ef.cmd.Env = append(os.Environ(),
		"KEA_DHCP_DATA_DIR="+ef.tmpDir,
		"KEA_LOCKFILE_DIR="+ef.tmpDir,
	)
	ef.cmd.Stdout = logF
	ef.cmd.Stderr = logF
	ef.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := ef.cmd.Start(); err != nil {
		ef.t.Fatalf("start ephemeral kea: %v", err)
	}

	// Measured (#356): with dnsmasq holding UDP/67, Kea logs DHCPSRV_NO_SOCKETS_OPEN and then DHCP4_STARTED anyway, so
	// readiness needs the interface listening and no socket failure since startMark.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(ef.logFile)
		if err != nil || len(data) <= startMark {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		window := string(data[startMark:])
		if why := keaSocketFailure(window); why != "" {
			ef.t.Fatalf("ephemeral kea started but opened no DHCP socket (%s).\n"+
				"On a host this usually means another DHCP server holds UDP/67 — the fixture "+
				"runs kea in netns %q precisely to avoid that, so check the namespace was created.\n"+
				"config:\n%s\nlog:\n%s", why, ephemeralNetns, ef.renderedConfig, ef.readLog())
		}
		if strings.Contains(window, "DHCP4_STARTED") && strings.Contains(window, "DHCPSRV_CFGMGR_ADD_IFACE") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// An empty log with no readiness marker usually means AppArmor denied Kea its config (#869); the log is read once.
	keaLog := ef.readLog()
	ef.t.Fatalf("ephemeral kea did not become ready; config:\n%s\nlog:\n%s\n%s",
		ef.renderedConfig, keaLog, appArmorKeaHint(ef.tmpDir, keaLog == ""))
}

func (ef *EphemeralFixture) startDnsmasq() {
	ef.t.Helper()
	logF, err := os.OpenFile(ef.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		ef.t.Fatalf("open ephemeral dnsmasq log: %v", err)
	}
	defer logF.Close()

	startMark := ef.logSize()
	// DNS is off by default (--port=0); WithDNS enables it (#261).
	portArg := "--port=0"
	args := []string{
		"--no-daemon",
		"--conf-file=/dev/null",
		"--interface=" + ephemeralDhcpVeth,
		"--bind-interfaces",
		"--except-interface=lo",
		fmt.Sprintf("--dhcp-range=%s,%s,%ds", ef.poolStart, ef.poolEnd, ef.leaseSeconds),
		"--dhcp-leasefile=" + ef.leaseFile,
		"--dhcp-no-override",
		// Without --dhcp-authoritative dnsmasq stays silent on unknown REQUESTs and never NAKs.
		"--dhcp-authoritative",
		"--dhcp-broadcast",
		"--log-dhcp",
		"--log-facility=-",
	}
	if ef.ignoreClientID {
		args = append(args, "--dhcp-ignore-clid")
	}
	if ef.renewT1 > 0 {
		args = append(args, fmt.Sprintf("--dhcp-option=58,%d", ef.renewT1))
	}
	if ef.renewT2 > 0 {
		args = append(args, fmt.Sprintf("--dhcp-option=59,%d", ef.renewT2))
	}
	if ef.dnsDomain != "" {
		portArg = fmt.Sprintf("--port=%d", ef.dnsPort)
		args = append(args,
			"--domain="+ef.dnsDomain,
			"--dhcp-fqdn",
		)
	}
	args = append(args, portArg)
	ef.cmd = withCLocale(exec.Command("/usr/sbin/dnsmasq", args...))
	ef.cmd.Stdout = logF
	ef.cmd.Stderr = logF
	ef.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := ef.cmd.Start(); err != nil {
		ef.t.Fatalf("start ephemeral dnsmasq: %v", err)
	}

	// Keyed on the pool start address: dnsmasq's "IP range" is "IP-Bereich" on the runner's German locale.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(ef.logFile)
		if err == nil && len(data) > startMark &&
			strings.Contains(string(data[startMark:]), ef.poolStart) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	ef.t.Fatalf("ephemeral dnsmasq did not become ready; log:\n%s", ef.readLog())
}

// keaSocketFailures are Kea's socket-open failures, each meaning the server will not answer (#356).
var keaSocketFailures = []string{
	"DHCPSRV_NO_SOCKETS_OPEN",
	"DHCPSRV_OPEN_SOCKET_FAIL",
	"DHCP4_OPEN_SOCKETS_FAILED",
}

func keaSocketFailure(window string) string {
	for _, marker := range keaSocketFailures {
		if strings.Contains(window, marker) {
			return marker
		}
	}
	return ""
}

func (ef *EphemeralFixture) logSize() int {
	st, err := os.Stat(ef.logFile)
	if err != nil {
		return 0
	}
	return int(st.Size())
}

// Stop SIGKILLs the DHCP server and leaves the lease DB on disk.
func (ef *EphemeralFixture) Stop() {
	ef.t.Helper()
	if ef.cmd == nil || ef.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-ef.cmd.Process.Pid, syscall.SIGKILL)
	_ = ef.cmd.Wait()
	ef.cmd = nil
}

// StartAgain restarts the server with the same pool and the preserved lease DB.
func (ef *EphemeralFixture) StartAgain() {
	ef.t.Helper()
	if ef.cmd != nil {
		ef.t.Fatal("StartAgain: server still running; call Stop first")
	}
	ef.start()
}

// Restart brings the server back with a different pool and a wiped lease DB.
//
// Kea and dnsmasq may ignore an out-of-range renewal REQUEST instead of sending a DHCPNAK (#356).
func (ef *EphemeralFixture) Restart(poolStart, poolEnd string) {
	ef.t.Helper()
	ef.Stop()
	ef.wipeLeaseDB()
	ef.poolStart, ef.poolEnd = poolStart, poolEnd
	ef.start()
}

// RestartOnSubnet brings the server back on a different subnet with a wiped lease DB.
func (ef *EphemeralFixture) RestartOnSubnet(serverCIDR, poolStart, poolEnd string) {
	ef.t.Helper()
	ef.Stop()
	ef.wipeLeaseDB()
	if ef.isolated() {
		ef.runNetns("ip", "addr", "del", ef.serverCIDR, "dev", ephemeralDhcpVeth)
		ef.runNetns("ip", "addr", "add", serverCIDR, "dev", ephemeralDhcpVeth)
	} else {
		link, err := netlink.LinkByName(ephemeralDhcpVeth)
		if err != nil {
			ef.t.Fatalf("LinkByName %s: %v", ephemeralDhcpVeth, err)
		}
		old, err := netlink.ParseAddr(ef.serverCIDR)
		if err != nil {
			ef.t.Fatalf("ParseAddr old server CIDR: %v", err)
		}
		if err := netlink.AddrDel(link, old); err != nil {
			ef.t.Fatalf("AddrDel %s: %v", ef.serverCIDR, err)
		}
		fresh, err := netlink.ParseAddr(serverCIDR)
		if err != nil {
			ef.t.Fatalf("ParseAddr new server CIDR: %v", err)
		}
		if err := netlink.AddrAdd(link, fresh); err != nil {
			ef.t.Fatalf("AddrAdd %s: %v", serverCIDR, err)
		}
	}
	ef.serverCIDR = serverCIDR
	ef.poolStart, ef.poolEnd = poolStart, poolEnd
	ef.start()
}

func (ef *EphemeralFixture) wipeLeaseDB() {
	ef.t.Helper()
	if err := os.Remove(ef.leaseFile); err != nil && !os.IsNotExist(err) {
		ef.t.Fatalf("wipe ephemeral lease DB: %v", err)
	}
}

// keaLeaseCSVHeader is Kea 2.6's memfile schema, copied from a file Kea wrote and verified to load (#356).
const keaLeaseCSVHeader = "address,hwaddr,client_id,valid_lifetime,expire,subnet_id,fqdn_fwd,fqdn_rev,hostname,state,user_context,pool_id"

// SeedStolenLease overwrites the stopped server's lease DB so ip belongs to a foreign client (#128).
func (ef *EphemeralFixture) SeedStolenLease(ip string) {
	ef.t.Helper()
	if ef.cmd != nil {
		ef.t.Fatal("SeedStolenLease: stop the server first; the lease DB is read only at startup")
	}
	expiry := time.Now().Add(time.Hour).Unix()
	var line string
	if ef.backend == backendKea {
		// subnet_id must match keaConfig's subnet4 id, or Kea ignores the lease silently.
		line = fmt.Sprintf("%s\n%s,aa:bb:cc:dd:ee:ff,,3600,%d,1,0,0,stolen-by,0,,0\n",
			keaLeaseCSVHeader, ip, expiry)
	} else {
		// dnsmasq(8) lease format: "expiry MAC IP hostname client-id".
		line = fmt.Sprintf("%d aa:bb:cc:dd:ee:ff %s stolen-by *\n", expiry, ip)
	}
	if err := os.WriteFile(ef.leaseFile, []byte(line), 0o644); err != nil {
		ef.t.Fatalf("seed stolen lease: %v", err)
	}
}

// ServerIP returns the server's bare IP.
func (ef *EphemeralFixture) ServerIP() string {
	return strings.SplitN(ef.serverCIDR, "/", 2)[0]
}

// DNSAddr returns the "ip:port" of the WithDNS resolver (#261).
func (ef *EphemeralFixture) DNSAddr() string {
	return fmt.Sprintf("%s:%d", ef.ServerIP(), ef.dnsPort)
}

// DNSDomain returns the WithDNS domain.
func (ef *EphemeralFixture) DNSDomain() string { return ef.dnsDomain }

// CountLogLines counts log lines containing every substring, case-insensitive, across restarts.
//
// Kea's ACK line depends on its version (#612), measured on real logs: 2.6.3 writes a DHCPACK line and
// DHCP4_LEASE_ALLOC on bind and only the DHCPACK line on renewal; 2.4.1 writes only DHCP4_LEASE_ALLOC. A log with any
// DHCPACK line counts those, otherwise LEASE_ALLOC stands in.
func (ef *EphemeralFixture) CountLogLines(substrings ...string) int {
	ef.t.Helper()
	log := ef.readLog()
	tokens := ef.logTokens(log)
	count := 0
	for _, line := range strings.Split(log, "\n") {
		if lineMatches(line, substrings, tokens) {
			count++
		}
	}
	return count
}

// Kea 2.4.1 also writes DHCP4_LEASE_ADVERT for an offer and no DHCPOFFER line: measured on hosted run 34537348413,
// every DHCPOFFER count read zero (#942). The stand-in is chosen per message type, from the log.
func (ef *EphemeralFixture) logTokens(log string) map[string]string {
	tokens := map[string]string{"dhcpack": "dhcpack", "dhcpoffer": "dhcpoffer"}
	if ef.backend != backendKea {
		return tokens
	}
	lower := strings.ToLower(log)
	for literal, standIn := range map[string]string{
		"dhcpack":   "dhcp4_lease_alloc",
		"dhcpoffer": "dhcp4_lease_advert",
	} {
		if !strings.Contains(lower, literal) {
			tokens[literal] = standIn
		}
	}
	return tokens
}

func lineMatches(line string, substrings []string, tokens map[string]string) bool {
	l := strings.ToLower(line)
	for _, s := range substrings {
		want := strings.ToLower(s)
		if t, ok := tokens[want]; ok {
			want = t
		}
		if !strings.Contains(l, want) {
			return false
		}
	}
	return true
}

// LastACKAddress returns the address the server last granted mac, from its own log, or "" if it never ACKed one.
//
// A broadcast-flag ACK (RFC 2131 section 2) is sent to 255.255.255.255, so Kea's "lease <addr> has been allocated"
// line is preferred over the packet destination (#899).
func (ef *EphemeralFixture) LastACKAddress(mac string) string {
	ef.t.Helper()
	// Kea's ACK line depends on its version (#612).
	log := ef.readLog()
	tokens := ef.logTokens(log)

	addr, matched := lastACKAddressFrom(ef.backend, log, tokens["dhcpack"], mac)
	// An empty answer for an ACKed client would disable the caller's `acked != ""` divergence check.
	if matched > 0 && addr == "" {
		ef.t.Errorf("LastACKAddress(%s): the server logged %d ACK line(s) for this MAC and no "+
			"address could be read from any of them. The log format changed; fix the reader "+
			"rather than letting the divergence check pass on an empty answer.", mac, matched)
	}
	return addr
}

// lastACKAddressFrom returns the last ACKed address for mac and how many ACK lines matched it.
func lastACKAddressFrom(backend ephemeralBackend, log, ackToken, mac string) (addr string, matched int) {
	tokens := map[string]string{"dhcpack": ackToken}
	// One ordered pass: Kea 2.6.3 logs the allocation line just before the ACK, and a broadcast ACK leaves it standing.
	for _, line := range strings.Split(log, "\n") {
		isAlloc := lineMatches(line, []string{"has been allocated", mac}, tokens)
		isACK := lineMatches(line, []string{"DHCPACK", mac}, tokens)
		if !isAlloc && !isACK {
			continue
		}
		matched++
		if ip := ackAddress(backend, line); ip != "" {
			addr = ip
		}
	}
	return addr, matched
}

// ackAddress refuses a non-unicast destination, since a broadcast ACK's destination is not the lease (#899).
func ackAddress(backend ephemeralBackend, line string) string {
	fields := strings.Fields(line)
	if backend == backendKea {
		for i, f := range fields {
			var candidate string
			fromDest := false
			switch {
			case f == "to" && i+1 < len(fields):
				candidate = strings.SplitN(fields[i+1], ":", 2)[0]
				fromDest = true
			case f == "lease" && i+1 < len(fields):
				candidate = fields[i+1]
			default:
				continue
			}
			ip := net.ParseIP(candidate)
			if ip == nil || ip.To4() == nil {
				continue
			}
			if fromDest && !isUnicastHost(ip) {
				continue
			}
			return candidate
		}
		return ""
	}
	for i, f := range fields {
		if !strings.HasPrefix(f, "DHCPACK") || i+1 >= len(fields) {
			continue
		}
		if ip := net.ParseIP(fields[i+1]); ip != nil && ip.To4() != nil {
			return fields[i+1]
		}
		break
	}
	return ""
}

// isUnicastHost reports whether ip can be a client's own address.
func isUnicastHost(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return !v4.Equal(net.IPv4bcast) && !v4.IsUnspecified() && !v4.IsMulticast()
}

type keaLeaseGrant struct {
	line    string
	seconds int
}

// keaLeaseGrants returns every lease lifetime Kea logged granting, the only outside evidence of lease timing (#472).
func keaLeaseGrants(log string) []keaLeaseGrant {
	var out []keaLeaseGrant
	for _, line := range strings.Split(log, "\n") {
		if n, ok := keaLeaseAllocSeconds(line); ok {
			out = append(out, keaLeaseGrant{line: strings.TrimSpace(line), seconds: n})
		}
	}
	return out
}

// keaLeaseAllocSeconds requires the "seconds" unit and reports whether it matched, so a reworded line fails (#472).
func keaLeaseAllocSeconds(line string) (int, bool) {
	if !strings.Contains(line, "DHCP4_LEASE_ALLOC") {
		return 0, false
	}
	fields := strings.Fields(line)
	for i, f := range fields {
		if f != "for" || i+2 >= len(fields) {
			continue
		}
		if !strings.HasPrefix(fields[i+2], "second") {
			continue
		}
		if n, err := strconv.Atoi(fields[i+1]); err == nil {
			return n, true
		}
	}
	return 0, false
}

// GrantedLease returns the lifetime the server last granted mac, and whether it logged one; Kea only.
func (ef *EphemeralFixture) GrantedLease(mac string) (int, bool) {
	ef.t.Helper()
	seconds, found := 0, false
	needle := strings.ToLower(mac)
	for _, g := range keaLeaseGrants(ef.readLog()) {
		if !strings.Contains(strings.ToLower(g.line), needle) {
			continue
		}
		seconds, found = g.seconds, true
	}
	return seconds, found
}

// verifyLeaseGrants requires every granted lifetime to equal the configured one, at teardown (#356, #278).
func (ef *EphemeralFixture) verifyLeaseGrants() {
	if !ef.started || ef.backend != backendKea {
		return
	}
	grants := keaLeaseGrants(ef.readLog())
	problems := checkLeaseGrants(grants, ef.leaseSeconds)
	for _, p := range problems {
		ef.t.Error(p)
	}
	if len(problems) == 0 {
		ef.t.Logf("lease-grant check: %d allocation(s), every one granted %ds as configured",
			len(grants), ef.leaseSeconds)
	}
}

// checkLeaseGrants returns one message per granted lifetime that differs from want.
func checkLeaseGrants(grants []keaLeaseGrant, want int) []string {
	if len(grants) == 0 {
		return []string{fmt.Sprintf(
			"lease-grant check: the server logged no DHCP4_LEASE_ALLOC at all, so the %ds lease "+
				"this fixture is built on was never confirmed against the server (#472). "+
				"Either no client bound, or the line this parses for has changed.", want)}
	}
	var problems []string
	bad := 0
	for _, g := range grants {
		if g.seconds == want {
			continue
		}
		bad++
		if bad == 1 {
			problems = append(problems, fmt.Sprintf(
				"lease-grant check: the fixture asked for a %ds lease and the server granted %ds.\n"+
					"Every timing this test rests on (T1/T2, outage windows, expiry waits) was sized "+
					"against %ds, so the boundary it names is not the boundary it crossed (#472).\n"+
					"server said: %s", want, g.seconds, want, g.line))
		}
	}
	if bad > 1 {
		problems = append(problems, fmt.Sprintf(
			"lease-grant check: %d of %d allocations were granted a lifetime other than %ds",
			bad, len(grants), want))
	}
	return problems
}

func (ef *EphemeralFixture) readLog() string {
	data, err := os.ReadFile(ef.logFile)
	if err != nil {
		return fmt.Sprintf("(could not read ephemeral DHCP server log: %v)", err)
	}
	return string(data)
}

// DumpLogs mirrors Fixture.DumpLogs for failure-path diagnostics.
func (ef *EphemeralFixture) DumpLogs(write func(string)) {
	write(fmt.Sprintf("--- ephemeral %s log (lease=%ds) ---\n%s", ef.backend, ef.leaseSeconds, ef.readLog()))
}

func (ef *EphemeralFixture) teardown() {
	// Before Stop, while the log and temp dir exist.
	ef.verifyLeaseGrants()
	ef.Stop()
	if ef.tmpDir != "" {
		_ = os.RemoveAll(ef.tmpDir)
	}
	cleanupEphemeralLinks()
}

// cleanupEphemeralLinks removes the veth pair and namespace on teardown and at setup.
func cleanupEphemeralLinks() {
	_ = withCLocale(exec.Command("ip", "netns", "del", ephemeralNetns)).Run()
	for _, name := range []string{EphemeralHostVeth, ephemeralDhcpVeth} {
		if link, err := netlink.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	}
}
