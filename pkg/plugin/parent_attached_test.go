package plugin

import (
	"context"
	"errors"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"strings"
	"testing"
	"time"
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

// #408: a restart re-applies the previous endpoint's MAC, so it collides
// with the link it is replacing until DeleteEndpoint removes it. The
// kernel says EADDRINUSE and the whole `docker restart` fails.
func TestLinkUpAwaitingAddress(t *testing.T) {
	swapSetUp := func(t *testing.T, fn func(netlink.Link) error) *int {
		t.Helper()
		calls := 0
		prev := nlLinkSetUp
		nlLinkSetUp = func(l netlink.Link) error {
			calls++
			return fn(l)
		}
		t.Cleanup(func() { nlLinkSetUp = prev })
		return &calls
	}
	link := &netlink.Macvlan{LinkAttrs: netlink.LinkAttrs{Name: "dh-test"}}

	t.Run("waits out the link it is replacing", func(t *testing.T) {
		// Busy at first, then the old endpoint's link goes away — which
		// is what DeleteEndpoint landing looks like from here.
		var n int
		calls := swapSetUp(t, func(netlink.Link) error {
			n++
			if n < 3 {
				return unix.EADDRINUSE
			}
			return nil
		})
		if err := linkUpAwaitingAddress(context.Background(), link, time.Second); err != nil {
			t.Fatalf("gave up on an address that became free: %v", err)
		}
		if *calls < 3 {
			t.Errorf("succeeded after %d attempts; the stub only frees the address on the 3rd", *calls)
		}
	})

	t.Run("succeeds first time without waiting", func(t *testing.T) {
		calls := swapSetUp(t, func(netlink.Link) error { return nil })
		start := time.Now()
		if err := linkUpAwaitingAddress(context.Background(), link, time.Second); err != nil {
			t.Fatal(err)
		}
		if *calls != 1 {
			t.Errorf("retried %d times on a link that came up immediately", *calls)
		}
		if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
			t.Errorf("took %v on the happy path", elapsed)
		}
	})

	t.Run("gives up, and says what it was waiting for", func(t *testing.T) {
		swapSetUp(t, func(netlink.Link) error { return unix.EADDRINUSE })
		err := linkUpAwaitingAddress(context.Background(), link, 300*time.Millisecond)
		if err == nil {
			t.Fatal("an address that never frees must fail — coming up on a different one " +
				"is the outcome restart stability exists to prevent")
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			t.Errorf("the kernel's reason was lost: %v", err)
		}
		if !strings.Contains(err.Error(), "still held by the link this one replaces") {
			t.Errorf("error does not explain the wait: %v", err)
		}
	})

	t.Run("any other error is immediate, not retried", func(t *testing.T) {
		// Retrying a permission problem or a missing link just burns the
		// budget and reports the wrong cause at the end.
		boom := errors.New("operation not permitted")
		calls := swapSetUp(t, func(netlink.Link) error { return boom })
		err := linkUpAwaitingAddress(context.Background(), link, time.Second)
		if !errors.Is(err, boom) {
			t.Errorf("want the original error, got %v", err)
		}
		if *calls != 1 {
			t.Errorf("retried a non-EADDRINUSE error %d times", *calls)
		}
	})

	t.Run("a cancelled context stops the wait", func(t *testing.T) {
		swapSetUp(t, func(netlink.Link) error { return unix.EADDRINUSE })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := linkUpAwaitingAddress(ctx, link, time.Minute)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
		// The kernel's reason still has to survive, or a cancelled
		// restart reports nothing about why it was waiting.
		if !errors.Is(err, unix.EADDRINUSE) {
			t.Errorf("the last attempt's error was discarded: %v", err)
		}
	})
}
