//go:build linux

package runtime

import (
	"context"
	"fmt"
	"net"
	"os"
	goruntime "runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// M5's proof standard: INIT-REBOOT, read out of a real server's log.
//
// IT IS THE ONLY EVIDENCE THAT MEANS ANYTHING FOR THIS MESSAGE. RFC 2131
// section 4.3.2 says what a server does with a DHCPREQUEST that gets the
// INIT-REBOOT column wrong — it reads a message carrying a server identifier
// as one generated during SELECTING, compares that identifier with its own,
// and STAYS SILENT when it does not match — and silence is also what a message
// that never left the host produces. Every unit assertion about the message is
// this library's opinion of itself. dnsmasq writing DHCPREQUEST and DHCPACK
// for an address it leased to a PREVIOUS process, with no DHCPDISCOVER between
// them, is not.
//
// Three runs, three namespaces, three servers, because the three answers are
// three different servers: one that still has the binding, one that refuses
// it, and one asked nothing at all because the remembered lease had expired.

const (
	// A pool of three, so every address in it can be given a static neighbour
	// entry before anything runs — see renewalAgainstDnsmasq for why that is
	// necessary for a unicast DHCPACK.
	testRebootLo = "192.168.99.110"
	testRebootHi = "192.168.99.112"
	// The replacement server's pool, disjoint from the first, so that a
	// remembered address is one the replacement cannot serve.
	testRebootNakLo = "192.168.99.210"
	testRebootNakHi = "192.168.99.212"
)

// rebootFixture is the veth pair, the neighbour entries and the client
// interface, shared by the three runs below.
type rebootFixture struct {
	iface *net.Interface
	mac   string
}

func setUpRebootFixture(t *testing.T) rebootFixture {
	t.Helper()
	mustRun(t, "ip", "link", "add", testClientIf, "type", "veth", "peer", "name", testServerIf)
	mustRun(t, "ip", "addr", "add", testServerIP+"/24", "dev", testServerIf)
	mustRun(t, "ip", "link", "set", testServerIf, "up")
	mustRun(t, "ip", "link", "set", testClientIf, "up")

	iface, err := net.InterfaceByName(testClientIf)
	if err != nil {
		t.Fatalf("InterfaceByName(%s): %v", testClientIf, err)
	}
	mac := iface.HardwareAddr.String()
	for _, a := range append(addrRange(t, testRebootLo, testRebootHi), addrRange(t, testRebootNakLo, testRebootNakHi)...) {
		mustRun(t, "ip", "neigh", "replace", a, "lladdr", mac, "dev", testServerIf, "nud", "permanent")
	}
	return rebootFixture{iface: iface, mac: mac}
}

func rebootParams(f rebootFixture, hostname string) proto.Params {
	p := proto.DefaultParams(f.iface.HardwareAddr)
	// The desync delay would add up to ten seconds of nothing to a test whose
	// subject is the exchange. Ring 1 pins the RFC default separately, and
	// TestDesyncDoesNotDelayTheInitRebootRequest pins that the INIT-REBOOT
	// DHCPREQUEST is not subject to it at all.
	p.DesyncMin, p.DesyncMax = 0, 0
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
	p.Conflict = proto.ConflictAsync
	p.ACD = briskACD()
	p.Hostname = hostname
	return p
}

// runClient starts a client and returns it with a stop function that cancels
// it and waits for Run to return. A restart is two of these, and the second
// one's Config.Resume is the first one's lease.
func runClient(t *testing.T, cfg ClientConfig) (*Client, func()) {
	t.Helper()
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		<-runErr
	}
	t.Cleanup(stop)
	return c, stop
}

// dhcpTail is the server's log from mark on, reduced to the DHCP transaction
// lines belonging to one client.
//
// The SEQUENCE is the assertion these tests make, and a count cannot carry it:
// "a DHCPREQUEST and a DHCPACK and no DHCPDISCOVER" is satisfied by a client
// that discovered, was refused, and requested — which is the thing INIT-REBOOT
// exists not to do.
func dhcpTail(s *dnsmasqServer, mark int, mac string) []string {
	var out []string
	for _, l := range s.lines()[mark:] {
		if strings.Contains(l, "DHCP") && strings.Contains(l, mac) {
			out = append(out, l)
		}
	}
	return out
}

