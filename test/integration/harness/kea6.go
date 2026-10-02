// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// Kea as the DHCPv6 server of an existing V6Fixture segment (#214). dnsmasq cannot delegate a prefix and its address
// side is the one the suite already measures; Kea is the only server of the lane that does both. The plugin has no
// IPv6-only mode, so the segment stays the V6Fixture's dual-stack bridge: dnsmasq serves v4 (and, in mode V6NoRA, ignores
// DHCPv6 and sends no advertisement) and the harness RASender is the router. Kea sends no advertisement itself.
//
// Kea runs in a network namespace of its own on a veth whose host end is a port of the fixture's bridge, so it shares the
// segment's broadcast domain with the containers and holds UDP/547 apart from any server of the host.
const (
	Kea6Netns       = "dh-itest-kea6"
	Kea6HostVeth    = "dh-itest-k6h"
	kea6SrvVeth     = "dh-itest-k6s"
	kea6Binary      = "kea-dhcp6"
	kea6ReadyBudget = 15 * time.Second
	udp547          = ":0223 "
)

type Kea6Fixture struct {
	t      V6FixtureT
	cfg    Kea6Config
	bridge string

	cmd      *exec.Cmd
	tmpDir   string
	logFile  string
	confFile string
	rendered string

	cap       *RACapture
	startedAt time.Time
}

