// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// route is one RFC 3442 descriptor: the mask width, the significant octets of the destination, then the router.
func route(width byte, dest []byte, router [4]byte) []byte {
	return append(append([]byte{width}, dest...), router[:]...)
}

// ackedLease drives the machine the plugin's buildParams makes through a DISCOVER, an OFFER, a REQUEST and an ACK that
// carries extra, and renders the lease the way the library's toLease does for the fields infoFromLease reads. toLease
// is unexported; Routes and Options are the two fields this proof depends on (#1030).
func ackedLease(t *testing.T, extra wire.Options) lease.Lease {
	t.Helper()
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
	ack := reply(req, wire.MsgAck, 3600)
	for code, v := range extra {
		ack.Options[code] = v
	}
	m.Step(instant(2), 3, received(t, ack))
	pl, ok := m.Lease()
	if !ok {
		t.Fatal("the machine holds no lease after the ACK")
	}
	l := lease.Lease{
		Addr:    pl.Addr,
		DNS:     pl.DNS,
		Routes:  pl.Routes,
		Options: pl.Options.Clone(),
	}
	if g, ok := pl.Gateway(); ok {
		l.Gateway = g
	}
	return l
}

func routesOf(t *testing.T, extra wire.Options) Info {
	t.Helper()
	info, _ := infoFromLease(ackedLease(t, extra), proto.RouterObservation{}, time.Now(), netip.Prefix{})
	return info
}

var (
	via7 = [4]byte{192, 168, 99, 7}
	via5 = [4]byte{192, 168, 99, 5}
	via9 = [4]byte{192, 168, 99, 9}
)

// A server that sends option 249 and no 121 gets its routes installed, and they supersede option 3 as 121's do: RFC 3442
// says the router option is ignored when classless routes are present, default route or not (#1030).
func TestInfoFromLease_Option249AloneYieldsRoutes(t *testing.T) {
	info := routesOf(t, wire.Options{wire.OptMSClasslessStaticRte: route(16, []byte{10, 77}, via7)})

	want := []Route{{Destination: "10.77.0.0/16", Gateway: "192.168.99.7"}}
	if !reflect.DeepEqual(info.Routes, want) {
		t.Errorf("Routes = %+v, want %+v from option 249 alone", info.Routes, want)
	}
	if info.Gateway != "" {
		t.Errorf("Gateway = %q, want none: option 249 supersedes the router option", info.Gateway)
	}
}

// A 0.0.0.0/0 entry in option 249 supersedes option 3 as it does in 121, and is never a second route (#899, #1030).
func TestInfoFromLease_Option249DefaultRouteSupersedesTheRouterOption(t *testing.T) {
	info := routesOf(t, wire.Options{wire.OptMSClasslessStaticRte: append(
		route(0, nil, via9), route(16, []byte{10, 77}, via7)...)})

	if info.Gateway != "192.168.99.9" {
		t.Errorf("Gateway = %q, want 192.168.99.9 from the 249 default route", info.Gateway)
	}
	want := []Route{{Destination: "10.77.0.0/16", Gateway: "192.168.99.7"}}
	if !reflect.DeepEqual(info.Routes, want) {
		t.Errorf("Routes = %+v, want %+v with no default route among them", info.Routes, want)
	}
}

// With both options present 121 wins and 249 is ignored (#1030).
func TestInfoFromLease_Option121WinsWhenBothArrive(t *testing.T) {
	info := routesOf(t, wire.Options{
		wire.OptClasslessStaticRte:   route(16, []byte{10, 1}, via5),
		wire.OptMSClasslessStaticRte: route(16, []byte{10, 2}, via7),
	})

	want := []Route{{Destination: "10.1.0.0/16", Gateway: "192.168.99.5"}}
	if !reflect.DeepEqual(info.Routes, want) {
		t.Errorf("Routes = %+v, want %+v: option 249 must not add to or replace option 121", info.Routes, want)
	}
}

// A 121 that is present and does not decode is not a reason to read 249 (#1030).
func TestInfoFromLease_MalformedOption121DoesNotFallBackTo249(t *testing.T) {
	info := routesOf(t, wire.Options{
		wire.OptClasslessStaticRte:   {24, 10},
		wire.OptMSClasslessStaticRte: route(16, []byte{10, 2}, via7),
	})

	if len(info.Routes) != 0 {
		t.Errorf("Routes = %+v, want none: a present 121 that fails to decode leaves 249 ignored", info.Routes)
	}
	if info.Gateway != testServer {
		t.Errorf("Gateway = %q, want option 3's %q: a 121 that does not decode supersedes nothing", info.Gateway, testServer)
	}
}
