//go:build linux

package runtime

import (
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/wire"
)

// testDADProbe is a DADProbe with one address already installed as running and
// no goroutine, no socket and no schedule behind it.
//
// Everything RFC 4862 sections 5.4.3 and 5.4.4 decide is decided in inspect,
// from one frame and the map, so this is the whole subject. The netns proofs
// drive the schedule, where a real socket exists; putting the DECISION there
// too would price each of these rows at a network namespace and would still
// only reach the arms a live dnsmasq happens to produce.
func testDADProbe(t *testing.T, addr netip.Addr) (*DADProbe, *dadRun) {
	t.Helper()
	run := &dadRun{
		cancel:  make(chan struct{}),
		verdict: make(chan struct{}, 1),
		report:  func(netip.Addr, bool) { t.Errorf("inspect reported a verdict; only the probe goroutine may") },
	}
	p := &DADProbe{
		nd:      &NDSocket{hw: cliMAC},
		running: map[netip.Addr]*dadRun{addr: run},
		done:    make(chan struct{}),
	}
	return p, run
}

// naBody builds RFC 4861 section 4.4's Neighbor Advertisement for target.
//
// Built rather than captured because the TARGET is the field under test and a
// captured advertisement names whatever address the capture's node held. The
// checksum field is left zero: inspect never looks at it, because NDSocket
// already refused every frame whose checksum did not verify, and duplicating
// that check here would be one fact derived twice.
func naBody(target netip.Addr, solicited bool) []byte {
	b := make([]byte, 24)
	b[0] = wire.ICMPv6NeighborAdvert
	if solicited {
		b[4] = 0x60 // Solicited | Override, what a real node answers with.
	}
	t16 := target.As16()
	copy(b[8:24], t16[:])
	return b
}

// nsBody builds RFC 4861 section 4.3's Neighbor Solicitation for target.
func nsBody(t *testing.T, target netip.Addr) []byte {
	t.Helper()
	pkt, err := wire.EncodeDADNeighborSolicit(target)
	if err != nil {
		t.Fatalf("EncodeDADNeighborSolicit: %v", err)
	}
	return pkt.Body
}