func kinds(lines []string) string {
	var out []string
	for _, l := range lines {
		for _, k := range []string{"DHCPDISCOVER", "DHCPOFFER", "DHCPREQUEST", "DHCPACK", "DHCPNAK", "DHCPRELEASE", "DHCPDECLINE"} {
			if strings.Contains(l, k+"(") {
				out = append(out, k)
				break
			}
		}
	}
	return strings.Join(out, " ")
}

// TestARestartResumesItsLeaseAgainstRealDnsmasq is P-3's headline: the client
// is stopped and started again with the lease it held, and the server sees a
// DHCPREQUEST and a DHCPACK for that address with NO DHCPDISCOVER between.
func TestARestartResumesItsLeaseAgainstRealDnsmasq(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		restartAgainstDnsmasq(t)
		return
	}
	reexecInNamespaces(t)
}

func restartAgainstDnsmasq(t *testing.T) {
	f := setUpRebootFixture(t)
	srv := startDnsmasqCfg(t, dnsmasqConfig{rangeLo: testRebootLo, rangeHi: testRebootHi})

	// ------------------------------------------------- the first process --
	first, stopFirst := runClient(t, ClientConfig{
		Interface: testClientIf, Params: rebootParams(f, "m5-first"), EventBuffer: 8,
	})
	acquired := awaitAcquired(t, first)
	leased := acquired.Lease.Addr.Addr().String()
	if !inRange(leased, testRebootLo, testRebootHi) {
		t.Fatalf("leased %s, outside the pool %s..%s", leased, testRebootLo, testRebootHi)
	}
	if acquired.Requested.IsValid() {
		t.Fatalf("the FIRST acquisition reports having asked for %s; it asked for nothing", acquired.Requested)
	}
	srv.waitFor(t, "DHCPACK("+testServerIf+") "+leased+" "+f.mac)
	stopFirst()

	// Cancelling a client is not a DHCPRELEASE (P-7): the lease is dropped
	// locally with ReasonStopped and the server keeps the binding, which is
	// the whole premise of resuming it. If this stopped being true the second
	// client would be acquiring rather than rebooting, and every assertion
	// below would still pass.
	if n := srv.count("DHCPRELEASE("); n != 0 {
		t.Fatalf("stopping the client sent %d DHCPRELEASE(s); the server no longer holds the binding this test resumes.\nServer log:\n%s",
			n, strings.Join(srv.lines(), "\n"))
	}

	// ------------------------------------------------ the second process --
	mark := len(srv.lines())
	discoversBefore := srv.count("DHCPDISCOVER(")

	second, _ := runClient(t, ClientConfig{
		Interface:   testClientIf,
		Params:      rebootParams(f, "m5-second"),
		Resume:      &acquired.Lease,
		EventBuffer: 8,
	})
	back := awaitAcquired(t, second)

	if got := back.Lease.Addr.Addr().String(); got != leased {
		t.Fatalf("the restarted client came back with %s, want the remembered %s", got, leased)
	}
	if back.Requested.String() != leased {
		t.Fatalf("Event.Requested = %s, want the address it asked to keep, %s", back.Requested, leased)
	}

	srv.waitFor(t, "DHCPACK("+testServerIf+") "+leased+" "+f.mac)
	// Wait for the ACK BY COUNT, not by line: the first client's DHCPACK for
	// the same address is already in the buffer, so waitFor on that string
	// returns without waiting for anything.
	srv.waitCount(t, "DHCPACK("+testServerIf+") "+leased, 2, "the server never acked the INIT-REBOOT request")

	tail := dhcpTail(srv, mark, f.mac)
	t.Logf("dnsmasq log for the restart:\n%s", strings.Join(tail, "\n"))

	if got := kinds(tail); got != "DHCPREQUEST DHCPACK" {
		t.Fatalf("the restart's exchange was %q, want exactly \"DHCPREQUEST DHCPACK\".\nLines:\n%s",
			got, strings.Join(tail, "\n"))
	}
	if !strings.Contains(tail[0], "DHCPREQUEST("+testServerIf+") "+leased+" "+f.mac) {
		t.Fatalf("the first line of the restart is %q, want a DHCPREQUEST for %s", tail[0], leased)
	}
	if !strings.Contains(tail[1], "DHCPACK("+testServerIf+") "+leased+" "+f.mac) {
		t.Fatalf("the second line of the restart is %q, want a DHCPACK for %s", tail[1], leased)
	}
	if got := srv.count("DHCPDISCOVER("); got != discoversBefore {
		t.Fatalf("the server logged %d DHCPDISCOVER lines, %d before the restart: this was a re-acquisition, not an INIT-REBOOT.\nServer log:\n%s",
			got, discoversBefore, strings.Join(srv.lines(), "\n"))
	}
	if st := second.Stats(); st.LeasesAcquired != 1 || st.SendFailures != 0 {
		t.Fatalf("the restarted client's stats are %+v; want one acquisition and no send failures", st)
	}

	// WHAT THE LOG CANNOT SHOW, checked on the bytes that went out.
	//
	// dnsmasq's "DHCPREQUEST(srv0) <ip> <mac>" line is the same line for an
	// INIT-REBOOT request and for a SELECTING one: the difference is option
	// 54 and 'ciaddr', and --log-dhcp prints neither for a received message.
	// So the outside evidence above says a DHCPREQUEST was answered, and this
	// says WHICH DHCPREQUEST it was — on the frame the transport recorded,
	// not on the machine's opinion of it.
	var out *wire.Message
	for _, p := range second.Packets() {
		if p.Dir != lease.DirOut {
			continue
		}
		out = p.Msg
		break
	}
	if out == nil {
		t.Fatal("the packet ring recorded nothing outbound")
	}
	if ty, _ := out.Type(); ty != wire.MsgRequest {
		t.Fatalf("the first frame out was a %s", ty)
	}
	if v, ok := out.Addr4(wire.OptRequestedIP); !ok || v.String() != leased {
		t.Fatalf("the frame's option 50 = %v/%v, want %s (RFC 2131 4.3.2, a MUST)", v, ok, leased)
	}
	if v, ok := out.Addr4(wire.OptServerID); ok {
		t.Fatalf("the frame carries server identifier %s; RFC 2131 Table 5 makes it a MUST NOT after INIT-REBOOT, and section 4.3.2 answers it with silence", v)
	}
	if out.CIAddr.IsValid() && !out.CIAddr.IsUnspecified() {
		t.Fatalf("the frame's ciaddr = %s, want zero: this is a REBOOTING request, not a renewal", out.CIAddr)
	}
}

