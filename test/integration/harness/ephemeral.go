//go:build integration

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// EphemeralShiftedPoolStart/End are a disjoint range for
	// Restart() in the NAK test: an address leased from the original
	// pool is out-of-range for an authoritative server configured
	// with this one, so its renewal REQUEST draws a DHCPNAK.
	EphemeralShiftedPoolStart = "192.168.101.150"
	EphemeralShiftedPoolEnd   = "192.168.101.199"
)

// EphemeralFixture is a per-test DHCP server on its own veth pair,
// for tests that break the server on purpose: SIGKILL it, bring it
// back with the lease DB intact, or bring it back reconfigured so
// held leases get NAKed. The suite-static Fixture must never be
// touched by failure tests — every other test depends on it staying
// up (#128).
//
// dnsmasq runs --dhcp-authoritative, like a production DHCP server
// that owns its subnet: REQUESTs for out-of-pool or unknown addresses
// are NAKed immediately instead of ignored.
type EphemeralFixture struct {
	t *testing.T

	cmd       *exec.Cmd
	tmpDir    string
	leaseFile string
	logFile   string

	poolStart, poolEnd string
	serverCIDR         string

	// renewT1 / renewT2 are server-advertised DHCP option 58
	// (renewal) / 59 (rebind) times in seconds. dnsmasq's minimum
	// *lease* is a hard 2m, so a renewal test can't ride a short
	// lease — but the server may advertise T1/T2 explicitly,
	// independent of lease length, and dhcpcd honours them. Zero
	// means "don't set the option" (dhcpcd then derives T1/T2 from
	// the lease as usual). See WithRenewTimes (#253).
	renewT1, renewT2 int
}

// EphemeralOption configures an EphemeralFixture before its dnsmasq
// starts. Options are applied in NewEphemeralFixture.
type EphemeralOption func(*EphemeralFixture)

// WithRenewTimes makes the fixture's dnsmasq advertise DHCP option 58
// (T1, renewal) and option 59 (T2, rebind) at the given seconds,
// regardless of the 2m lease floor. This lets a renewal test drive a
// real DHCPACK-renewal on a fast clock (T1 small) instead of waiting
// out half of a 2m lease. t1 must stay above dhcpcd's internal
// renewal flooring to round-trip; t2 should exceed t1 so the test
// observes a renewal, not a rebind (#253).
func WithRenewTimes(t1, t2 int) EphemeralOption {
	return func(ef *EphemeralFixture) {
		ef.renewT1 = t1
		ef.renewT2 = t2
	}
}

