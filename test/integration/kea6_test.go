// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestKea6ServesAnAddress: a container on an ipv6_mode=dhcp bridge network whose DHCPv6 server is Kea holds an address
// from Kea's pool, Kea's lease file holds it as an IA_NA lease of the DUID the container's link implies, and the
// advertisements captured on the segment and on Kea's link come from the sender alone (#214).
func TestKea6ServesAnAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cli := dockerClientFor(t)

	// M=1 and a router lifetime make the segment a managed one with a default router; the prefix is on-link and not
	// autonomous, so no address on it comes from anywhere but the DHCPv6 server (#214).
	spec := harness.RASpec{
		Managed:        true,
		RouterLifetime: harness.AdvertRouteLifetime,
		Prefixes:       []harness.RAPrefix{advertPrefix(t, harness.V6SubnetV6CIDR, false, 1800, 1800)},
	}
	f, sender := startSenderSegment(t, harness.RangeArgsFor(harness.V6NoRA), spec)
	dumpOnFailure(t, f)
	kea := harness.NewKea6Fixture(t, f)
	t.Cleanup(func() {
		if t.Failed() {
			kea.DumpLogs(func(s string) { t.Log(s) })
		}
	})
	answerRouterSolicits(t, f.Bridge(), sender, spec)

	id, err := startOnV6SegmentWithOpts(t, ctx, cli, f, "dh-itest-kea6net",
		map[string]string{"ipv6_mode": "dhcp", "propagate_dns": ""})
	if err != nil {
		t.Fatalf("ContainerStart on the Kea segment: %v", err)
	}

	addr := linkGlobalV6(t, ctx, id, 30*time.Second)
	if addr == "" {
		t.Fatalf("the container holds no global IPv6 address 30s after it started; Kea's lease file:\n%s", kea.LeaseFileText())
	}
	got := netip.MustParseAddr(addr)
	lo, hi := netip.MustParseAddr(harness.Kea6PoolStart), netip.MustParseAddr(harness.Kea6PoolEnd)
	if got.Compare(lo) < 0 || got.Compare(hi) > 0 {
		t.Errorf("the container's address %s is outside Kea's pool %s-%s: something else served it", got, lo, hi)
	}

	iface := containerV6Iface(t, ctx, id, addr)
	macText := strings.TrimSpace(harness.ExecOutput(t, ctx, id, "cat", "/sys/class/net/"+iface+"/address"))
	mac, err := net.ParseMAC(macText)
	if err != nil {
		t.Fatalf("the container's link %s has MAC %q: %v", iface, macText, err)
	}
	duid := harness.DUIDLLFromMAC(mac)

	var held []harness.Kea6Row
	deadline := time.Now().Add(10 * time.Second)
	for {
		held = kea.HeldRows(duid, harness.Kea6LeaseNA)
		if keaHoldsAddr(held, got) || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !keaHoldsAddr(held, got) {
		t.Errorf("Kea holds no IA_NA lease of %s for DUID %s (the DUID-LL of the container's link %s, MAC %s); "+
			"held rows for that DUID: %+v\nKea's lease file:\n%s", got, duid, iface, mac, held, kea.LeaseFileText())
	}

	// The control comes first: a capture that heard no sender frame on Kea's link cannot show that Kea sent none.
	if _, ok := kea.RACapture().AwaitRAAfter(time.Time{}, 2*harness.RASenderInterval); !ok {
		t.Fatalf("no router advertisement reached the capture on Kea's link within %s", 2*harness.RASenderInterval)
	}
	if findings := harness.RouterAddressFindings(f.RACapture().Frames(), kea.RACapture().Frames(),
		kea.SegmentLinkLocals(), kea.ServerAddrs()); len(findings) > 0 {
		t.Errorf("the segment's only router address is not the sender's:\n  %s", strings.Join(findings, "\n  "))
	}
}

func keaHoldsAddr(rows []harness.Kea6Row, want netip.Addr) bool {
	for _, r := range rows {
		if a, err := netip.ParseAddr(r.Addr); err == nil && a == want {
			return true
		}
	}
	return false
}
