package plugin

import (
	"net"
	"strings"
	"testing"

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
