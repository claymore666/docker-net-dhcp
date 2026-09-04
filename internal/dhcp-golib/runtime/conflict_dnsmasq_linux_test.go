//go:build linux

package runtime

import (
	"context"
	"net"
	"net/netip"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// M6's outside evidence: RFC 5227 against a real dnsmasq, with a real squatter
// on the other end of a real veth pair.
//
// WHAT MAKES THIS PROOF RATHER THAN A DEMONSTRATION. Every assertion below is
// on something the client under test did not write:
//
//   - dnsmasq's own log — the DHCPDECLINE line, and the DHCPOFFER of a
//     DIFFERENT address that follows it. The client cannot make a server it
//     does not control write those.
//   - a SECOND packet socket, on the server's end of the wire, which sees the
//     Probes as frames. Its record of them is not the library's packet ring;
//     it is what an observer with tcpdump would have seen, built from the same
//     AF_PACKET the kernel offers everyone. That is where the all-zero sender
//     IP is checked, and where the acquisition delay is timed.
//
// The library's own counters appear only as a cross-check, never as the
// assertion.
//
// The three runs the milestone asks for are four tests, because the squatter
// case is run in BOTH conflict modes (D23) and the modes differ in exactly
// what the caller is told and when:
//
//	TestASquatterInTheProbeWindowMakesAWaitingClientDecline   section 2.1, wait
//	TestASquatterInTheProbeWindowMakesAnAsyncClientDecline    section 2.1, async
//	TestASquatterAfterBoundTakesSection24sPath                section 2.4
//	TestTheDelayBeforeAnAcquisitionIsRFC5227sArithmetic       no squatter

// squatterMode is what the fixture on the server's end of the wire does with
// what it hears.
type squatterMode int

const (
	// squatterObserve records every ARP frame and answers nothing. It is the
	// wire observer for the timing run, and it is what makes "no squatter"
	// a measured claim rather than an absence.
	squatterObserve squatterMode = iota
	// squatterDefendFirstProbed answers the first address it sees probed, and
	// only that one. RFC 5227 section 2.1.1's first conflict rule is about the
	// SENDER IP of the answer, so an ARP Reply carrying the probed address is
	// the minimum a squatter has to emit to be seen.
	//
	// ONLY THE FIRST. After the DECLINE the server offers a different address
	// and the client probes that one; a squatter that answered everything
	// would loop the client forever and the test would measure a livelock
	// instead of a recovery.
	squatterDefendFirstProbed
	// squatterAnnounceOnCue sends one gratuitous ARP for an address it is
	// handed, after the client already holds it: section 2.4's predicate,
	// which is about a packet whose sender IP is ours and whose sender
	// hardware address is not.
	squatterAnnounceOnCue
)

// squatter is another host on the link, built out of the same ARP socket the
// client uses.
//
// It runs in the test process because the namespace has one process in it, and
// that costs nothing here: it holds its own socket, on its own interface, with
// its own hardware address. The client has no reference to it and cannot tell
// it from a machine that was plugged in.
type squatter struct {
	sock *ARPSocket
	hw   net.HardwareAddr

	mu       sync.Mutex
	seen     []squatterSighting
	defended netip.Addr

	announce chan netip.Addr
	// heard is pinged after every frame is recorded, so a test waits on a
	// channel instead of spinning a core: this file has no sleep in it and a
	// bare polling loop is the shape that invites one.
	heard   chan struct{}
	done    chan struct{}
	stopped sync.Once
}

// squatterSighting is one frame, with the wall-clock time it was read off the
// wire. The time is the point: the timing run's arithmetic is over these.
type squatterSighting struct {
	at time.Time
	p  *wire.ARPPacket
}

func newSquatter(t *testing.T, ifName string, mode squatterMode) *squatter {
	t.Helper()
	sock, err := NewARPSocket(ifName)
	if err != nil {
		t.Fatalf("the squatter could not open an ARP socket on %s: %v", ifName, err)
	}
	s := &squatter{
		sock:     sock,
		hw:       sock.HardwareAddr(),
		announce: make(chan netip.Addr, 1),
		heard:    make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	go s.run(t, mode)
	t.Cleanup(s.stop)
	return s
}

func (s *squatter) stop() {
	s.stopped.Do(func() {
		close(s.done)
		_ = s.sock.Close()
	})
}

func (s *squatter) run(t *testing.T, mode squatterMode) {
	in := s.sock.Received()
	for {
		select {
		case <-s.done:
			return
		case addr := <-s.announce:
			s.send(&wire.ARPPacket{
				Op:       wire.ARPRequest,
				SenderHW: s.hw,
				SenderIP: addr,
				TargetIP: addr,
			})
		case f, ok := <-in:
			if !ok {
				return
			}
			if f.Err != nil {
				return
			}
			p, err := wire.DecodeARP(f.Frame)
			if err != nil {
				continue
			}
			at := time.Now()
			// Frames the squatter itself sent come back on its own socket —
			// AF_PACKET echoes this host's outgoing frames — and recording
			// them would put the squatter's own answer into the evidence it
			// is providing about the client.
			if hwEqual(p.SenderHW, s.hw) {
				continue
			}
			s.mu.Lock()
			s.seen = append(s.seen, squatterSighting{at: at, p: p})
			s.mu.Unlock()
			select {
			case s.heard <- struct{}{}:
			default:
			}

			if mode != squatterDefendFirstProbed || !p.IsProbe() {
				continue
			}
			s.mu.Lock()
			if !s.defended.IsValid() {
				s.defended = p.TargetIP
				t.Logf("the squatter is taking %s", s.defended)
			}
			mine := s.defended == p.TargetIP
			addr := s.defended
			s.mu.Unlock()
			if mine {
				s.send(&wire.ARPPacket{
					Op:       wire.ARPReply,
					SenderHW: s.hw,
					SenderIP: addr,
					TargetHW: p.SenderHW,
					TargetIP: addr,
				})
			}
		}
	}
}

func (s *squatter) send(p *wire.ARPPacket) {
	raw, err := wire.EncodeARP(p)
	if err != nil {
		panic("runtime: the squatter built an unencodable ARP packet: " + err.Error())
	}
	_ = s.sock.Send(raw)
}

func (s *squatter) sightings() []squatterSighting {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]squatterSighting(nil), s.seen...)
}

// isProbeFor and isAnnouncementFor are the two frame classes this file waits
// on and counts. They are spelled once so that a wait and the count taken
// afterwards cannot describe different sets of frames.
//
// A Probe is RFC 5227 section 2.1.1's: an ARP Request with an all-zero sender
// IP. An Announcement is section 2.3's: an ARP Request with sender and target
// IP both the address being claimed.
func isProbeFor(addr netip.Addr) func(*wire.ARPPacket) bool {
	return func(p *wire.ARPPacket) bool { return p.IsProbe() && p.TargetIP == addr }
}

func isAnnouncementFor(addr netip.Addr) func(*wire.ARPPacket) bool {
	return func(p *wire.ARPPacket) bool {
		return p.Op == wire.ARPRequest && p.SenderIP == addr && p.TargetIP == addr
	}
}

