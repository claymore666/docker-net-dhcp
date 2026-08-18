// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// mustAddr builds the netlink.Addr shape the manager stores for a
// lease, so a test can put a manager in the "one-shot acquired an
// address" state without a DHCP server.
func mustAddr(t *testing.T, cidr string) *netlink.Addr {
	t.Helper()
	a, err := netlink.ParseAddr(cidr)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", cidr, err)
	}
	return a
}

// orphanManager is a manager in the state the #370 paths hand to the
// reclaim: Start never succeeded, but the CreateEndpoint one-shot left
// an address behind.
func orphanManager(t *testing.T, opts DHCPNetworkOptions, addr string) *dhcpManager {
	t.Helper()
	m := &dhcpManager{
		opts:    opts,
		joinReq: JoinRequest{EndpointID: "0123456789abcdef"},
	}
	if addr != "" {
		m.setLastIP(false, mustAddr(t, addr))
	}
	return m
}

// A manager that never got an address has nothing to hand back. It must
// not count a failure — the one-shot failing is already counted where
// it happens, and double-counting it here would make the orphan-release
// failure rate unreadable as a signal about releases.
func TestReleaseOrphanedLease_NoAddressIsNotAFailure(t *testing.T) {
	p := &Plugin{}
	m := orphanManager(t, DHCPNetworkOptions{Parent: "eth0"}, "")

	p.releaseOrphanedLease(m, m.joinReq.EndpointID)

	if got := p.orphanedLeaseReleaseFailures.Load(); got != 0 {
		t.Errorf("orphaned_lease_release_failures = %d, want 0", got)
	}
	if got := p.orphanedLeasesReleased.Load(); got != 0 {
		t.Errorf("orphaned_leases_released = %d, want 0", got)
	}
}

// A network with neither a parent NIC nor a bridge gives no path to put
// a release on the wire. That is a genuine failure to reclaim and must
// be counted, not swallowed: the lease stays held upstream either way,
// and the counter is the only thing that says so.
func TestReleaseOrphanedLease_NoAttachmentPathCountsFailure(t *testing.T) {
	p := &Plugin{}
	m := orphanManager(t, DHCPNetworkOptions{}, "192.168.99.95/24")

	p.releaseOrphanedLease(m, m.joinReq.EndpointID)

	if got := p.orphanedLeaseReleaseFailures.Load(); got != 1 {
		t.Errorf("orphaned_lease_release_failures = %d, want 1", got)
	}
	if got := p.orphanedLeasesReleased.Load(); got != 0 {
		t.Errorf("orphaned_leases_released = %d, want 0", got)
	}
}

// The double-release guard. Join's Start goroutine and a concurrent
// Leave can both decide the same manager is orphaned; a second
// DHCPRELEASE would be sent for an address the server has already
// freed and may have handed to somebody else by then.
//
// Asserted through the failure counter because this manager cannot
// reach the wire — one attempt, one count, however many callers ask.
func TestSpawnOrphanRelease_ReleasesAtMostOnce(t *testing.T) {
	p := &Plugin{}
	m := orphanManager(t, DHCPNetworkOptions{}, "192.168.99.95/24")

	for i := 0; i < 5; i++ {
		p.spawnOrphanRelease(m)
	}
	p.orphanReleases.Wait()

	if got := p.orphanedLeaseReleaseFailures.Load(); got != 1 {
		t.Errorf("release attempted %d times, want exactly 1", got)
	}
}

// A nil plugin is the unit-test shape of dhcpManager (managers built
// without withPlugin). Stop() calls through unconditionally, so this
// must be a no-op rather than a panic.
func TestSpawnOrphanRelease_NilPluginIsNoOp(t *testing.T) {
	var p *Plugin
	p.spawnOrphanRelease(orphanManager(t, DHCPNetworkOptions{}, "192.168.99.95/24"))
}

// The generated name has to fit the kernel's interface-name limit with
// room for the veth peer's trailing "p" — LinkAdd refuses anything
// longer, which would turn every bridge-mode reclaim into a failure.
func TestNewReleaseLinkName(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		name, err := newReleaseLinkName()
		if err != nil {
			t.Fatalf("newReleaseLinkName: %v", err)
		}
		if !strings.HasPrefix(name, "dh-rel-") {
			t.Errorf("name %q lacks the dh-rel- prefix", name)
		}
		if len(name)+1 > 15 {
			t.Errorf("name %q is %d chars; +1 for the veth peer suffix exceeds IFNAMSIZ-1", name, len(name))
		}
		seen[name] = true
	}
	// Not a strict uniqueness guarantee, but 100 collisions out of a
	// 16M space would mean the randomness is broken.
	if len(seen) < 95 {
		t.Errorf("only %d distinct names in 100 draws — suffix is not random", len(seen))
	}
}

