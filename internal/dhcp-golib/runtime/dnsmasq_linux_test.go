//go:build linux

package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
)

// This file is done-condition (a): a lease acquired from a REAL dnsmasq over a
// real AF_PACKET socket, asserted against THE SERVER'S OWN LOG rather than
// against the library's opinion of what happened.
//
// The distinction is the whole point and it is a lesson this project already
// paid for: every place its suite checked a counter alone, something shipped
// broken; every place it checked the server, it stayed sound. A client that
// invented a lease out of nothing would satisfy every assertion about its own
// state. It cannot make dnsmasq write DHCPACK into a log.
//
// # Why this runs without root, and what that costs
//
// The test re-executes itself into a new USER and NETWORK namespace. Inside
// the user namespace the process is uid 0 and holds CAP_NET_ADMIN and
// CAP_NET_RAW over its own network namespace, which is exactly what creating a
// veth pair, binding AF_PACKET and binding UDP port 67 need. No password, no
// setuid binary, no capability granted on the host.
//
// It also means requirement T7 holds by construction rather than by care:
// every interface, address and socket here lives in a namespace that ceases to
// exist when the process does. The test cannot mutate host state because it
// cannot see host state.
//
// # It FAILS rather than skips when it cannot run
//
// A skip here would be indistinguishable from a pass in every summary anyone
// reads, on the one test that talks to a real server. If unprivileged user
// namespaces are disabled, or dnsmasq is absent, that is a fact about the
// machine that must be visible, and the fix is to install the one and enable
// the other.
//
// # And it fails rather than passing when it runs NOTHING
//
// The parent process cannot see the exchange; it sees a child's exit status.
// `go test` exits 0 when its -test.run filter matches no test, so an exit
// status alone cannot tell a completed exchange from a run that never
// started. MEASURED 2026-08-30 against the version of this file that passed
// its own name as a literal: renaming the test function took the run from
// `ok 3.084s` to `ok 0.006s`, printed `--- PASS`, and put
// `testing: warning: no tests to run` in a log nobody reads.
//
// So the filter is derived from t.Name() rather than written twice, and the
// parent asserts on the child's own report of the named test. See
// childReport.

const nsChildEnv = "DHCP_GOLIB_NETNS_CHILD"

const (
	testClientIf = "cli0"
	testServerIf = "srv0"
	testServerIP = "192.168.99.1"
	testSubnet   = "255.255.255.0"
	testRangeLo  = "192.168.99.100"
	testRangeHi  = "192.168.99.150"
	testDomain   = "dhcp.test"
	testMTU      = 1400
	testLeaseSec = 120
)

func TestAcquiresFromRealDnsmasq(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		runAgainstDnsmasq(t)
		return
	}
	reexecInNamespaces(t)
}