// matching returns, in the order they crossed the wire, the frames the
// squatter has read so far that pred accepts. It does not wait: every caller
// below first waits on a LATER frame, so that what it then counts is complete.
func (s *squatter) matching(pred func(*wire.ARPPacket) bool) []squatterSighting {
	var out []squatterSighting
	for _, g := range s.sightings() {
		if pred(g.p) {
			out = append(out, g)
		}
	}
	return out
}

// waitForSighting blocks until pred has matched a frame the squatter read.
//
// It carries no duration of its own: a frame that never comes hangs the test
// until go test's own timeout, which says more than a deadline chosen here.
func (s *squatter) waitForSighting(pred func(*wire.ARPPacket) bool) squatterSighting {
	for {
		for _, g := range s.sightings() {
			if pred(g.p) {
				return g
			}
		}
		<-s.heard
	}
}

// waitForCount blocks until pred has matched at least n of the frames read.
func (s *squatter) waitForCount(pred func(*wire.ARPPacket) bool, n int) []squatterSighting {
	for {
		if out := s.matching(pred); len(out) >= n {
			return out
		}
		<-s.heard
	}
}

func hwEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// conflictFixture is the wiring every test in this file starts from.
type conflictFixture struct {
	srv       *dnsmasqServer
	client    *Client
	clientMAC string
	cancel    context.CancelFunc
	runErr    chan error
}

// briskACD is RFC 5227 section 1.1's table with its DURATIONS scaled down and
// its COUNTS untouched.
//
// The three conflict runs use it and the timing run does not. What those three
// are about is a verdict — a real squatter on a real wire makes a real dnsmasq
// log a DHCPDECLINE and hand out a different address — and that verdict does
// not depend on how long the gaps were. Paying the RFC's own four to seven
// seconds three more times would add twenty seconds to a suite with a
// sixty-second ceiling and measure nothing the fourth test does not measure
// properly.
//
// PROBE_NUM and ANNOUNCE_NUM keep the RFC's values, because those are counts
// of packets and every one of them is asserted on the wire. The durations are
// pinned at their RFC values in proto's TestACDConstantsAreTheRFCValues and
// MEASURED against a real wire in
// TestTheDelayBeforeAnAcquisitionIsRFC5227sArithmetic; nothing here can reach
// either.
func briskACD() proto.ACDParams {
	d := proto.DefaultACDParams()
	d.ProbeWait = 50 * proto.Millisecond
	d.ProbeMin = 50 * proto.Millisecond
	d.ProbeMax = 100 * proto.Millisecond
	d.AnnounceWait = 100 * proto.Millisecond
	d.AnnounceInterval = 100 * proto.Millisecond
	return d
}

// newConflictClient wires the veth pair, starts dnsmasq and BUILDS a client
// with conflict detection in the given mode and the given ACD table. It does
// not run it: conflictFixture.start does, and takes the observer.
func newConflictClient(t *testing.T, mode proto.ConflictMode, acd proto.ACDParams, hostname string) *conflictFixture {
	t.Helper()
	return newConflictClientCfg(t, mode, acd, hostname, dnsmasqConfig{})
}

// newConflictClientCfg is newConflictClient with the server's command line
// open to the caller. Round 2's two runs need option 58, because a lease that
// SURVIVES is proved by a renewal and dnsmasq's default T1 is a minute away.
func newConflictClientCfg(t *testing.T, mode proto.ConflictMode, acd proto.ACDParams, hostname string, cfg dnsmasqConfig) *conflictFixture {
	t.Helper()

	mustRun(t, "ip", "link", "add", testClientIf, "type", "veth", "peer", "name", testServerIf)
	mustRun(t, "ip", "addr", "add", testServerIP+"/24", "dev", testServerIf)
	mustRun(t, "ip", "link", "set", testServerIf, "up")
	mustRun(t, "ip", "link", "set", testClientIf, "up")

	srv := startDnsmasqCfg(t, cfg)

	iface, err := net.InterfaceByName(testClientIf)
	if err != nil {
		t.Fatalf("InterfaceByName(%s): %v", testClientIf, err)
	}

	params := proto.DefaultParams(iface.HardwareAddr)
	params.DesyncMin, params.DesyncMax = 0, 0
	// The RFC minimum is ten seconds and this fixture waits one, on the same
	// terms TestDeclineAndReleaseReachRealDnsmasq took it: the subject here is
	// what a real server does with a real DECLINE, and ten seconds of nothing
	// in the middle measures the timer instead. proto's
	// TestRestartDelayMeetsTheRFCMinimum pins the default at the RFC floor,
	// and this line cannot reach it.
	params.RestartDelay = 1 * proto.Second
	params.Conflict = mode
	params.ACD = acd
	params.Hostname = hostname

	c, err := NewClient(ClientConfig{Interface: testClientIf, Params: params, EventBuffer: 8})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return &conflictFixture{
		srv:       srv,
		client:    c,
		clientMAC: iface.HardwareAddr.String(),
	}
}

// start runs the client, and takes the observer that must already be watching
// the wire.
//
// The squatter is a parameter so that arming the observer LATE is a compile
// error rather than a rare red. The first ARP Probe leaves within
// U(0, PROBE_WAIT) of the DHCPACK — 50ms under briskACD — so an observer whose
// socket is bound after the client has started can miss it, and a missing
// probe 1 reads exactly like a client that sent PROBE_NUM-1 probes.
//
// MEASURED 2026-09-04, with the client started first: "the wire carried 2
// probe(s) for 192.168.99.129, want PROBE_NUM = 3", the two frames seen being
// probes 2 and 3 — their gaps, 1.477s and 2.148s, are PROBE_MIN..PROBE_MAX
// and ANNOUNCE_WAIT, not two inter-probe gaps.
func (f *conflictFixture) start(t *testing.T, sq *squatter) {
	t.Helper()
	if sq == nil {
		t.Fatal("the observer must be on the wire before the client starts")
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- f.client.Run(ctx) }()
	f.cancel, f.runErr = cancel, runErr
	t.Cleanup(func() {
		cancel()
		<-runErr
	})
}

// offers returns the addresses dnsmasq has logged a DHCPOFFER for, in order.
func (f *conflictFixture) offers() []string {
	var out []string
	for _, l := range f.srv.lines() {
		if a, ok := addrInLog(l, "DHCPOFFER("+f.srv.iface+")"); ok {
			out = append(out, a)
		}
	}
	return out
}

// addrInLog pulls the address out of a dnsmasq log line of the shape
// "<prefix> <address> <mac> ...".
func addrInLog(line, prefix string) (string, bool) {
	i := strings.Index(line, prefix)
	if i < 0 {
		return "", false
	}
	fields := strings.Fields(line[i+len(prefix):])
	if len(fields) == 0 {
		return "", false
	}
	if net.ParseIP(fields[0]).To4() == nil {
		return "", false
	}
	return fields[0], true
}

// quote renders the log lines this milestone reports verbatim, so the excerpt
// in the handover is the test's own output and not a transcription.
func (f *conflictFixture) quote(t *testing.T, what string) {
	t.Helper()
	// The transaction lines only. dnsmasq's --log-dhcp prose ("available DHCP
	// range", "requested options", "sent size") is what makes this log
	// unreadable in a report, and every one of those lines contains the string
	// DHCP.
	var keep []string
	for _, l := range f.srv.lines() {
		for _, kind := range []string{
			"DHCPDISCOVER(", "DHCPOFFER(", "DHCPREQUEST(", "DHCPACK(",
			"DHCPNAK(", "DHCPDECLINE(", "DHCPRELEASE(", "DHCPINFORM(",
		} {
			if strings.Contains(l, kind) {
				keep = append(keep, l)
				break
			}
		}
	}
	t.Logf("dnsmasq log excerpt (%s):\n%s", what, strings.Join(keep, "\n"))
}

