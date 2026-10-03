// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"bytes"
	"encoding/base64"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	"github.com/claymore666/dhcp-golib/proto"
	"github.com/claymore666/dhcp-golib/wire"
)

// dhcp-golib v1.4.0 (#1177) adds Lease.OptionsV6 and Params6.IID with no plugin switch; #1033 reads the first and #1032
// moves the second, so each test below is the observer that change must edit.

// A build against v1.3.0 stops compiling here.
var _ wire.OptionsV6 = lease.Lease{}.OptionsV6

func TestBuildParams6_TheV140PinLeavesTheIdentifierAtModifiedEUI64(t *testing.T) {
	p, err := buildParams6(testOpts6(t), false)
	if err != nil {
		t.Fatalf("buildParams6: %v", err)
	}
	if p.IID != proto.IIDModeEUI64 {
		t.Errorf("IID is %v, want %v: the plugin chooses no stable-privacy identifier before #1032", p.IID, proto.IIDModeEUI64)
	}
	if len(p.IIDSecret) != 0 || len(p.IIDNetIface) != 0 || len(p.IIDNetworkID) != 0 {
		t.Errorf("identifier inputs are secret %x, interface %x, network %x, want all empty", p.IIDSecret, p.IIDNetIface, p.IIDNetworkID)
	}
}

// The plugin hands a v6 event to the lease record whole (#1137), so the Reply's options ride into the record under the
// pin; the library's own notes say a resumed lease carries them only from the next Reply.
func TestRecords6_TheReplyOptionsOfAV6LeaseReachTheRecord(t *testing.T) {
	r, path := testRecords(t)
	const network = "net-1"
	mac := []byte{0x02, 0x42, 0xac, 0x11, 0x00, 0x02}
	id6 := testIdentity6(t, "02:42:ac:11:00:02")
	now := time.Now()
	ntp := []byte{0, 1, 0, 16, 0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x7b}

	held := lease.Lease{
		Addr:       netip.MustParsePrefix("2001:db8::5/128"),
		Addrs:      []lease.Addr6{{Addr: netip.MustParsePrefix("2001:db8::5/128"), Preferred: now.Add(time.Hour), Valid: now.Add(2 * time.Hour)}},
		Acquired:   now,
		Renew:      now.Add(30 * time.Minute),
		Rebind:     now.Add(time.Hour),
		Expire:     now.Add(2 * time.Hour),
		ServerDUID: []byte{0, 3, 0, 1, 2, 2, 2, 2, 2, 2},
		OptionsV6:  wire.OptionsV6{{Code: wire.OptV6NTPServer, Data: ntp}},
	}
	if got, ok := held.OptionsV6.First(wire.OptV6NTPServer); !ok || !bytes.Equal(got, ntp) {
		t.Fatalf("the hand-built lease reads option 56 back as %x (found %v), want %x", got, ok, ntp)
	}
	if err := r.Created6("ep-v6", network, mac, id6.Bytes()); err != nil {
		t.Fatalf("Created6: %v", err)
	}
	if err := r.Bound("ep-v6"); err != nil {
		t.Fatalf("Bound: %v", err)
	}
	if err := r.Observed("ep-v6", lease.Event{Kind: lease.Acquired, Lease: held}, nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenRecords(path, "instance-b")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	_, res, _, ok := reopened.Resume6(network, mac, now.Add(time.Minute))
	if !ok || res.Lease == nil {
		t.Fatalf("the record did not resume after a reopen (ok=%v)", ok)
	}
	got, found := res.Lease.OptionsV6.First(wire.OptV6NTPServer)
	t.Logf("the record's offer after a reopen holds OptionsV6: %v", res.Lease.OptionsV6)
	if !found || !bytes.Equal(got, ntp) {
		t.Errorf("the resumed lease reads option 56 as %x (found %v), want %x", got, found, ntp)
	}
}

// translateV6EventIntoRecord returns the record text after one v6 Acquired went through translate (#1177).
func translateV6EventIntoRecord(t *testing.T, resume *lease.Lease, l lease.Lease) string {
	t.Helper()
	c, src, path := newTranslateHarness(t)
	c.opts.V6 = true
	c.opts.Resume = resume

	go c.translate()

	select {
	case src <- lease.Event{Kind: lease.Acquired, Lease: l}:
	case <-time.After(wedgeBudget):
		t.Fatalf("the event could not be handed to translate within %v", wedgeBudget)
	}
	select {
	case <-c.events:
	case <-time.After(wedgeBudget):
		t.Fatalf("translate emitted nothing within %v", wedgeBudget)
	}
	close(src)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the record file: %v", err)
	}
	return string(raw)
}

func TestTranslate_ARecordedV6EventKeepsTheReplyOptions(t *testing.T) {
	ntp := []byte{0, 1, 0, 16, 0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x7b}
	raw := translateV6EventIntoRecord(t, nil, lease.Lease{
		Addr:      netip.MustParsePrefix("fd00:6470:6865::61/128"),
		OptionsV6: wire.OptionsV6{{Code: wire.OptV6NTPServer, Data: ntp}},
	})
	if !strings.Contains(raw, `"options_v6"`) || !strings.Contains(raw, base64.StdEncoding.EncodeToString(ntp)) {
		t.Errorf("the record translate wrote for a v6 event carrying option 56 does not hold its bytes %x. "+
			"Record file:\n%s", ntp, raw)
	}
}

// The library's note for #53: a lease resumed from its record has no OptionsV6 until the next Reply, and the plugin
// copies none from the remembered lease (carryResumedConfig6 carries the resolver lists only).
func TestTranslate_AResumedV6EventDoesNotInventTheRememberedReplyOptions(t *testing.T) {
	raw := translateV6EventIntoRecord(t, &lease.Lease{
		OptionsV6: wire.OptionsV6{{Code: wire.OptV6NTPServer, Data: []byte{0, 1, 0, 16, 0x20, 0x01, 0x0d, 0xb8}}},
	}, lease.Lease{Addr: netip.MustParsePrefix("fd00:6470:6865::61/128")})
	if strings.Contains(raw, `"options_v6"`) {
		t.Errorf("the record translate wrote for a v6 event that carried no options holds an options_v6 key, "+
			"which only a Reply may fill. Record file:\n%s", raw)
	}
}