// TestTheDuplicateCheckSortsOneFrameTheWayRFC4862Does is the whole of sections
// 5.4.3 and 5.4.4, one row per sentence.
//
// THE ROW TO READ IS THE FIRST. Section 5.4.3: "If the solicitation is from the
// node itself (because the node loops back multicast packets), the
// solicitation does not indicate the presence of a duplicate address." On this
// socket that frame never arrives — NDSocket's BOUNDS carry the measurement —
// so the netns proofs cannot reach this branch and assert OwnIgnored is ZERO
// instead. This is the only place the exclusion executes, and without it a
// client would decline the very address it just asked for, every time, on any
// link that echoes.
//
// The three counters are separate for the reason NDSocket's are: "nobody
// solicited that address" and "somebody solicited it and we correctly read it
// as address resolution" are the two readings of a clean run and only one of
// them is evidence that the code ran.
func TestTheDuplicateCheckSortsOneFrameTheWayRFC4862Does(t *testing.T) {
	const probed = "fd00:99::1a3"
	target := netip.MustParseAddr(probed)
	other := netip.MustParseAddr("fd00:99::999")
	unspec := netip.IPv6Unspecified()
	peerLL := netip.MustParseAddr("fe80::30e6:f9ff:fe2f:aa1e")

	for _, tc := range []struct {
		name        string
		frame       NDFrame
		wantVerdict bool
		counter     func(DADStats) uint64
	}{
		{
			// Section 5.4.3's loopback sentence. The frame is a perfectly
			// valid foreign-looking duplicate report in every field but one.
			name: "this node's own duplicate-address solicitation",
			frame: NDFrame{
				Src: unspec, SenderHW: cliMAC,
				Type: wire.ICMPv6NeighborSolicit, Body: nsBody(t, target),
			},
			counter: func(s DADStats) uint64 { return s.OwnIgnored },
		},
		{
			// Same exclusion, the other message type: an advertisement wearing
			// this interface's address is this host's, and section 5.4.4's
			// "the tentative address is not unique" is not what it means.
			name: "this node's own advertisement for the target",
			frame: NDFrame{
				Src: peerLL, SenderHW: cliMAC,
				Type: wire.ICMPv6NeighborAdvert, Body: naBody(target, true),
			},
			counter: func(s DADStats) uint64 { return s.OwnIgnored },
		},
		{
			// Section 5.4.3: "If the source address of the Neighbor
			// Solicitation is the unspecified address, the solicitation is
			// from a node performing Duplicate Address Detection.  If the
			// solicitation is from another node, the tentative address is a
			// duplicate and should not be used (by either node)."
			name: "another node probing the same address",
			frame: NDFrame{
				Src: unspec, SenderHW: srvMAC,
				Type: wire.ICMPv6NeighborSolicit, Body: nsBody(t, target),
			},
			wantVerdict: true,
			counter:     func(s DADStats) uint64 { return s.ForeignSolicits },
		},
		{
			// Section 5.4.3: "If the target address is tentative, and the
			// source address is a unicast address, the solicitation's sender
			// is performing address resolution on the target; the solicitation
			// should be silently ignored." Reading this as a duplicate would
			// make any neighbour asking after the address enough to lose it.
			name: "a neighbour resolving the address, from a unicast source",
			frame: NDFrame{
				Src: peerLL, SenderHW: srvMAC,
				Type: wire.ICMPv6NeighborSolicit, Body: nsBody(t, target),
			},
			counter: func(s DADStats) uint64 { return s.ResolvingSolicits },
		},
		{
			// Section 5.4.4: "If the target address is tentative, the
			// tentative address is not unique." Somebody answers for it, so
			// somebody holds it.
			name: "another node answering for the address",
			frame: NDFrame{
				Src: peerLL, SenderHW: srvMAC,
				Type: wire.ICMPv6NeighborAdvert, Body: naBody(target, true),
			},
			wantVerdict: true,
			counter:     func(s DADStats) uint64 { return s.Adverts },
		},
		{
			// An unsolicited advertisement is still section 5.4.4's evidence:
			// the flag says whether it answers a query, not whether the sender
			// holds the address.
			name: "an unsolicited advertisement for the address",
			frame: NDFrame{
				Src: peerLL, SenderHW: srvMAC,
				Type: wire.ICMPv6NeighborAdvert, Body: naBody(target, false),
			},
			wantVerdict: true,
			counter:     func(s DADStats) uint64 { return s.Adverts },
		},
		{
			// The link is busy and almost none of it is about us. A probe that
			// took any duplicate report as its own would decline on the first
			// unrelated conflict on the segment.
			name: "another node probing a DIFFERENT address",
			frame: NDFrame{
				Src: unspec, SenderHW: srvMAC,
				Type: wire.ICMPv6NeighborSolicit, Body: nsBody(t, other),
			},
		},
		{
			name: "an advertisement for a DIFFERENT address",
			frame: NDFrame{
				Src: peerLL, SenderHW: srvMAC,
				Type: wire.ICMPv6NeighborAdvert, Body: naBody(other, true),
			},
		},
		{
			// Neither type carries a target, so neither says anything about a
			// tentative address, and reading one as a duplicate would decline
			// every address on a link with a router on it.
			name: "a Router Advertisement",
			frame: NDFrame{
				Src: peerLL, SenderHW: srvMAC,
				Type: wire.ICMPv6RouterAdvert, Body: capV6RouterAdvert[40:],
			},
		},
		{
			// The codec refuses it (RFC 4861 section 7.1.2's "Target Address
			// is not a multicast address"), and a decode that failed is not a
			// verdict.
			name: "an advertisement whose target is multicast",
			frame: NDFrame{
				Src: peerLL, SenderHW: srvMAC,
				Type: wire.ICMPv6NeighborAdvert, Body: naBody(netip.MustParseAddr("ff02::1"), true),
			},
		},
		{
			name: "a solicitation too short to decode",
			frame: NDFrame{
				Src: unspec, SenderHW: srvMAC,
				Type: wire.ICMPv6NeighborSolicit, Body: nsBody(t, target)[:12],
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, run := testDADProbe(t, target)
			p.inspect(tc.frame)

			got := len(run.verdict) == 1
			if got != tc.wantVerdict {
				t.Errorf("verdict signalled = %v, want %v", got, tc.wantVerdict)
			}
			st := p.Stats()
			if st.Free != 0 || st.Duplicate != 0 {
				t.Errorf("inspect reported a verdict itself: Free=%d Duplicate=%d", st.Free, st.Duplicate)
			}
			total := st.OwnIgnored + st.ForeignSolicits + st.ResolvingSolicits + st.Adverts
			if tc.counter == nil {
				if total != 0 {
					t.Errorf("an ignored frame moved a counter: %+v", st)
				}
				return
			}
			if tc.counter(st) != 1 || total != 1 {
				t.Errorf("the frame did not land on exactly its own counter: %+v", st)
			}
		})
	}
}