// ------------------------------------------- section 2.1: the probe window --

func TestASquatterInTheProbeWindowMakesAWaitingClientDecline(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		squatterInProbeWindow(t, proto.ConflictWait)
		return
	}
	reexecInNamespaces(t)
}

func TestASquatterInTheProbeWindowMakesAnAsyncClientDecline(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		squatterInProbeWindow(t, proto.ConflictAsync)
		return
	}
	reexecInNamespaces(t)
}

// squatterInProbeWindow is the milestone's first and second runs.
//
// A second host on the link already holds the address dnsmasq offers, and says
// so when the client probes for it. RFC 2131 section 4.4.1: "If the client
// detects that the address is already in use (e.g., through the use of ARP),
// the client MUST send a DHCPDECLINE message to the server and restarts the
// configuration process."
//
// THE TWO MODES DIFFER IN ONE OBSERVABLE AND ONLY ONE. In ConflictWait no
// Acquired for the squatted address ever reaches the caller, because section
// 2.1's check has not passed; in ConflictAsync the caller is told Acquired
// first and Lost{ReasonConflict} afterwards. dnsmasq's log is identical either
// way, which is the point: the mode is a promise to the CALLER about when it
// may use the address, not a change to the protocol on the wire.
func squatterInProbeWindow(t *testing.T, mode proto.ConflictMode) {
	f := newConflictClient(t, mode, briskACD(), "m6-client")
	sq := newSquatter(t, testServerIf, squatterDefendFirstProbed)
	f.start(t, sq)

	// The first address the server hands out. Read from the SERVER's log, not
	// from the client: the whole question is whether the client gave back the
	// address the server thinks it gave.
	f.srv.waitCount(t, "DHCPACK("+f.srv.iface+")", 1, "the first acquisition never reached DHCPACK")
	first := f.offers()
	if len(first) == 0 {
		t.Fatalf("dnsmasq logged no DHCPOFFER.\nLog:\n%s", strings.Join(f.srv.lines(), "\n"))
	}
	squatted := first[0]

	if mode == proto.ConflictAsync {
		// D23: usable at once. This event arrives before the probing that
		// will take the address away, and it says so.
		ev := awaitAcquired(t, f.client)
		if got := ev.Lease.Addr.Addr().String(); got != squatted {
			t.Fatalf("async client acquired %s, the server offered %s", got, squatted)
		}
		if ev.ACD != proto.ACDProbing {
			t.Fatalf("async Acquired carries ACD phase %s, want probing: a chassis that restarts here would skip the check", ev.ACD)
		}
	}

	// ---------------------------------------------------- the wire itself --
	//
	// The Probe as another host sees it. Sender IP all zeroes is what makes
	// the frame a Probe (RFC 5227 section 1.1) and is the whole difference
	// from the datagram trick 1.x used, which poisons the ARP cache of every
	// host that hears it.
	target := netip.MustParseAddr(squatted)
	g := sq.waitForSighting(func(p *wire.ARPPacket) bool { return p.IsProbe() && p.TargetIP == target })
	if !g.p.SenderIP.IsUnspecified() {
		t.Fatalf("the probe carried sender IP %s, want RFC 5227 1.1's all-zero", g.p.SenderIP)
	}
	if got := net.HardwareAddr(g.p.SenderHW).String(); got != f.clientMAC {
		t.Fatalf("the probe carried sender hardware address %s, want the client's %s (section 2.1.1 makes it a MUST)", got, f.clientMAC)
	}

	// ----------------------------------------------- the server's own log --
	f.srv.waitFor(t, "DHCPDECLINE("+f.srv.iface+") "+squatted+" "+f.clientMAC)
	f.srv.waitCount(t, "DHCPDISCOVER("+f.srv.iface+")", 2, "the client did not restart the configuration process after the DECLINE")
	f.srv.waitCount(t, "DHCPOFFER("+f.srv.iface+")", 2, "the server never offered a second address")

	offers := f.offers()
	if offers[len(offers)-1] == squatted {
		t.Fatalf("the server re-offered the declined address %s; the log is\n%s", squatted, strings.Join(f.srv.lines(), "\n"))
	}
	f.quote(t, mode.String()+" mode, squatter in the probe window")

	if mode == proto.ConflictAsync {
		// D23's other half: the caller WAS told it held this address, so the
		// conflict has to be reported as a loss of it. Seam row G-5.
		lost := awaitEvent(t, f.client, lease.Lost)
		if lost.Reason != proto.ReasonConflict {
			t.Fatalf("Lost carries reason %s, want conflict", lost.Reason)
		}
		if got := lost.Lease.Addr.Addr().String(); got != squatted {
			t.Fatalf("Lost names %s, the client was told it held %s", got, squatted)
		}
	}

	// The second address is probed too. A client that declined and then took
	// the replacement on faith would pass every assertion above.
	// Two of them, not one, and the second is the BARRIER for the counter
	// assertion at the end of this function. ProbesSent is bumped after the
	// send returns, so the wire runs one frame ahead of it; waiting for a
	// LATER frame -- one the assertion is not about -- puts the earlier
	// bumps behind us without spinning on the number under test, which is
	// how a broken counter stays a red rather than becoming a hang.
	second := netip.MustParseAddr(offers[len(offers)-1])
	sq.waitForCount(isProbeFor(second), 2)

	// The caller's side of it, which differs by mode and is the reason D23
	// exists at all.
	if mode == proto.ConflictWait {
		ev, failures := awaitAcquiredThroughAConflict(t, f.client)
		if got := ev.Lease.Addr.Addr().String(); got == squatted {
			t.Fatalf("the waiting client announced the squatted address %s", got)
		}
		if ev.ACD == proto.ACDProbing {
			t.Fatalf("a waiting client was told Acquired while still probing (%s)", ev.ACD)
		}
		// WHAT THE CHASSIS COUNTS WHEN NOTHING WAS ACQUIRED. Lost is the
		// confirmation that a held lease was given back, and in this mode the
		// caller was never told it held one, so a Lost here would name an
		// address the caller had never seen. Failed{conflict} is what carries
		// it instead, and this is the assertion that says so.
		if len(failures) != 1 {
			t.Fatalf("the waiting client emitted %d Failed event(s) before acquiring, want exactly the one conflict", len(failures))
		}
		if failures[0].Reason != proto.ReasonConflict {
			t.Fatalf("Failed carries reason %s, want conflict", failures[0].Reason)
		}
		if !strings.Contains(failures[0].Note, squatted) {
			t.Fatalf("Failed says %q and does not name the squatted address %s", failures[0].Note, squatted)
		}
	}

	// Counters, as a cross-check on the evidence above and never as the
	// evidence: exactly one conflict, and the probes that were sent were sent.
	st := f.client.Stats()
	if st.ConflictsDetected != 1 {
		t.Errorf("ConflictsDetected = %d, want 1", st.ConflictsDetected)
	}
	if st.ProbesSent < 2 {
		t.Errorf("ProbesSent = %d, want at least one for each of the two addresses", st.ProbesSent)
	}
	t.Logf("client stats: %d conflict(s), %d probe(s), %d announcement(s), %d ARP frames seen, %d ignored",
		st.ConflictsDetected, st.ProbesSent, st.AnnouncementsSent, st.ARPSeen, st.ARPIgnored)
}