// TestARefusedResumeRestartsAgainstRealDnsmasq is RFC 2131 Figure 5's
// "DHCPNAK/Restart" edge and section 3.2(3)'s "(non-abbreviated) procedure",
// against a server that really refuses.
//
// The refusal is SERVER-SIDE — dnsmasq replaced with a pool that does not
// contain the remembered address — because a DHCPNAK the client manufactures
// for itself proves nothing about what a server sends.
func TestARefusedResumeRestartsAgainstRealDnsmasq(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		refusedResumeAgainstDnsmasq(t)
		return
	}
	reexecInNamespaces(t)
}

func refusedResumeAgainstDnsmasq(t *testing.T) {
	f := setUpRebootFixture(t)
	srv := startDnsmasqCfg(t, dnsmasqConfig{rangeLo: testRebootLo, rangeHi: testRebootHi})

	first, stopFirst := runClient(t, ClientConfig{
		Interface: testClientIf, Params: rebootParams(f, "m5-first"), EventBuffer: 8,
	})
	acquired := awaitAcquired(t, first)
	leased := acquired.Lease.Addr.Addr().String()
	srv.waitFor(t, "DHCPACK("+testServerIf+") "+leased+" "+f.mac)
	stopFirst()
	srv.stop()

	// The replacement server: same segment, different pool, no record of this
	// client.
	srv2 := startDnsmasqCfg(t, dnsmasqConfig{rangeLo: testRebootNakLo, rangeHi: testRebootNakHi})

	second, _ := runClient(t, ClientConfig{
		Interface:   testClientIf,
		Params:      rebootParams(f, "m5-second"),
		Resume:      &acquired.Lease,
		EventBuffer: 8,
	})

	// A DHCPNAK in REBOOTING is a Failed event and NOT a Lost one: no lease
	// was held to lose. awaitEventTolerating lets exactly that one past.
	back := awaitEventTolerating(t, second, lease.Acquired)
	if inRange(back.Lease.Addr.Addr().String(), testRebootLo, testRebootHi) {
		t.Fatalf("after the DHCPNAK the client kept %s, from the pool the replacement server does not serve", back.Lease.Addr)
	}
	if !inRange(back.Lease.Addr.Addr().String(), testRebootNakLo, testRebootNakHi) {
		t.Fatalf("the client took %s, outside the replacement pool %s..%s",
			back.Lease.Addr, testRebootNakLo, testRebootNakHi)
	}
	newAddr := back.Lease.Addr.Addr().String()
	srv2.waitFor(t, "DHCPACK("+testServerIf+") "+newAddr+" "+f.mac)

	tail := dhcpTail(srv2, 0, f.mac)
	t.Logf("dnsmasq log for the refused restart:\n%s", strings.Join(tail, "\n"))

	if got := kinds(tail); got != "DHCPREQUEST DHCPNAK DHCPDISCOVER DHCPOFFER DHCPREQUEST DHCPACK" {
		t.Fatalf("the refused restart's exchange was %q, want the INIT-REBOOT request, its refusal, and then the FULL procedure of RFC 2131 section 3.2(3).\nLines:\n%s",
			got, strings.Join(tail, "\n"))
	}
	if !strings.Contains(tail[0], leased) {
		t.Fatalf("the INIT-REBOOT request names %q, want the remembered %s", tail[0], leased)
	}
	if !strings.Contains(tail[1], leased) {
		t.Fatalf("the DHCPNAK names %q, want the remembered %s", tail[1], leased)
	}
	// The DHCPDISCOVER that follows must NOT ask for the address that was
	// just refused: the Resume is consumed by one attempt.
	if strings.Contains(tail[2], leased) {
		t.Fatalf("the restart's DHCPDISCOVER names the refused address: %q", tail[2])
	}

	if st := second.Stats(); st.NaksSeen < 1 || st.NaksAccepted < 1 {
		t.Fatalf("NAK counters = %d seen / %d accepted, want at least one of each: %+v", st.NaksSeen, st.NaksAccepted, st)
	}
}