// TestAVerdictDuringTheWaitWindowIsTheAnswer drives the wait itself.
//
// It is defeat row D-2: a probe that reported free because its listening
// window was not actually open would pass every row above — inspect would fill
// run.verdict and nothing would read it — and would report every taken address
// free on a real link. This is the row that says the channel is being watched
// while the schedule runs, not only after it.
//
// The one-hour interval is the point rather than a shortcut. If wait were
// reading the timer instead of the verdict the test would not fail slowly, it
// would hang, and a hang is a third verdict a passing run cannot produce.
func TestAVerdictDuringTheWaitWindowIsTheAnswer(t *testing.T) {
	target := netip.MustParseAddr("fd00:99::1a3")

	var (
		gotAddr netip.Addr
		gotDup  bool
		calls   int
	)
	run := &dadRun{
		cancel:  make(chan struct{}),
		verdict: make(chan struct{}, 1),
		report: func(a netip.Addr, dup bool) {
			calls++
			gotAddr, gotDup = a, dup
		},
	}
	p := &DADProbe{nd: &NDSocket{hw: cliMAC}, running: map[netip.Addr]*dadRun{target: run}, done: make(chan struct{})}

	p.inspect(NDFrame{
		Src: netip.MustParseAddr("fe80::30e6:f9ff:fe2f:aa1e"), SenderHW: srvMAC,
		Type: wire.ICMPv6NeighborAdvert, Body: naBody(target, true),
	})

	if p.wait(target, run, time.Hour) {
		t.Fatalf("wait returned true after a duplicate was found; the remaining solicitations would have gone out for an address somebody holds")
	}
	if calls != 1 || gotAddr != target || !gotDup {
		t.Fatalf("report calls=%d addr=%s duplicate=%v, want 1 %s true", calls, gotAddr, gotDup, target)
	}
	if st := p.Stats(); st.Duplicate != 1 || st.Free != 0 {
		t.Errorf("Duplicate=%d Free=%d, want 1 and 0", st.Duplicate, st.Free)
	}
}

// TestSilenceThroughTheWindowIsNotAVerdict is the control for the row above,
// and it is the one that makes the pair mean something.
//
// A wait that reported a duplicate unconditionally would pass D-2's row and
// would never let any address through. This drives the same call with nothing
// in the channel: it must run the interval out, return true so the schedule
// continues, and report NOTHING — the verdict for silence belongs to probe,
// after the last interval, and a wait that answered here would answer one
// interval early on every acquisition.
func TestSilenceThroughTheWindowIsNotAVerdict(t *testing.T) {
	target := netip.MustParseAddr("fd00:99::1a3")
	run := &dadRun{
		cancel:  make(chan struct{}),
		verdict: make(chan struct{}, 1),
		report:  func(netip.Addr, bool) { t.Errorf("wait reported a verdict for an interval that simply elapsed") },
	}
	p := &DADProbe{nd: &NDSocket{hw: cliMAC}, running: map[netip.Addr]*dadRun{target: run}, done: make(chan struct{})}

	if !p.wait(target, run, time.Millisecond) {
		t.Fatalf("wait returned false with no verdict and no cancel")
	}
	if st := p.Stats(); st.Free != 0 || st.Duplicate != 0 {
		t.Errorf("an elapsed interval moved a verdict counter: %+v", st)
	}
}

// TestACancelledProbeAnswersNothing drives Close's half of the contract.
//
// Section 5.4 has no answer for "the client went away", and neither does this:
// a probe cut short reports nothing at all, which is what keeps Free+Duplicate
// an honest count of the questions actually answered. The alternative — a
// cancelled probe reporting free — would hand ring 1 a verdict about an
// address nobody checked, on the way out of a client that is closing.
func TestACancelledProbeAnswersNothing(t *testing.T) {
	target := netip.MustParseAddr("fd00:99::1a3")
	run := &dadRun{
		cancel:  make(chan struct{}),
		verdict: make(chan struct{}, 1),
		report:  func(netip.Addr, bool) { t.Errorf("a cancelled probe reported a verdict") },
	}
	p := &DADProbe{nd: &NDSocket{hw: cliMAC}, running: map[netip.Addr]*dadRun{target: run}, done: make(chan struct{})}

	close(run.cancel)
	if p.wait(target, run, time.Hour) {
		t.Fatalf("wait returned true after cancellation")
	}
	if st := p.Stats(); st.Free != 0 || st.Duplicate != 0 {
		t.Errorf("a cancelled probe moved a verdict counter: %+v", st)
	}
}