// reexecInNamespaces runs this test binary again, for the CALLING test only,
// inside a fresh user and network namespace, and fails unless the child's
// output shows that test reporting its own PASS.
//
// The name is taken from t.Name() and never passed in: a name written twice is
// a filter that stops matching the moment the test is renamed, and the failure
// mode of that is a silent pass.
func reexecInNamespaces(t *testing.T) {
	t.Helper()
	name := t.Name()

	if _, err := os.Stat("/proc/self/ns/user"); err != nil {
		t.Fatalf("this kernel has no user namespaces (%v); done-condition (a) cannot be measured here", err)
	}
	if _, err := findDnsmasq(); err != nil {
		t.Fatalf("%v — install dnsmasq; this test is the only one that talks to a real server", err)
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Fatalf("the ip command is not on PATH (%v); the namespace cannot be wired up", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v=true", "-test.count=1")
	cmd.Env = append(os.Environ(), nsChildEnv+"=1", "LC_ALL=C", "LANG=C")
	// uid 0 inside the namespace, and it has to be 0: capabilities are
	// recalculated at execve, and a process that is not root in its user
	// namespace and carries no file capabilities comes out of exec with an
	// empty set. MEASURED 2026-08-29 by mapping the caller's own id instead —
	// `ip link add` then fails with EPERM, because CAP_NET_ADMIN was dropped
	// by the exec rather than never granted.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		// setgroups must be denied before an unprivileged process may write a
		// gid map; the kernel refuses the mapping otherwise.
		GidMappingsEnableSetgroups: false,
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the namespaced run failed (%v). Its output follows.\n%s", err, out)
	}
	if reportErr := childReport(string(out), name); reportErr != nil {
		t.Fatalf("the namespaced run exited 0, but %v — so this test measured nothing.\nchild output:\n%s", reportErr, out)
	}
	t.Logf("namespaced run output:\n%s", out)
}

// childReport reports whether out is a test binary's account of having RUN the
// test called name.
//
// It is a pure function of (output, name) so that the thing standing between a
// green run and a vacuous one is itself driven by a table rather than by a
// namespace only Linux can enter. See TestChildReportRejectsARunThatRanNothing.
//
// It fails closed: empty output, a truncated stream, a child killed before it
// printed, or a future `go test` that words its summary differently all leave
// the "--- PASS: name" line unfound, and an unfound line is an error.
//
// The "--- PASS" arm is the load-bearing one and deleting it kills a case in
// that table. The "no tests to run" arm above it is NOT independently driven,
// and is honest about being a better diagnosis rather than a second check:
// deleting it leaves every case still correctly judged, one of them with a
// worse message.
func childReport(out, name string) error {
	if strings.Contains(out, "no tests to run") {
		return fmt.Errorf("the child reported %q, so the -test.run filter matched no test", "testing: warning: no tests to run")
	}
	want := "--- PASS: " + name + " ("
	if !strings.Contains(out, want) {
		return fmt.Errorf("the child's output carries no %q line", want)
	}
	return nil
}

// TestChildReportRejectsARunThatRanNothing drives childReport over the outputs
// it has to tell apart, including the one the old exit-status-only check
// accepted.
func TestChildReportRejectsARunThatRanNothing(t *testing.T) {
	const name = "TestAcquiresFromRealDnsmasq"
	for _, c := range []struct {
		what string
		out  string
		ok   bool
	}{
		{"a real pass", "=== RUN   " + name + "\n--- PASS: " + name + " (3.08s)\nPASS\nok  \tpkg\t3.084s\n", true},
		{"the renamed-test output that used to pass", "testing: warning: no tests to run\nPASS\nok  \tpkg\t0.006s\n", false},
		{"a pass belonging to a different test", "--- PASS: TestSomethingElse (0.01s)\nPASS\n", false},
		{"a prefix of the name", "--- PASS: " + name + "Extra (0.01s)\nPASS\n", false},
		{"nothing at all", "", false},
		{"a stream cut off mid-run", "=== RUN   " + name + "\n", false},
	} {
		err := childReport(c.out, name)
		if c.ok && err != nil {
			t.Errorf("%s: childReport refused a run that did happen: %v", c.what, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: childReport accepted an output that is not a report of %s having run", c.what, name)
		}
	}
}

func findDnsmasq() (string, error) {
	if p, err := exec.LookPath("dnsmasq"); err == nil {
		return p, nil
	}
	// dnsmasq installs into sbin, which is usually absent from a non-root
	// PATH. Looking there is not a workaround: the binary is world-executable
	// and needs no privilege to run inside our own namespace.
	for _, p := range []string{"/usr/sbin/dnsmasq", "/sbin/dnsmasq", "/usr/local/sbin/dnsmasq"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("dnsmasq was not found on PATH or in the usual sbin directories")
}

// runAgainstDnsmasq is the body, executed inside the namespaces.
func runAgainstDnsmasq(t *testing.T) {
	mustRun(t, "ip", "link", "add", testClientIf, "type", "veth", "peer", "name", testServerIf)
	mustRun(t, "ip", "addr", "add", testServerIP+"/24", "dev", testServerIf)
	mustRun(t, "ip", "link", "set", testServerIf, "up")
	mustRun(t, "ip", "link", "set", testClientIf, "up")

	srv := startDnsmasq(t)

	iface, err := net.InterfaceByName(testClientIf)
	if err != nil {
		t.Fatalf("InterfaceByName(%s): %v", testClientIf, err)
	}
	clientMAC := iface.HardwareAddr.String()
	t.Logf("client interface %s has hardware address %s", testClientIf, clientMAC)

	params := proto.DefaultParams(iface.HardwareAddr)
	// The desync delay would add up to ten seconds of nothing to a test whose
	// subject is the exchange. Ring 1 pins the RFC default separately.
	params.DesyncMin, params.DesyncMax = 0, 0
	// ConflictAsync, and it is the ONE line in this fixture that is a
	// judgement rather than a scaling.
	//
	// The subject of this test is the DHCP exchange. RFC 5227's
	// probe-before-use — the default, ConflictWait — adds four to seven
	// seconds to EVERY acquisition here, which is the price D22 charges a
	// container and not something worth paying in a test of something else;
	// conflict_dnsmasq_linux_test.go pays it once, with the RFC's own
	// constants, and measures it.
	//
	// Async rather than off, because off would take the ARP socket, the
	// probes and section 2.4's listener out of the path of every one of these
	// tests. Under async they are all still there and still running beside
	// the exchange; what changes is only when the caller is told it may use
	// the address. A conflict check that broke an ordinary acquisition, or
	// that saw this host's own frames as somebody else's, would still redden
	// these tests.
	//
	// briskACD scales the schedule's DURATIONS and not its counts, so the
	// probing here is over in a third of a second instead of overlapping the
	// renewal and the reboot these tests are about. Its reasons are at its
	// definition.
	params.Conflict = proto.ConflictAsync
	params.ACD = briskACD()
	params.Hostname = "m1-client"

	c, err := NewClient(ClientConfig{Interface: testClientIf, Params: params, EventBuffer: 8})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	// Barrier: the Acquired event. No duration appears anywhere in this file —
	// if the exchange never completes, the test hangs until go test's own
	// timeout, which is a louder and more honest failure than a sleep that
	// was too short.
	var acquired lease.Event
	for ev := range c.Events() {
		t.Logf("client event: %s", ev)
		if ev.Kind == lease.Acquired {
			acquired = ev
			break
		}
		if ev.Kind == lease.Failed {
			t.Fatalf("acquisition failed: %s", ev)
		}
	}
	if acquired.Kind != lease.Acquired {
		t.Fatal("the event stream ended before a lease was acquired")
	}

	leased := acquired.Lease.Addr.Addr().String()
	t.Logf("client acquired %s", acquired.Lease)

	// ------------------------------------------------- the server's own log --
	//
	// This is the assertion that matters. Everything above is the client
	// describing itself.
	want := []string{
		"DHCPDISCOVER(" + testServerIf + ") " + clientMAC,
		"DHCPOFFER(" + testServerIf + ") " + leased + " " + clientMAC,
		"DHCPREQUEST(" + testServerIf + ") " + leased + " " + clientMAC,
		"DHCPACK(" + testServerIf + ") " + leased + " " + clientMAC,
	}
	srv.waitFor(t, want[len(want)-1])
	log := srv.lines()
	for _, w := range want {
		if !containsLine(log, w) {
			t.Fatalf("the server's log has no line containing %q.\nServer log:\n%s", w, strings.Join(log, "\n"))
		}
	}

	// The server's log is also the check on the lease CONTENTS: the address
	// the client reports is the address dnsmasq says it handed out, and it is
	// inside the range dnsmasq was configured with.
	if !inRange(leased, testRangeLo, testRangeHi) {
		t.Fatalf("the client leased %s, outside the configured range %s..%s", leased, testRangeLo, testRangeHi)
	}
	if got := acquired.Lease.Addr.Bits(); got != 24 {
		t.Fatalf("prefix = /%d, want /24 from subnet mask %s", got, testSubnet)
	}
	if got := acquired.Lease.Gateway.String(); got != testServerIP {
		t.Fatalf("gateway = %s, want %s", got, testServerIP)
	}
	if got := acquired.Lease.Domain; got != testDomain {
		t.Fatalf("domain = %q, want %q", got, testDomain)
	}
	if got := acquired.Lease.MTU; got != testMTU {
		t.Fatalf("mtu = %d, want %d", got, testMTU)
	}
	if got := acquired.Lease.Expire.Sub(acquired.Lease.Acquired); int(got.Seconds()) != testLeaseSec {
		t.Fatalf("lease runs for %s, want %ds", got, testLeaseSec)
	}

	// ----------------------------------------- done-condition (b), for real --
	//
	// The exchange that just happened, replayed offline through ring 1, must
	// produce the identical lease. This is the unit replay test again with the
	// one thing a fixture cannot supply: packets a real server actually sent.
	cancel()
	<-runErr

	if dropped := c.JournalDropped(); dropped != 0 {
		t.Fatalf("the journal dropped %d entries, so it is not replayable", dropped)
	}
	entries := c.Journal()
	if len(entries) == 0 {
		t.Fatal("the journal is empty")
	}
	var toBound []proto.JournalEntry
	for _, e := range entries {
		toBound = append(toBound, e)
		if e.To == proto.StateBound {
			break
		}
	}
	if toBound[len(toBound)-1].To != proto.StateBound {
		t.Fatal("the journal never records reaching BOUND")
	}
	res, err := proto.Replay(params, toBound)
	if err != nil {
		t.Fatalf("the real exchange does not replay: %v", err)
	}
	if !res.Held {
		t.Fatal("the replay produced no lease")
	}
	if res.Lease.Addr != acquired.Lease.Addr {
		t.Fatalf("replayed %s, the live client got %s", res.Lease.Addr, acquired.Lease.Addr)
	}
	if res.Lease.Domain != acquired.Lease.Domain || res.Lease.MTU != acquired.Lease.MTU {
		t.Fatalf("replayed lease differs: %+v", res.Lease)
	}

	// The packet ring holds the real exchange, decoded (G1). Four packets:
	// DISCOVER, OFFER, REQUEST, ACK.
	pkts := c.Packets()
	if len(pkts) < 4 {
		t.Fatalf("the packet ring holds %d packets, want at least the four of an acquisition", len(pkts))
	}
	ts := c.TransportStats()
	t.Logf("transport: %d reads, %d skipped as not-for-us, %d sends, %d uncompleted checksums, %d absent",
		ts.Reads, ts.Skipped, ts.Sends, ts.Uncompleted, ts.Absent)
	// MEASURED 2026-08-29 on this path: every reply arrives with its UDP
	// checksum UNCOMPLETED. The sending kernel writes the pseudo-header sum
	// and leaves the rest to hardware that a veth pair does not have, so the
	// count equals the number of replies read.
	//
	// This is asserted rather than logged because an uncounted counter is the
	// failure this project keeps paying for: the first run of this test hung
	// for two minutes on exactly these frames being discarded, and a count
	// nobody checks would let that return silently. It is also the one
	// assertion here that could go red for a HEALTHY reason — a kernel that
	// completes the sum on a local delivery path would drive it to zero — so
	// the message says so, and ipudp_test.go pins the parsing behaviour
	// against captured bytes where no environment can move it.
	if ts.Uncompleted != ts.Reads {
		t.Fatalf("%d of %d replies had an uncompleted checksum, want all of them. "+
			"If this host's kernel now completes the UDP checksum on a local "+
			"delivery path, the right answer is 0 and this assertion is what "+
			"needs revisiting -- not the parser.", ts.Uncompleted, ts.Reads)
	}
	if ts.Reads == 0 {
		t.Fatal("the transport read nothing, so the count above is vacuous")
	}
	if ts.Sends < 2 {
		t.Fatalf("the transport sent %d frames, want at least the DISCOVER and the REQUEST", ts.Sends)
	}
}

func inRange(addr, lo, hi string) bool {
	a := net.ParseIP(addr).To4()
	l := net.ParseIP(lo).To4()
	h := net.ParseIP(hi).To4()
	if a == nil || l == nil || h == nil {
		return false
	}
	return string(a) >= string(l) && string(a) <= string(h)
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// dnsmasqServer is a running dnsmasq whose log is being read line by line.
//
// Reading the log as a STREAM is what lets this file contain no duration at
// all: "the server is ready" and "the server logged the ACK" are both channel
// receives, so the test is as fast as the exchange and never faster than the
// truth.
type dnsmasqServer struct {
	cmd  *exec.Cmd
	once sync.Once

	// leasefile is the path dnsmasq keeps its own record of what it handed
	// out. It is the outside evidence a rebuilt journal is checked against:
	// the client's account of its lease is the client's opinion, and the
	// server's file is not.
	leasefile string

	// iface is the link dnsmasq serves on, so a test that builds a different
	// topology can read its own log lines back.
	iface string

	mu  sync.Mutex
	buf []string

	arrived chan string
}

// count returns how many log lines contain want.
//
// A COUNT and not a presence check, because the question M3 asks of this log
// is "did the DHCPDISCOVER count stay where it was while the DHCPREQUEST
// count moved" — which is what tells a renewal apart from a client that lost
// its lease and acquired a new one, and which no containsLine can answer.
func (s *dnsmasqServer) count(want string) int {
	n := 0
	for _, l := range s.lines() {
		if strings.Contains(l, want) {
			n++
		}
	}
	return n
}

// dnsmasqConfig is what a test varies about the server. The zero value is the
// acquisition fixture every test before M3 used.
type dnsmasqConfig struct {
	// rangeLo and rangeHi default to testRangeLo and testRangeHi.
	rangeLo, rangeHi string
	// iface is the link to serve on. Empty means testServerIf.
	iface string
	// extra is appended to the command line.
	extra []string
}

func startDnsmasq(t *testing.T) *dnsmasqServer {
	t.Helper()
	return startDnsmasqCfg(t, dnsmasqConfig{})
}

func startDnsmasqCfg(t *testing.T, cfg dnsmasqConfig) *dnsmasqServer {
	t.Helper()

	if cfg.rangeLo == "" {
		cfg.rangeLo, cfg.rangeHi = testRangeLo, testRangeHi
	}
	if cfg.iface == "" {
		cfg.iface = testServerIf
	}

	bin, err := findDnsmasq()
	if err != nil {
		t.Fatalf("%v", err)
	}
	dir := t.TempDir()
	leasefile := filepath.Join(dir, "leases")

	cmd := exec.Command(bin,
		"--conf-file=/dev/null",
		// --no-daemon, not --keep-in-foreground, and the difference decides
		// whether this test can run at all. Both stay in the foreground; only
		// --no-daemon also skips dnsmasq's privilege drop.
		//
		// MEASURED 2026-08-29: as uid 0 with --keep-in-foreground, dnsmasq
		// calls setgroups(0, ...) to shed supplementary groups and exits with
		// "failed to change group-id to root: Operation not permitted",
		// because an unprivileged user namespace must DENY setgroups before it
		// is allowed to write a gid map. The two requirements are in direct
		// conflict and no combination of --user and --group resolves it.
		"--no-daemon",
		// "-" is stderr. The log is the ORACLE for this test, so it is read
		// directly rather than through syslog, which this namespace has no
		// route to anyway.
		"--log-facility=-",
		"--log-dhcp",
		"--port=0", // DNS off: this test is about DHCP.
		"--interface="+cfg.iface,
		"--bind-interfaces",
		"--except-interface=lo",
		"--dhcp-range="+cfg.rangeLo+","+cfg.rangeHi+","+testSubnet+","+fmt.Sprint(testLeaseSec),
		"--dhcp-option=3,"+testServerIP,
		"--dhcp-option=6,"+testServerIP,
		"--dhcp-option=15,"+testDomain,
		"--dhcp-option=26,"+fmt.Sprint(testMTU),
		"--dhcp-authoritative",
		"--dhcp-leasefile="+leasefile,
		"--pid-file="+filepath.Join(dir, "pid"),
		"--no-resolv",
		"--no-hosts",
		// MEASURED 2026-09-02: without this, every DHCPDISCOVER costs THREE
		// SECONDS. dnsmasq ICMP-pings a candidate address before offering it
		// and waits for the silence to prove nothing is using it; in an empty
		// namespace the silence is guaranteed and the wait is pure cost.
		// Removing it weakens no assertion here — no test in this file is
		// about dnsmasq's conflict detection — and it takes six seconds off
		// the two tests that acquire twice.
		"--no-ping",
	)
	cmd.Args = append(cmd.Args, cfg.extra...)
	// C locale: dnsmasq translates its startup messages, and a German or
	// French box would otherwise fail this test on a string nobody changed.
	// The DHCP transaction lines are protocol keywords and are not
	// translated, but the readiness line is.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")

	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	cmd.Stdout = io.Discard

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting dnsmasq: %v", err)
	}

	s := &dnsmasqServer{cmd: cmd, arrived: make(chan string, 256), leasefile: leasefile, iface: cfg.iface}
	go s.read(stderr)

	t.Cleanup(func() {
		s.stop()
		t.Logf("dnsmasq log:\n%s", strings.Join(s.lines(), "\n"))
	})

	// Readiness, as a line rather than as a wait: dnsmasq announces the range
	// once its DHCP socket is up.
	s.waitFor(t, "DHCP, IP range "+cfg.rangeLo)
	return s
}

// stop kills the server and waits for it. Idempotent, because the test that
// replaces a server mid-run calls it and so does the Cleanup.
func (s *dnsmasqServer) stop() {
	s.once.Do(func() {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	})
}

func (s *dnsmasqServer) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		s.mu.Lock()
		s.buf = append(s.buf, line)
		s.mu.Unlock()
		select {
		case s.arrived <- line:
		default:
		}
	}
	close(s.arrived)
}

func (s *dnsmasqServer) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.buf...)
}

