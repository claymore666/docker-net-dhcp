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
	// EphemeralHostVeth / ephemeralDhcpVeth are the per-test veth pair
	// the failure-injection tests (#128) attach to. Distinct names and
	// subnet from the suite-static fixture so killing this DHCP server
	// can never starve the rest of the suite. The suite runs serially,
	// so static names are safe — each test tears its instance down.
	EphemeralHostVeth = "dh-itest-ehost"
	ephemeralDhcpVeth = "dh-itest-edhcp"

	// ephemeralNetns is the network namespace the Kea backend's server
	// end of the veth pair is moved into. The container-facing end
	// stays in the host namespace, so the plugin attaches to it
	// exactly as before.
	//
	// This is not tidiness, it is required (#356). dnsmasq binds its
	// DHCP socket as a device-scoped wildcard, 0.0.0.0%<iface>:67, and
	// Kea binds its fallback socket to a specific address — the kernel
	// refuses the second bind, because only dnsmasq sets SO_REUSEADDR.
	// TestMain always has the suite-static dnsmasq up, so in a shared
	// namespace Kea opens NO sockets at all. Measured both ways: Kea
	// alone binds fine; Kea with any dnsmasq present does not.
	//
	// The dnsmasq backend deliberately stays in the host namespace —
	// two dnsmasqs coexist happily, and the FQDN test queries the
	// fixture's resolver from the test process (see DNSAddr), which a
	// namespace would cut off.
	ephemeralNetns = "dh-itest-eph"

	EphemeralServerAddr = "192.168.101.1/24"
	// EphemeralAltServerAddr / pools: a wholly different subnet for
	// RestartOnSubnet — the "site got renumbered" shape. A renewal
	// REQUEST carrying the old subnet's address against this server
	// is a wrong-network refusal; the client must re-acquire here.
	EphemeralAltServerAddr = "192.168.102.1/24"
	EphemeralAltPoolStart  = "192.168.102.10"
	EphemeralAltPoolEnd    = "192.168.102.99"
	EphemeralPoolStart     = "192.168.101.10"
	EphemeralPoolEnd       = "192.168.101.99"
	// EphemeralParentAddr is the host's own address on the segment, for
	// tests that pass WithParentAddress. Outside the pool on purpose:
	// the server must never be able to lease the address the probe
	// sends from.
	EphemeralParentAddr = "192.168.101.2/24"
	// EphemeralShiftedPoolStart/End are a disjoint range for
	// Restart() in the NAK test: an address leased from the original
	// pool is out-of-range for an authoritative server configured
	// with this one, so its renewal REQUEST draws a DHCPNAK.
	EphemeralShiftedPoolStart = "192.168.101.150"
	EphemeralShiftedPoolEnd   = "192.168.101.199"

	// EphemeralDefaultLeaseSeconds is the lease this fixture grants
	// unless a test asks for another. 120s is not a floor any more —
	// Kea honours whatever it is told (#356) — it is deliberately the
	// value dnsmasq used to impose, so that every test which did not
	// opt into a shorter lease keeps the exact timing it was written
	// and tuned against.
	EphemeralDefaultLeaseSeconds = 120

	// EphemeralOutageLeaseSeconds is the short lease the outage tests
	// ask for. Their wall clock is dominated by "wait for a bound lease
	// to lapse", which under dnsmasq could never be under two minutes.
	//
	// 20s, not 5s: #356's own probe watched dhcpcd renew cleanly at 20s
	// (four consecutive renewals, no rebind fallback, no churn) and the
	// issue records that as the floor worth keeping. Going lower buys
	// seconds and starts testing dhcpcd's retry pathology instead of
	// the plugin's outage handling.
	EphemeralOutageLeaseSeconds = 20
)

// ephemeralBackend selects which DHCP server the fixture runs.
type ephemeralBackend int

const (
	// backendKea is the default: ISC Kea, whose valid-lifetime /
	// renew-timer / rebind-timer are honoured verbatim at any value
	// (#356). It is a real production server with real quirks, which
	// is the property the failure suite has been earning its keep on —
	// a hand-rolled Go fixture would exhibit none of them.
	backendKea ephemeralBackend = iota
	// backendDnsmasq stays for WithDNS only. Kea has no integrated
	// resolver: DNS registration there means kea-dhcp-ddns driving a
	// separate BIND, which is a great deal of moving parts for one
	// test that gains nothing from short leases (the FQDN test's cost
	// is container lifecycle, not lease timing). See WithDNS.
	backendDnsmasq
)

func (b ephemeralBackend) String() string {
	if b == backendDnsmasq {
		return "dnsmasq"
	}
	return "kea"
}

