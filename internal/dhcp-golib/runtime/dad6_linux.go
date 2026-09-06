//go:build linux

package runtime

import (
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// DADStats is what a DADProbe has done and seen.
type DADStats struct {
	// Present says there IS a probe behind these numbers, for ARPStats.Present's
	// reason: a client built without one reports the zero value, and a zero
	// Started would otherwise be indistinguishable from "nothing was ever
	// checked because nothing was ever asked".
	Present bool
	// Started counts addresses handed to Start.
	Started uint64
	// Solicits counts Neighbor Solicitations sent — DupAddrDetectTransmits per
	// address, so it is Started × that constant unless a probe was cancelled
	// mid-flight.
	Solicits uint64
	// Free and Duplicate count the verdicts reported. Their sum is Started
	// minus the probes Close or a Restart cut short and minus SendFailures,
	// which is the arithmetic that makes a probe that silently stopped
	// answering visible.
	Free      uint64
	Duplicate uint64
	// SendFailures counts probes that reported NOTHING because the
	// solicitation could not be put on the wire at all — the socket was
	// closed under the probe, or the interface went away between the Reply
	// and the check.
	//
	// IT IS A COUNTER AND NOT A VERDICT, and that is the whole point of the
	// field. RFC 4862 section 5.4 has three outcomes and a send failure is
	// none of them: the address is not free, because nothing was asked, and
	// it is not a duplicate, because nobody claimed it. Reporting a duplicate
	// would make ring 1 send a DHCPDECLINE (RFC 9915 section 18.2.10.1) for
	// an address no node on the link has answered for, and a server that
	// takes a Decline at its word puts that address out of service — dnsmasq
	// for ten minutes, measured. So the probe says nothing, ring 1's own
	// deadline fires, and the acquisition fails with proto.ReasonDADIncomplete,
	// whose documentation already names exactly this case: "this host's own
	// machinery not reporting, which is a local fault and is followed by
	// nothing being said to the server at all".
	//
	// THE COST IS proto.DefaultDADTimeout OF WAITING, said rather than
	// implied. A send that fails is known here immediately and the client
	// still sits out the deadline before it can retry. Reporting the failure
	// through the callback would save those seconds, and it would need a
	// third value in a port whose whole signature is (address, duplicate) —
	// which is a ring-1 change to save four seconds on a fault that ends in
	// a retry either way.
	SendFailures uint64
	// OwnIgnored counts Neighbor Discovery frames for a tentative address that
	// carried THIS interface's link-layer source: RFC 4862 section 5.4.3's
	// "If the solicitation is from the node itself (because the node loops
	// back multicast packets), the solicitation does not indicate the presence
	// of a duplicate address."
	//
	// IT IS ZERO ON EVERY RUN AGAINST THIS SOCKET, AND THAT IS WHY IT IS
	// PUBLISHED. MEASURED in this milestone: a packet socket bound to
	// ETH_P_IPV6 does not read back this host's own transmissions at all, so
	// the exclusion never fires on this link and the netns proofs assert
	// OwnIgnored is ZERO — the opposite of the assertion that would have been
	// written from the assumption that it does. NDSocket's BOUNDS carry the
	// measurement and the two reasons the exclusion is kept regardless. What
	// drives the branch is the unit test that hands inspect a frame wearing
	// this interface's own address; a real run in which this number moved
	// would mean something on the link is echoing, which is worth knowing.
	OwnIgnored uint64
	// ForeignSolicits counts another node's duplicate-address-detection
	// solicitation for an address this client is probing — section 5.4.3's
	// "If the solicitation is from another node, the tentative address is a
	// duplicate and should not be used (by either node)."
	ForeignSolicits uint64
	// ResolvingSolicits counts solicitations for a tentative address that came
	// from a UNICAST source: section 5.4.3's "the solicitation's sender is
	// performing address resolution on the target; the solicitation should be
	// silently ignored". Counted rather than silent, because "nobody asked"
	// and "somebody asked and we correctly did not treat it as a duplicate"
	// are the two readings of a clean run and only one of them is evidence.
	ResolvingSolicits uint64
	// Adverts counts another node's Neighbor Advertisement for an address this
	// client is probing — section 5.4.4's "If the target address is tentative,
	// the tentative address is not unique."
	Adverts uint64
	// Restarted counts Start calls that displaced a probe already running for
	// the same address.
	Restarted uint64
	// JoinFailures counts solicited-node multicast joins that failed. Above
	// zero the probe still ran, and said so; see joinSolicitedNode.
	JoinFailures uint64
}

// DADProbe performs RFC 4862 section 5.4's duplicate address detection on the
// link, and is the other half of proto.ActStartDAD.
//
// WHAT IT IS FOR. The machine asks for an address to be checked and waits for
// exactly one answer per address; lease.Manager's ActStartDAD arm counts the
// request and journals it, and lease.Config.DAD is where this runner is
// plugged in so that something actually performs the exchange. Without it a
// client acquires, binds and looks healthy having skipped the one check RFC
// 9915 section 18.2.10.1 makes a MUST.
//
// HOW IT DECIDES, in the RFC's own words.
//
//   - It sends. Section 5.4.2: "To check an address, a node sends
//     DupAddrDetectTransmits Neighbor Solicitations, each separated by
//     RetransTimer milliseconds.  The solicitation's Target Address is set to
//     the address being checked, the IP source is set to the unspecified
//     address, and the IP destination is set to the solicited-node multicast
//     address of the target address." All three of those fields are set inside
//     wire.EncodeDADNeighborSolicit, where this runner cannot get them wrong.
//   - A Neighbor Advertisement for the target means taken. Section 5.4.4: "If
//     the target address is tentative, the tentative address is not unique."
//   - Another node's DAD solicitation for the target means taken. Section
//     5.4.3: "If the source address of the Neighbor Solicitation is the
//     unspecified address, the solicitation is from a node performing
//     Duplicate Address Detection.  If the solicitation is from another node,
//     the tentative address is a duplicate and should not be used (by either
//     node)."
//   - A solicitation from a UNICAST source is not evidence. Same section: "If
//     the target address is tentative, and the source address is a unicast
//     address, the solicitation's sender is performing address resolution on
//     the target; the solicitation should be silently ignored."
//   - Silence through the last interval means free. Section 5.4.2's send
//     schedule plus RFC 7527 section 4.1's restatement of the wait: "If the
//     interface does not receive any DAD failure indications within
//     RetransTimer milliseconds (see [RFC4861]) after having sent
//     DupAddrDetectTransmits Neighbor Solicitations, the interface moves the
//     Target Address to the assigned state."
//
// HOW IT TELLS ITS OWN FRAME FROM SOMEBODY ELSE'S, and why the choice matters
// more than it looks. Section 5.4.3 excludes the loopback case — "If the
// solicitation is from the node itself (because the node loops back multicast
// packets), the solicitation does not indicate the presence of a duplicate
// address" — and gives no mechanism. There are two.
//
// The first is RFC 7527 section 4.1's nonce: "the sender MUST generate a
// random nonce associated with the interface address, MUST store the nonce
// internally, and MUST include the nonce in the Nonce option included in the
// NS(DAD)." THE NONCE IS NOT WHAT THIS RUNNER USES, and the reason is that it
// answers a narrower question than the one being asked. A nonce identifies
// this PROBE; the exclusion needs to identify this NODE. Two probes of the
// same address from this same client — a Start that displaced an earlier one,
// or a second acquisition after a Decline — carry different nonces, so a frame
// from the first would read as another node to the second. Section 5.4.3's own
// wording is "from the node itself", and the link-layer source address is what
// says that.
//
// The second, and the one used, is the LINK-LAYER SOURCE. It is read off the
// packet socket's sockaddr (NDFrame.SenderHW) and compared against this
// interface's own address by NDSocket's single isOwn predicate — one
// predicate, so the port and this runner cannot disagree about whose frame it
// is. It also covers a case the kernel's own PACKET_OUTGOING marking does not:
// a repeater or hairpinning switch that echoes this host's frame back arrives
// as an INCOMING frame with this host's address on it.
//
// AND ON THIS SOCKET IT NEVER FIRES, which is measured and does not make it
// dead code. NDSocket's BOUNDS carry the measurement: a socket bound to
// ETH_P_IPV6 reads none of this host's own frames. Section 5.4.3 anticipates
// exactly that configuration in its second duplicate test — "If the actual
// number of Neighbor Solicitations received exceeds the number expected based
// on the loopback semantics (e.g., the interface does not loop back the
// packet, yet one or more solicitations was received), the tentative address
// is a duplicate" — so on this link every solicitation for the target that
// arrives is another node's, and the exclusion is what keeps that reading
// correct on a link where the premise does not hold.
//
// BOUNDS, and the first of them is the one to read.
//
//   - THIS LIBRARY PROBES AN ADDRESS THE CALLER HAS NOT INSTALLED, and RFC
//     4862 is written for a node that has. Section 5.4 calls such an address
//     tentative and requires that "the interface must accept Neighbor
//     Solicitation and Advertisement messages containing the tentative address
//     in the Target Address field" — which the kernel does for ITS OWN
//     tentative addresses and does not do for one this library invented,
//     because the address is on no interface. Two things follow and both are
//     real. Nothing here answers a Neighbor Solicitation for the address,
//     which is correct — section 5.4 also says "a node MUST NOT respond to a
//     Neighbor Solicitation for a tentative address" — but it stays true after
//     the probe reports the address free, because installing it is the
//     caller's and this library does not install addresses. And a duplicate
//     that appears AFTER the probe window is not seen: the answer is a
//     snapshot of RetransTimer milliseconds, exactly as section 5.4's is, and
//     "the method for detecting duplicates is not completely reliable" is the
//     RFC's own sentence about it. lease.Manager.ReportAddressLost is how a
//     caller reports what it learns later; producing that report is the
//     caller's and is not built here.
//   - RFC 7527's EXTENDED PROBING IS NOT IMPLEMENTED, and it is not needed for
//     the reason it exists. Section 4.1 continues "If any probe is looped back
//     within RetransTimer milliseconds after having sent
//     DupAddrDetectTransmits NS(DAD) messages, the interface continues with
//     another MAX_MULTICAST_SOLICIT number of NS(DAD) messages" — that is the
//     remedy for a loop that would otherwise be read as a duplicate. This
//     runner does not read a loop as a duplicate at all; it identifies the
//     frame as its own by its link-layer source and ignores it, so there is
//     nothing to disambiguate with further probes. The consequence is that
//     the whole exchange fits inside one RetransTimer wait and well inside
//     ring 1's proto.DefaultDADTimeout, which is sized for the extended case.
//   - THE SOLICITED-NODE GROUP IS JOINED ON A SEPARATE SOCKET. See
//     joinSolicitedNode: the join is section 5.4.2's MUST, and a packet socket
//     cannot express it.
type DADProbe struct {
	nd *NDSocket

	mu      sync.Mutex
	running map[netip.Addr]*dadRun
	closed  bool

	started           atomic.Uint64
	solicits          atomic.Uint64
	free              atomic.Uint64
	duplicate         atomic.Uint64
	sendFailures      atomic.Uint64
	ownIgnored        atomic.Uint64
	foreignSolicits   atomic.Uint64
	resolvingSolicits atomic.Uint64
	adverts           atomic.Uint64
	restarted         atomic.Uint64
	joinFailures      atomic.Uint64

	done chan struct{}
	wg   sync.WaitGroup
}

// dadRun is one address in flight.
type dadRun struct {
	// cancel stops the probe's goroutine; the goroutine reports nothing after
	// it is closed, which is what makes a displaced or closed probe silent
	// rather than late.
	cancel chan struct{}
	// verdict carries a duplicate finding from the frame reader to the
	// probe's own goroutine. Buffered by one and written under the probe's
	// lock exactly once, so a second finding for the same address cannot
	// block the reader.
	verdict chan struct{}
	once    sync.Once
	report  func(netip.Addr, bool)
	leave   func()
}

// NewDADProbe builds a duplicate-address-detection runner over an open ND
// socket.
//
// IT DOES NOT OWN THE SOCKET and does not close it: NewClient6 opens every
// socket in one namespace and closes them together, and a runner that closed a
// socket the lease.ND port is still reading from would take the manager's
// Router Advertisements with it.
//
// IT MUST BE THE ONLY CONSUMER OF nd.Frames(). NDSocket delivers every
// validated frame there and drops what nobody drains (NDStats.Dropped), so a
// second reader would take frames this runner needs — and a missed
// advertisement is an address reported free that is not.
func NewDADProbe(nd *NDSocket) (*DADProbe, error) {
	if nd == nil {
		return nil, fmt.Errorf("runtime: duplicate address detection needs an ND socket")
	}
	p := &DADProbe{
		nd:      nd,
		running: make(map[netip.Addr]*dadRun),
		done:    make(chan struct{}),
	}
	p.wg.Add(1)
	go p.watch()
	return p, nil
}

// Start begins RFC 4862 section 5.4's exchange for addr and calls report
// exactly once with the verdict.
//
// It returns immediately: ring 2 calls it from the manager's own goroutine
// while stepping the machine, and blocking there for RetransTimer would stop
// the manager answering anything — including the Advertise this very address
// came out of.
//
// report IS CALLED FROM ANOTHER GOROUTINE, and it is a callback rather than a
// back-reference to the manager on purpose: a runner holding the manager and a
// manager holding the runner is a construction cycle, and the one place that
// would have to break it is the one place a test cannot reach.
//
// A SECOND Start FOR AN ADDRESS ALREADY IN FLIGHT DISPLACES THE FIRST, whose
// report is then never called. The newest request is the one the machine is
// waiting on — a Decline and a fresh acquisition can name the same address —
// and two reports for one address would answer a question ring 1 asked once.
// Counted as Restarted.
func (p *DADProbe) Start(addr netip.Addr, report func(netip.Addr, bool)) {
	if report == nil {
		return
	}
	solicit, err := wire.EncodeDADNeighborSolicit(addr)
	if err != nil {
		// An address the codec refuses is not one to probe, and reporting it
		// free would be the worst of the three possible answers. Reported as a
		// duplicate: the machine's arm for that is "do not use this address",
		// which is what a malformed target deserves.
		p.started.Add(1)
		p.duplicate.Add(1)
		report(addr, true)
		return
	}

	run := &dadRun{
		cancel:  make(chan struct{}),
		verdict: make(chan struct{}, 1),
		report:  report,
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if old, ok := p.running[addr]; ok {
		close(old.cancel)
		p.restarted.Add(1)
	}
	p.running[addr] = run
	p.mu.Unlock()

	p.started.Add(1)
	run.leave = p.joinSolicitedNode(addr)
	p.wg.Add(1)
	go p.probe(addr, solicit, run)
}

// Close stops every probe in flight and the frame reader. Safe to call more
// than once. Probes cut short report nothing, which keeps Free+Duplicate an
// honest count of the questions actually answered.
func (p *DADProbe) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	for _, run := range p.running {
		close(run.cancel)
	}
	p.running = make(map[netip.Addr]*dadRun)
	close(p.done)
	p.mu.Unlock()
	p.wg.Wait()
	return nil
}

// Stats reports what the probe has done and seen.
func (p *DADProbe) Stats() DADStats {
	return DADStats{
		Present:           true,
		Started:           p.started.Load(),
		Solicits:          p.solicits.Load(),
		Free:              p.free.Load(),
		Duplicate:         p.duplicate.Load(),
		SendFailures:      p.sendFailures.Load(),
		OwnIgnored:        p.ownIgnored.Load(),
		ForeignSolicits:   p.foreignSolicits.Load(),
		ResolvingSolicits: p.resolvingSolicits.Load(),
		Adverts:           p.adverts.Load(),
		Restarted:         p.restarted.Load(),
		JoinFailures:      p.joinFailures.Load(),
	}
}

// probe runs one address's schedule: DupAddrDetectTransmits solicitations
// RetransTimer apart, then one more RetransTimer of listening.
//
// THE LISTENING STARTS BEFORE THE FIRST SOLICITATION, because the map entry is
// installed by Start before this goroutine exists. Section 5.4.3's first
// duplicate test is "If a Neighbor Solicitation for a tentative address is
// received BEFORE one is sent, the tentative address is a duplicate" — a
// reader armed only after the send would miss exactly the case that sentence
// names.
func (p *DADProbe) probe(addr netip.Addr, solicit wire.ICMPv6Packet, run *dadRun) {
	defer p.wg.Done()
	defer p.finish(addr, run)

	// THE SCHEDULE IS proto's CONSTANTS AND NOT A KNOB ON THIS TYPE. RFC 4862
	// section 5.1 makes DupAddrDetectTransmits and RetransTimer per-interface
	// variables, and ring 1 has already derived proto.DefaultDADTimeout — the
	// deadline it will fault this probe on — from exactly these two. A second
	// copy here that a caller could set would let the probe outlive the
	// deadline that is waiting for it, and the fault would name ring 3's
	// silence rather than the mismatch that caused it.
	const transmits = proto.DupAddrDetectTransmits
	interval := time.Duration(proto.RetransTimer)

	for i := 0; i < transmits; i++ {
		if err := p.nd.Send(solicit); err != nil {
			// A SEND THAT FAILED IS NEITHER OF THE TWO VERDICTS. Reporting
			// the address free on the strength of a solicitation that never
			// left would be the vacuous pass this whole runner exists to
			// prevent; reporting it a duplicate — which this line did, until
			// the third answer was written down — declines an address nothing
			// on the link has claimed. So it reports nothing, and ring 1's
			// deadline turns the silence into proto.ReasonDADIncomplete. See
			// DADStats.SendFailures for the argument and the cost.
			p.sendFailures.Add(1)
			return
		}
		p.solicits.Add(1)
		if !p.wait(addr, run, interval) {
			return
		}
	}
	// The loop already waited one interval after the last solicitation, which
	// is RFC 7527 section 4.1's "within RetransTimer milliseconds ... after
	// having sent DupAddrDetectTransmits Neighbor Solicitations". Reaching
	// here is silence through it.
	p.deliver(addr, run, false)
}

// wait blocks for d, returning false if the probe was answered or cancelled
// first. A duplicate found mid-schedule stops the remaining solicitations:
// there is nothing left to learn and section 5.4.5's address "cannot be
// assigned to the interface" either way.
func (p *DADProbe) wait(addr netip.Addr, run *dadRun, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-run.cancel:
		return false
	case <-run.verdict:
		p.deliver(addr, run, true)
		return false
	case <-t.C:
		return true
	}
}