// awaitAcquiredThroughAConflict reads events until an Acquired, keeping the
// Failed events it passed on the way.
//
// The shared awaitAcquired treats any Failed as fatal, which is right
// everywhere else and wrong here: in ConflictWait a conflict found during
// section 2.1's probing IS a Failed, because nothing was ever acquired for
// there to be a Lost about. See the assertion at the call site.
func awaitAcquiredThroughAConflict(t *testing.T, c *Client) (lease.Event, []lease.Event) {
	t.Helper()
	var failures []lease.Event
	for ev := range c.Events() {
		t.Logf("client event: %s", ev)
		switch ev.Kind {
		case lease.Acquired:
			return ev, failures
		case lease.Failed:
			if ev.Reason != proto.ReasonConflict {
				t.Fatalf("the client failed for a reason this test did not arrange: %s", ev)
			}
			failures = append(failures, ev)
		}
	}
	t.Fatal("the event stream ended before a lease was acquired")
	return lease.Event{}, nil
}

// ------------------------------------ section 2.4: after the lease is held --

func TestASquatterAfterBoundTakesSection24sPath(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		squatterAfterBound(t)
		return
	}
	reexecInNamespaces(t)
}

// squatterAfterBound is the milestone's third run.
//
// Nothing answers the probes, the client binds, and only then does another
// host start claiming the address — RFC 5227 section 2.4, whose predicate is
// an ARP packet "where the 'sender IP address' is the address being defended
// and the 'sender hardware address' does not match" this host's.
//
// Arm (a) is what a DHCP client takes: "Upon receiving a conflicting ARP
// packet, a host MAY immediately cease using the address, and signal an error
// to the configuring agent". The configuring agent is the DHCP server and the
// signal is DHCPDECLINE. The address is never defended, because it was never
// this host's to defend.
func squatterAfterBound(t *testing.T) {
	f := newConflictClient(t, proto.ConflictWait, briskACD(), "m6-client")
	sq := newSquatter(t, testServerIf, squatterAnnounceOnCue)
	f.start(t, sq)

	ev := awaitAcquired(t, f.client)
	held := ev.Lease.Addr.Addr()
	if ev.ACD == proto.ACDProbing {
		t.Fatalf("a waiting client reached Acquired while still probing (%s); D22 says the address is not used until the check passes", ev.ACD)
	}
	f.srv.waitCount(t, "DHCPACK("+f.srv.iface+") "+held.String(), 1, "the server never acknowledged the address the client says it holds")

	// The listener is still open, which is the half of section 2.4 that is
	// easiest to lose: the probing is over and its socket is not.
	cue := time.Now()
	sq.announce <- held

	lost := awaitEvent(t, f.client, lease.Lost)
	if lost.Reason != proto.ReasonConflict {
		t.Fatalf("Lost carries reason %s, want conflict (plugin seam row G-5)", lost.Reason)
	}
	if lost.Lease.Addr.Addr() != held {
		t.Fatalf("Lost names %s, the client held %s", lost.Lease.Addr, held)
	}

	f.srv.waitFor(t, "DHCPDECLINE("+f.srv.iface+") "+held.String()+" "+f.clientMAC)
	f.srv.waitCount(t, "DHCPDISCOVER("+f.srv.iface+")", 2, "the client did not restart after the section 2.4 conflict")
	f.quote(t, "squatter after BOUND, RFC 5227 section 2.4")

	// The client never answered. Arm (a) is "cease using", not "defend", and
	// the way to see the absence is to look for what a defender would have
	// sent: an ARP Reply, or a fresh Announcement, for the address after the
	// squatter claimed it.
	for _, g := range sq.sightings() {
		if g.at.Before(cue) {
			continue
		}
		if g.p.Op == wire.ARPReply && g.p.SenderIP == held {
			t.Fatalf("the client defended %s with %s; arm (a) never answers", held, g.p)
		}
	}

	st := f.client.Stats()
	if st.ConflictsDetected != 1 {
		t.Errorf("ConflictsDetected = %d, want 1", st.ConflictsDetected)
	}
}

// -------------------------------------- no squatter: the schedule, MEASURED --

func TestTheDelayBeforeAnAcquisitionIsRFC5227sArithmetic(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		measureTheProbeDelay(t)
		return
	}
	reexecInNamespaces(t)
}