// EphemeralFixture is a per-test DHCP server on its own veth pair,
// for tests that break the server on purpose: SIGKILL it, bring it
// back with the lease DB intact, or bring it back reconfigured so
// held leases get refused. The suite-static Fixture must never be
// touched by failure tests — every other test depends on it staying
// up (#128).
//
// The server runs authoritative, like a production DHCP server that
// owns its subnet, so REQUESTs for out-of-pool or unknown addresses
// are refused rather than ignored.
type EphemeralFixture struct {
	t *testing.T

	backend ephemeralBackend

	cmd        *exec.Cmd
	tmpDir     string
	leaseFile  string
	configFile string
	// renderedConfig is the exact text written to configFile, kept so a
	// failure prints what kea was given rather than a fresh render.
	renderedConfig string
	logFile        string

	poolStart, poolEnd string
	serverCIDR         string

	// parentCIDR, when set, is an address put on the HOST side of the
	// veth pair — the interface tests hand to the driver as `parent`.
	// Empty by default, which leaves the parent bare.
	//
	// A bare parent is not the neutral choice it looks like; see
	// WithParentAddress.
	parentCIDR string

	// leaseSeconds is the granted lease lifetime (Kea valid-lifetime).
	// Under dnsmasq this was pinned to its 2m floor; it is now a knob,
	// which is the whole point of #356.
	leaseSeconds int

	// renewT1 / renewT2 are the server-advertised renewal (DHCP option
	// 58) and rebind (option 59) times in seconds. Zero means "don't
	// advertise them", and the client then derives both from the lease
	// as usual (T1 = lease/2, T2 = lease*7/8).
	//
	// Setting them independently of the lease is how a renewal test
	// drives a real DHCPACK-renewal on a fast clock without shortening
	// the lease itself — which matters because a renewal test needs the
	// lease to OUTLIVE the window it watches. See WithRenewTimes (#253).
	renewT1, renewT2 int

	// dnsDomain, when set, selects the dnsmasq backend and enables its
	// DNS resolver (instead of the default --port=0) on dnsPort with
	// this domain and --dhcp-fqdn, so a client that sends the DHCP FQDN
	// option (81) gets its <hostname>.<domain> registered and
	// resolvable. --dhcp-fqdn makes registration require the FQDN
	// option, so a plain option-12 hostname is NOT registered — which
	// is exactly what lets the FQDN test distinguish register_dns
	// on/off. See WithDNS (#261).
	dnsDomain string
	dnsPort   int

	// started records that the server reached readiness at least once.
	// verifyLeaseGrants keys on it so a fixture that died during start
	// is not additionally accused of never granting a lease — the
	// startup failure has already said what went wrong.
	started bool
}

// EphemeralOption configures an EphemeralFixture before its server
// starts. Options are applied in NewEphemeralFixture.
type EphemeralOption func(*EphemeralFixture)

// WithPool narrows the fixture's address pool. Passing the same
// address as start and end leaves the server exactly one address to
// give, which is how a test can know in advance which address an
// endpoint will be leased.
//
// That matters for the conflict scenario (#524), which has to park a
// squatter on the address BEFORE the container asks for it. Guessing
// is not an option: allocators do not hand out the low end of a range
// in order — dnsmasq hashes client identity across the whole pool, and
// a test that assumed otherwise was a coin flip that passed three runs
// and then failed twice on the same commit (see TestStaticIP_DriverOpt).
func WithPool(start, end string) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.poolStart = start
		ef.poolEnd = end
	}
}

// WithParentAddress puts addr (CIDR form) on the host side of the veth
// pair — the interface the test hands to the driver as `parent`.
//
// This models the ordinary deployment, where the macvlan or ipvlan
// parent is the host's own NIC and carries the host's address on the
// segment, and where a bridge parent always has one.
//
// It is required by any test that expects the address-conflict probe to
// reach a verdict, and the reason is a kernel behaviour rather than a
// harness detail: a host answers an ARP request only if it can route a
// reply back to the SENDER. With no address on the leased subnet the
// probe has to fall back to a link-local source, and a responder with no
// default route then stays silent. Measured on 6.12 over a veth pair,
// squatter on 192.168.101.42:
//
//	responder routes    link-local sender   on-subnet sender
//	none                INCOMPLETE          answered
//	link-local route    answered            -
//	default route       answered            -
//
// The fixture's namespace has no default route, so it is the strict
// left-hand column. That is deliberate: it is the configuration in which
// a detector that only works by luck goes red (#524).
// It is ON BY DEFAULT, derived from the fixture's server address, so
// the suite models the ordinary deployment rather than the exotic one.
// Use WithBareParent for the opposite case, and say why.
func WithParentAddress(addr string) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.parentCIDR = addr
	}
}