// waitCount blocks until at least n of the lines logged so far contain want.
//
// It is waitFor's answer to a barrier that has to be about a line the log may
// ALREADY hold a copy of: a renewal's DHCPACK looks exactly like the
// acquisition's, so the thing to wait for is not the line but the count.
func (s *dnsmasqServer) waitCount(t *testing.T, want string, n int, why string) {
	t.Helper()
	if s.count(want) >= n {
		return
	}
	for range s.arrived {
		if s.count(want) >= n {
			return
		}
	}
	t.Fatalf("%s: dnsmasq exited with %d line(s) containing %q, want %d.\nLog:\n%s",
		why, s.count(want), want, n, strings.Join(s.lines(), "\n"))
}

// waitFor blocks until a log line contains want.
func (s *dnsmasqServer) waitFor(t *testing.T, want string) {
	t.Helper()
	// Anything already read counts: the line may have arrived before this
	// call, and a watcher that only looks at future lines misses exactly the
	// events that happen quickly.
	if containsLine(s.lines(), want) {
		return
	}
	for line := range s.arrived {
		if strings.Contains(line, want) {
			return
		}
	}
	t.Fatalf("dnsmasq exited before logging a line containing %q.\nLog:\n%s",
		want, strings.Join(s.lines(), "\n"))
}