// TestAnExpiredResumeDiscoversAgainstRealDnsmasq is the wall-clock half.
//
// RFC 2131 section 4.3.2: "If the DHCP server has no record of this client,
// then it MUST remain silent". An expired lease is the case where no server is
// obliged to have a record, so an INIT-REBOOT of one buys a retransmission
// budget of silence and then the DHCPDISCOVER that should have gone first.
//
// NOTHING WAITS HERE. The remembered lease's wall-clock deadline is rewound
// rather than waited out: waiting would need dnsmasq's minimum lease of two
// minutes, and a test that sleeps is a test this project's gate T2 refuses.
func TestAnExpiredResumeDiscoversAgainstRealDnsmasq(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		expiredResumeAgainstDnsmasq(t)
		return
	}
	reexecInNamespaces(t)
}

func expiredResumeAgainstDnsmasq(t *testing.T) {
	f := setUpRebootFixture(t)
	srv := startDnsmasqCfg(t, dnsmasqConfig{rangeLo: testRebootLo, rangeHi: testRebootHi})

	first, stopFirst := runClient(t, ClientConfig{
		Interface: testClientIf, Params: rebootParams(f, "m5-first"), EventBuffer: 8,
	})
	acquired := awaitAcquired(t, first)
	leased := acquired.Lease.Addr.Addr().String()
	srv.waitFor(t, "DHCPACK("+testServerIf+") "+leased+" "+f.mac)
	stopFirst()

	// The one edit: the deadline the record would have stored, moved into the
	// past. The server still holds the binding — that is the point. A client
	// that INIT-REBOOTed on the strength of the server's memory rather than
	// its own deadline would get an ACK here and pass every other assertion.
	stale := acquired.Lease
	stale.Expire = time.Now().Add(-time.Minute)

	mark := len(srv.lines())
	acksBefore := srv.count("DHCPACK(" + testServerIf + ")")

	second, _ := runClient(t, ClientConfig{
		Interface:   testClientIf,
		Params:      rebootParams(f, "m5-second"),
		Resume:      &stale,
		EventBuffer: 8,
	})
	back := awaitAcquired(t, second)

	// THE BARRIER HAS TO NAME THE LAST LINE OF THE EXCHANGE, and it has to be
	// a COUNT because the first client already put an identical DHCPACK in
	// this log.
	//
	// MEASURED 2026-09-03 under load, by the concurrent arbiter copy of the
	// loaded verify run: waiting on the DHCPDISCOVER count reddened this test
	// with the log holding "DHCPDISCOVER DHCPOFFER" and nothing after it. The
	// client had its Acquired event, the server had answered, and the scanner
	// goroutine had not yet appended the two lines being asserted about. A
	// barrier on the FIRST line of a sequence is not a barrier on the
	// sequence.
	srv.waitCount(t, "DHCPACK("+testServerIf+")", acksBefore+1,
		"the client with an expired remembered lease never completed an acquisition")

	tail := dhcpTail(srv, mark, f.mac)
	t.Logf("dnsmasq log for the expired remembered lease:\n%s", strings.Join(tail, "\n"))

	if got := kinds(tail); got != "DHCPDISCOVER DHCPOFFER DHCPREQUEST DHCPACK" {
		t.Fatalf("the exchange was %q, want the full acquisition: an expired lease is not rebooted.\nLines:\n%s",
			got, strings.Join(tail, "\n"))
	}
	// The DHCPDISCOVER is first, and it does not ask for the stale address:
	// that hint is lease.Record.Prefer's job and this client was not given
	// one.
	if strings.Contains(tail[0], "DHCPREQUEST") {
		t.Fatalf("the first message was a DHCPREQUEST: %q", tail[0])
	}
	if back.Requested.IsValid() {
		t.Fatalf("the acquisition reports having asked for %s; the expired Resume was dropped, not converted into a request", back.Requested)
	}
	// The journal says WHY, which is the only place a stale deadline is
	// visible at all — the wire shows an ordinary acquisition.
	var why string
	for _, e := range second.Journal() {
		for _, a := range e.Actions {
			if strings.Contains(a, "has expired") {
				why = a
			}
		}
	}
	if why == "" {
		t.Fatalf("nothing in the client's journal says the remembered lease was dropped for age:\n%+v", second.Journal())
	}
	t.Logf("journal: %s", why)
}

