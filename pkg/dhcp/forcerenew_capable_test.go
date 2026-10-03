// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// RFC 6704 section 3.1.1: the client says it can authenticate a FORCERENEW on every DISCOVER and REQUEST (#1119). The
// params struct has no switch, so the proof reads the bytes the plugin's client puts on the wire.
func TestBuildParams_TheEncodedDiscoverAndRequestCarryTheForcerenewNonce(t *testing.T) {
	p, err := buildParams(&DHCPClientOptions{MAC: ourMAC}, false)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	p.Conflict = proto.ConflictOff
	m, err := proto.New(p)
	if err != nil {
		t.Fatalf("proto.New: %v", err)
	}
	_, acts := m.Step(0, 1, proto.Simple(proto.EvStart))
	disc := mustSend(t, acts)
	_, acts = m.Step(instant(1), 2, received(t, reply(disc, wire.MsgOffer, 3600)))
	req := mustSend(t, acts)

	for name, msg := range map[string]*wire.Message{"DHCPDISCOVER": disc, "DHCPREQUEST": req} {
		raw, err := wire.Encode(msg)
		if err != nil {
			t.Fatalf("%s: wire.Encode: %v", name, err)
		}
		dec, err := wire.Decode(raw)
		if err != nil {
			t.Fatalf("%s: wire.Decode: %v", name, err)
		}
		if got := dec.Options[wire.OptForcerenewNonce]; !bytes.Equal(got, wire.EncodeForcerenewNonceCapable()) {
			t.Errorf("%s option 145 on the wire = %x, want %x", name, got, wire.EncodeForcerenewNonceCapable())
		}
	}
}
