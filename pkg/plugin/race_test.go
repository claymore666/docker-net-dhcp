package plugin

import (
	"fmt"
	"sync"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestPlugin_JoinHints_ConcurrentAccess exercises the joinHints map
// under concurrent goroutines that mimic CreateEndpoint / Join /
// Leave call patterns. Run with `go test -race ./pkg/plugin/`.
//
// Without proper synchronisation in Plugin, this triggers Go's race
// detector. It's the regression test for the fix that adds a
// sync.Mutex to Plugin.
func TestPlugin_JoinHints_ConcurrentAccess(t *testing.T) {
	p := &Plugin{
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}

	const writers = 8
	const iters = 200

	var wg sync.WaitGroup
	wg.Add(writers * 3)

	// Writers: simulate CreateEndpoint storing a hint, then Join consuming it.
	for w := 0; w < writers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := fmt.Sprintf("ep-%d-%d", w, i)
				p.storeJoinHint(id, joinHint{Gateway: "192.168.0.1"})
			}
		}(w)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := fmt.Sprintf("ep-%d-%d", w, i)
				_, _ = p.takeJoinHint(id)
			}
		}(w)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				id := fmt.Sprintf("ep-%d-%d", w, i)
				p.registerDHCPManager(id, &dhcpManager{})
				_, _ = p.takeDHCPManager(id)
			}
		}(w)
	}

	wg.Wait()
}

// TestPlugin_RecoverOneEndpointIsIdempotent guards the recovery
// fast-path: if an endpoint already has a manager (e.g. because a
// concurrent Join beat us to it), recoverOneEndpoint must not
// register a second one.
func TestPlugin_RecoverOneEndpointIsIdempotent(t *testing.T) {
	p := &Plugin{
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
	existing := &dhcpManager{}
	p.registerDHCPManager("ep-existing", existing)

	// Call should bail early because an entry already exists.
	// We pass a syntactically-invalid MAC to confirm the early-out
	// runs before MAC parsing — if it didn't, this would error.
	err := p.recoverOneEndpoint(t.Context(), "net-1", "ep-existing", "not-a-mac", "", "", DHCPNetworkOptions{})
	if err != nil {
		t.Errorf("recoverOneEndpoint should be idempotent on existing entry, got %v", err)
	}

	// Confirm we still hold the original manager, not a replacement.
	got, ok := p.takeDHCPManager("ep-existing")
	if !ok || got != existing {
		t.Errorf("existing manager was replaced; got %v ok=%v", got, ok)
	}
}

// TestPlugin_JoinHintFlow walks one CreateEndpoint -> Join -> Leave
// sequence through the helper accessors and verifies the values
// land where expected.
func TestPlugin_JoinHintFlow(t *testing.T) {
	p := &Plugin{
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}

	hint := joinHint{
		Gateway:    "192.168.0.1",
		MacAddress: netlink.NewLinkAttrs().HardwareAddr, // empty but valid
	}
	p.storeJoinHint("ep-1", hint)

	got, ok := p.takeJoinHint("ep-1")
	if !ok {
		t.Fatal("takeJoinHint missed an entry that was just stored")
	}
	if got.Gateway != hint.Gateway {
		t.Errorf("gateway mismatch: got %q want %q", got.Gateway, hint.Gateway)
	}
	// takeJoinHint should remove the entry
	if _, ok := p.takeJoinHint("ep-1"); ok {
		t.Error("takeJoinHint must remove the entry it returns")
	}

	m := &dhcpManager{}
	p.registerDHCPManager("ep-1", m)
	got2, ok := p.takeDHCPManager("ep-1")
	if !ok || got2 != m {
		t.Errorf("takeDHCPManager mismatch: got %v ok=%v want %v", got2, ok, m)
	}
	// Same: take must remove
	if _, ok := p.takeDHCPManager("ep-1"); ok {
		t.Error("takeDHCPManager must remove the entry it returns")
	}
}

// TestTakeDHCPManagersForNetwork_PrunesByNetwork covers the #44 fix:
// DeleteNetwork must evict every manager attached to the disappearing
// network, leaving managers on other networks untouched. It's the
// underlying primitive — DeleteNetwork's HTTP path also calls Stop on
// each, but Stop's blocking semantics are exercised separately in
// integration testing.
func TestTakeDHCPManagersForNetwork_PrunesByNetwork(t *testing.T) {
	p := &Plugin{
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
	mkMgr := func(networkID string) *dhcpManager {
		return &dhcpManager{joinReq: JoinRequest{NetworkID: networkID}}
	}
	mA1 := mkMgr("net-A")
	mA2 := mkMgr("net-A")
	mB1 := mkMgr("net-B")
	p.registerDHCPManager("ep-A1", mA1)
	p.registerDHCPManager("ep-A2", mA2)
	p.registerDHCPManager("ep-B1", mB1)

	got := p.takeDHCPManagersForNetwork("net-A")
	if len(got) != 2 {
		t.Fatalf("expected 2 managers evicted for net-A, got %d", len(got))
	}
	// Order isn't guaranteed (map iteration); verify by set membership.
	have := map[*dhcpManager]bool{got[0]: true, got[1]: true}
	if !have[mA1] || !have[mA2] {
		t.Errorf("evicted set missing one of mA1/mA2: %+v", got)
	}
	if have[mB1] {
		t.Error("net-B manager was wrongly evicted")
	}

	// Registry should now hold only the unrelated manager.
	if _, ok := p.takeDHCPManager("ep-A1"); ok {
		t.Error("ep-A1 should already be gone")
	}
	if _, ok := p.takeDHCPManager("ep-A2"); ok {
		t.Error("ep-A2 should already be gone")
	}
	if m, ok := p.takeDHCPManager("ep-B1"); !ok || m != mB1 {
		t.Errorf("ep-B1 should still be there: ok=%v m=%v", ok, m)
	}

	// Calling again on the now-empty network is a clean no-op.
	if got := p.takeDHCPManagersForNetwork("net-A"); len(got) != 0 {
		t.Errorf("expected empty result on second call, got %d managers", len(got))
	}
}
