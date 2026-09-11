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

// TestDeclinedAddressIsNotHinted pins the library contract the plugin's
// persistent DHCPv6 path now rests on: a machine does not ask for an
// address it knows has been declined.
//
// WHY THIS LIVES HERE AND NOT ONLY UPSTREAM. It is the remedy
// chassis6_declineloop_test.go handed to the library round, and the
// plugin took delivery of it by pinning the module. The persistent
// client is built once, with the preferred address as Params6.Hint, and
// nothing exported can clear that hint on a running client -- so if the
// machine ever re-hinted a declined address again, the endpoint would
// go back to declining once a second for as long as it lives, and every
// plugin-side test would stay green. A dependency contract this
// load-bearing gets a check on THIS side of the module boundary.
//
// THE ADDRESS IS READ OFF THE ENCODED MESSAGE, not off a getter: what
// costs the endpoint is an IA Address option on the wire, and a machine
// that cleared a field and built the option from somewhere else would
// satisfy a field assertion exactly.
//
// WHAT IT DOES NOT COVER, stated so nobody reads more into it: the set
// here is SEEDED through Params6.Declined rather than earned by driving
// a DAD conflict and a Decline exchange. Earning it needs the library's
// own test fakes, which are not exported; the library's suite does it
// (proto/machine6_declinehint_test.go). What this side can drive is the
// suppression rule itself, and that is the rule the plugin depends on.
func TestDeclinedAddressIsNotHinted(t *testing.T) {
	const preferred = "2001:db8::5"

	params, err := buildParams6(testOpts6(t), false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	params.Hint = netip.MustParseAddr(preferred)

	// The control first. With nothing declined the same machine MUST
	// carry the hint -- an endpoint keeps its address across a restart
	// because of it (#213) -- so a suppression that fired unconditionally
	// would be caught here rather than looking like a pass.
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

// solicitedAddr builds a machine from params, starts it, and returns the
// address its first Solicit asks for, or the zero Addr if it asks for
// none.
func solicitedAddr(t *testing.T, params proto.Params6) netip.Addr {
	t.Helper()

	m, err := proto.New6(params)
	if err != nil {
		t.Fatalf("proto.New6: %v", err)
	}
	// EvStart does not put a Solicit on the wire: RFC 9915 section
	// 18.2.1 has the client wait a random SOL_MAX_DELAY first, so
	// EvStart arms Timer6Delay and the Solicit is that timer's action.
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
