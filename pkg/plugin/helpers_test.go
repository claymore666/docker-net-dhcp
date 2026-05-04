package plugin

import (
	"sync"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestShortID covers the safety-net behaviour the function exists for:
// it must not panic on IDs shorter than 12 chars (which can happen on
// malformed daemon responses during recovery).
func TestShortID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"short", "abc", "abc"},
		{"exactly_12", "abcdefghijkl", "abcdefghijkl"},
		{"longer", "abcdefghijklmnop", "abcdefghijkl"},
		{"docker_endpoint_64hex", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "0123456789ab"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shortID(c.in); got != c.want {
				t.Errorf("shortID(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestNewChildLink covers the per-mode netlink type selection. The
// macvlan submode must be MACVLAN_MODE_BRIDGE so children on the same
// parent can talk to each other; the ipvlan submode must be
// IPVLAN_MODE_L2 because DHCP needs L2 broadcast to reach the upstream
// server. Both invariants are subtle enough to deserve a test.
func TestNewChildLink(t *testing.T) {
	la := netlink.NewLinkAttrs()
	la.Name = "dh-test"

	macv, ok := newChildLink(ModeMacvlan, la).(*netlink.Macvlan)
	if !ok {
		t.Fatalf("ModeMacvlan: expected *netlink.Macvlan, got %T", newChildLink(ModeMacvlan, la))
	}
	if macv.Mode != netlink.MACVLAN_MODE_BRIDGE {
		t.Errorf("macvlan submode: got %v want MACVLAN_MODE_BRIDGE", macv.Mode)
	}
	if macv.LinkAttrs.Name != "dh-test" {
		t.Errorf("macvlan attrs not threaded: got %q", macv.LinkAttrs.Name)
	}

	ipv, ok := newChildLink(ModeIPvlan, la).(*netlink.IPVlan)
	if !ok {
		t.Fatalf("ModeIPvlan: expected *netlink.IPVlan, got %T", newChildLink(ModeIPvlan, la))
	}
	if ipv.Mode != netlink.IPVLAN_MODE_L2 {
		t.Errorf("ipvlan submode: got %v want IPVLAN_MODE_L2 (DHCP needs L2 broadcast)", ipv.Mode)
	}

	// Default falls back to macvlan — protects bridge-mode callers
	// that pass through here on a code-path that doesn't validate mode
	// strings (currently none, but cheap to guard).
	if _, ok := newChildLink("", la).(*netlink.Macvlan); !ok {
		t.Errorf("empty mode should default to macvlan")
	}
}

// TestUpdateJoinHint covers the read-modify-write helper that
// CreateEndpoint uses to layer in successive bits of state without
// holding the plugin lock across user callbacks. A refactor that
// dropped the locking around fn would race with concurrent
// storeJoinHint calls; a refactor that broke the read-modify-write
// would clobber prior fields.
func TestUpdateJoinHint(t *testing.T) {
	p := newPluginForTest()

	// First update — store IPv4.
	p.updateJoinHint("ep-1", func(h *joinHint) {
		v4, _ := netlink.ParseAddr("192.168.0.50/24")
		h.IPv4 = v4
		h.Gateway = "192.168.0.1"
	})

	// Second update — must preserve the v4 we just stored, layer in v6.
	p.updateJoinHint("ep-1", func(h *joinHint) {
		if h.IPv4 == nil || h.IPv4.IP.String() != "192.168.0.50" {
			t.Errorf("update lost prior IPv4: %+v", h.IPv4)
		}
		if h.Gateway != "192.168.0.1" {
			t.Errorf("update lost prior Gateway: %q", h.Gateway)
		}
		v6, _ := netlink.ParseAddr("fe80::1/64")
		h.IPv6 = v6
	})

	got, ok := p.takeJoinHint("ep-1")
	if !ok {
		t.Fatal("hint disappeared")
	}
	if got.IPv4 == nil || got.IPv4.IP.String() != "192.168.0.50" {
		t.Errorf("final IPv4: %+v", got.IPv4)
	}
	if got.IPv6 == nil || got.IPv6.IP.String() != "fe80::1" {
		t.Errorf("final IPv6: %+v", got.IPv6)
	}
	if got.Gateway != "192.168.0.1" {
		t.Errorf("final Gateway: %q", got.Gateway)
	}
}

// TestUpdateJoinHint_Concurrent guards the locking discipline. N
// goroutines layering successive updates onto disjoint endpoint IDs
// must not race; a refactor that dropped the mutex would trip -race.
func TestUpdateJoinHint_Concurrent(t *testing.T) {
	p := newPluginForTest()

	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			id := "ep-" + string(rune('a'+i%26))
			p.updateJoinHint(id, func(h *joinHint) { h.Gateway = "10.0.0.1" })
			p.updateJoinHint(id, func(h *joinHint) { h.Gateway += "/24" })
		}(i)
	}
	wg.Wait()
}

// TestDHCPManager_LastIPsAndSetter covers the ipMu-guarded accessor
// pair. Writes happen on the udhcpc renew goroutine; reads happen on
// the Leave path. Without the mutex (or with a partial implementation
// that updated only one side) the race detector would flag — and
// stale-read bugs would silently feed wrong IPs into the tombstone.
func TestDHCPManager_LastIPsAndSetter(t *testing.T) {
	m := &dhcpManager{}

	// Zero state.
	if v4, v6 := m.lastIPs(); v4 != nil || v6 != nil {
		t.Errorf("zero manager: expected nil/nil, got %+v / %+v", v4, v6)
	}

	v4, _ := netlink.ParseAddr("10.0.0.1/24")
	v6, _ := netlink.ParseAddr("fe80::1/64")

	m.setLastIP(false, v4)
	if got4, got6 := m.lastIPs(); got4 != v4 || got6 != nil {
		t.Errorf("after v4 set: got4=%v got6=%v want v4 only", got4, got6)
	}

	m.setLastIP(true, v6)
	if got4, got6 := m.lastIPs(); got4 != v4 || got6 != v6 {
		t.Errorf("after v6 set: got4=%v got6=%v", got4, got6)
	}

	// Overwrite v4 — v6 must survive.
	v4b, _ := netlink.ParseAddr("10.0.0.2/24")
	m.setLastIP(false, v4b)
	if got4, got6 := m.lastIPs(); got4 != v4b || got6 != v6 {
		t.Errorf("after v4 overwrite: got4=%v got6=%v", got4, got6)
	}
}

// TestDHCPManager_LogFields is a thin guard against the structured-log
// keys drifting — operators have grafana queries that pivot on
// `network` / `endpoint` / `is_ipv6`, and a rename would silently
// break them.
func TestDHCPManager_LogFields(t *testing.T) {
	m := &dhcpManager{
		joinReq: JoinRequest{
			NetworkID:  "0123456789abcdef0000000000000000",
			EndpointID: "fedcba9876543210ffffffffffffffff",
			SandboxKey: "/var/run/docker/netns/abc",
		},
	}
	for _, v6 := range []bool{false, true} {
		f := m.logFields(v6)
		for _, key := range []string{"network", "endpoint", "sandbox", "is_ipv6"} {
			if _, ok := f[key]; !ok {
				t.Errorf("v6=%v: missing log key %q", v6, key)
			}
		}
		if got := f["is_ipv6"].(bool); got != v6 {
			t.Errorf("is_ipv6: got %v want %v", got, v6)
		}
		// network/endpoint must be the shortened form so logs stay scannable.
		if got := f["network"].(string); got != "0123456789ab" {
			t.Errorf("network field not shortened: %q", got)
		}
		if got := f["endpoint"].(string); got != "fedcba987654" {
			t.Errorf("endpoint field not shortened: %q", got)
		}
	}
}