// WithBareParent leaves the parent with no address at all.
//
// This is the deployment where a NIC exists only to be a macvlan
// parent, and it is the configuration in which the address-conflict
// probe is degraded: with no on-subnet source to send from it cannot
// get an answer out of a gateway-less host, and must report
// "undetermined" instead of "clean". A test that wants that state asks
// for it here rather than getting it by accident, because getting it by
// accident is what made the first conflict probe look like it worked.
func WithBareParent() EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.parentCIDR = ""
	}
}

// defaultParentAddr derives the host's own address on a fixture's
// segment from the server address it was configured with, so a fixture
// on an alternate subnet gets a parent address on THAT subnet rather
// than a stale one from the default.
//
// Host octet 2: the server holds .1, and the pools start at .10.
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

// WithRenewTimes makes the fixture advertise DHCP option 58 (T1,
// renewal) and option 59 (T2, rebind) at the given seconds, leaving
// the lease itself alone. This lets a renewal test drive a real
// DHCPACK-renewal on a fast clock (T1 small) while the lease stays
// long enough that the window under test is unambiguously a renewal
// and not a rebind or a re-acquisition.
//
// t1 must stay above dhcpcd's internal renewal flooring to round-trip;
// t2 should exceed t1 so the test observes a renewal, not a rebind;
// and both should stay below the lease (#253).
func WithRenewTimes(t1, t2 int) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.renewT1 = t1
		ef.renewT2 = t2
	}
}

// WithLeaseSeconds sets the granted lease lifetime.
//
// This is the knob #356 existed to create. Under dnsmasq the lease was
// a hard 2m floor, so every outage test paid two minutes to watch a
// bound lease lapse; Kea honours 20s verbatim. Use
// EphemeralOutageLeaseSeconds unless a test needs something else, and
// state in the test WHY its value is what it is — these tests turn on
// inequalities between the outage length, T1/T2 and the lease, and a
// value picked without one is how a test silently stops exercising
// the boundary it names.
func WithLeaseSeconds(seconds int) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.leaseSeconds = seconds
	}
}

// WithDNS turns on a DNS resolver (on a dedicated high port, bound to
// the fixture interface) with the given domain and --dhcp-fqdn. A
// client that sends the DHCP FQDN option (81/39) then has its
// <hostname>.<domain> registered in this DNS; query it via DNSAddr.
// --dhcp-fqdn deliberately ignores plain option-12 hostnames, so a
// container WITHOUT register_dns does not resolve — the on/off proof
// for the FQDN test (#261).
//
// This option selects the dnsmasq backend, because only dnsmasq has an
// integrated resolver. The FQDN test's cost is container lifecycle,
// not lease timing, so it gains nothing from Kea's settable lease and
// is not worth a kea-dhcp-ddns + BIND stack to migrate (#356).
func WithDNS(domain string) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.backend = backendDnsmasq
		ef.dnsDomain = domain
		ef.dnsPort = 15353
	}
}

// NewEphemeralFixture creates the veth pair and starts the
// authoritative DHCP server. Teardown is registered via t.Cleanup and
// is idempotent against a previous panicked run's leftovers.
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
	// Set before the options run, so WithParentAddress can override it
	// and WithBareParent can clear it. No option changes serverCIDR —
	// only RestartOnSubnet does, at runtime, and a fixture renumbered
	// out from under its parent address probes in the degraded mode
	// from then on, which is the truth about that situation.
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
		// Server end goes into its own namespace so Kea can bind
		// UDP/67 at all — see ephemeralNetns. The container-facing end
		// stays here.
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

// LeaseSeconds is the lease lifetime this fixture grants, for tests
// that size a wait against it rather than hard-coding a number.
func (ef *EphemeralFixture) LeaseSeconds() int { return ef.leaseSeconds }

// isolated reports whether this fixture's server runs in its own
// network namespace. Only the Kea backend does; see ephemeralNetns.
func (ef *EphemeralFixture) isolated() bool { return ef.backend == backendKea }