// TestDeclineAndReleaseReachRealDnsmasq is the proof standard applied to the
// two messages nothing answers.
//
// It is the only kind of evidence that means anything for these two. A
// DHCPDECLINE and a DHCPRELEASE get no reply, are not retransmitted, and
// change no state the client can read back — so a message that is malformed,
// misaddressed, or never sent at all produces EXACTLY the observable
// behaviour of a correct one. Every unit assertion about them is an assertion
// about the library's own opinion. dnsmasq writing DHCPDECLINE and
// DHCPRELEASE into its log is not.
//
// The DHCPRELEASE is the one that could not have worked before this test
// existed: RFC 2131 section 4.4.4 unicasts it, and a unicast has to be
// addressed at the link layer to a server this library never ARPs for. The
// transport learns that address from the frame that carried the DHCPACK. If
// that is wrong, the kernel on the other end drops the frame and dnsmasq
// never sees it — which is why the log line, and not the client's counter, is
// what this test reads.
func TestDeclineAndReleaseReachRealDnsmasq(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		declineAndReleaseAgainstDnsmasq(t)
		return
	}
	reexecInNamespaces(t)
}

func declineAndReleaseAgainstDnsmasq(t *testing.T) {
	mustRun(t, "ip", "link", "add", testClientIf, "type", "veth", "peer", "name", testServerIf)
	mustRun(t, "ip", "addr", "add", testServerIP+"/24", "dev", testServerIf)
	mustRun(t, "ip", "link", "set", testServerIf, "up")
	mustRun(t, "ip", "link", "set", testClientIf, "up")

	srv := startDnsmasq(t)

	iface, err := net.InterfaceByName(testClientIf)
	if err != nil {
		t.Fatalf("InterfaceByName(%s): %v", testClientIf, err)
	}
	clientMAC := iface.HardwareAddr.String()

	params := proto.DefaultParams(iface.HardwareAddr)
	params.DesyncMin, params.DesyncMax = 0, 0
	// ConflictAsync, and it is the ONE line in this fixture that is a
	// judgement rather than a scaling.
	//
	// The subject of this test is the DHCP exchange. RFC 5227's
	// probe-before-use — the default, ConflictWait — adds four to seven
	// seconds to EVERY acquisition here, which is the price D22 charges a
	// container and not something worth paying in a test of something else;
	// conflict_dnsmasq_linux_test.go pays it once, with the RFC's own
	// constants, and measures it.
	//
	// Async rather than off, because off would take the ARP socket, the
	// probes and section 2.4's listener out of the path of every one of these
	// tests. Under async they are all still there and still running beside
	// the exchange; what changes is only when the caller is told it may use
	// the address. A conflict check that broke an ordinary acquisition, or
	// that saw this host's own frames as somebody else's, would still redden
	// these tests.
	//
	// briskACD scales the schedule's DURATIONS and not its counts, so the
	// probing here is over in a third of a second instead of overlapping the
	// renewal and the reboot these tests are about. Its reasons are at its
	// definition.
	params.Conflict = proto.ConflictAsync
	params.ACD = briskACD()
	// The RFC minimum is ten seconds and this fixture waits one. It is the
	// same trade the desync window above gets, for the same reason: the
	// subject here is whether the two messages reach a real server, and ten
	// seconds of nothing in the middle measures the timer instead. Ring 1 pins
	// the default at the RFC floor separately, and refuses a negative, in
	// TestRestartDelayMeetsTheRFCMinimum — this line cannot reach that.
	params.RestartDelay = 1 * proto.Second
	params.Hostname = "m2-client"

	c, err := NewClient(ClientConfig{Interface: testClientIf, Params: params, EventBuffer: 8})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	// ------------------------------------------------------ the DHCPDECLINE --
	first := awaitAcquired(t, c)
	declined := first.Lease.Addr.Addr().String()
	t.Logf("acquired %s, declining it", declined)

	declineLeftTheHost := watchSendFailures(t, c)
	c.ReportConflict()
	if ev := awaitEvent(t, c, lease.Lost); ev.Reason != proto.ReasonConflict {
		t.Fatalf("lease lost for %s, want conflict", ev.Reason)
	}
	declineLeftTheHost("DHCPDECLINE")
	srv.waitFor(t, "DHCPDECLINE("+testServerIf+") "+declined)

	// The restart, on the server's evidence rather than the client's: after
	// section 3.1(5)'s wait the configuration process starts again, and
	// dnsmasq hands out a DIFFERENT address because it has marked the declined
	// one as in use. Both halves are the DECLINE having been understood.
	second := awaitAcquired(t, c)
	leased := second.Lease.Addr.Addr().String()
	t.Logf("reacquired %s after the decline", leased)
	if leased == declined {
		t.Fatalf("the server handed back %s, the address this client had just declined", leased)
	}

	// ------------------------------------------------------ the DHCPRELEASE --
	releaseLeftTheHost := watchSendFailures(t, c)
	c.Release()
	if ev := awaitEvent(t, c, lease.Lost); ev.Reason != proto.ReasonReleased {
		t.Fatalf("lease lost for %s, want released", ev.Reason)
	}
	releaseLeftTheHost("DHCPRELEASE")
	srv.waitFor(t, "DHCPRELEASE("+testServerIf+") "+leased)

	// Read back as a set, so the two lines are asserted against the same log
	// and the failure message carries the whole of it.
	want := []string{
		"DHCPDECLINE(" + testServerIf + ") " + declined + " " + clientMAC,
		"DHCPRELEASE(" + testServerIf + ") " + leased + " " + clientMAC,
	}
	log := srv.lines()
	for _, w := range want {
		if !containsLine(log, w) {
			t.Fatalf("the server's log has no line containing %q.\nServer log:\n%s", w, strings.Join(log, "\n"))
		}
	}

	cancel()
	<-runErr

	// The client's own counters, checked LAST and only against the log that
	// has already been asserted: they are corroboration, never the evidence.
	st := c.Stats()
	if st.DeclinesSent != 1 || st.ReleasesSent != 1 {
		t.Fatalf("stats = %+v, want one decline and one release", st)
	}
}