// ------------------------------------------------------------------- G-8 --

// TestTheClientKeepsTheNamespaceItWasBuiltIn measures seam note row G-8, which
// the note itself marks INFERRED: an AF_PACKET socket belongs to the network
// namespace that was current in the CREATING THREAD, and keeps it afterwards.
//
// The measurement is stronger than the claim. The client is built on a
// goroutine locked to a thread that has unshared into a fresh network
// namespace, and that goroutine then RETURNS WITHOUT UNLOCKING — which makes
// the Go runtime destroy the thread. The client is then run from an ordinary
// goroutine that has never been in that namespace and cannot enter it, and it
// leases from a dnsmasq that only exists there.
//
// THE CONTROL IS THE HALF THAT MATTERS. Without it, a client leasing from a
// server on cli0 proves nothing: the two might simply be in the same namespace
// as everything else. So the test also asserts that cli0 CANNOT BE SEEN from
// the goroutine that runs the client. The lease crossed a boundary the caller
// could not.
func TestTheClientKeepsTheNamespaceItWasBuiltIn(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		namespaceCaptureAgainstDnsmasq(t)
		return
	}
	reexecInNamespaces(t)
}

// nsBuild is what the locked goroutine hands back before its thread dies.
type nsBuild struct {
	client *Client
	server *dnsmasqServer
	mac    string
	err    error
}