// run executes a command, failing the test with its combined output.
// Used for the `ip` calls that have no clean netlink equivalent once a
// namespace is in play.
func (ef *EphemeralFixture) run(name string, args ...string) {
	ef.t.Helper()
	if out, err := withCLocale(exec.Command(name, args...)).CombinedOutput(); err != nil {
		ef.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// runNetns runs a command inside the fixture's network namespace.
func (ef *EphemeralFixture) runNetns(name string, args ...string) {
	ef.t.Helper()
	ef.run("ip", append([]string{"netns", "exec", ephemeralNetns, name}, args...)...)
}

// Squat parks a device on addr so that something other than the plugin's
// endpoint answers ARP for it — the condition #524 is about, where the
// DHCP server hands out an address a host already has because that host
// never asked the server for anything.
//
// The address is added on the SERVER side of the veth pair, inside the
// fixture's network namespace. That placement is the whole trick and it
// is not interchangeable with the alternatives:
//
//   - A second address on the host side would be answered locally. Both
//     ends of a veth pair in one namespace never put the request on the
//     wire, so the probe would see nothing and the test would pass while
//     proving nothing.
//   - A macvlan sibling of the parent is unreachable from the parent by
//     design, so that squatter is invisible too — a real limitation,
//     tracked as #528, and not the case this scenario is for.
//
// Across the namespace boundary the squatter is a genuinely remote
// device on the segment, which is the shape of the production incident.
//
// Returns the squatter's MAC, so a test can assert the conflict was
// reported against the right device rather than merely reported.
func (ef *EphemeralFixture) Squat(addr string) string {
	ef.t.Helper()
	if !ef.isolated() {
		ef.t.Fatal("Squat needs the namespaced fixture: in a shared namespace the kernel answers for the address locally and never ARPs, so the probe under test would never see it")
	}
	ef.runNetns("ip", "addr", "add", addr+"/24", "dev", ephemeralDhcpVeth)
	ef.t.Cleanup(func() {
		// Best-effort: the namespace is torn down with the fixture
		// anyway, so a failure here cannot leak past the test.
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

// netnsCommand builds a command that will run inside the fixture's
// namespace, or a plain one when the fixture is not isolated.
func (ef *EphemeralFixture) netnsCommand(name string, args ...string) *exec.Cmd {
	if !ef.isolated() {
		return withCLocale(exec.Command(name, args...))
	}
	return withCLocale(exec.Command("ip", append([]string{"netns", "exec", ephemeralNetns, name}, args...)...))
}

// PingFromServer pings ip from the DHCP server's side of the veth
// pair, returning ping's combined output and error.
//
// It exists because the server address may live in a namespace the
// test process cannot reach (see ephemeralNetns), so `ping -I
// <ServerIP>` run from the test binary would fail for the wrong
// reason — "no such address here" reported as "container unreachable",
// which is a false failure that looks exactly like a real one.
func (ef *EphemeralFixture) PingFromServer(ip string) ([]byte, error) {
	ef.t.Helper()
	return ef.netnsCommand("ping", "-c", "1", "-W", "2", "-I", ef.ServerIP(), ip).CombinedOutput()
}

// start launches the configured backend and blocks until it is ready
// to serve.
func (ef *EphemeralFixture) start() {
	ef.t.Helper()
	if ef.backend == backendKea {
		ef.startKea()
	} else {
		ef.startDnsmasq()
	}
	// Both start paths t.Fatalf on failure, so reaching here means the
	// server is serving.
	ef.started = true
}

// keaConfig renders the server config for the fixture's current pool,
// subnet and timers.
//
// Three Kea path restrictions shape this (#356, all found the hard
// way, all reported as a path that is "invalid" without saying why):
//
//   - the lease file must live under Kea's compiled-in data directory
//     (/var/lib/kea) unless KEA_DHCP_DATA_DIR says otherwise. The
//     fixture sets that env var to its own temp dir, which also means
//     two fixtures can never collide over a lease file;
//   - a logger's `output` is validated the same way, so the log goes
//     to stdout and the fixture captures the pipe — the same shape the
//     dnsmasq fixture already used;
//   - the PID file name is derived from the CONFIG file name, and its
//     directory (/run/kea) must exist before startup or Kea dies
//     before reporting any config error. teardown creates it.
//
// Severity stays at INFO deliberately. Every token the assertions key
// on — DHCPDISCOVER/DHCPREQUEST/DHCPOFFER/DHCPACK, the client MAC, the
// allocated address — is present at INFO. DEBUG additionally logs
// DHCP4_RESPONSE_DATA, which repeats "DHCPACK" for the same packet and
// would double every ACK count in this file.
// keaLoggerOutputKey is the name Kea gives the logger's output list.
// It was renamed from output_options to output-options in Kea 2.5.4,
// and the older spelling is what Debian/Ubuntu's stable 2.4.x expects.
// Resolved ONCE per test binary: one kea is on PATH and its answer
// cannot change underneath us.
var (
	keaLoggerKeyOnce sync.Once
	keaLoggerKey     string
)

// resolveKeaLoggerKey asks the installed Kea which spelling it accepts,
// rather than deciding from its version string.
//
// A version comparison would have to encode the 2.5.4 boundary, guess
// how a distribution numbers its backports, and be revisited whenever
// the name changes again. Feeding kea the real config under `-t` asks
// the only question that matters — will this server load this file —
// and is right by construction on versions nobody has thought about
// yet. The fallback is taken only for the specific parse error naming
// the key, so an unrelated config mistake still surfaces as itself.
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

// subnet is the CIDR of the network the server address sits on, which
// is what Kea's subnet4 entry needs (the server address itself is a
// host address inside it).
func (ef *EphemeralFixture) subnet() string {
	_, ipNet, err := net.ParseCIDR(ef.serverCIDR)
	if err != nil {
		ef.t.Fatalf("ParseCIDR %s: %v", ef.serverCIDR, err)
	}
	return ipNet.String()
}

// keaBinary is the DHCP server the ephemeral fixture runs, resolved
// through PATH rather than hard-coded.
//
// The failure this avoids was worth a CI round trip: with the binary
// missing, `ip netns exec` reports `exec of "/usr/sbin/kea-dhcp4"
// failed: No such file or directory` INTO THE SERVER LOG, and the
// fixture then reported the readiness timeout with that buried in a
// wall of config. The real answer — this environment has no kea — is
// one line, so say it as one line.
const keaBinary = "kea-dhcp4"

// requireKea fails the test with an actionable message when the DHCP
// server this fixture needs is not installed, rather than letting it
// surface as a readiness timeout.
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
	// Kea derives its PID file name from the config file name and will
	// not create the directory itself; without this it dies before any
	// config error is reported (#356).
	if err := os.MkdirAll("/run/kea", 0o755); err != nil {
		ef.t.Fatalf("mkdir /run/kea: %v", err)
	}
	// Kept so the diagnostics below print the config kea was actually
	// given, rather than re-rendering it and risking a message that
	// disagrees with the file that failed.
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
	// KEA_DHCP_DATA_DIR moves the lease file out of Kea's compiled-in
	// /var/lib/kea; KEA_LOCKFILE_DIR does the same for the logger's
	// interprocess lockfile. Together they keep every file this server
	// writes inside the fixture's own temp dir, so two fixtures can
	// never collide and teardown is a single RemoveAll.
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

	// Readiness is "a DISCOVER would be answered", and DHCP4_STARTED
	// alone does NOT mean that.
	//
	// Measured (#356): with any dnsmasq already holding UDP/67, Kea
	// fails every socket bind, logs DHCPSRV_NO_SOCKETS_OPEN — and then
	// logs DHCP4_STARTED anyway and sits there. A probe that keys on
	// DHCP4_STARTED alone therefore returns "ready" for a server that
	// will never answer anything, and every test built on it fails
	// later, somewhere else, for a reason that looks like a plugin bug.
	//
	// So the probe requires the interface to be listening AND no
	// socket failure in the same window. Matching from startMark, not
	// from the top of the file, is what makes it "this instance": the
	// log is appended to across every Stop/StartAgain cycle.
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
	ef.t.Fatalf("ephemeral kea did not become ready; config:\n%s\nlog:\n%s", ef.renderedConfig, ef.readLog())
}

func (ef *EphemeralFixture) startDnsmasq() {
	ef.t.Helper()
	logF, err := os.OpenFile(ef.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		ef.t.Fatalf("open ephemeral dnsmasq log: %v", err)
	}
	defer logF.Close()

	startMark := ef.logSize()
	// DNS off by default (--port=0). WithDNS turns the resolver on with a
	// domain + --dhcp-fqdn so FQDN-option clients become resolvable (#261).
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
		// Authoritative: NAK requests for leases this instance
		// doesn't recognise, like a real production server that owns
		// the subnet. Without it dnsmasq stays silent on unknown
		// REQUESTs and the NAK test would never see a NAK.
		"--dhcp-authoritative",
		"--dhcp-broadcast",
		"--log-dhcp",
		"--log-facility=-",
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

	// Ready when the new instance has logged its DHCP range. Match on
	// the pool's start address, not the surrounding words — dnsmasq
	// localizes its log strings ("IP range" is "IP-Bereich" under a
	// German locale, which is what the integration runner speaks),
	// but addresses are addresses in every language.
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

// keaSocketFailures are the messages Kea logs when it could not open
// the sockets it needs. Each means "this server will not answer",
// regardless of the DHCP4_STARTED that follows.
var keaSocketFailures = []string{
	"DHCPSRV_NO_SOCKETS_OPEN",
	"DHCPSRV_OPEN_SOCKET_FAIL",
	"DHCP4_OPEN_SOCKETS_FAILED",
}

// keaSocketFailure returns the first socket-failure marker present in
// a slice of Kea log output, or "" if there is none.
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

// Stop SIGKILLs the DHCP server — the unclean "router died" shape,
// no shutdown-side effects, lease DB left as-is on disk.
func (ef *EphemeralFixture) Stop() {
	ef.t.Helper()
	if ef.cmd == nil || ef.cmd.Process == nil {
		return
	}
	// Negative pid: the whole process group (Setpgid above).
	_ = syscall.Kill(-ef.cmd.Process.Pid, syscall.SIGKILL)
	_ = ef.cmd.Wait()
	ef.cmd = nil
}

// StartAgain restarts the server with the same pool and the preserved
// lease DB — the "router came back" shape. Existing leases are still
// known, so renewals from before the outage ACK on the same address.
func (ef *EphemeralFixture) StartAgain() {
	ef.t.Helper()
	if ef.cmd != nil {
		ef.t.Fatal("StartAgain: server still running; call Stop first")
	}
	ef.start()
}

// Restart brings the server back with a different pool and a wiped
// lease DB — the "subnet got renumbered / pool reconfigured" shape.
//
// Refusal caveat (learned from the first CI run, and true of Kea as
// well as dnsmasq): a server may silently IGNORE renewal REQUESTs for
// addresses outside its configured range rather than emit a DHCPNAK —
// the client recovers via expiry + re-DISCOVER either way. Tests here
// assert the recovery, not the wire message; see
// TestFailure_LeaseRefusedOnRenewal.
func (ef *EphemeralFixture) Restart(poolStart, poolEnd string) {
	ef.t.Helper()
	ef.Stop()
	ef.wipeLeaseDB()
	ef.poolStart, ef.poolEnd = poolStart, poolEnd
	ef.start()
}

// RestartOnSubnet brings the server back on a DIFFERENT subnet with
// a wiped lease DB — the "site got renumbered" shape. The old server
// address disappears from the veth, so unicast renewals die silently;
// the client's broadcast REBIND carries an address foreign to the new
// subnet (a wrong-network refusal, which the server may signal or stay
// silent about) and re-acquisition lands in the new pool.
func (ef *EphemeralFixture) RestartOnSubnet(serverCIDR, poolStart, poolEnd string) {
	ef.t.Helper()
	ef.Stop()
	ef.wipeLeaseDB()
	if ef.isolated() {
		// The server end lives in the fixture's namespace, so netlink
		// against the test process's own namespace cannot see it.
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

// keaLeaseCSVHeader is the memfile schema Kea 2.6 writes, copied from
// a lease file Kea itself produced. SeedStolenLease emits it verbatim;
// a seeded file carrying it was verified to load and to make the
// address unavailable (the probe's client was offered the next address
// in the pool instead). Whether a DRIFTED header is rejected loudly or
// ignored silently was not established — if a future Kea changes the
// schema, check DHCPSRV_MEMFILE_LEASE_FILE_LOAD in the fixture log
// before trusting a seed.
const keaLeaseCSVHeader = "address,hwaddr,client_id,valid_lifetime,expire,subnet_id,fqdn_fwd,fqdn_rev,hostname,state,user_context,pool_id"

// SeedStolenLease overwrites the (stopped) server's lease DB with a
// single entry assigning ip to a foreign client. On StartAgain the
// server loads it and treats ip as taken — the rightful client's
// renewal REQUEST then hits the classic "address in use" refusal, the
// scenario where a server reassigns a live lease (#128).
func (ef *EphemeralFixture) SeedStolenLease(ip string) {
	ef.t.Helper()
	if ef.cmd != nil {
		ef.t.Fatal("SeedStolenLease: stop the server first; the lease DB is read only at startup")
	}
	expiry := time.Now().Add(time.Hour).Unix()
	var line string
	if ef.backend == backendKea {
		// subnet_id must match the id in keaConfig's subnet4 entry, or
		// the loaded lease belongs to no configured subnet and is
		// ignored — seeding nothing, silently.
		line = fmt.Sprintf("%s\n%s,aa:bb:cc:dd:ee:ff,,3600,%d,1,0,0,stolen-by,0,,0\n",
			keaLeaseCSVHeader, ip, expiry)
	} else {
		// Lease-file format per dnsmasq(8):
		// "expiry MAC IP hostname client-id".
		line = fmt.Sprintf("%d aa:bb:cc:dd:ee:ff %s stolen-by *\n", expiry, ip)
	}
	if err := os.WriteFile(ef.leaseFile, []byte(line), 0o644); err != nil {
		ef.t.Fatalf("seed stolen lease: %v", err)
	}
}

// ServerIP returns the server's bare IP (the gateway it advertises by
// default — its own listen address).
func (ef *EphemeralFixture) ServerIP() string {
	return strings.SplitN(ef.serverCIDR, "/", 2)[0]
}

// DNSAddr returns the "ip:port" of the fixture's DNS resolver, for use
// as a custom net.Resolver target. Only meaningful when the fixture was
// built WithDNS; empty port otherwise (#261).
func (ef *EphemeralFixture) DNSAddr() string {
	return fmt.Sprintf("%s:%d", ef.ServerIP(), ef.dnsPort)
}

// DNSDomain returns the domain the fixture resolver appends to DHCP
// hostnames (set via WithDNS).
func (ef *EphemeralFixture) DNSDomain() string { return ef.dnsDomain }

// CountLogLines counts log lines containing every one of the given
// substrings (case-insensitive), e.g. ("DHCPACK", mac) or
// ("DHCPNAK", mac). The log accumulates across Stop/StartAgain/
// Restart cycles, so counts are monotonic for the fixture's lifetime.
//
// Backend-independent by construction: both servers name the message
// type and the client MAC on one line. Kea writes
//
//	DHCP4_PACKET_SEND [hwtype=1 <mac>], cid=[...], tid=0x...: trying to
//	send packet DHCPACK (type 5) from <server>:67 to <client>:68 on
//	interface <iface>
//
// and dnsmasq writes `DHCPACK(<iface>) <ip> <mac> [<hostname>]`.
//
// EXCEPT that which line Kea writes for an ACK depends on its version,
// and the two versions this project meets are on opposite sides of the
// change (#612). Measured on real logs, one client, bind plus renewals:
//
//	kea 2.6.3 (runner image)   bind:    DHCPACK line AND DHCP4_LEASE_ALLOC
//	                            renewal: DHCPACK line only
//	kea 2.4.1 (Ubuntu stable)  bind:    DHCP4_LEASE_ALLOC only
//	                            renewal: DHCP4_LEASE_ALLOC only
//	                            (no line containing DHCPACK at INFO, ever)
//
// So on 2.6.3 the DHCPACK line is the complete record and LEASE_ALLOC
// would double count the bind; on 2.4.1 LEASE_ALLOC is the only record
// there is. Neither token works alone and both together over-count.
// The rule is therefore decided per log, from the log: if it contains
// any DHCPACK line the server is one that writes them and only those
// are counted; if it contains none, LEASE_ALLOC stands in. That is
// version detection by what the server actually wrote rather than by
// what it calls itself, and it is pinned on both captures in
// ephemeral_test.go.
//
// Callers keep saying DHCPACK: it names what they mean, and the fact
// that Kea spells it two ways is one fact about one server, kept here.
func (ef *EphemeralFixture) CountLogLines(substrings ...string) int {
	ef.t.Helper()
	log := ef.readLog()
	ackToken := ef.keaACKToken(log)
	count := 0
	for _, line := range strings.Split(log, "\n") {
		if lineMatches(line, substrings, ackToken) {
			count++
		}
	}
	return count
}

// keaACKToken decides, for one log, which line stands for "the server
// ACKed a lease": the DHCPACK line where the server writes one, the
// lease allocation line where it does not. Non-Kea backends always
// mean the literal token. See CountLogLines for the measurements.
func (ef *EphemeralFixture) keaACKToken(log string) string {
	if ef.backend != backendKea {
		return "dhcpack"
	}
	if strings.Contains(strings.ToLower(log), "dhcpack") {
		return "dhcpack"
	}
	return "dhcp4_lease_alloc"
}

// lineMatches is the per-line predicate behind CountLogLines and
// LastACKAddress: every substring must appear (case-insensitive), with
// a caller's "DHCPACK" satisfied by ackToken instead.
func lineMatches(line string, substrings []string, ackToken string) bool {
	l := strings.ToLower(line)
	for _, s := range substrings {
		want := strings.ToLower(s)
		if want == "dhcpack" {
			want = ackToken
		}
		if !strings.Contains(l, want) {
			return false
		}
	}
	return true
}

// LastACKAddress returns the address in the most recent DHCPACK the
// server logged for mac, or "" if it never ACKed one.
//
// Use it to check that the container and the server agree on which
// address the container holds — the container's own view alone cannot
// catch a divergence, and this is the outside evidence that the health
// counters cannot supply.
func (ef *EphemeralFixture) LastACKAddress(mac string) string {
	ef.t.Helper()
	// Same per-log token choice as CountLogLines, for the same reason:
	// which line Kea writes for an ACK depends on its version (#612).
	log := ef.readLog()
	ackToken := ef.keaACKToken(log)
	last := ""
	for _, line := range strings.Split(log, "\n") {
		if !lineMatches(line, []string{"DHCPACK", mac}, ackToken) {
			continue
		}
		if ip := ackAddress(ef.backend, line); ip != "" {
			last = ip
		}
	}
	return last
}

// ackAddress pulls the ACKed address out of one server log line.
//
// Kea has two shapes, by version: `... DHCPACK ... to <addr>:68 ...`
// names the recipient of the packet, and `lease <addr> has been
// allocated` names the address granted. dnsmasq puts the address
// immediately after the DHCPACK token. All three are the address the
// server told the client to use, which is the claim callers check.
func ackAddress(backend ephemeralBackend, line string) string {
	fields := strings.Fields(line)
	if backend == backendKea {
		for i, f := range fields {
			var candidate string
			switch {
			case f == "to" && i+1 < len(fields):
				candidate = strings.SplitN(fields[i+1], ":", 2)[0]
			case f == "lease" && i+1 < len(fields):
				candidate = fields[i+1]
			default:
				continue
			}
			if ip := net.ParseIP(candidate); ip != nil && ip.To4() != nil {
				return candidate
			}
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

// keaLeaseGrant is one lifetime the server stated it granted, kept
// with the line it came from so a mismatch can quote the server
// verbatim instead of paraphrasing it.
type keaLeaseGrant struct {
	line    string
	seconds int
}

// keaLeaseGrants returns every lease lifetime Kea logged granting, in
// order.
//
// This is the fixture's only outside evidence about lease TIMING.
// Everything else the failure suite believes about the lease — T1, T2,
// the outage windows sized against them — is the number the fixture
// wrote into its own config, which is intent, not effect (#472). Kea
// states the effect on its way past:
//
//	DHCP4_LEASE_ALLOC [hwtype=1 <mac>], cid=[...], tid=0x...: lease
//	192.168.101.10 has been allocated for 20 seconds
//
// Worth having because this server demonstrably logs success while
// doing something else: DHCP4_STARTED is emitted after every socket
// bind has failed, which is why startKea's readiness probe had to
// grow a second condition (#356). A server that reports a clean start
// while deaf will report a clean start while clamping a lifetime.
func keaLeaseGrants(log string) []keaLeaseGrant {
	var out []keaLeaseGrant
	for _, line := range strings.Split(log, "\n") {
		if n, ok := keaLeaseAllocSeconds(line); ok {
			out = append(out, keaLeaseGrant{line: strings.TrimSpace(line), seconds: n})
		}
	}
	return out
}

// keaLeaseAllocSeconds pulls the granted lifetime out of one Kea
// DHCP4_LEASE_ALLOC line, reporting whether it found one.
//
// The bool is the whole point. A parser that returned a bare 0 on
// no-match would turn every check built on it into a silent no-op the
// day Kea rewords the message — the reasoning #356 applied to
// LastACKAddress, and the trap #472 exists to avoid here.
//
// The unit token is required for the same reason: a future "allocated
// for 2 minutes" must fail to parse rather than compare 2 against 120.
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

// GrantedLease returns the lease lifetime the server most recently
// told mac it was granting, and whether it said so at all.
//
// Use it where a test wants to reason about the lease actually in
// force rather than the one the fixture asked for. The blanket check
// in verifyLeaseGrants already holds the two equal for every run, so
// this is for tests that want to say so at the point they depend on
// it. Kea only; the dnsmasq backend does not log a granted lifetime.
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

// verifyLeaseGrants holds the server's granted lifetime against the
// one the fixture configured, for every allocation of this fixture's
// life. Called from teardown so it runs once per fixture and no test
// has to remember it — a check each test opts into is a check the next
// test forgets.
//
// Absolute, not "close enough": Kea honours valid-lifetime verbatim
// (#356 measured 20s against dnsmasq's 120s floor), so any difference
// means the fixture is not serving the timings its tests are built on.
// The failure suite turns on inequalities like T1 < outage < lease; if
// the lease is not the lease, TestFailure_ServerReturnsBeforeExpiry
// stops crossing T1 and passes without testing a renewal, green the
// whole way. That is the #278 shape, which this suite has shipped once
// already.
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

// checkLeaseGrants compares what the server granted against what the
// fixture asked for, returning one message per problem and nothing at
// all when they agree.
//
// Split out from verifyLeaseGrants so the negative control is an
// ordinary unit test rather than a thing someone has to remember to
// stage by hand: a check that has never been observed failing is not
// known to work, and this repo has shipped a guard that passed with
// the call it guarded deleted.
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
	// Before Stop, while the log is still readable and the temp dir
	// still exists.
	ef.verifyLeaseGrants()
	ef.Stop()
	if ef.tmpDir != "" {
		_ = os.RemoveAll(ef.tmpDir)
	}
	cleanupEphemeralLinks()
}

// cleanupEphemeralLinks removes the fixture's veth pair and namespace,
// both on teardown and defensively at setup so a previous panicked run
// cannot poison the next one.
//
// Deleting the namespace takes the server end of the veth with it, and
// deleting either end removes the pair — so the two steps overlap by
// design and both are best-effort.
func cleanupEphemeralLinks() {
	_ = withCLocale(exec.Command("ip", "netns", "del", ephemeralNetns)).Run()
	for _, name := range []string{EphemeralHostVeth, ephemeralDhcpVeth} {
		if link, err := netlink.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	}
}