// TestOneRunReportsExactlyOnce pins the arithmetic every other count here
// rests on.
//
// Two findings for one address are ordinary — another node's solicitation and
// then its advertisement, inside the same window — and ring 1 asked the
// question once. A second report would be an answer to a question that is no
// longer being asked, and it would also make Free+Duplicate exceed Started,
// which is the sum a reader uses to spot a probe that silently stopped
// answering.
func TestOneRunReportsExactlyOnce(t *testing.T) {
	target := netip.MustParseAddr("fd00:99::1a3")
	calls := 0
	run := &dadRun{
		cancel:  make(chan struct{}),
		verdict: make(chan struct{}, 1),
		report:  func(netip.Addr, bool) { calls++ },
	}
	p := &DADProbe{nd: &NDSocket{hw: cliMAC}, running: map[netip.Addr]*dadRun{target: run}, done: make(chan struct{})}

	p.deliver(target, run, true)
	p.deliver(target, run, false)
	p.deliver(target, run, true)

	if calls != 1 {
		t.Errorf("report called %d time(s), want 1", calls)
	}
	if st := p.Stats(); st.Duplicate != 1 || st.Free != 0 {
		t.Errorf("Duplicate=%d Free=%d, want 1 and 0; a second delivery moved a counter", st.Duplicate, st.Free)
	}
}

// TestAnAddressTheCodecRefusesIsReportedTakenNotFree drives the worst of the
// three answers Start could give.
//
// An address wire.EncodeDADNeighborSolicit will not build a solicitation for
// is one no solicitation ever goes out for, so silence about it is guaranteed
// and means nothing. Reporting it FREE on that silence would be the vacuous
// pass this whole runner exists to prevent; reporting it duplicate lands on
// the machine's "do not use this address" arm, which is what a target the
// codec refuses deserves.
//
// It needs no socket and no goroutine because Start refuses before it opens
// either, which is the same shape NewClient6 uses for an empty DUID.
func TestAnAddressTheCodecRefusesIsReportedTakenNotFree(t *testing.T) {
	for _, addr := range []netip.Addr{
		netip.MustParseAddr("ff02::1"),             // RFC 4861 section 4.3: the Target MUST NOT be multicast.
		netip.MustParseAddr("192.168.99.2"),        // not an IPv6 address at all.
		netip.MustParseAddr("::ffff:192.168.99.2"), // IPv4-mapped, which is not one either.
	} {
		t.Run(addr.String(), func(t *testing.T) {
			p := &DADProbe{running: make(map[netip.Addr]*dadRun), done: make(chan struct{})}
			var (
				gotAddr netip.Addr
				gotDup  bool
				calls   int
			)
			p.Start(addr, func(a netip.Addr, dup bool) { calls++; gotAddr, gotDup = a, dup })

			if calls != 1 || gotAddr != addr || !gotDup {
				t.Fatalf("report calls=%d addr=%s duplicate=%v, want 1 %s true", calls, gotAddr, gotDup, addr)
			}
			st := p.Stats()
			if st.Started != 1 || st.Duplicate != 1 || st.Free != 0 || st.Solicits != 0 {
				t.Errorf("Started=%d Duplicate=%d Free=%d Solicits=%d, want 1 1 0 0", st.Started, st.Duplicate, st.Free, st.Solicits)
			}
		})
	}
}

// TestStartWithNoReportDoesNothing drives the guard that keeps the map clean.
//
// A run installed with a nil report is one whose probe goroutine would panic
// at the end of its schedule — after RetransTimer, in another goroutine, with
// the client apparently healthy until then. Refusing at the door makes it a
// caller error with no consequences instead.
func TestStartWithNoReportDoesNothing(t *testing.T) {
	p := &DADProbe{running: make(map[netip.Addr]*dadRun), done: make(chan struct{})}
	p.Start(netip.MustParseAddr("fd00:99::1a3"), nil)
	if st := p.Stats(); st.Started != 0 || st.Duplicate != 0 {
		t.Errorf("a Start with no report was counted: %+v", st)
	}
	if len(p.running) != 0 {
		t.Errorf("a Start with no report installed a run")
	}
}