// measureTheProbeDelay is the milestone's fourth run: the price of D22, timed
// on the wire.
//
// THE ARITHMETIC, from RFC 5227 section 2.1. The host "SHOULD then wait for a
// random time interval selected uniformly in the range zero to PROBE_WAIT
// seconds, and should then send PROBE_NUM probe packets, each of these probe
// packets spaced randomly and uniformly, PROBE_MIN to PROBE_MAX seconds
// apart"; then, "if, by ANNOUNCE_WAIT seconds after the transmission of the
// last ARP Probe no conflicting ARP Reply or ARP Probe has been received",
// the address is free. Section 2.3 then says the host "may begin legitimately
// using the IP address immediately after sending the first of the two ARP
// Announcements".
//
// So, with section 1.1's constants (PROBE_WAIT 1s, PROBE_NUM 3, PROBE_MIN 1s,
// PROBE_MAX 2s, ANNOUNCE_WAIT 2s), the delay from the DHCPACK to the first
// Announcement is
//
//	U(0,1) + U(1,2) + U(1,2) + 2   =  4.0 s at best, 5.5 s on average, 7.0 s
//	                                  at worst.
//
// The brief that commissioned this milestone said "≈3 s (worst ≈5 s)". That
// is low, and the difference is not an implementation choice: it is the two
// inter-probe gaps, which the RFC makes PROBE_MIN..PROBE_MAX and not zero.
// The MEASURED number below is what a container will actually wait, and it is
// the reason D23's async mode exists.
func measureTheProbeDelay(t *testing.T) {
	f := newConflictClient(t, proto.ConflictWait, proto.DefaultACDParams(), "m6-client")
	sq := newSquatter(t, testServerIf, squatterObserve)
	f.start(t, sq)

	ev := awaitAcquired(t, f.client)
	held := ev.Lease.Addr.Addr()

	d := proto.DefaultACDParams()

	// THE ANNOUNCEMENT IS THE BARRIER, and it is waited for rather than
	// sampled. Acquired is emitted from the manager's own goroutine the
	// instant the first Announcement is handed to the socket; the squatter is
	// a second process reading a second socket, and it has not necessarily
	// stamped that frame yet. Section 2.3 puts the Announcement after the
	// whole probe schedule, so once one has been read no Probe is still to
	// come.
	//
	// That closes the LATE edge of the window. The EARLY edge is closed by
	// conflictFixture.start, which will not run the client until the observer
	// holds a bound socket — read its comment, because a probe sent before
	// the observer existed is invisible here and looks like a probe never
	// sent.
	//
	// MEASURED 2026-09-04: sampled instead of waited for, this test failed
	// under load with "the wire carried no ARP Announcement" on both a loaded
	// and a concurrent verify run, having timed nothing.
	anns := sq.waitForCount(isAnnouncementFor(held), 1)
	probes := sq.matching(isProbeFor(held))
	if len(probes) != d.ProbeNum {
		t.Fatalf("the wire carried %d probe(s) for %s, want PROBE_NUM = %d.\nsightings: %v",
			len(probes), held, d.ProbeNum, sq.sightings())
	}

	// The DHCPACK's own moment, taken from the packet ring: the frame the
	// server sent, timestamped when the client read it off the socket.
	var ackAt time.Time
	for _, p := range f.client.Packets() {
		if p.Dir != lease.DirIn || p.Msg == nil {
			continue
		}
		if mt, ok := p.Msg.Type(); ok && mt == wire.MsgAck {
			ackAt = p.At
		}
	}
	if ackAt.IsZero() {
		t.Fatal("the packet ring holds no DHCPACK, so there is nothing to measure from")
	}

	// MEASURED, all of it. Nothing below is a constant this file chose.
	toFirstProbe := probes[0].at.Sub(ackAt)
	gap1 := probes[1].at.Sub(probes[0].at)
	gap2 := probes[2].at.Sub(probes[1].at)
	toAnnounce := anns[0].at.Sub(probes[len(probes)-1].at)
	total := anns[0].at.Sub(ackAt)

	t.Logf("MEASURED on the wire, %s:\n"+
		"  DHCPACK -> probe 1      %8.3fs   RFC: U(0, PROBE_WAIT=%s)\n"+
		"  probe 1 -> probe 2      %8.3fs   RFC: U(PROBE_MIN=%s, PROBE_MAX=%s)\n"+
		"  probe 2 -> probe 3      %8.3fs   RFC: U(PROBE_MIN=%s, PROBE_MAX=%s)\n"+
		"  probe 3 -> announce 1   %8.3fs   RFC: ANNOUNCE_WAIT=%s\n"+
		"  DHCPACK -> announce 1   %8.3fs   RFC: 4.000s..7.000s, mean 5.500s",
		held,
		toFirstProbe.Seconds(), d.ProbeWait,
		gap1.Seconds(), d.ProbeMin, d.ProbeMax,
		gap2.Seconds(), d.ProbeMin, d.ProbeMax,
		toAnnounce.Seconds(), d.AnnounceWait,
		total.Seconds())

	// The tolerance is one-sided in principle — a timer cannot fire early —
	// and two-sided in practice by this much, because reading a frame off a
	// socket and stamping it happens after the send. It is not a fudge
	// factor for the schedule: shrinking PROBE_NUM or ANNOUNCE_WAIT moves
	// these by seconds, not milliseconds.
	const slack = 500 * time.Millisecond
	within := func(what string, got, lo, hi time.Duration) {
		t.Helper()
		if got < lo-slack || got > hi+slack {
			t.Errorf("%s took %s, RFC 5227 says %s..%s", what, got, lo, hi)
		}
	}
	within("the wait before the first probe", toFirstProbe, 0, dur(d.ProbeWait))
	within("the first inter-probe gap", gap1, dur(d.ProbeMin), dur(d.ProbeMax))
	within("the second inter-probe gap", gap2, dur(d.ProbeMin), dur(d.ProbeMax))
	within("the wait after the last probe", toAnnounce, dur(d.AnnounceWait), dur(d.AnnounceWait))
	within("the whole delay from DHCPACK to first announcement", total,
		dur(d.AnnounceWait)+2*dur(d.ProbeMin),
		dur(d.ProbeWait)+2*dur(d.ProbeMax)+dur(d.AnnounceWait))

	// ORDERING, which is the assertion D22 actually makes: the caller is told
	// Acquired only after the probing is over.
	//
	// The phase at Acquired is ANNOUNCING and not DEFENDING, and that is the
	// RFC rather than an off-by-one. Section 2.3: the host "may begin
	// legitimately using the IP address immediately after sending the first of
	// the two ARP Announcements". The second is still owed at this moment.
	// What D22 promises is that this is not PROBING.
	if ev.ACD != proto.ACDAnnouncing {
		t.Errorf("Acquired carries ACD phase %s, want announcing (section 2.3's first announcement is sent, the second is not)", ev.ACD)
	}
	if !probes[len(probes)-1].at.Before(anns[0].at) {
		t.Errorf("the announcement did not follow the last probe")
	}
	if f.srv.count("DHCPDECLINE") != 0 {
		t.Errorf("dnsmasq logged a DECLINE on a link with nothing on it:\n%s", strings.Join(f.srv.lines(), "\n"))
	}
	f.quote(t, "no squatter, the clean acquisition")

	// The announcements are not the probes. A run where the "probes" were
	// really announcements would satisfy the timing above and would have
	// polluted every ARP cache on the link.
	for i, p := range probes {
		if !p.p.SenderIP.IsUnspecified() {
			t.Errorf("probe %d carried sender IP %s, want all-zero", i+1, p.p.SenderIP)
		}
		if p.p.TargetIP != held {
			t.Errorf("probe %d targeted %s, want the leased %s", i+1, p.p.TargetIP, held)
		}
	}
	if anns[0].p.SenderIP != held || anns[0].p.TargetIP != held {
		t.Errorf("the announcement is %s, want sender and target both %s (section 2.3)", anns[0].p, held)
	}

	// ANNOUNCE_NUM is two and the second one is owed ANNOUNCE_INTERVAL after
	// the first, so it is waited for on the wire rather than read out of the
	// counter at a moment the RFC says it has not been sent yet.
	anns = sq.waitForCount(isAnnouncementFor(held), d.AnnounceNum)
	if gap := anns[1].at.Sub(anns[0].at); gap < dur(d.AnnounceInterval)-slack || gap > dur(d.AnnounceInterval)+slack {
		t.Errorf("the two announcements are %s apart, RFC 5227 says ANNOUNCE_INTERVAL = %s", gap, d.AnnounceInterval)
	}

	// THE BARRIER IS THE PACKET RING, not the counters this then asserts on.
	//
	// A send bumps its counter and only then records the frame in the ring,
	// so a ring that holds ANNOUNCE_NUM outgoing Announcements is proof that
	// every bump owed for them has already happened. The obvious shortcut --
	// spinning until the counters reach the expected number -- would make a
	// counter that never counts an infinite spin instead of a failed
	// assertion, and the tree has MEASURED that once already: written that
	// way in transport_packet_linux_test.go, two counter mutants came back as
	// 200-second hangs, and a hang is not a kill.
	for countOutgoing(f.client, isAnnouncementFor(held)) < d.AnnounceNum {
		goruntime.Gosched()
	}
	st := f.client.Stats()
	if st.ProbesSent != uint64(d.ProbeNum) || st.AnnouncementsSent != uint64(d.AnnounceNum) {
		t.Errorf("the client counted %d probe(s) and %d announcement(s), want %d and %d",
			st.ProbesSent, st.AnnouncementsSent, d.ProbeNum, d.AnnounceNum)
	}
}