// deliver reports one verdict, at most once per run.
func (p *DADProbe) deliver(addr netip.Addr, run *dadRun, duplicate bool) {
	run.once.Do(func() {
		if duplicate {
			p.duplicate.Add(1)
		} else {
			p.free.Add(1)
		}
		run.report(addr, duplicate)
	})
}

// finish removes the run and leaves the multicast group.
func (p *DADProbe) finish(addr netip.Addr, run *dadRun) {
	if run.leave != nil {
		run.leave()
	}
	p.mu.Lock()
	if cur, ok := p.running[addr]; ok && cur == run {
		delete(p.running, addr)
	}
	p.mu.Unlock()
}

// watch reads every validated Neighbor Discovery frame and routes the ones
// naming an address currently being probed.
func (p *DADProbe) watch() {
	defer p.wg.Done()
	frames := p.nd.Frames()
	for {
		select {
		case <-p.done:
			return
		case f, ok := <-frames:
			if !ok {
				return
			}
			p.inspect(f)
		}
	}
}

// inspect applies RFC 4862 sections 5.4.3 and 5.4.4 to one frame.
func (p *DADProbe) inspect(f NDFrame) {
	var target netip.Addr
	switch f.Type {
	case wire.ICMPv6NeighborAdvert:
		na, err := wire.DecodeNeighborAdvert(f.Body)
		if err != nil {
			return
		}
		target = na.Target
	case wire.ICMPv6NeighborSolicit:
		ns, err := wire.DecodeNeighborSolicit(f.Body)
		if err != nil {
			return
		}
		target = ns.Target
	default:
		return
	}

	p.mu.Lock()
	run, probing := p.running[target]
	p.mu.Unlock()
	if !probing {
		return
	}

	// The own-frame test comes FIRST and before any per-type counter, so that
	// this host's own solicitation lands on exactly one number and can never
	// reach a duplicate arm. See DADProbe's note on the link-layer source.
	if p.nd.isOwn(f.SenderHW) {
		p.ownIgnored.Add(1)
		return
	}

	if f.Type == wire.ICMPv6NeighborSolicit && !f.Src.IsUnspecified() {
		// Section 5.4.3: address resolution, not duplicate address detection.
		p.resolvingSolicits.Add(1)
		return
	}
	if f.Type == wire.ICMPv6NeighborSolicit {
		p.foreignSolicits.Add(1)
	} else {
		p.adverts.Add(1)
	}
	select {
	case run.verdict <- struct{}{}:
	default:
	}
}