// NewEphemeralFixture creates the veth pair and starts the
// authoritative dnsmasq. Teardown is registered via t.Cleanup and is
// idempotent against a previous panicked run's leftovers.
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
		t:          t,
		poolStart:  EphemeralPoolStart,
		poolEnd:    EphemeralPoolEnd,
		serverCIDR: EphemeralServerAddr,
	}
	for _, opt := range opts {
		opt(ef)
	}
	t.Cleanup(ef.teardown)

	hostLink, err := netlink.LinkByName(EphemeralHostVeth)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", EphemeralHostVeth, err)
	}
	dhcpLink, err := netlink.LinkByName(ephemeralDhcpVeth)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", ephemeralDhcpVeth, err)
	}
	if err := netlink.LinkSetUp(hostLink); err != nil {
		t.Fatalf("LinkSetUp %s: %v", EphemeralHostVeth, err)
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

	tmp, err := os.MkdirTemp("", "dh-itest-ephemeral-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	ef.tmpDir = tmp
	ef.leaseFile = filepath.Join(tmp, "leases")
	ef.logFile = filepath.Join(tmp, "dnsmasq.log")

	ef.start()
	return ef
}

// start launches dnsmasq with the fixture's current pool and waits
// until it has logged its DHCP range (the readiness probe used for
// the static fixture — binding UDP/67 — can't distinguish this
// instance from the suite fixture's dnsmasq, which is also up).
func (ef *EphemeralFixture) start() {
	ef.t.Helper()
	logF, err := os.OpenFile(ef.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		ef.t.Fatalf("open ephemeral dnsmasq log: %v", err)
	}
	defer logF.Close()

	startMark := ef.logSize()
	args := []string{
		"--no-daemon",
		"--conf-file=/dev/null",
		"--port=0",
		"--interface=" + ephemeralDhcpVeth,
		"--bind-interfaces",
		"--except-interface=lo",
		"--dhcp-range=" + ef.poolStart + "," + ef.poolEnd + "," + LeaseTime,
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
	// Explicit renewal/rebind times (options 58/59), independent of the
	// 2m lease floor: lets a renewal test drive a fast DHCPACK-renewal
	// instead of waiting out half the lease (#253). dnsmasq encodes
	// these known options as 32-bit seconds.
	if ef.renewT1 > 0 {
		args = append(args, fmt.Sprintf("--dhcp-option=58,%d", ef.renewT1))
	}
	if ef.renewT2 > 0 {
		args = append(args, fmt.Sprintf("--dhcp-option=59,%d", ef.renewT2))
	}
	ef.cmd = exec.Command("/usr/sbin/dnsmasq", args...)
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

func (ef *EphemeralFixture) logSize() int {
	st, err := os.Stat(ef.logFile)
	if err != nil {
		return 0
	}
	return int(st.Size())
}

// Stop SIGKILLs the DHCP server — the unclean "router died" shape, no
// DHCPRELEASE-side effects, lease DB left as-is on disk.
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

// StartAgain restarts dnsmasq with the same pool and the preserved
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
// NAK caveat (learned from the first CI run): dnsmasq 2.91 silently
// IGNORES renewal REQUESTs for addresses outside its configured range
// even with --dhcp-authoritative — the client recovers via expiry +
// re-DISCOVER, but no DHCPNAK is ever emitted. To provoke an actual
// NAK, use SeedStolenLease + StartAgain instead.
func (ef *EphemeralFixture) Restart(poolStart, poolEnd string) {
	ef.t.Helper()
	ef.Stop()
	if err := os.Remove(ef.leaseFile); err != nil && !os.IsNotExist(err) {
		ef.t.Fatalf("wipe ephemeral lease DB: %v", err)
	}
	ef.poolStart, ef.poolEnd = poolStart, poolEnd
	ef.start()
}

// RestartOnSubnet brings the server back on a DIFFERENT subnet with
// a wiped lease DB — the "site got renumbered" shape. The old server
// address disappears from the veth, so unicast renewals die silently;
// the client's broadcast REBIND carries an address foreign to the new
// subnet (wrong-network refusal — dnsmasq may NAK or stay silent
// depending on the shape) and re-acquisition lands in the new pool.
func (ef *EphemeralFixture) RestartOnSubnet(serverCIDR, poolStart, poolEnd string) {
	ef.t.Helper()
	ef.Stop()
	if err := os.Remove(ef.leaseFile); err != nil && !os.IsNotExist(err) {
		ef.t.Fatalf("wipe ephemeral lease DB: %v", err)
	}
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
	ef.serverCIDR = serverCIDR
	ef.poolStart, ef.poolEnd = poolStart, poolEnd
	ef.start()
}

// SeedStolenLease overwrites the (stopped) server's lease DB with a
// single entry assigning ip to a foreign client. On StartAgain,
// dnsmasq loads it and treats ip as taken — the rightful client's
// renewal REQUEST then draws the classic "address in use" DHCPNAK,
// the scenario where a server reassigns a live lease (#128).
// Lease-file format per dnsmasq(8): "expiry MAC IP hostname client-id".
func (ef *EphemeralFixture) SeedStolenLease(ip string) {
	ef.t.Helper()
	if ef.cmd != nil {
		ef.t.Fatal("SeedStolenLease: stop the server first; dnsmasq reads the lease DB only at startup")
	}
	expiry := time.Now().Add(time.Hour).Unix()
	line := fmt.Sprintf("%d aa:bb:cc:dd:ee:ff %s stolen-by *\n", expiry, ip)
	if err := os.WriteFile(ef.leaseFile, []byte(line), 0o644); err != nil {
		ef.t.Fatalf("seed stolen lease: %v", err)
	}
}

// ServerIP returns the server's bare IP (the gateway dnsmasq
// advertises by default — its own listen address).
func (ef *EphemeralFixture) ServerIP() string {
	return strings.SplitN(ef.serverCIDR, "/", 2)[0]
}

// CountLogLines counts log lines containing every one of the given
// substrings (case-insensitive), e.g. ("DHCPACK", mac) or
// ("DHCPNAK", mac). The log accumulates across Stop/StartAgain/
// Restart cycles, so counts are monotonic for the fixture's lifetime.
func (ef *EphemeralFixture) CountLogLines(substrings ...string) int {
	ef.t.Helper()
	count := 0
	for _, line := range strings.Split(ef.readLog(), "\n") {
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

func (ef *EphemeralFixture) readLog() string {
	data, err := os.ReadFile(ef.logFile)
	if err != nil {
		return fmt.Sprintf("(could not read ephemeral dnsmasq log: %v)", err)
	}
	return string(data)
}

// DumpLogs mirrors Fixture.DumpLogs for failure-path diagnostics.
func (ef *EphemeralFixture) DumpLogs(write func(string)) {
	write("--- ephemeral dnsmasq log ---\n" + ef.readLog())
}

func (ef *EphemeralFixture) teardown() {
	ef.Stop()
	if ef.tmpDir != "" {
		_ = os.RemoveAll(ef.tmpDir)
	}
	cleanupEphemeralLinks()
}

func cleanupEphemeralLinks() {
	for _, name := range []string{EphemeralHostVeth, ephemeralDhcpVeth} {
		if link, err := netlink.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	}
}