// TestTheOwnFramePredicateIsLengthSafe drives the direction isOwn fails in.
//
// A frame whose link-layer source is not six octets cannot be shown to be this
// host's, and reporting it as own would silence a real duplicate — the failure
// that looks like success. The opposite mistake declines an address that is
// free, which is loud. NewNDSocket refuses such an interface outright, so this
// is the per-frame backstop rather than the only guard.
func TestTheOwnFramePredicateIsLengthSafe(t *testing.T) {
	s := &NDSocket{hw: cliMAC}
	for _, tc := range []struct {
		name string
		hw   net.HardwareAddr
		want bool
	}{
		{"this interface's own address", cliMAC, true},
		{"another node's address", srvMAC, false},
		{"a prefix of our own address", cliMAC[:5], false},
		{"our own address with a trailing octet", append(append(net.HardwareAddr{}, cliMAC...), 0), false},
		{"no address at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.isOwn(tc.hw); got != tc.want {
				t.Errorf("isOwn(%s) = %v, want %v", tc.hw, got, tc.want)
			}
		})
	}
}

// TestAProbeThatCouldNotSendDeclinesNothing is the third answer, driven.
//
// THE DEFECT IT PINS DOWN. A p.nd.Send that failed used to report
// duplicate=true, and every consequence of that is wrong in the same
// direction: ring 1 turns EvDADResult{Duplicate:true} into RFC 9915 section
// 18.2.10.1's DHCPDECLINE, dnsmasq takes the Decline at its word and puts the
// address out of service — ten minutes, measured, in this milestone's own log
// excerpt — and lease.Stats.DADConflicts records a conflict that did not
// happen. The evidence for a conflict is another node answering for the
// address, and what actually happened here is that nothing was asked.
//
// WHAT IT REPORTS INSTEAD IS NOTHING, and the silence is not a hang: ring 1
// armed proto.DefaultDADTimeout before ring 3 was ever called, so the
// acquisition fails with proto.ReasonDADIncomplete — whose own documentation
// names this case — and no message is sent to the server at all. The proto
// half is already driven by TestTheDADDeadlineIsAFaultAndNotAnAcquisition,
// which asserts both the reason and that no Decline goes out; this row is the
// ring-3 half, where the send fails.
//
// THE SOCKET IS A PIPE, which is what makes the send fail for a reason that is
// not this library's: sendto(2) on a descriptor that is not a socket is
// ENOTSOCK, from the kernel, on the same code path a real interface that went
// away takes. Setting NDSocket's own closed flag would drive the guard at the
// top of Send instead, which is this library refusing rather than the send
// failing.
func TestAProbeThatCouldNotSendDeclinesNothing(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	nd := &NDSocket{
		f:       w,
		hw:      cliMAC,
		inbound: make(chan lease.NDInbound, 1),
		frames:  make(chan NDFrame, 1),
	}
	p, err := NewDADProbe(nd)
	if err != nil {
		t.Fatalf("NewDADProbe: %v", err)
	}

	addr := netip.MustParseAddr("fd00:99::1a3")
	reports := make(chan bool, 1)
	p.Start(addr, func(_ netip.Addr, duplicate bool) { reports <- duplicate })

	// Close joins the probe's goroutine, so by here the schedule has run as
	// far as it is going to. No wall clock is consulted: the send fails on the
	// first solicitation, before the first RetransTimer wait.
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case duplicate := <-reports:
		t.Fatalf("the probe reported duplicate=%v for %s after failing to put a single Neighbor Solicitation on the wire; a Decline for an address nobody answered for takes it out of service at the server", duplicate, addr)
	default:
	}

	st := p.Stats()
	if st.SendFailures != 1 {
		t.Errorf("DADStats.SendFailures = %d, want 1: the failure has to be visible somewhere, or a probe that asked nothing is indistinguishable from one that was cancelled", st.SendFailures)
	}
	if st.Free != 0 || st.Duplicate != 0 {
		t.Errorf("DADStats Free=%d Duplicate=%d, want 0 and 0: neither verdict was earned", st.Free, st.Duplicate)
	}
	if st.Started != 1 {
		t.Errorf("DADStats.Started = %d, want 1", st.Started)
	}
	if st.Solicits != 0 {
		t.Errorf("DADStats.Solicits = %d; nothing left the socket", st.Solicits)
	}
}