// countOutgoing counts the ARP frames the client's own packet ring says it
// sent and pred accepts. It is a barrier, never evidence: what the client
// believes it sent is exactly the thing the squatter's socket is here to
// check independently.
func countOutgoing(c *Client, pred func(*wire.ARPPacket) bool) int {
	n := 0
	for _, p := range c.Packets() {
		if p.Dir == lease.DirOut && p.ARP != nil && pred(p.ARP) {
			n++
		}
	}
	return n
}

// dur converts ring 1's Duration to the one testing prints.
func dur(d proto.Duration) time.Duration { return time.Duration(d) }

// ------- round 2, finding 1: the address is IN USE while the window is open --

// testNeighbourIP is an address in the subnet that nothing holds. It is what
// the client's own stack is made to resolve, so that the ARP Request it emits
// is an ordinary one — sender IP the leased address, sender hardware address
// the client's — and not something this test built.
//
// It is NOT the gateway, which is what a container would really ARP for, and
// the reason is the fixture: both ends of the veth pair live in one network
// namespace, so testServerIP is a LOCAL address and the kernel answers it
// internally without ever putting an ARP Request on the wire. An unassigned
// neighbour is the same frame aimed at an address that forces the resolution.
const testNeighbourIP = "192.168.99.222"

// testAskerIP is the address the observer puts in the 'sender IP address' of
// the ARP Requests it uses to make the client's kernel answer.
//
// It is NOT testServerIP and NOT testNeighbourIP, and both exclusions are
// measured rather than stylistic. MEASURED 2026-09-04: with testServerIP —
// which is configured on the server's end of the same veth pair and is
// therefore a LOCAL address to the one kernel both ends share — no ARP Reply
// is ever sent, and the run hangs. With testNeighbourIP the request would
// populate the client's neighbour table for the very address resolveNeighbour
// then needs to resolve, so the ARP Request that test drives would never be
// sent. A third address, configured nowhere, is an ordinary neighbour asking
// an ordinary question.
const testAskerIP = "192.168.99.221"

// strictARP stops the SERVER's end of the veth pair from answering ARP for an
// address configured on the CLIENT's end.
//
// It is a property of this fixture and of no deployment. Both ends live in one
// network namespace, so one kernel owns both, and Linux's default
// arp_ignore = 0 means "reply for any local address, whichever interface the
// request arrived on". The moment a test configures the leased address on the
// client's interface, the server's interface starts answering the client's own
// ARP Probes for it — with the SERVER's hardware address, which is a foreign
// host claiming our address and IS a conflict under RFC 5227 section 2.1.1's
// first rule. A correct verdict about a wrong wire.
//
// MEASURED 2026-09-04 before this was set: DHCPACK 192.168.99.122, then
// "address conflict: RFC 5227 2.1.1: an ARP packet from fe:ae:9c:4b:e1:4e
// claims 192.168.99.122 while we are probing for it" — that hardware address
// being the server's end of the pair, not the client's.
//
// arp_ignore = 1 is "reply only if the target address is configured on the
// interface the request arrived on", which is what two hosts on a wire do. The
// client's own interface still answers for its own address, which is what
// section 2.5 requires and what this test needs it to keep doing.
func strictARP(t *testing.T) {
	t.Helper()
	const knob = "/proc/sys/net/ipv4/conf/all/arp_ignore"
	if err := os.WriteFile(knob, []byte("1\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", knob, err)
	}
}

// slowACD widens RFC 5227's window enough for a test to do work inside it,
// while keeping the counts.
//
// briskACD's window is 150-300ms end to end, which is not a window a test can
// configure an address in and then race. This one puts the first Announcement
// about 1.5s after the DHCPACK. The test does not TRUST that number: it reads
// the Announcement off the observer's socket and asserts that the frames it
// drove were seen before it.
func slowACD() proto.ACDParams {
	d := proto.DefaultACDParams()
	d.ProbeWait = 100 * proto.Millisecond
	d.ProbeMin = 500 * proto.Millisecond
	d.ProbeMax = 500 * proto.Millisecond
	d.AnnounceWait = 400 * proto.Millisecond
	d.AnnounceInterval = 100 * proto.Millisecond
	return d
}

// fromClient matches the frames the CLIENT's stack put on the wire: the
// section 2.5 ARP Reply the kernel owes for the leased address, and the
// ordinary ARP Request it sends resolving a neighbour from that address.
func fromClientReplyFor(mac string, addr netip.Addr) func(*wire.ARPPacket) bool {
	return func(p *wire.ARPPacket) bool {
		return p.Op == wire.ARPReply && p.SenderIP == addr &&
			net.HardwareAddr(p.SenderHW).String() == mac
	}
}

func fromClientRequestTo(mac string, addr netip.Addr, target netip.Addr) func(*wire.ARPPacket) bool {
	return func(p *wire.ARPPacket) bool {
		return p.Op == wire.ARPRequest && p.SenderIP == addr && p.TargetIP == target &&
			net.HardwareAddr(p.SenderHW).String() == mac
	}
}

func fromMAC(mac string) func(*wire.ARPPacket) bool {
	return func(p *wire.ARPPacket) bool {
		return net.HardwareAddr(p.SenderHW).String() == mac
	}
}

// resolveNeighbour makes the client's own kernel ARP for testNeighbourIP from
// the leased address.
//
// A UDP datagram to an address with no neighbour entry is the cheapest way to
// oblige a resolution, and the route added first is what pins the frame to the
// client's link and its source address: both interfaces carry the /24, so a
// route lookup without it could leave through the server's end.
//
// The send is expected to succeed at the socket layer and nothing is expected
// to answer. What is asserted is the ARP Request, on the observer's socket.
func resolveNeighbour(t *testing.T, from netip.Addr) {
	t.Helper()
	mustRun(t, "ip", "route", "add", testNeighbourIP+"/32", "dev", testClientIf, "src", from.String())
	c, err := net.Dial("udp4", testNeighbourIP+":9")
	if err != nil {
		t.Fatalf("dialling %s to force an ARP resolution: %v", testNeighbourIP, err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("m6")); err != nil {
		t.Fatalf("writing to %s to force an ARP resolution: %v", testNeighbourIP, err)
	}
}

// The round-2 pool. THREE ADDRESSES, and the size is the requirement rather
// than tidiness: every address in it gets a permanent neighbour entry before
// anything starts, and renewalAgainstDnsmasq's reason applies unchanged here —
// a renewal DHCPACK is unicast to the client's address (RFC 2131 section
// 4.3.2), the client's address lives only inside an AF_PACKET socket, so
// without the entry the server's kernel ARPs into silence and the renewal
// never completes. Both round-2 runs that wait for a renewal need it.
const (
	testR2PoolLo = "192.168.99.120"
	testR2PoolHi = "192.168.99.122"
)

// renewableConflictClient is newConflictClientCfg with the pool small, the
// neighbour entries pinned and option 58 set, which is what a run that waits
// for a renewal needs.
func renewableConflictClient(t *testing.T, mode proto.ConflictMode, acd proto.ACDParams, hostname string, renewSec int) *conflictFixture {
	t.Helper()
	f := newConflictClientCfg(t, mode, acd, hostname, dnsmasqConfig{
		rangeLo: testR2PoolLo, rangeHi: testR2PoolHi,
		extra: []string{"--dhcp-option=58," + strconv.Itoa(renewSec)},
	})
	for _, a := range addrRange(t, testR2PoolLo, testR2PoolHi) {
		mustRun(t, "ip", "neigh", "replace", a, "lladdr", f.clientMAC, "dev", testServerIf, "nud", "permanent")
	}
	return f
}

func TestOurOwnTrafficInTheProbeWindowDoesNotDeclineOurLease(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		ownTrafficInTheProbeWindow(t, false)
		return
	}
	reexecInNamespaces(t)
}

