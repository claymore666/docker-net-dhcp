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

// RFC 2131 section 4.4.2: a restart resumes with an INIT-REBOOT DHCPREQUEST, and a silent server holds the machine in
// REBOOTING for 4+8+16+32+64 = 124s, each delay jittered by +/-1s, with no desync on this path (#899).
// Decision: the chassis does not shorten this window, because a DISCOVER can return a different address to a container
// that already uses its lease; D-1 zeroed the Join desync (#899).

func TestRebootBudget_TheChassisDoesNotShortenTheInitRebootWindow(t *testing.T) {
	mac, err := net.ParseMAC("02:42:c0:a8:63:07")
	if err != nil {
		t.Fatalf("parse MAC: %v", err)
	}

	p, err := buildParams(&DHCPClientOptions{MAC: mac}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
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

	// Five delays of 4, 8, 16, 32 and 64 seconds, each +/-1s: 124s nominal, 119s..129s (#899).
	const (
		wantLo = (4 + 8 + 16 + 32 + 64 - 5) * proto.Second
		wantHi = (4 + 8 + 16 + 32 + 64 + 5) * proto.Second
	)
	if lo < wantLo || hi > wantHi {
		t.Errorf("the INIT-REBOOT window measured %.2fs..%.2fs, outside the schedule's "+
			"%.2fs..%.2fs. Something is trimming the Request backoff or its retransmission "+
			"count — which reverses the decision recorded above, that a restart must not "+
			"give up an address the container is still using.",
			float64(lo)/float64(proto.Second), float64(hi)/float64(proto.Second),
			float64(wantLo)/float64(proto.Second), float64(wantHi)/float64(proto.Second))
	}

	// The control: with no Resume the window is exactly zero since D-1 (#899).
	var noResume proto.Params = p
	noResume.Resume = nil
	bare := measureRebootWindow(t, noResume, 1)
	if bare != 0 {
		t.Errorf("with no remembered lease the first DISCOVER took %.2fs. D-1 sets "+
			"DesyncMin/Max to zero on both managers, so a cold start sends in the same "+
			"step as EvStart; anything else is RFC 2131 4.4.1's fleet delay applied to "+
			"one container, and it also means this test cannot tell a machine that "+
			"rebooted from one that did not",
			float64(bare)/float64(proto.Second))
	}
	if bare >= wantLo {
		t.Errorf("the no-resume control took %.2fs, inside the %.2fs window this test "+
			"measures: the two paths are indistinguishable and the measurement above "+
			"means nothing",
			float64(bare)/float64(proto.Second), float64(wantLo)/float64(proto.Second))
	}
	t.Logf("control: no remembered lease, first DHCPDISCOVER at %.2fs",
		float64(bare)/float64(proto.Second))
}

// measureRebootWindow drives one machine from EvStart to its first DHCPDISCOVER against a silent server.
func measureRebootWindow(t *testing.T, p proto.Params, rnd uint64) proto.Duration {
	t.Helper()

	if p.Resume != nil {
		// RFC 2131 section 4.3.2: a lease expired before EvStart is refused, which would measure the DISCOVER path.
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