// watchSendFailures opens a window on the send-failure counter and returns the
// close, which fails NOW if the transport refused THIS step's send — instead of
// leaving the log assertion below to discover it by never being satisfied.
//
// It is a DELTA and not a reading of the total, because the total answers a
// different question. This client has already sent a DISCOVER and a REQUEST by
// the time the DECLINE goes out, and it acquires a second lease between the
// two windows; a cumulative counter that had moved for any of those reasons
// would fail here and name the DECLINE, which did nothing wrong. Opening the
// window before the trigger makes the step being guarded a data dependency of
// the guard rather than a convention about where the call sits.
//
// It is not the evidence and does not replace it: the server's log line is
// still what proves the message arrived, and this counter would say 0 for a
// message that left correctly formed and was dropped on the way. What it
// separates is the two failures — "the transport would not send it" from "the
// server never saw it" — which are one 90-second hang apart otherwise.
// MEASURED 2026-09-02 by reverting the transport's unicast path: without this
// line the DHCPRELEASE half hangs into go test's timeout; with it the run
// fails in four seconds naming the refusal.
//
// The ordering it depends on is exact rather than lucky: the machine emits the
// send before it announces the loss, drain executes an action list in order,
// and emit blocks until the caller takes the event. So a Lost in hand means
// the Send call has already returned and its counter has already moved.
func watchSendFailures(t *testing.T, c *Client) func(what string) {
	t.Helper()
	before := c.Stats().SendFailures
	return func(what string) {
		t.Helper()
		st := c.Stats()
		if st.SendFailures > before {
			t.Fatalf("the %s was not transmitted: %d send failure(s) during this step (%d before it) — the transport refused or could not send it. Stats: %+v",
				what, st.SendFailures-before, before, st)
		}
	}
}