func TestASquatterInTheProbeWindowStillDeclinesWithTheAddressConfigured(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		ownTrafficInTheProbeWindow(t, true)
		return
	}
	reexecInNamespaces(t)
}

// ownTrafficInTheProbeWindow is round 2's proof for review finding 1, and the
// two arms are ONE CONTROL WITH ONE VARIABLE MOVED.
//
// Both arms run a ConflictAsync client, configure the leased address on the
// client's interface at Acquired — which is what D23 tells the chassis to do
// and what no round-1 fixture ever did — and then put two frames on the wire
// out of the client's OWN kernel while RFC 5227 section 2.1's window is still
// open:
//
//   - the section 2.5 ARP Reply. "From the time a host sends its first ARP
//     Announcement, until the time it ceases using that IP address, the host
//     MUST answer ARP Requests in the usual way required by the ARP
//     specification." The observer asks for the address; the kernel answers.
//     Sender IP is the lease, sender hardware address is the client's.
//   - an ordinary ARP Request resolving a neighbour, sender IP the lease.
//
// Round 1's predicate called both of those conflicts, because section 2.1.1's
// first rule as written has no hardware-address clause. The ONLY difference
// between the arms is whether a foreign host also claims the address in the
// same window. Arm one must end with no DHCPDECLINE in dnsmasq's log and a
// lease that survives to a renewal; arm two must still end with the DECLINE.
// An exemption widened from "our hardware address" to "the leased sender IP"
// passes arm one and fails arm two, which is why they are one function.
func ownTrafficInTheProbeWindow(t *testing.T, withSquatter bool) {
	// Option 58 is the renewal clock: arm one proves the lease SURVIVED, and
	// a lease survives by being renewed, not by nothing having happened yet.
	f := renewableConflictClient(t, proto.ConflictAsync, slowACD(), "m6-own", 3)
	strictARP(t)
	sq := newSquatter(t, testServerIf, squatterAnnounceOnCue)
	f.start(t, sq)

	// async: the caller is told at the DHCPACK, with the phase saying the
	// check has not finished. This is the moment the chassis configures the
	// address, and the whole of what finding 1 is about.
	ev := awaitAcquired(t, f.client)
	addr := ev.Lease.Addr.Addr()
	if ev.ACD != proto.ACDProbing {
		t.Fatalf("async Acquired carries ACD phase %s, want probing: this test needs the window open", ev.ACD)
	}
	mustRun(t, "ip", "addr", "add", ev.Lease.Addr.String(), "dev", testClientIf)

	// Frame one: the observer asks who has the address, and the client's
	// kernel answers because section 2.5 obliges it to.
	sq.send(&wire.ARPPacket{
		Op:       wire.ARPRequest,
		SenderHW: sq.hw,
		SenderIP: netip.MustParseAddr(testAskerIP),
		TargetIP: addr,
	})
	// Frame two: the client's kernel resolves a neighbour from the address.
	resolveNeighbour(t, addr)

	reply := sq.waitForSighting(fromClientReplyFor(f.clientMAC, addr))
	request := sq.waitForSighting(fromClientRequestTo(f.clientMAC, addr, netip.MustParseAddr(testNeighbourIP)))

	// THE LINK ECHO, and without it this run cannot see finding 1 at all.
	//
	// MEASURED 2026-09-04, first run of this test: three Probes and two
	// Announcements sent, ARPSeen = 1. A packet socket bound to ETH_P_ARP
	// rather than ETH_P_ALL is registered in the kernel's ptype_base, and
	// dev_queue_xmit_nit walks ptype_all — so the socket is delivered INBOUND
	// frames only, and the client's own kernel traffic above, both frames of
	// it, never reaches ring 1 by itself. A run that stopped at the two
	// sightings would therefore pass with round 1's sender-IP-only predicate
	// still in place: it would be measuring the link, not the rule.
	//
	// So the observer replays both frames back onto the wire. That is not a
	// contrivance to make the test fire; it is the case RFC 5227 section
	// 2.1.1 names in its own words, "note that a host may see its own Probes
	// echoed back by the link", and section 2.4's hardware-address clause is
	// what makes it not a conflict. What goes back out is the client's own
	// ARP payload, byte for byte, sender hardware address included, which is
	// the field every rule in this file is written over; only the Ethernet
	// source is the relay's, which is exactly what a relaying bridge or
	// access point does with a frame it forwards.
	sq.send(reply.p)
	sq.send(request.p)

	if withSquatter {
		// The one variable. A foreign hardware address claiming the same
		// address, in the same window, from the same fixture.
		sq.announce <- addr
		f.srv.waitFor(t, "DHCPDECLINE("+f.srv.iface+") "+addr.String()+" "+f.clientMAC)
		f.srv.waitCount(t, "DHCPDISCOVER("+f.srv.iface+")", 2, "the client did not restart after the DECLINE")
		lost := awaitEvent(t, f.client, lease.Lost)
		if lost.Reason != proto.ReasonConflict {
			t.Fatalf("Lost carries reason %s, want conflict", lost.Reason)
		}
		f.quote(t, "the address configured AND a squatter, both inside the probe window")
		return
	}

	// The barrier that makes "inside the window" a measurement and not a
	// hope: the first ARP Announcement is what section 2.1's check ending
	// looks like on the wire, and both frames above were read off the same
	// socket before it.
	announcement := sq.waitForSighting(isAnnouncementFor(addr))
	if !reply.at.Before(announcement.at) {
		t.Fatalf("the section 2.5 reply was seen at %s, the first Announcement at %s: "+
			"the probe window had already closed and this run proves nothing about it",
			reply.at, announcement.at)
	}
	if !request.at.Before(announcement.at) {
		t.Fatalf("the neighbour ARP Request was seen at %s, the first Announcement at %s: "+
			"the probe window had already closed and this run proves nothing about it",
			request.at, announcement.at)
	}

	// THE ADDRESS COMES OFF BEFORE T1, and this is a property of the FIXTURE
	// and not of the client.
	//
	// Both ends of the veth pair live in one network namespace, so an address
	// configured on the client's end is a LOCAL address to the server's
	// kernel too. A renewal DHCPACK is unicast to it (RFC 2131 section 4.3.2),
	// and a unicast to a local address is routed to loopback whatever
	// interface the sender names — it never reaches the wire, and the
	// client's AF_PACKET socket never sees it. MEASURED 2026-09-04: with the
	// address left on, this run hangs at the renewal.
	//
	// It weakens nothing this test asserts. Both frames have already crossed
	// the wire, been read off the observer's socket, been shown to precede
	// the first Announcement, and — by ARPIgnored below — been classified by
	// the machine. What the renewal then proves is that no DHCPDECLINE was
	// sent for them, and the log is read for the whole run.
	mustRun(t, "ip", "addr", "del", ev.Lease.Addr.String(), "dev", testClientIf)

	// THE SERVER'S OWN LOG, and the lease surviving to a renewal. A renewal
	// is a second DHCPACK on the same address after a DHCPREQUEST that was
	// not preceded by a DISCOVER, which is exactly what a client that never
	// gave the address back does.
	renewed := awaitEvent(t, f.client, lease.Renewed)
	if renewed.Lease.Addr != ev.Lease.Addr {
		t.Fatalf("renewed onto %s, want the address it was told to keep, %s", renewed.Lease.Addr, ev.Lease.Addr)
	}
	if n := f.srv.count("DHCPDECLINE(" + f.srv.iface + ")"); n != 0 {
		t.Fatalf("the client sent %d DHCPDECLINE(s) for its own traffic.\nLog:\n%s",
			n, strings.Join(f.srv.lines(), "\n"))
	}
	if n := f.srv.count("DHCPDISCOVER(" + f.srv.iface + ")"); n != 1 {
		t.Fatalf("dnsmasq logged %d DHCPDISCOVER(s), want the one acquisition: a restart means the lease was given up", n)
	}
	f.quote(t, "the address configured on the client's interface inside the probe window")

	st := f.client.Stats()
	if st.ConflictsDetected != 0 {
		t.Errorf("ConflictsDetected = %d, want 0", st.ConflictsDetected)
	}
	// THE FRAMES REACHED THE MACHINE, AND REACHED IT WITH THE WINDOW OPEN.
	//
	// Every assertion above is outside evidence, and every one of them also
	// passes if the echoes were dropped before ring 1 ever classified them.
	// This is the one check that has to read the client's own record, because
	// "what the machine saw" is not observable from outside the machine. The
	// capture ring holds exactly the inbound frames Machine.ARPRelevant
	// admitted, plus the ARP the client sent, on one clock (lease/ports.go).
	var ownIn []lease.CapturedPacket
	var firstAnnounce time.Time
	for _, cp := range f.client.Packets() {
		if cp.ARP == nil {
			continue
		}
		if cp.Dir == lease.DirIn && cp.ARP.SenderIP == addr &&
			net.HardwareAddr(cp.ARP.SenderHW).String() == f.clientMAC {
			ownIn = append(ownIn, cp)
			continue
		}
		if cp.Dir == lease.DirOut && !cp.ARP.IsProbe() && firstAnnounce.IsZero() {
			firstAnnounce = cp.At
		}
	}
	if len(ownIn) < 2 {
		t.Fatalf("the machine classified %d frame(s) carrying our own hardware address and %s, want the 2 echoed back: "+
			"a run where they never arrived proves nothing about the predicate", len(ownIn), addr)
	}
	if firstAnnounce.IsZero() {
		t.Fatalf("the client never captured an Announcement, so there is no window boundary to measure against")
	}
	for _, cp := range ownIn {
		if !cp.At.Before(firstAnnounce) {
			t.Errorf("the machine read our own %s at %s, after its first Announcement at %s: "+
				"section 2.1's window had closed and this frame was judged by the section 2.4 arm, not the probing arm",
				cp.ARP.Op, cp.At, firstAnnounce)
		}
	}
	t.Logf("client stats: %d conflict(s), %d probe(s), %d announcement(s), %d ARP frames seen, %d ignored",
		st.ConflictsDetected, st.ProbesSent, st.AnnouncementsSent, st.ARPSeen, st.ARPIgnored)
}