// joinSolicitedNode joins the solicited-node multicast group of addr and
// returns the function that leaves it.
//
// SECTION 5.4.2 MAKES THIS A MUST: "Before sending a Neighbor Solicitation, an
// interface MUST join the all-nodes multicast address and the solicited-node
// multicast address of the tentative address. The former ensures that the node
// receives Neighbor Advertisements from other nodes already using the address;
// the latter ensures that two nodes attempting to use the same address
// simultaneously should detect each other's presence." The all-nodes group is
// not joined here because the kernel joins it for every IPv6 interface on
// (re)initialisation; the solicited-node group of an address NO INTERFACE
// HOLDS is joined by nobody, so this runner joins it.
//
// ON A SEPARATE SOCKET, because a packet socket has no way to say it. The join
// is an IPPROTO_IPV6 socket option and AF_PACKET does not carry it; the
// datagram socket opened here exists only to hold the membership and to make
// the kernel emit the MLD report section 5.4.2 requires for MLD-snooping
// switches. Nothing is ever sent or received on it.
//
// A FAILED JOIN DOES NOT STOP THE PROBE, and it is counted rather than fatal.
// MEASURED in this milestone: on a veth pair in an unprivileged network
// namespace, the AF_PACKET socket sees the solicited-node traffic with or
// without the membership, because a veth delivers every frame written to its
// peer and there is no hardware multicast filter to program. So the join is
// what makes this correct on hardware, and refusing to probe when it fails
// would turn a working case into a failure on every kernel that declines the
// option — while a probe that ran WITHOUT the join on hardware that needed it
// could report an address free having heard nothing, which is why the failure
// is a counter somebody can read rather than a silence.
func (p *DADProbe) joinSolicitedNode(addr netip.Addr) func() {
	group, err := wire.SolicitedNodeMulticast(addr)
	if err != nil {
		p.joinFailures.Add(1)
		return nil
	}
	fd, err := syscall.Socket(syscall.AF_INET6,
		syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		p.joinFailures.Add(1)
		return nil
	}
	mreq := &syscall.IPv6Mreq{Interface: uint32(p.nd.ifIndex)}
	mreq.Multiaddr = group.As16()
	if err := syscall.SetsockoptIPv6Mreq(fd, syscall.IPPROTO_IPV6, syscall.IPV6_JOIN_GROUP, mreq); err != nil {
		p.joinFailures.Add(1)
		_ = syscall.Close(fd)
		return nil
	}
	// Closing the socket drops the membership, which is the leave.
	return func() { _ = syscall.Close(fd) }
}
