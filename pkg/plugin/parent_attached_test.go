package plugin

import (
	"testing"
)

// TestParentAttachedEndpointOperInfo_NoLink covers the expected case:
// by the time anyone polls EndpointOperInfo for a macvlan/ipvlan
// endpoint, the child link has typically been moved into the
// container's netns. The host-side LinkByName lookup fails — that's
// not an error; we still return the static fields (mode, parent,
// host-link name) so libnetwork has something to display.
func TestParentAttachedEndpointOperInfo_NoLink(t *testing.T) {
	p := newPluginForTest()

	opts := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "ens18"}
	r := InfoRequest{
		NetworkID:  "0123456789abcdef0123456789abcdef",
		EndpointID: "fedcba9876543210fedcba9876543210",
	}
	res, err := p.parentAttachedEndpointOperInfo(opts, r)
	if err != nil {
		t.Fatalf("oper info: %v", err)
	}

	want := map[string]string{
		"mode":          ModeMacvlan,
		"parent":        "ens18",
		"sub_link_host": subLinkName(r.EndpointID),
		"sub_link_mac":  "", // expected empty: the link is in the container netns by now
	}
	for k, v := range want {
		if got := res.Value[k]; got != v {
			t.Errorf("Value[%q]: got %q want %q", k, got, v)
		}
	}
}

// TestParentAttachedEndpointOperInfo_IPvlan covers the ipvlan path —
// same flow as macvlan but the mode field is encoded differently and
// libnetwork-facing operators rely on it being honest.
func TestParentAttachedEndpointOperInfo_IPvlan(t *testing.T) {
	p := newPluginForTest()
	opts := DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "ens18"}
	r := InfoRequest{NetworkID: "n", EndpointID: "0123456789abcdef0123456789abcdef"}

	res, err := p.parentAttachedEndpointOperInfo(opts, r)
	if err != nil {
		t.Fatalf("oper info: %v", err)
	}
	if res.Value["mode"] != ModeIPvlan {
		t.Errorf("mode: got %q want %q", res.Value["mode"], ModeIPvlan)
	}
}

// TestDeleteParentAttachedEndpoint_LinkAlreadyGone is the expected
// path on container teardown: the macvlan/ipvlan child has been
// reaped along with the container netns by the time we get here.
// LinkByName fails with "not found"; the function logs and returns
// nil. A regression that propagated the netlink error here would
// surface as spurious DeleteEndpoint failures on every clean shutdown.
func TestDeleteParentAttachedEndpoint_LinkAlreadyGone(t *testing.T) {
	p := newPluginForTest()
	r := DeleteEndpointRequest{
		NetworkID:  "n",
		EndpointID: "deadbeef0001deadbeef0002deadbeef0003deadbeef0004deadbeef0005dead",
	}
	if err := p.deleteParentAttachedEndpoint(r); err != nil {
		t.Errorf("expected nil for missing link, got %v", err)
	}
}

// TestNewDHCPManager covers the constructor — verifies the channels
// are initialized non-nil (a refactor that swapped to lazy
// initialization would deadlock Stop's <-startedCh on a manager
// whose Start was never called).
func TestNewDHCPManager(t *testing.T) {
	r := JoinRequest{NetworkID: "net-1", EndpointID: "ep-1"}
	opts := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "ens18"}
	m := newDHCPManager(nil, r, opts)

	if m.joinReq.NetworkID != "net-1" {
		t.Errorf("joinReq not threaded: %+v", m.joinReq)
	}
	if m.opts.Mode != ModeMacvlan {
		t.Errorf("opts not threaded: %+v", m.opts)
	}
	if m.stopChan == nil {
		t.Error("stopChan must be non-nil so Stop's close() doesn't panic")
	}
	if m.startedCh == nil {
		t.Error("startedCh must be non-nil so Stop's <-startedCh doesn't deadlock")
	}
	// Channel must be unclosed initially — Start closes it on completion.
	select {
	case <-m.startedCh:
		t.Error("startedCh should not be closed at construction")
	default:
	}
}