// The synthetic MAC is only reached when the endpoint's own MAC is
// unknown; it must still be a usable unicast address or the release
// link will not come up.
func TestNewProbeMAC_IsLocallyAdministeredUnicast(t *testing.T) {
	mac, err := newProbeMAC()
	if err != nil {
		t.Fatalf("newProbeMAC: %v", err)
	}
	if len(mac) != 6 {
		t.Fatalf("MAC length = %d, want 6", len(mac))
	}
	if mac[0]&0x01 != 0 {
		t.Errorf("MAC %v has the multicast bit set", net.HardwareAddr(mac))
	}
	if mac[0]&0x02 == 0 {
		t.Errorf("MAC %v is not locally administered", net.HardwareAddr(mac))
	}
}

// The MAC the release link wears is the whole of #402: getting it wrong
// means the link cannot come up and the lease is never handed back.
func TestReleaseMACPlan(t *testing.T) {
	endpointMAC := net.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x05}
	synthMAC := net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}
	synth := func() (net.HardwareAddr, error) { return synthMAC, nil }

	t.Run("macvlan prefers the endpoint's own MAC, with a fallback", func(t *testing.T) {
		got, err := releaseMACPlan(DHCPNetworkOptions{Mode: "macvlan"}, endpointMAC, synth)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("want the real MAC then a fallback, got %d entries", len(got))
		}
		if !bytes.Equal(got[0], endpointMAC) {
			t.Errorf("first attempt = %v, want the endpoint's own MAC — a server keyed on "+
				"the hardware address will only honour a release carrying it", got[0])
		}
		if !bytes.Equal(got[1], synthMAC) {
			t.Errorf("fallback = %v, want the synthetic MAC", got[1])
		}
	})

	t.Run("ipvlan goes straight to a synthetic MAC", func(t *testing.T) {
		// An ipvlan child's recorded MAC IS the parent NIC's, and the
		// kernel's duplicate check tests the parent's own address. It
		// can never be free, so trying it first would burn the retry
		// window on every single release and fall back anyway.
		parentMAC := net.HardwareAddr{0x00, 0x1b, 0x21, 0x11, 0x22, 0x33}
		got, err := releaseMACPlan(DHCPNetworkOptions{Mode: "ipvlan"}, parentMAC, synth)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || !bytes.Equal(got[0], synthMAC) {
			t.Fatalf("want exactly one synthetic attempt, got %v", got)
		}
	})

	t.Run("nothing recorded still yields a usable plan", func(t *testing.T) {
		for _, recorded := range []net.HardwareAddr{nil, {}} {
			got, err := releaseMACPlan(DHCPNetworkOptions{Mode: "macvlan"}, recorded, synth)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || !bytes.Equal(got[0], synthMAC) {
				t.Errorf("recorded=%v: want one synthetic attempt, got %v", recorded, got)
			}
		}
	})

	t.Run("bridge behaves like macvlan", func(t *testing.T) {
		// Bridge mode is not affected by the duplicate-MAC rule — a
		// bridge port is not a macvlan child — but it records a MAC and
		// there is no reason to treat it differently.
		got, err := releaseMACPlan(DHCPNetworkOptions{Bridge: "br0"}, endpointMAC, synth)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || !bytes.Equal(got[0], endpointMAC) {
			t.Errorf("bridge plan = %v, want the endpoint MAC first", got)
		}
	})

	t.Run("a MAC generator failure is reported, not swallowed", func(t *testing.T) {
		boom := errors.New("no entropy")
		if _, err := releaseMACPlan(DHCPNetworkOptions{Mode: "macvlan"}, endpointMAC,
			func() (net.HardwareAddr, error) { return nil, boom }); !errors.Is(err, boom) {
			t.Errorf("want the generator's error, got %v", err)
		}
	})
}