// awaitAcquired blocks until the client reports a lease.
//
// No duration appears here: an exchange that never completes hangs into go
// test's own timeout, which is louder and more honest than a sleep that was
// too short. See the head of this file.
func awaitAcquired(t *testing.T, c *Client) lease.Event {
	t.Helper()
	return awaitEvent(t, c, lease.Acquired)
}

func awaitEvent(t *testing.T, c *Client, kind lease.EventKind) lease.Event {
	t.Helper()
	for ev := range c.Events() {
		t.Logf("client event: %s", ev)
		if ev.Kind == kind {
			return ev
		}
		if ev.Kind == lease.Failed {
			t.Fatalf("the client failed while waiting for %s: %s", kind, ev)
		}
	}
	t.Fatalf("the event stream ended before %s arrived", kind)
	return lease.Event{}
}

// The M3 fixture's own constants. Two SMALL pools, so every address in either
// one can be given a static neighbour entry before anything runs (see
// renewalAgainstDnsmasq on why that is necessary), and the second pool is
// disjoint from the first so that a renewal of the first pool's address is an
// address the replacement server cannot serve.
const (
	testRenewLo = "192.168.99.100"
	testRenewHi = "192.168.99.102"
	testNakLo   = "192.168.99.200"
	testNakHi   = "192.168.99.202"
	// testRenewSec is what dnsmasq is told to put in option 58, so T1 arrives
	// three seconds after each DHCPACK instead of at half of a two-minute
	// lease. It is a PROTOCOL VALUE the server sends, not a wait in this
	// file: every barrier below is still a channel receive, and the client
	// renews when the server told it to.
	//
	// Three rather than one: the DHCPNAK half has to replace the running
	// server between two renewals, and killing one dnsmasq and waiting for
	// the next one's readiness line takes a fraction of a second. One second
	// would still pass, until a loaded CI box made it not.
	testRenewSec = 3
)

// TestRenewalAndNakReachRealDnsmasq is M3's proof standard: RENEWING and the
// DHCPNAK that ends a lease, read out of a real server's log.
//
// IT IS THE ONLY EVIDENCE THAT MEANS ANYTHING FOR A RENEWAL. RFC 2131 Table 5
// makes 'ciaddr' a MUST and options 50 and 54 a MUST NOT in the RENEWING
// column, and section 4.3.2 says what a server does with a DHCPREQUEST that
// gets that wrong: it reads the message as one generated during SELECTING,
// compares the server identifier with its own, and STAYS SILENT when it does
// not match. Silence is also what a message that never left the host produces.
// So every unit assertion about the renewal message is an assertion about this
// library's opinion of itself; dnsmasq writing DHCPREQUEST and DHCPACK for an
// address it had already leased is not.
//
// The DHCPNAK half is driven by a SERVER-SIDE change — dnsmasq restarted with
// a pool that no longer contains the leased address — because a NAK the client
// manufactures for itself proves nothing about what a server sends.
func TestRenewalAndNakReachRealDnsmasq(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		renewalAgainstDnsmasq(t)
		return
	}
	reexecInNamespaces(t)
}

