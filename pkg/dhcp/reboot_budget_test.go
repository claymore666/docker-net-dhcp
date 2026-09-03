// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net"
	"net/netip"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// The restart cost carried from M5, MEASURED rather than argued, and
// the decision it settles: THE CHASSIS DOES NOT BOUND IT.
//
// THE QUESTION. The plugin is restarted routinely — a plugin upgrade,
// a daemon restart, the operator's nightly recycle. Every endpoint's
// Join manager then resumes the address the record remembers, which
// RFC 2131 section 4.4.2 makes an INIT-REBOOT DHCPREQUEST rather than
// a DHCPDISCOVER. That is the whole of why an address survives a
// restart. But if the server is unreachable at that moment, the
// machine sits in REBOOTING for a full retransmission budget of
// silence before it gives up and DISCOVERs. How long is that window,
// and should the chassis shorten it?
//
// WHY IT CANNOT BE READ OFF THE CONSTANTS, and what reading them got
// wrong. The obvious arithmetic is Backoff.Initial doubling four times
// — 4+8+16+32 = 60s — plus the section 4.4.1 desync. Both halves are
// wrong, and the measurement is what said so:
//
//   - There is NO desync on this path. takeResume sends the
//     INIT-REBOOT DHCPREQUEST at EvStart itself; the desync window
//     belongs to beginAcquisition, which is the path NOT taken.
//   - The budget is 4+8+16+32+64 = 124s, not 60. The machine arms the
//     delay for the NEXT retransmission and only tests Exhausted when
//     that timer FIRES, so the final 64-second delay is waited out
//     with nothing sent on the wire. Half the window buys no packet.
//
// Each of the five delays is independently jittered by plus or minus
// one second, so the answer is a band and not a number; a figure
// written from the constants would be one point of a distribution.
//
// So this drives the pinned library's own state machine on a virtual
// clock over a spread of entropy and reports the band. It needs no
// socket, no fixture, no root and no CI: the machine is pure, and its
// clock is an int64 this test advances itself.
//
// THE DECISION, and why it is not "bound it". Shortening the window
// means abandoning a lease this endpoint still holds and asking for a
// new address from INIT. The address is what the container is USING;
// a DISCOVER can come back with a different one, and the container
// then has an address its neighbours, its DNS records and every
// long-lived connection through it do not. The failure this bounds
// against — a container waiting a minute longer for an address it does
// not have yet — is the one the ordinary CreateEndpoint path already
// covers with lease_timeout, on the call that actually blocks
// `docker run`. Join does not block: it spawns the manager and
// returns, so the whole of this window is spent by a container that
// already has its address configured, holding an unexpired lease.
//
// Priced, not waved away. The cost of NOT bounding it is that an
// endpoint whose lease has genuinely been reissued elsewhere carries
// on believing in it for the length of the band below. The cost of
// bounding it is a restart that changes addresses whenever the server
// is briefly unreachable — daily, on a fleet that restarts daily. The
// second is a worse failure and a more frequent one.
//
// This test is therefore an ASSERTION THAT NOTHING SHORTENS IT: it
// fails if the chassis ever starts trimming the Request backoff, the
// retransmission count or the Join manager's desync, which is exactly
// how the decision above would be reversed by accident.
func TestRebootBudget_TheChassisDoesNotShortenTheInitRebootWindow(t *testing.T) {
	mac, err := net.ParseMAC("02:42:c0:a8:63:07")
	if err != nil {
		t.Fatalf("parse MAC: %v", err)
	}

	// once=false is the JOIN manager: the persistent client that
	// resumes the record's lease. The CreateEndpoint one-shot (true)
	// is a different shape and is not what this measures.
	p, err := buildParams(&DHCPClientOptions{MAC: mac}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	// Resume is set here rather than by buildParams because the
	// conversion from the record's wall-clock deadline to the
	// machine's monotonic Instant is ring 2's, done once inside the
	// library at lease.Config.Resume. What reaches ring 1 is this.
	p.Resume = &proto.Resume{Addr: netip.MustParseAddr("192.168.99.7")}

	var lo, hi proto.Duration
	const draws = 64
	for i := 0; i < draws; i++ {
		got := measureRebootWindow(t, p, uint64(i)*0x9e3779b97f4a7c15+1)
		if i == 0 || got < lo {
			lo = got
		}
		if got > hi {
			hi = got
		}
	}

	t.Logf("MEASURED: resume -> first DHCPDISCOVER with a silent server: "+
		"%.2fs to %.2fs over %d entropy draws (pinned library, virtual clock)",
		float64(lo)/float64(proto.Second), float64(hi)/float64(proto.Second), draws)

	// The band, derived from the schedule the measurement above
	// exposed: five delays of 4, 8, 16, 32 and 64 seconds, each
	// jittered by +/-1s. 124s nominal, so 119s..129s.
	const (
		wantLo = (4 + 8 + 16 + 32 + 64 - 5) * proto.Second // 119s
		wantHi = (4 + 8 + 16 + 32 + 64 + 5) * proto.Second // 129s
	)
	if lo < wantLo || hi > wantHi {
		t.Errorf("the INIT-REBOOT window measured %.2fs..%.2fs, outside the schedule's "+
			"%.2fs..%.2fs. Something is trimming the Request backoff or its retransmission "+
			"count — which reverses the decision recorded above, that a restart must not "+
			"give up an address the container is still using.",
			float64(lo)/float64(proto.Second), float64(hi)/float64(proto.Second),
			float64(wantLo)/float64(proto.Second), float64(wantHi)/float64(proto.Second))
	}

	// The control. Without it every assertion above is satisfied by a
	// machine that never rebooted at all: a DISCOVER sent immediately
	// would be a SMALLER number, and "not shortened" is a one-sided
	// claim. So the same drive with no Resume must produce a
	// materially smaller window — the desync alone.
	var noResume proto.Params = p
	noResume.Resume = nil
	bare := measureRebootWindow(t, noResume, 1)
	if bare >= 11*proto.Second {
		t.Errorf("with no remembered lease the first DISCOVER took %.2fs, past the 1..10s "+
			"desync that is the whole of the INIT path's delay: this test cannot tell a "+
			"machine that rebooted from one that did not, and its measurement above "+
			"means nothing",
			float64(bare)/float64(proto.Second))
	}
	t.Logf("control: no remembered lease, first DHCPDISCOVER at %.2fs (the desync alone)",
		float64(bare)/float64(proto.Second))
}

// measureRebootWindow drives one machine from EvStart to its first
// DHCPDISCOVER against a server that answers nothing, and returns the
// simulated time that took.
//
// The server's silence is modelled by feeding back ONLY the timers the
// machine arms. Nothing else is injected, so the elapsed time is the
// machine's own schedule and not this harness's idea of one.
func measureRebootWindow(t *testing.T, p proto.Params, rnd uint64) proto.Duration {
	t.Helper()

	if p.Resume != nil {
		// Re-stamped per call: Resume.Expire is on the same clock this
		// harness drives, and a lease that expired before the walk
		// begins is refused at EvStart (section 4.3.2) — which would
		// measure the DISCOVER path while claiming to measure REBOOTING.
		r := *p.Resume
		r.Expire = proto.Instant(24 * proto.Hour)
		r.HasExpire = true
		p.Resume = &r
	}

	m, err := proto.New(p)
	if err != nil {
		t.Fatalf("proto.New: %v", err)
	}

	now := proto.Instant(0)
	ev := proto.Simple(proto.EvStart)
	// A generous ceiling on steps, not on time: the loop must end
	// because the machine reached a DISCOVER, and a machine that never
	// does must fail loudly rather than spin.
	for step := 0; step < 200; step++ {
		_, acts := m.Step(now, rnd+uint64(step), ev)

		var next proto.Duration
		var armed bool
		for _, a := range acts {
			switch a.Kind {
			case proto.ActSend:
				if ty, ok := a.Msg.Type(); ok && ty == wire.MsgDiscover {
					return proto.Duration(now)
				}
			case proto.ActSetTimer:
				next, armed = a.After, true
			}
		}
		if !armed {
			t.Fatalf("the machine armed no timer and sent no DISCOVER at %v; "+
				"this harness would idle forever", now)
		}
		now += proto.Instant(next)
		ev = proto.TimerFired(lastTimer(acts))
	}
	t.Fatal("no DHCPDISCOVER within 200 steps")
	return 0
}

func lastTimer(acts []proto.Action) proto.TimerID {
	var id proto.TimerID
	for _, a := range acts {
		if a.Kind == proto.ActSetTimer {
			id = a.Timer
		}
	}
	return id
}