func namespaceCaptureAgainstDnsmasq(t *testing.T) {
	// The outer namespace is the re-exec's own, and it is EMPTY: nothing
	// creates cli0 here.
	if _, err := net.InterfaceByName(testClientIf); err == nil {
		t.Fatalf("%s already exists in the outer namespace, so the control below cannot fail", testClientIf)
	}

	built := make(chan nsBuild, 1)
	go func() {
		var out nsBuild
		// Deferred, so that a t.Fatalf inside — which unwinds this goroutine
		// with runtime.Goexit — still delivers the result and the test fails
		// with its own message rather than hanging on this channel.
		defer func() { built <- out }()

		// LOCKED AND NEVER UNLOCKED. The unshare below changes the network
		// namespace of THIS THREAD, and an unlocked goroutine can be moved to
		// another thread between two lines. Returning while still locked
		// makes the runtime terminate the thread, which is the disposal this
		// test wants: the namespace afterwards has no thread of ours in it.
		goruntime.LockOSThread()

		if err := syscall.Unshare(syscall.CLONE_NEWNET); err != nil {
			out.err = fmt.Errorf("unshare(CLONE_NEWNET): %w", err)
			return
		}

		// Everything from here runs in the inner namespace, on this thread:
		// `ip`, dnsmasq and NewClient are all forked or opened from it.
		mustRun(t, "ip", "link", "add", testClientIf, "type", "veth", "peer", "name", testServerIf)
		mustRun(t, "ip", "addr", "add", testServerIP+"/24", "dev", testServerIf)
		mustRun(t, "ip", "link", "set", testServerIf, "up")
		mustRun(t, "ip", "link", "set", testClientIf, "up")

		iface, err := net.InterfaceByName(testClientIf)
		if err != nil {
			out.err = fmt.Errorf("InterfaceByName(%s) inside the namespace: %w", testClientIf, err)
			return
		}
		out.mac = iface.HardwareAddr.String()
		for _, a := range addrRange(t, testRebootLo, testRebootHi) {
			mustRun(t, "ip", "neigh", "replace", a, "lladdr", out.mac, "dev", testServerIf, "nud", "permanent")
		}
		out.server = startDnsmasqCfg(t, dnsmasqConfig{rangeLo: testRebootLo, rangeHi: testRebootHi})

		p := proto.DefaultParams(iface.HardwareAddr)
		p.DesyncMin, p.DesyncMax = 0, 0
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
		p.Conflict = proto.ConflictAsync
		p.ACD = briskACD()
		p.Hostname = "m5-netns"
		c, err := NewClient(ClientConfig{Interface: testClientIf, Params: p, EventBuffer: 8})
		if err != nil {
			out.err = fmt.Errorf("NewClient inside the namespace: %w", err)
			return
		}
		out.client = c
	}()

	res := <-built
	if res.err != nil {
		t.Fatal(res.err)
	}
	if res.client == nil {
		t.Fatal("the namespaced build produced no client; see the failure above")
	}

	// THE CONTROL. This goroutine is not the one that built the client and
	// was never in that namespace. If it can see cli0, the acquisition below
	// proves nothing about where the socket lives.
	if iface, err := net.InterfaceByName(testClientIf); err == nil {
		t.Fatalf("%s is visible from the goroutine that runs the client (index %d): the client was not built in a namespace of its own",
			testClientIf, iface.Index)
	} else {
		t.Logf("control: %s is invisible here (%v)", testClientIf, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	// An ORDINARY goroutine: no lock, no namespace, any thread the scheduler
	// picks. The socket is what carries the namespace.
	go func() { runErr <- res.client.Run(ctx) }()

	acquired := awaitAcquired(t, res.client)
	leased := acquired.Lease.Addr.Addr().String()
	if !inRange(leased, testRebootLo, testRebootHi) {
		t.Fatalf("leased %s, outside the pool %s..%s", leased, testRebootLo, testRebootHi)
	}
	res.server.waitFor(t, "DHCPACK("+testServerIf+") "+leased+" "+res.mac)
	t.Logf("the client built inside a namespace this goroutine cannot enter leased %s", leased)

	cancel()
	<-runErr
}