// ------------------------- round 2, finding 3: off, at ring 3, on a wire --

func TestAnOffClientPutsNoARPOnTheWire(t *testing.T) {
	if os.Getenv(nsChildEnv) == "1" {
		offClientOnAWire(t)
		return
	}
	reexecInNamespaces(t)
}

// offClientOnAWire is round 2's proof for review finding 3 and defeat row
// M6-22's ring-3 half.
//
// The row's stated closing move was "runtime.NewClient never calls
// NewARPSocket", and round 1 left that sentence with no test behind it: no
// ConflictOff client existed anywhere under runtime, so the mutant that made
// an off client open the socket anyway had no execution to change. The
// operator's complaint the row names — ARP on a macvlan parent shared with the
// host, from an endpoint configured not to do conflict detection — is about
// frames on a wire, so the assertion is about frames on a wire.
//
// THE BARRIER IS THE RENEWAL, and it is chosen rather than convenient. "No ARP
// frame" is a claim about a window, and a window that ends the moment the
// lease is acquired would be shorter than briskACD's own schedule: a client
// that probed would not have finished. Option 58 puts the renewal three
// seconds out, which is an order of magnitude past the 150-300ms briskACD
// would have taken to send PROBE_NUM probes and ANNOUNCE_NUM announcements.
func offClientOnAWire(t *testing.T) {
	f := renewableConflictClient(t, proto.ConflictOff, briskACD(), "m6-off", 3)
	sq := newSquatter(t, testServerIf, squatterObserve)
	f.start(t, sq)

	ev := awaitAcquired(t, f.client)
	if ev.ACD != proto.ACDIdle {
		t.Fatalf("an off client's Acquired carries ACD phase %s, want idle", ev.ACD)
	}

	// There is no ARP port. Present is the only thing that can say so: the
	// counters are zero for a socket that read nothing too.
	if st := f.client.ARPStats(); st.Present {
		t.Fatalf("an off client reports an ARP port: %+v", st)
	}

	renewed := awaitEvent(t, f.client, lease.Renewed)
	if renewed.Lease.Addr != ev.Lease.Addr {
		t.Fatalf("renewed onto %s, want %s", renewed.Lease.Addr, ev.Lease.Addr)
	}

	// The wire, read by another host's socket. Not one frame, of any kind,
	// carrying this client's hardware address.
	if got := sq.matching(fromMAC(f.clientMAC)); len(got) != 0 {
		var lines []string
		for _, g := range got {
			lines = append(lines, g.p.String())
		}
		t.Fatalf("an off client put %d ARP frame(s) on the wire:\n%s", len(got), strings.Join(lines, "\n"))
	}
	if st := f.client.Stats(); st.ProbesSent != 0 || st.AnnouncementsSent != 0 {
		t.Errorf("an off client counted %d probe(s) and %d announcement(s), want none",
			st.ProbesSent, st.AnnouncementsSent)
	}
	f.quote(t, "conflict detection off")

	// DRIVE THE ABSENCE. The measurement above is "the observer heard no
	// frame carrying this hardware address", and an observer whose socket was
	// never bound reports exactly that. So make one such frame exist, in this
	// namespace, on this socket, after the assertion has been taken: the
	// address goes onto the client's interface and the observer asks who has
	// it, and the KERNEL answers — the library is not involved and this is
	// not the client probing. What it proves is that the silence above was
	// the client's and not the instrument's.
	mustRun(t, "ip", "addr", "add", ev.Lease.Addr.String(), "dev", testClientIf)
	sq.send(&wire.ARPPacket{
		Op:       wire.ARPRequest,
		SenderHW: sq.hw,
		SenderIP: netip.MustParseAddr(testAskerIP),
		TargetIP: ev.Lease.Addr.Addr(),
	})
	sq.waitForSighting(fromMAC(f.clientMAC))
}