// NewKea6Fixture brings Kea up on f's bridge and returns once it listens on UDP/547 with no socket failure logged.
func NewKea6Fixture(t V6FixtureT, f *V6Fixture, opts ...Kea6Option) *Kea6Fixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("Kea6Fixture needs root (got uid=%d)", os.Geteuid())
	}
	keaPath := requireKea6(t)
	cleanupKea6()

	k := &Kea6Fixture{t: t, bridge: f.Bridge(), cfg: Kea6Config{
		Interface: kea6SrvVeth, Subnet: V6SubnetV6CIDR, PoolStart: Kea6PoolStart, PoolEnd: Kea6PoolEnd,
	}}
	for _, o := range opts {
		o(&k.cfg)
	}
	t.Cleanup(k.teardown)

	k.addLink()
	k.run("ip", "netns", "add", Kea6Netns)
	k.run("ip", "link", "set", kea6SrvVeth, "netns", Kea6Netns)
	k.runNetns("ip", "link", "set", "lo", "up")
	// Without accept_dad=0 the link-local is tentative for a second and Kea opens no socket; without accept_ra=0 the
	// namespace would configure itself from the sender's advertisement and send solicitations of its own (#214).
	for _, kv := range [][2]string{{"disable_ipv6", "0"}, {"accept_dad", "0"}, {"accept_ra", "0"}} {
		k.runNetns("sh", "-c", fmt.Sprintf("echo %s > /proc/sys/net/ipv6/conf/%s/%s", kv[1], kea6SrvVeth, kv[0]))
	}
	k.runNetns("ip", "link", "set", kea6SrvVeth, "up")
	k.awaitLinkLocal()

	// Before Kea starts, so a server advertisement could not precede the capture.
	k.cap = StartRACaptureInNetns(t, Kea6Netns, kea6SrvVeth)

	tmp, err := os.MkdirTemp("", "dh-itest-kea6-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	k.tmpDir = tmp
	k.logFile = filepath.Join(tmp, "kea-dhcp6.log")
	for _, d := range []string{Kea6ConfDir, Kea6PidDir, Kea6LockDir, Kea6LeaseDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	k.confFile = filepath.Join(Kea6ConfDir, Kea6ConfFile)
	k.rendered = k.cfg.JSON()
	if err := os.WriteFile(k.confFile, []byte(k.rendered), 0o644); err != nil {
		t.Fatalf("write %s: %v", k.confFile, err)
	}

	conf := kea6ConfinementEvidence()
	t.Logf("kea6 fixture: bridge=%s pool=%s-%s pd=%t timers=%v %s", k.bridge, k.cfg.PoolStart, k.cfg.PoolEnd,
		k.cfg.PD != nil, timersOf(k.cfg), conf)
	k.start(keaPath, conf)
	return k
}

func timersOf(c Kea6Config) [4]int {
	v, p, r, b := c.Timers()
	return [4]int{v, p, r, b}
}

func requireKea6(t V6FixtureT) string {
	t.Helper()
	path, err := exec.LookPath(kea6Binary)
	if err != nil {
		t.Fatalf("%s not found in PATH: %v\n"+
			"The Kea6 fixture needs it (#214). On the pool runner it comes from the dhcp-ci-runner image "+
			"(ci/runner-image/Dockerfile installs kea-dhcp6-server); on the arm64 runner from its image; on the "+
			"hosted lane from the apt step. If this fires in CI the runner is on an image built before that "+
			"landed. Locally: install kea-dhcp6-server, see test/integration/README.md.", kea6Binary, err)
	}
	return path
}

func (k *Kea6Fixture) run(name string, args ...string) {
	k.t.Helper()
	if out, err := withCLocale(exec.Command(name, args...)).CombinedOutput(); err != nil {
		k.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

func (k *Kea6Fixture) runNetns(name string, args ...string) {
	k.t.Helper()
	k.run("ip", append([]string{"netns", "exec", Kea6Netns, name}, args...)...)
}

func (k *Kea6Fixture) addLink() {
	k.t.Helper()
	la := netlink.NewLinkAttrs()
	la.Name = Kea6HostVeth
	if err := netlink.LinkAdd(&netlink.Veth{LinkAttrs: la, PeerName: kea6SrvVeth}); err != nil {
		k.t.Fatalf("LinkAdd veth %s/%s: %v", Kea6HostVeth, kea6SrvVeth, err)
	}
	if err := os.WriteFile("/proc/sys/net/ipv6/conf/"+Kea6HostVeth+"/disable_ipv6", []byte("1"), 0o644); err != nil {
		k.t.Fatalf("disable IPv6 on %s: %v", Kea6HostVeth, err)
	}
	host, err := netlink.LinkByName(Kea6HostVeth)
	if err != nil {
		k.t.Fatalf("LinkByName %s: %v", Kea6HostVeth, err)
	}
	br, err := netlink.LinkByName(k.bridge)
	if err != nil {
		k.t.Fatalf("LinkByName %s: %v", k.bridge, err)
	}
	if err := netlink.LinkSetMaster(host, br); err != nil {
		k.t.Fatalf("enslave %s to %s: %v", Kea6HostVeth, k.bridge, err)
	}
	if err := netlink.LinkSetUp(host); err != nil {
		k.t.Fatalf("LinkSetUp %s: %v", Kea6HostVeth, err)
	}
}

func (k *Kea6Fixture) awaitLinkLocal() {
	k.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		out, err := withCLocale(exec.Command("ip", "netns", "exec", Kea6Netns, "ip", "-6", "-o", "addr", "show", "dev", kea6SrvVeth, "scope", "link")).CombinedOutput()
		last = string(out)
		if err == nil && strings.Contains(last, "inet6 fe80") && !strings.Contains(last, "tentative") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	k.t.Fatalf("no settled link-local address on %s in netns %s after 5s; Kea would open no socket.\n`ip -6 addr` said:\n%s",
		kea6SrvVeth, Kea6Netns, last)
}

func (k *Kea6Fixture) start(keaPath string, conf kea6Confinement) {
	k.t.Helper()
	logF, err := os.OpenFile(k.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		k.t.Fatalf("open kea6 log: %v", err)
	}
	defer logF.Close()

	k.startedAt = time.Now()
	k.cmd = withCLocale(exec.Command("ip", "netns", "exec", Kea6Netns, keaPath, "-c", k.confFile))
	k.cmd.Env = append(k.cmd.Env, "KEA_PIDFILE_DIR="+Kea6PidDir, "KEA_LOCKFILE_DIR="+Kea6LockDir)
	k.cmd.Stdout, k.cmd.Stderr = logF, logF
	k.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := k.cmd.Start(); err != nil {
		k.t.Fatalf("start kea6: %v", err)
	}

	deadline := time.Now().Add(kea6ReadyBudget)
	for time.Now().Before(deadline) {
		window := k.readLog()
		if why := Kea6SocketFailure(window); why != "" {
			k.t.Fatalf("kea6 started but opened no DHCPv6 socket (%s).\nconfig:\n%s\nlog:\n%s", why, k.rendered, window)
		}
		if strings.Contains(window, "DHCP6_STARTED") && k.listensOn547() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	log := k.readLog()
	k.t.Fatalf("kea6 did not listen on UDP/547 within %s (DHCP6_STARTED logged=%t)\nconfig:\n%s\nlog:\n%s\n%s",
		kea6ReadyBudget, strings.Contains(log, "DHCP6_STARTED"), k.rendered, log, kea6ConfinementHint(kea6ConfinementEvidence()))
}

// listensOn547 reads the namespace's own UDP table; the log line alone is printed before the sockets are opened.
func (k *Kea6Fixture) listensOn547() bool {
	out, err := withCLocale(exec.Command("ip", "netns", "exec", Kea6Netns, "cat", "/proc/net/udp6")).Output()
	return err == nil && strings.Contains(strings.ToUpper(string(out)), udp547)
}

func (k *Kea6Fixture) readLog() string {
	data, err := os.ReadFile(k.logFile)
	if err != nil {
		return fmt.Sprintf("(could not read the kea6 log: %v)", err)
	}
	return string(data)
}

func (k *Kea6Fixture) Stop() {
	if k.cmd == nil || k.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-k.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = k.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-k.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	k.cmd = nil
}

func (k *Kea6Fixture) teardown() {
	k.Stop()
	if k.tmpDir != "" {
		_ = os.RemoveAll(k.tmpDir)
	}
	cleanupKea6()
}

// cleanupKea6 removes the server, namespace, links and state files, on teardown and at setup so a killed run leaves none.
func cleanupKea6() {
	if err := withCLocale(exec.Command("pkill", "-f", kea6Binary+" -c "+Kea6ConfDir)).Run(); err == nil {
		time.Sleep(300 * time.Millisecond)
	}
	_ = withCLocale(exec.Command("ip", "netns", "del", Kea6Netns)).Run()
	for _, name := range []string{Kea6HostVeth, kea6SrvVeth} {
		if link, err := netlink.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	}
	_ = os.RemoveAll(Kea6ConfDir)
	leases, _ := filepath.Glob(filepath.Join(Kea6LeaseDir, Kea6LeaseFile+"*"))
	for _, p := range append(leases, filepath.Join(Kea6LeaseDir, Kea6ServerIDFile)) {
		_ = os.Remove(p)
	}
}

func (k *Kea6Fixture) Config() Kea6Config { return k.cfg }

func (k *Kea6Fixture) ConfigText() string { return k.rendered }

func (k *Kea6Fixture) StartedAt() time.Time { return k.startedAt }

func (k *Kea6Fixture) RACapture() *RACapture { return k.cap }

func (k *Kea6Fixture) Rows() []Kea6Row {
	data, err := os.ReadFile(filepath.Join(Kea6LeaseDir, Kea6LeaseFile))
	if err != nil {
		if !os.IsNotExist(err) {
			k.t.Fatalf("read Kea's lease file: %v", err)
		}
		return nil
	}
	return ParseKea6Leases(string(data))
}

func (k *Kea6Fixture) LeaseFileText() string {
	data, err := os.ReadFile(filepath.Join(Kea6LeaseDir, Kea6LeaseFile))
	if err != nil {
		return fmt.Sprintf("(no lease file: %v)", err)
	}
	return string(data)
}

func (k *Kea6Fixture) HeldRows(duid string, leaseType int) []Kea6Row {
	return Kea6Held(k.Rows(), duid, leaseType, time.Now().Unix())
}

func (k *Kea6Fixture) ServerAddrs() []netip.Addr {
	k.t.Helper()
	out, err := withCLocale(exec.Command("ip", "netns", "exec", Kea6Netns, "ip", "-6", "-o", "addr", "show", "dev", kea6SrvVeth)).Output()
	if err != nil {
		k.t.Fatalf("read the server link's addresses: %v", err)
	}
	var addrs []netip.Addr
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "inet6" && i+1 < len(fields) {
			bare, _, _ := strings.Cut(fields[i+1], "/")
			if a, err := netip.ParseAddr(bare); err == nil {
				addrs = append(addrs, a)
			}
		}
	}
	return addrs
}

func (k *Kea6Fixture) SegmentLinkLocals() []netip.Addr {
	k.t.Helper()
	link, err := netlink.LinkByName(k.bridge)
	if err != nil {
		k.t.Fatalf("LinkByName %s: %v", k.bridge, err)
	}
	all, err := util.DumpResult(netlink.AddrList(link, netlink.FAMILY_V6))
	if err != nil {
		k.t.Fatalf("AddrList %s: %v", k.bridge, err)
	}
	var out []netip.Addr
	for _, a := range all {
		if ip, ok := netip.AddrFromSlice(a.IP); ok && ip.IsLinkLocalUnicast() {
			out = append(out, ip)
		}
	}
	return out
}

func (k *Kea6Fixture) CountLogLines(substrings ...string) int {
	n := 0
	for _, line := range strings.Split(k.readLog(), "\n") {
		all := true
		for _, s := range substrings {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

func (k *Kea6Fixture) DumpLogs(write func(string)) {
	write(fmt.Sprintf("--- kea-dhcp6 config ---\n%s--- kea-dhcp6 log ---\n%s--- kea-leases6.csv ---\n%s",
		k.rendered, k.readLog(), k.LeaseFileText()))
}