func renewalAgainstDnsmasq(t *testing.T) {
	mustRun(t, "ip", "link", "add", testClientIf, "type", "veth", "peer", "name", testServerIf)
	mustRun(t, "ip", "addr", "add", testServerIP+"/24", "dev", testServerIf)
	mustRun(t, "ip", "link", "set", testServerIf, "up")
	mustRun(t, "ip", "link", "set", testClientIf, "up")

	iface, err := net.InterfaceByName(testClientIf)
	if err != nil {
		t.Fatalf("InterfaceByName(%s): %v", testClientIf, err)
	}
	clientMAC := iface.HardwareAddr.String()

	// THE RENEWAL DHCPACK IS UNICAST, and this fixture has to make that
	// deliverable.
	//
	// dnsmasq sends a reply to a DHCPREQUEST with 'ciaddr' set to that
	// address (dhcp.c: "else if (mess->ciaddr.s_addr)"), whatever the
	// BROADCAST flag says. On a real host the leased address belongs to the
	// client's kernel, which answers the ARP the server's kernel sends
	// first. Here nothing owns it: this client is an AF_PACKET socket and
	// this library does not answer ARP until M6. So the server's kernel would
	// ARP into silence and drop the DHCPACK, and the test would hang into go
	// test's timeout with a correct client and a correct server.
	//
	// The entries go in for the WHOLE of both pools, BEFORE the client
	// starts, rather than for the leased address once it is known. Adding one
	// on the Acquired event would work most of the time and would be a race
	// against T1 the rest of it — the sort that passes here and fails on a
	// loaded CI box.
	for _, a := range append(addrRange(t, testRenewLo, testRenewHi), addrRange(t, testNakLo, testNakHi)...) {
		mustRun(t, "ip", "neigh", "replace", a, "lladdr", clientMAC, "dev", testServerIf, "nud", "permanent")
	}

	srv := startDnsmasqCfg(t, dnsmasqConfig{
		rangeLo: testRenewLo, rangeHi: testRenewHi,
		extra: []string{"--dhcp-option=58," + fmt.Sprint(testRenewSec)},
	})

	params := proto.DefaultParams(iface.HardwareAddr)
	params.DesyncMin, params.DesyncMax = 0, 0
	// ConflictAsync, and it is the ONE line in this fixture that is a
	// judgement rather than a scaling.
	//
	// The subject of this test is the DHCP exchange. RFC 5227's
	// probe-before-use — the default, ConflictWait — adds four to seven
	// seconds to EVERY acquisition here, which is the price D22 charges a
	// container and not something worth paying in a test of something else;
	// conflict_dnsmasq_linux_test.go pays it once, with the RFC's own
	// constants, and measures it.
	//
	// Async rather than off, because off would take the ARP socket, the
	// probes and section 2.4's listener out of the path of every one of these
	// tests. Under async they are all still there and still running beside
	// the exchange; what changes is only when the caller is told it may use
	// the address. A conflict check that broke an ordinary acquisition, or
	// that saw this host's own frames as somebody else's, would still redden
	// these tests.
	//
	// briskACD scales the schedule's DURATIONS and not its counts, so the
	// probing here is over in a third of a second instead of overlapping the
	// renewal and the reboot these tests are about. Its reasons are at its
	// definition.
	params.Conflict = proto.ConflictAsync
	params.ACD = briskACD()
	params.Hostname = "m3-client"

	c, err := NewClient(ClientConfig{Interface: testClientIf, Params: params, EventBuffer: 8})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	acquired := awaitAcquired(t, c)
	leased := acquired.Lease.Addr.Addr().String()
	if !inRange(leased, testRenewLo, testRenewHi) {
		t.Fatalf("leased %s, outside the pool %s..%s", leased, testRenewLo, testRenewHi)
	}
	srv.waitFor(t, "DHCPACK("+testServerIf+") "+leased+" "+clientMAC)

	// The server said "renew in three seconds", and the client has to have
	// heard it. Read from the lease rather than from the option, because it
	// is the DEADLINE the machine arms a timer for and a client that decoded
	// option 58 and then ignored it would pass an assertion on the option.
	if got := acquired.Lease.Renew.Sub(acquired.Lease.Acquired); int(got.Seconds()) != testRenewSec {
		t.Fatalf("T1 is %s after the lease began, want %ds from the server's option 58", got, testRenewSec)
	}
	if !acquired.Lease.Rebind.After(acquired.Lease.Renew) {
		t.Fatalf("T2 %s is not after T1 %s", acquired.Lease.Rebind, acquired.Lease.Renew)
	}

	// ---------------------------------------------------------- RENEWING --

	discoversBefore := srv.count("DHCPDISCOVER(")
	requestsBefore := srv.count("DHCPREQUEST(" + testServerIf + ") " + leased)
	acksBefore := srv.count("DHCPACK(" + testServerIf + ") " + leased)
	// RFC 5227 section 2.1's trigger list, on a real renewal. The list is
	// "whenever a host ... is booting, ... awakening from sleep, ... a link
	// status change, or ... a new IP address": a renewal that keeps the
	// address is none of them, and re-probing one would add the whole
	// schedule to every T1 for nothing. The count is taken across the
	// renewal because "no probes were sent" is only a claim if some were
	// sent before it. Defeat row M6-15; proto's
	// TestARenewalOnTheSameAddressDoesNotReProbe holds the same property
	// where it is decidable.
	//
	// The acquisition's own probes are waited for first, because a count
	// taken before any were sent would be a comparison of zero with zero.
	// This client runs in ConflictAsync, so Acquired arrived before the
	// first probe was due.
	want := uint64(briskACD().ProbeNum)
	for c.Stats().ProbesSent < want {
	}
	probesBefore := c.Stats().ProbesSent
	closeWindow := watchSendFailures(t, c)

	renewed := awaitEvent(t, c, lease.Renewed)
	closeWindow("renewal DHCPREQUEST")

	if renewed.Lease.Addr != acquired.Lease.Addr {
		t.Fatalf("renewed onto %s, want the same address %s", renewed.Lease.Addr, acquired.Lease.Addr)
	}
	if !renewed.Lease.Expire.After(acquired.Lease.Expire) {
		t.Fatalf("the renewed lease expires at %s, no later than the old %s",
			renewed.Lease.Expire, acquired.Lease.Expire)
	}
	if got := c.Stats().ProbesSent; got != probesBefore {
		t.Fatalf("the renewal sent %d further ARP Probe(s) for an address the client already held; RFC 5227 2.1 does not list a renewal among its triggers", got-probesBefore)
	}

	// THE BARRIER HAS TO NAME THE RENEWAL'S OWN LINES, not lines the
	// acquisition already put in the log. Waiting for
	// "DHCPACK(srv0) <ip> <mac>" here — the string the acquisition waited for
	// at the top of this function — returns from the buffer without waiting
	// for anything, and the counts below then read the log before the
	// scanner goroutine has appended what it is being asked about. That is
	// what made this test false-red under load: the client had its renewal
	// event in hand, the server had answered, and the log had not caught up.
	//
	// A count that must EXCEED what it was before the renewal cannot be
	// satisfied by a line that was already there. These two waits carry the
	// assertions the counts used to make: if the renewal never reaches the
	// server, or the server never answers it, the wait ends when dnsmasq
	// exits and says so.
	srv.waitCount(t, "DHCPREQUEST("+testServerIf+") "+leased, requestsBefore+1,
		"the renewal never reached the server")
	srv.waitCount(t, "DHCPACK("+testServerIf+") "+leased, acksBefore+1,
		"the server never answered the renewal")
	// THE DISCOVER COUNT IS WHAT MAKES THIS A RENEWAL. A client that lost its
	// lease and acquired the same address again would satisfy every
	// assertion above; it could not leave this one unmoved.
	if got := srv.count("DHCPDISCOVER("); got != discoversBefore {
		t.Fatalf("the server logged %d DHCPDISCOVER lines, %d before the renewal: this was a re-acquisition, not a renewal.\nServer log:\n%s",
			got, discoversBefore, strings.Join(srv.lines(), "\n"))
	}

	// ------------------------------------------------------------ DHCPNAK --
	//
	// The server-side change: a pool that no longer holds the leased address.
	// dnsmasq's rfc2131.c reaches "address not available" for a renewal of an
	// address address_available() rejects, and broadcasts the DHCPNAK because
	// the client's ciaddr is on the same net as the server's own address.
	srv.stop()
	srv2 := startDnsmasqCfg(t, dnsmasqConfig{
		rangeLo: testNakLo, rangeHi: testNakHi,
		extra: []string{"--dhcp-option=58," + fmt.Sprint(testRenewSec)},
	})

	lost := awaitEventTolerating(t, c, lease.Lost)
	if lost.Reason != proto.ReasonNak {
		t.Fatalf("the lease was lost for %s, want %s", lost.Reason, proto.ReasonNak)
	}
	// THE EVENT'S OWN PAYLOAD, not a later Lease() snapshot. The Lost event
	// carries the lease that was given up, and the client is free to have
	// completed the post-NAK re-acquisition before this line runs — the
	// event channel is eight deep, so emit does not block and the reader is
	// not a barrier. Asking Lease() here answers a question about NOW; the
	// question this test has is what the client gave up, and only the event
	// answers that.
	//
	// That the manager holds nothing AT THE MOMENT it announces the loss is
	// the manager's contract, and it is pinned where the answer is
	// deterministic: TestNakDuringRenewalEndsTheHeldLease in package lease,
	// against a server that goes silent after the DHCPNAK so nothing can
	// re-acquire behind the assertion.
	if lost.Lease.Addr.Addr().String() != leased {
		t.Fatalf("the lost lease names %s, want the address the server nakked, %s", lost.Lease.Addr, leased)
	}

	srv2.waitFor(t, "DHCPNAK("+testServerIf+") "+leased+" "+clientMAC)
	nak := ""
	for _, l := range srv2.lines() {
		if strings.Contains(l, "DHCPNAK("+testServerIf+") "+leased) {
			nak = l
		}
	}
	if !strings.Contains(nak, "address not available") {
		t.Fatalf("the DHCPNAK line is %q; want dnsmasq's reason for refusing a renewal outside its pool.\nServer log:\n%s",
			nak, strings.Join(srv2.lines(), "\n"))
	}

	// And RFC 2131 Figure 5's other half: the client restarts the
	// configuration process and takes an address from the new pool.
	second := awaitEventTolerating(t, c, lease.Acquired)
	if !inRange(second.Lease.Addr.Addr().String(), testNakLo, testNakHi) {
		t.Fatalf("after the DHCPNAK the client took %s, outside the replacement pool %s..%s",
			second.Lease.Addr, testNakLo, testNakHi)
	}
	srv2.waitFor(t, "DHCPACK("+testServerIf+") "+second.Lease.Addr.Addr().String()+" "+clientMAC)

	st := c.Stats()
	if st.RenewalsCompleted < 1 {
		t.Fatalf("Stats.RenewalsCompleted = %d, want at least the one dnsmasq acked", st.RenewalsCompleted)
	}
	if st.RenewalsSent < 2 {
		t.Fatalf("Stats.RenewalsSent = %d, want the renewal dnsmasq acked and the one it nakked", st.RenewalsSent)
	}
	if st.NaksSeen < 1 || st.NaksAccepted < 1 {
		t.Fatalf("NAK counters = %d seen / %d accepted, want at least one of each", st.NaksSeen, st.NaksAccepted)
	}
	if st.LeasesAcquired != 2 {
		t.Fatalf("Stats.LeasesAcquired = %d, want 2: the first lease and the one after the DHCPNAK", st.LeasesAcquired)
	}

	cancel()
	<-runErr
}

