// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// The persistent client carries Params6.Hint for life and nothing clears it, so a machine that re-hinted a declined
// address would decline once a second forever (#213). The set is seeded through Params6.Declined; the library's suite
// earns it through DAD (proto/machine6_declinehint_test.go).

func TestDeclinedAddressIsNotHinted(t *testing.T) {
	const preferred = "2001:db8::5"

	params, err := buildParams6(testOpts6(t), false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	params.Hint = netip.MustParseAddr(preferred)

	// With nothing declined the hint must be carried: it keeps an endpoint's address across a restart (#213).
	if got := solicitedAddr(t, params); got != params.Hint {
		t.Fatalf("with nothing declined the Solicit hints %v, want %v", got, params.Hint)
	}

	params.Declined = []netip.Addr{params.Hint}
	if got := solicitedAddr(t, params); got.IsValid() {
		t.Errorf("the Solicit hints %v after that address was declined, want no IA Address at all. "+
			"The persistent client would be offered the declined address again, decline again, "+
			"and never converge", got)
	}
}

// solicitedAddr returns the address a machine's first Solicit asks for, or the zero Addr.
func solicitedAddr(t *testing.T, params proto.Params6) netip.Addr {
	t.Helper()

	m, err := proto.New6(params)
	if err != nil {
		t.Fatalf("proto.New6: %v", err)
	}
	// RFC 9915 section 18.2.1: the client waits SOL_MAX_DELAY first, so EvStart arms Timer6Delay and the Solicit is its
	// action.
	if _, acts := m.Step(proto.Instant(0), 1, proto.Simple(proto.EvStart)); len(acts) == 0 {
		t.Fatal("EvStart produced no actions at all")
	}
	_, acts := m.Step(proto.Instant(time.Second), 2,
		proto.Event{Kind: proto.EvTimerFired, Timer: proto.Timer6Delay})

	var msg *wire.MessageV6
	for _, a := range acts {
		if a.Kind == proto.ActSendV6 {
			if msg != nil {
				t.Fatalf("the delay timer produced more than one SendV6; this test reads the first Solicit")
			}
			msg = a.MsgV6
		}
	}
	if msg == nil {
		t.Fatal("the delay timer produced no SendV6. There is no Solicit to read, and a " +
			"test that passed on that would be asserting nothing")
	}
	if msg.Type != wire.MsgSolicit {
		t.Fatalf("the first message is a %s, want a Solicit", msg.Type)
	}

	ias, err := msg.Options.IANAs()
	if err != nil || len(ias) != 1 {
		t.Fatalf("the Solicit's IA_NA options: %v (err %v); want exactly one", ias, err)
	}
	addrs, err := ias[0].Options.Addrs()
	if err != nil {
		t.Fatalf("the IA_NA's IA Address options: %v", err)
	}
	switch len(addrs) {
	case 0:
		return netip.Addr{}
	case 1:
		return addrs[0].Addr
	default:
		t.Fatalf("the Solicit hints %d addresses; this client asks for one IA_NA with one address", len(addrs))
		return netip.Addr{}
	}
}