// The address half of #402. Bringing the release link up contends for
// the MAC; putting the leased address on it contends for the address.
// Both are held by the endpoint's own link until DeleteEndpoint lands —
// observed one second after a failed assignment in CI.
func TestAddrAddAwaiting(t *testing.T) {
	link := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: "dh-rel-test"}}
	addr := &netlink.Addr{}

	swap := func(t *testing.T, fn func() error) *int {
		t.Helper()
		calls := 0
		prev := nlAddrAdd
		nlAddrAdd = func(netlink.Link, *netlink.Addr) error {
			calls++
			return fn()
		}
		t.Cleanup(func() { nlAddrAdd = prev })
		return &calls
	}

	t.Run("waits for the endpoint link to let the address go", func(t *testing.T) {
		var n int
		calls := swap(t, func() error {
			n++
			if n < 3 {
				return unix.EADDRINUSE
			}
			return nil
		})
		if err := addrAddAwaiting(context.Background(), link, addr, time.Second); err != nil {
			t.Fatalf("gave up on an address that became free: %v", err)
		}
		if *calls < 3 {
			t.Errorf("succeeded after %d attempts; the stub only frees on the 3rd", *calls)
		}
	})

	t.Run("gives up and explains", func(t *testing.T) {
		swap(t, func() error { return unix.EADDRINUSE })
		err := addrAddAwaiting(context.Background(), link, addr, 250*time.Millisecond)
		if err == nil {
			t.Fatal("an address that never frees must fail — the release has to be sourced " +
				"from the address being given back, so no other address would do")
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			t.Errorf("kernel reason lost: %v", err)
		}
		if !strings.Contains(err.Error(), "still held by the endpoint link this release replaces") {
			t.Errorf("error does not explain the wait: %v", err)
		}
	})

	t.Run("another error is not retried", func(t *testing.T) {
		boom := errors.New("network is down")
		calls := swap(t, func() error { return boom })
		if err := addrAddAwaiting(context.Background(), link, addr, time.Second); !errors.Is(err, boom) {
			t.Errorf("want the original error, got %v", err)
		}
		if *calls != 1 {
			t.Errorf("retried a non-EADDRINUSE error %d times", *calls)
		}
	})
}

// TestReleaseOrphanedLease_ReclaimsEveryNeverBoundFamily pins the
// dual-stack contract (#608): the reclaim owes one release per family
// whose persistent client never held its binding, and nothing for a
// family that did — that client released on its own way out, and a
// second release would tear down an address the server may already
// have handed to somebody else.
//
// Asserted through the failure counter, as the tests above are: this
// manager cannot reach the wire, so each family it decides to hand back
// costs exactly one count. Before #608 the v6 rows here read 0 — the v6
// half of every orphan was left held until it expired.
func TestReleaseOrphanedLease_ReclaimsEveryNeverBoundFamily(t *testing.T) {
	for _, tc := range []struct {
		name         string
		v4, v6       string
		boundV4      bool
		boundV6      bool
		wantAttempts int32
	}{
		{name: "dual stack, nothing bound: both owed", v4: "192.168.99.95/24", v6: "fd00::95/64", wantAttempts: 2},
		{name: "dual stack, v4 bound: only v6 owed", v4: "192.168.99.95/24", v6: "fd00::95/64", boundV4: true, wantAttempts: 1},
		{name: "dual stack, v6 bound: only v4 owed", v4: "192.168.99.95/24", v6: "fd00::95/64", boundV6: true, wantAttempts: 1},
		{name: "dual stack, both bound: nothing owed", v4: "192.168.99.95/24", v6: "fd00::95/64", boundV4: true, boundV6: true, wantAttempts: 0},
		{name: "v6 only acquired: v6 owed", v6: "fd00::95/64", wantAttempts: 1},
		{name: "v4 only acquired: unchanged", v4: "192.168.99.95/24", wantAttempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{}
			m := orphanManager(t, DHCPNetworkOptions{IPv6: tc.v6 != ""}, tc.v4)
			if tc.v6 != "" {
				m.setLastIP(true, mustAddr(t, tc.v6))
			}
			m.boundV4.Store(tc.boundV4)
			m.boundV6.Store(tc.boundV6)

			p.releaseOrphanedLease(m, m.joinReq.EndpointID)

			if got := p.orphanedLeaseReleaseFailures.Load(); got != tc.wantAttempts {
				t.Errorf("reclaim attempted %d release(s), want %d", got, tc.wantAttempts)
			}
			if got := p.orphanedLeasesReleased.Load(); got != 0 {
				t.Errorf("orphaned_leases_released = %d, want 0 — nothing here can reach the wire", got)
			}
		})
	}
}
