// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"fmt"
	"net"
	"slices"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// firstDiscover drives a machine from EvStart to the first DHCPDISCOVER it sends.
func firstDiscover(t *testing.T, p proto.Params) *wire.Message {
	t.Helper()
	m, err := proto.New(p)
	if err != nil {
		t.Fatalf("proto.New: %v", err)
	}
	now := proto.Instant(0)
	ev := proto.Simple(proto.EvStart)
	for step := 0; step < 20; step++ {
		_, acts := m.Step(now, uint64(step)+1, ev)
		var next proto.Duration
		for _, a := range acts {
			switch a.Kind {
			case proto.ActSend:
				if mt, ok := a.Msg.Type(); ok && mt == wire.MsgDiscover {
					return a.Msg
				}
			case proto.ActSetTimer:
				next = a.After
			}
		}
		now += proto.Instant(next)
		ev = proto.TimerFired(lastTimer(acts))
	}
	t.Fatal("no DHCPDISCOVER within 20 steps")
	return nil
}

// Measured against dhcp-golib v1.2.0 (#1137, #1119): option 145 rides every Discover with no Params switch, because RFC 6704
// section 3.1.1 makes it a MUST; every other v1.2.0 option stays off until its Params field is set.

func TestBuildParams_TheDiscoverCarriesTheForcerenewNonceAndNothingElseNew(t *testing.T) {
	p, err := buildParams(&DHCPClientOptions{MAC: testMAC(t)}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	msg := firstDiscover(t, p)

	var codes []int
	for c := range msg.Options {
		codes = append(codes, int(c))
	}
	slices.Sort(codes)
	t.Logf("DHCPDISCOVER option codes: %v", codes)

	if got := msg.Options[wire.OptForcerenewNonce]; !bytes.Equal(got, wire.EncodeForcerenewNonceCapable()) {
		t.Errorf("option 145 = %x, want %x: the library's FORCERENEW authentication needs the server to hear it",
			got, wire.EncodeForcerenewNonceCapable())
	}
	for _, off := range []wire.OptionCode{wire.OptUserClass, wire.OptRapidCommit} {
		if _, there := msg.Options[off]; there {
			t.Errorf("option %d is on the DHCPDISCOVER of a client whose network did not ask for it (#1120, #1031)", off)
		}
	}
	if pl := msg.Options[wire.OptParameterList]; bytes.IndexByte(pl, byte(wire.OptIPv6OnlyPreferred)) >= 0 {
		t.Errorf("option 108 is in the parameter request list %v, but the plugin sets no IPv6OnlyPreferred (#1027)",
			fmt.Sprint(pl))
	}
}

// RFC 8925 section 3.2 forbids an IPv4-requiring host to ask for option 108, and every endpoint here needs an IPv4 lease
// (#1027); the test above reads a bare client, this one every option at once.
func TestBuildParams_NeverAsksForIPv6OnlyPreferred(t *testing.T) {
	p, err := buildParams(&DHCPClientOptions{
		MAC:         testMAC(t),
		RapidCommit: true,
		FQDN:        "register",
		Hostname:    "ctr",
		UserClass:   "class",
		VendorClass: "vendor",
		ClientID:    []byte{0x01, 0x02, 0x03},
		RequestedIP: "192.0.2.77",
	}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if p.IPv6OnlyPreferred {
		t.Error("Params.IPv6OnlyPreferred = true, want false")
	}
	if slices.Contains(p.ParameterList, wire.OptIPv6OnlyPreferred) {
		t.Errorf("ParameterList %v names option 108", p.ParameterList)
	}
	msg := firstDiscover(t, p)
	if bytes.IndexByte(msg.Options[wire.OptParameterList], byte(wire.OptIPv6OnlyPreferred)) >= 0 {
		t.Errorf("the first DISCOVER's option 55 %v names option 108", msg.Options[wire.OptParameterList])
	}
}

// With rapid_commit the first DISCOVER carries option 80 as an empty flag, the parameter request list does not name it,
// and a requested address (option 50) rides beside it (RFC 4039 section 3, #1031).
func TestBuildParams_RapidCommitPutsOption80OnTheDiscoverOnly(t *testing.T) {
	for _, requested := range []string{"", "192.0.2.77"} {
		p, err := buildParams(&DHCPClientOptions{MAC: testMAC(t), RapidCommit: true, RequestedIP: requested}, false)
		if err != nil {
			t.Fatalf("buildParams: %v", err)
		}
		msg := firstDiscover(t, p)

		if v, there := msg.Options[wire.OptRapidCommit]; !there || len(v) != 0 {
			t.Errorf("requested %q: option 80 = %x (present %v), want present and empty on the DISCOVER", requested, v, there)
		}
		if pl := msg.Options[wire.OptParameterList]; bytes.IndexByte(pl, byte(wire.OptRapidCommit)) >= 0 {
			t.Errorf("requested %q: option 80 is in the parameter request list %v", requested, fmt.Sprint(pl))
		}
		got, there := msg.Options[wire.OptRequestedIP]
		if (requested != "") != there || (there && !slices.Equal(got, net.ParseIP(requested).To4())) {
			t.Errorf("requested %q: option 50 = %v (present %v), want the requested address beside option 80", requested, got, there)
		}
	}
}
