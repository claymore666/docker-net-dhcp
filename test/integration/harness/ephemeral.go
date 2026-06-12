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
	EphemeralPoolStart  = "192.168.101.10"
	EphemeralPoolEnd    = "192.168.101.99"
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
}

// NewEphemeralFixture creates the veth pair and starts the
// authoritative dnsmasq. Teardown is registered via t.Cleanup and is
// idempotent against a previous panicked run's leftovers.
func NewEphemeralFixture(t *testing.T) *EphemeralFixture {
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
		t:         t,
		poolStart: EphemeralPoolStart,
		poolEnd:   EphemeralPoolEnd,
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
	addr, err := netlink.ParseAddr(EphemeralServerAddr)
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
	ef.cmd = exec.Command("/usr/sbin/dnsmasq",
		"--no-daemon",
		"--conf-file=/dev/null",
		"--port=0",
		"--interface="+ephemeralDhcpVeth,
		"--bind-interfaces",
		"--except-interface=lo",
		"--dhcp-range="+ef.poolStart+","+ef.poolEnd+","+LeaseTime,
		"--dhcp-leasefile="+ef.leaseFile,
		"--dhcp-no-override",
		// Authoritative: NAK requests for leases this instance
		// doesn't recognise, like a real production server that owns
		// the subnet. Without it dnsmasq stays silent on unknown
		// REQUESTs and the NAK test would never see a NAK.
		"--dhcp-authoritative",
		"--dhcp-broadcast",
		"--log-dhcp",
		"--log-facility=-",
	)
	ef.cmd.Stdout = logF
	ef.cmd.Stderr = logF
	ef.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := ef.cmd.Start(); err != nil {
		ef.t.Fatalf("start ephemeral dnsmasq: %v", err)
	}

	// Ready when the new instance has logged its IP range.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(ef.logFile)
		if err == nil && len(data) > startMark &&
			strings.Contains(string(data[startMark:]), "IP range") {
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
// Being authoritative, the new instance NAKs renewal REQUESTs for
// addresses from the old pool.
func (ef *EphemeralFixture) Restart(poolStart, poolEnd string) {
	ef.t.Helper()
	ef.Stop()
	if err := os.Remove(ef.leaseFile); err != nil && !os.IsNotExist(err) {
		ef.t.Fatalf("wipe ephemeral lease DB: %v", err)
	}
	ef.poolStart, ef.poolEnd = poolStart, poolEnd
	ef.start()
}

// ServerIP returns the server's bare IP (the gateway dnsmasq
// advertises by default — its own listen address).
func (ef *EphemeralFixture) ServerIP() string {
	return strings.SplitN(EphemeralServerAddr, "/", 2)[0]
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
