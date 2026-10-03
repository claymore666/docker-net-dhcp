// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"net/netip"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/wire"
)

func rememberedTZ() *lease.Lease {
	return &lease.Lease{OptionsV6: wire.OptionsV6{{Code: wire.OptV6PosixTimezone, Data: []byte("PST8PDT")}}}
}

func TestCarryResumedOptions6_FillsEveryLeaseEventWithAnEmptyBag(t *testing.T) {
	for _, k := range []lease.EventKind{lease.Acquired, lease.Renewed, lease.Changed} {
		o := &DHCPClientOptions{V6: true, Resume: rememberedTZ()}
		for i := range 2 {
			ev := lease.Event{Kind: k}
			o.carryResumedOptions6(&ev)
			if v, _ := ev.Lease.OptionsV6.First(wire.OptV6PosixTimezone); string(v) != "PST8PDT" {
				t.Errorf("%v event %d: option 41 = %q, want the remembered one until a Reply brings a bag", k, i, v)
			}
		}
	}
}

func TestCarryResumedOptions6_LeavesWhatItMustNotFill(t *testing.T) {
	cases := []struct {
		name string
		o    DHCPClientOptions
		ev   lease.Event
	}{
		{"a v4 client", DHCPClientOptions{Resume: rememberedTZ()}, lease.Event{Kind: lease.Acquired}},
		{"no remembered lease", DHCPClientOptions{V6: true}, lease.Event{Kind: lease.Acquired}},
		{"a Lost", DHCPClientOptions{V6: true, Resume: rememberedTZ()}, lease.Event{Kind: lease.Lost}},
		{"a Failed", DHCPClientOptions{V6: true, Resume: rememberedTZ()}, lease.Event{Kind: lease.Failed}},
	}
	for _, tc := range cases {
		tc.o.carryResumedOptions6(&tc.ev)
		if len(tc.ev.Lease.OptionsV6) != 0 {
			t.Errorf("%s: OptionsV6 = %v, want none", tc.name, tc.ev.Lease.OptionsV6)
		}
	}
}

func TestCarryResumedOptions6_DoesNotShareTheRememberedBytes(t *testing.T) {
	o := &DHCPClientOptions{V6: true, Resume: rememberedTZ()}
	ev := lease.Event{Kind: lease.Acquired}
	o.carryResumedOptions6(&ev)
	ev.Lease.OptionsV6[0].Data[0] = 'X'
	if v, _ := o.Resume.OptionsV6.First(wire.OptV6PosixTimezone); string(v) != "PST8PDT" {
		t.Errorf("the remembered option 41 became %q after the event's copy was written to", v)
	}
}

// The advertisement watch renders the library's lease, which has no options until a Reply (#1033).
func TestTakeAdvertChange_AConfirmedLeaseKeepsTheRememberedOptions(t *testing.T) {
	c := &DHCPClient{opts: DHCPClientOptions{V6: true, Resume: rememberedTZ()}}
	l := lease.Lease{Addr: netip.MustParsePrefix("fd00:6470:6865::61/128"), Gateway: addr(t, "fe80::1")}
	c.view = func() (lease.Lease, bool) { return l, true }

	c.takeAdvertChange(time.Now())
	l.Gateway = addr(t, "fe80::2")
	out, ok := c.takeAdvertChange(time.Now())
	if !ok {
		t.Fatal("a changed gateway was not reported")
	}
	if out.Data.PosixTimezone != "PST8PDT" {
		t.Errorf("the routeradvert render has posix_tz=%q, want the remembered option 41", out.Data.PosixTimezone)
	}
}