// addrRange enumerates the IPv4 addresses from lo to hi inclusive.
func addrRange(t *testing.T, lo, hi string) []string {
	t.Helper()
	a, err := netip.ParseAddr(lo)
	if err != nil {
		t.Fatalf("ParseAddr(%s): %v", lo, err)
	}
	end, err := netip.ParseAddr(hi)
	if err != nil {
		t.Fatalf("ParseAddr(%s): %v", hi, err)
	}
	var out []string
	for {
		out = append(out, a.String())
		if a == end {
			return out
		}
		a = a.Next()
		if len(out) > 64 {
			t.Fatalf("the range %s..%s is larger than this fixture will enumerate", lo, hi)
		}
	}
}

// awaitEventTolerating is awaitEvent for the one case that has to see a Failed
// event go past: a DHCPNAK produces Lost AND Failed, in that order, and
// awaitEvent treats every Failed as the end of the world.
//
// It tolerates a NAK and NOTHING ELSE. A blanket tolerance would turn any
// other failure — a transport that stopped sending, a retransmission budget
// exhausted — into a hang until go test's timeout, with the reason sitting
// unread in the event stream.
func awaitEventTolerating(t *testing.T, c *Client, kind lease.EventKind) lease.Event {
	t.Helper()
	for ev := range c.Events() {
		t.Logf("client event: %s", ev)
		if ev.Kind == kind {
			return ev
		}
		if ev.Kind == lease.Failed && ev.Reason != proto.ReasonNak {
			t.Fatalf("the client failed while waiting for %s: %s", kind, ev)
		}
	}
	t.Fatalf("the event stream ended before %s arrived", kind)
	return lease.Event{}
}
