// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	dNetwork "github.com/moby/moby/api/types/network"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// A child left on the parent refuses every ipvlan network there until the host reboots (#1339).
func TestDeleteNetwork_ReclaimsATaggedChildItsEndpointsLeftBehind(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	withStateDir(t, t.TempDir())
	parent := netnsDummy(t, "hcp0")
	const network = "4369f5bd5eb9aa00aa00aa00aa00aa00aa00aa00aa00aa00aa00aa00aa00aa00"
	opts := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "hcp0"}
	if err := saveOptions(network, opts); err != nil {
		t.Fatal(err)
	}
	child := childFor(t, "e1e2e3e4e5e6f7f8", "dh-e1e2e3e4e5e6", opts, parent)

	p := newPluginForTest()
	p.docker = &fakeDocker{}
	if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: network}); err != nil {
		t.Fatal(err)
	}
	if _, err := netlink.LinkByName(child); !isLinkNotFound(err) {
		t.Errorf("%s is still on hcp0 after its network was removed (err %v); nothing reclaims it", child, err)
	}
}

// stubAbandoningOneShot leases after cancelling the call's context, as a daemon that stops waiting does (#1339).
func stubAbandoningOneShot(t *testing.T, cancel context.CancelFunc) *int {
	t.Helper()
	calls := new(int)
	restore := dhcpGetIP
	dhcpGetIP = func(context.Context, string, *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		*calls++
		cancel()
		return dhcp.Info{IP: "192.168.99.61/24", Gateway: "192.168.99.1"}, dhcp.RAObservation{}, nil
	}
	t.Cleanup(func() { dhcpGetIP = restore })
	return calls
}

func TestCreateEndpoint_LeavesNoLinkForACallTheDaemonStoppedWaitingFor(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	netnsDummy(t, "abpa0")
	netnsDummy(t, "abpb0")
	if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "abbr0"}}); err != nil {
		t.Fatalf("add abbr0: %v", err)
	}
	child := func(ep string) []string { return []string{subLinkName(ep)} }
	for si, shape := range []struct {
		name  string
		opts  DHCPNetworkOptions
		links func(ep string) []string
	}{
		{"macvlan", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "abpa0"}, child},
		{"ipvlan", DHCPNetworkOptions{Mode: ModeIPvlan, Parent: "abpb0"}, child},
		{"bridge", DHCPNetworkOptions{Bridge: "abbr0"}, func(ep string) []string {
			host, ctr := vethPairNames(ep)
			return []string{host, ctr}
		}},
	} {
		for _, when := range []string{"before the call", "during the exchange"} {
			t.Run(shape.name+" "+when, func(t *testing.T) {
				withStateDir(t, t.TempDir())
				if err := saveOptions("n1339", shape.opts); err != nil {
					t.Fatalf("saveOptions: %v", err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				calls := stubAbandoningOneShot(t, cancel)
				if when == "before the call" {
					cancel()
				}

				p := newPluginForTest()
				p.docker = &fakeDocker{}
				p.records = recordingPlugin(t).records
				ep := fmt.Sprintf("ab%d%061x", si, 0)
				_, err := p.CreateEndpoint(ctx, CreateEndpointRequest{NetworkID: "n1339", EndpointID: ep,
					Interface: &EndpointInterface{}})
				if !errors.Is(err, context.Canceled) {
					t.Errorf("CreateEndpoint answered %v, want the daemon's cancellation", err)
				}
				if want := map[string]int{"before the call": 0, "during the exchange": 1}[when]; *calls != want {
					t.Errorf("the one-shot ran %d times, want %d", *calls, want)
				}
				for _, name := range shape.links(ep) {
					if _, err := netlink.LinkByName(name); !isLinkNotFound(err) {
						t.Errorf("%s is still in the host namespace (err %v); nothing else removes it", name, err)
					}
				}
			})
		}
	}
}

// The cleanup of an abandoned endpoint fails when the network's options can no longer be read; the cancellation stays
// the cause and the left-over link is named in the log (#1339).
func TestCreateEndpoint_WarnsWhenRemovingAnAbandonedEndpointFails(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	netnsDummy(t, "abpw0")
	withStateDir(t, t.TempDir())
	if err := saveOptions("n1339", DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "abpw0"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	statePath, err := stateFilePath("n1339")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	restore := dhcpGetIP
	dhcpGetIP = func(context.Context, string, *dhcp.DHCPClientOptions) (dhcp.Info, dhcp.RAObservation, error) {
		calls++
		cancel()
		if err := os.Remove(statePath); err != nil {
			t.Errorf("removing the state file: %v", err)
		}
		return dhcp.Info{IP: "192.168.99.62/24", Gateway: "192.168.99.1"}, dhcp.RAObservation{}, nil
	}
	t.Cleanup(func() { dhcpGetIP = restore })

	p := newPluginForTest()
	p.docker = &fakeDocker{inspectErr: errors.New("inspect refused")}
	p.records = recordingPlugin(t).records
	ep := fmt.Sprintf("ac%062x", 0)
	t.Cleanup(func() {
		if l, err := netlink.LinkByName(subLinkName(ep)); err == nil {
			_ = netlink.LinkDel(l)
		}
	})
	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	_, err = p.CreateEndpoint(ctx, CreateEndpointRequest{NetworkID: "n1339", EndpointID: ep, Interface: &EndpointInterface{}})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "so it was removed") {
		t.Errorf("CreateEndpoint answered %v, want the daemon's cancellation worded as a removal", err)
	}
	if calls != 1 {
		t.Errorf("the one-shot ran %d times, want 1", calls)
	}
	var warns []*log.Entry
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "ip link del") {
			warns = append(warns, e)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("%d warnings name `ip link del`, want 1: %v", len(warns), hook.AllEntries())
	}
	if got := warns[0].Data["endpoint"]; got != shortID(ep) {
		t.Errorf("the warning names endpoint %v, want %s", got, shortID(ep))
	}
}

// Only a tagged dh- child of the network's parent whose endpoint the plugin does not know is removed (#1339).
func TestDeleteNetwork_SparesEveryLinkItCannotProveOrphaned(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	for i, mode := range []string{ModeMacvlan, ModeIPvlan} {
		t.Run(mode, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			pa, pb := fmt.Sprintf("spa%d", i), fmt.Sprintf("spb%d", i)
			parent := netnsDummy(t, pa)
			other := netnsDummy(t, pb)
			const network = "5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"
			opts := DHCPNetworkOptions{Mode: mode, Parent: pa}
			if err := saveOptions(network, opts); err != nil {
				t.Fatal(err)
			}
			p := newPluginForTest()
			p.docker = &fakeDocker{}

			orphan := childFor(t, "a1a1a1a1a1a1b0b0", "dh-a1a1a1a1a1a1", opts, parent)
			hinted := childFor(t, "b2b2b2b2b2b2b0b0", "dh-b2b2b2b2b2b2", opts, parent)
			p.updateJoinHint("b2b2b2b2b2b2b0b0", func(*joinHint) {})
			inflight := childFor(t, "c3c3c3c3c3c3b0b0", "dh-c3c3c3c3c3c3", opts, parent)
			p.creatingEndpoints.Store("c3c3c3c3c3c3b0b0", struct{}{})
			fingerprinted := childFor(t, "d4d4d4d4d4d4b0b0", "dh-d4d4d4d4d4d4", opts, parent)
			p.rememberEndpoint("d4d4d4d4d4d4b0b0", endpointFingerprint{MAC: "02:00:00:00:00:d4"}, dhcpHostname{})
			managed := childFor(t, "c9c9c9c9c9c9b0b0", "dh-c9c9c9c9c9c9", opts, parent)
			p.persistentDHCP["c9c9c9c9c9c9b0b0"] = &dhcpManager{}
			untagged := childFor(t, "", "dh-e5e5e5e5e5e5", opts, parent)
			foreign := childFor(t, "", "dh-b8b8b8b8b8b8", opts, parent)
			if err := netlink.LinkSetAlias(mustLinkByName(t, foreign), "b8b8b8b8b8b8"); err != nil {
				t.Fatalf("alias %s: %v", foreign, err)
			}
			renamed := childFor(t, "f6f6f6f6f6f6b0b0", "eth-f6", opts, parent)
			elsewhere := childFor(t, "a7a7a7a7a7a7b0b0", "dh-a7a7a7a7a7a7", DHCPNetworkOptions{Mode: mode, Parent: pb}, other)
			kept := []string{hinted, inflight, fingerprinted, managed, untagged, foreign, renamed, elsewhere}
			t.Cleanup(func() { delLinks(append(kept, orphan)...) })

			if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: network}); err != nil {
				t.Fatal(err)
			}
			if _, err := netlink.LinkByName(orphan); !isLinkNotFound(err) {
				t.Errorf("the orphan %s survived (err %v)", orphan, err)
			}
			for _, k := range kept {
				if _, err := netlink.LinkByName(k); err != nil {
					t.Errorf("%s was removed (%v); its endpoint is known, it is not the plugin's, or it is on another parent", k, err)
				}
			}
		})
	}
}

func delLinks(names ...string) {
	for _, n := range names {
		if l, err := netlink.LinkByName(n); err == nil {
			_ = netlink.LinkDel(l)
		}
	}
}

// Docker's list of every network sharing the parent protects a link at start, and an unread network stops it (#1339).
func TestRecoverEndpoints_ReclaimsOrphanedLinksOnlyWhenEveryNetworkWasRead(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	const (
		netA   = "6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b"
		netB   = "7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c7c"
		driver = "ghcr.io/claymore666/docker-net-dhcp:latest"
	)
	for i, mode := range []string{ModeMacvlan, ModeIPvlan} {
		for _, unread := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/unread=%v", mode, unread), func(t *testing.T) {
				withStateDir(t, t.TempDir())
				pa := fmt.Sprintf("rp%d%v", i, unread)[:5]
				parent := netnsDummy(t, pa)
				opts := DHCPNetworkOptions{Mode: mode, Parent: pa}
				for _, id := range []string{netA, netB} {
					if err := saveOptions(id, opts); err != nil {
						t.Fatal(err)
					}
				}
				orphan := childFor(t, "a1a1a1a1a1a1c0c0", "dh-a1a1a1a1a1a1", opts, parent)
				listedA := childFor(t, "b2b2b2b2b2b2c0c0", "dh-b2b2b2b2b2b2", opts, parent)
				listedB := childFor(t, "c3c3c3c3c3c3c0c0", "dh-c3c3c3c3c3c3", opts, parent)
				t.Cleanup(func() { delLinks(orphan, listedA, listedB) })
				// A created endpoint is keyed ep-<id>, a joined one by its container ID (#1339).
				fd := &fakeDocker{
					listResult: []dNetwork.Summary{{ID: netA, Driver: driver}, {ID: netB, Driver: driver}},
					inspectResult: map[string]dNetwork.Inspect{
						netA: {Containers: map[string]dNetwork.EndpointResource{"ep-b2b2b2b2b2b2c0c0": {}}},
						netB: {Containers: map[string]dNetwork.EndpointResource{"c0ffeec0ffee": {EndpointID: "c3c3c3c3c3c3c0c0"}}},
					},
				}
				if unread {
					fd.inspectErr, fd.inspectErrFromCall = errors.New("daemon busy"), 2
				}
				p := withRecords(t, &Plugin{ipamIndex: newIPAMIndex(), joinHints: map[string]joinHint{},
					persistentDHCP: map[string]*dhcpManager{}, endpointFingerprints: map[string]endpointFingerprint{}})
				p.docker = fd
				recoverOnce(p)

				if _, err := netlink.LinkByName(orphan); unread == isLinkNotFound(err) {
					t.Errorf("orphan %s: lookup %v with a network unread=%v", orphan, err, unread)
				}
				for _, k := range []string{listedA, listedB} {
					if _, err := netlink.LinkByName(k); err != nil {
						t.Errorf("%s, whose endpoint Docker lists, was removed (%v)", k, err)
					}
				}
			})
		}
	}
}

// The sibling is removed after the link is tagged and before the Join hint exists (#1339).
func TestDeleteNetwork_SparesTheLinkOfACreateInFlightOnTheSameParent(t *testing.T) {
	if !inOwnNetns(t) {
		return
	}
	withStateDir(t, t.TempDir())
	netnsDummy(t, "ifpa0")
	const (
		network = "8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d8d"
		sibling = "9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e"
		ep      = "f0f0f0f0f0f0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
	)
	opts := DHCPNetworkOptions{Mode: ModeMacvlan, Parent: "ifpa0"}
	for _, id := range []string{network, sibling} {
		if err := saveOptions(id, opts); err != nil {
			t.Fatal(err)
		}
	}
	stubV6OneShot(t, dhcp.Info{}, dhcp.RAObservation{}, nil)
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	p.records = recordingPlugin(t).records

	restore := nlLinkSetAlias
	var removeErr error
	removed := false
	nlLinkSetAlias = func(l netlink.Link, alias string) error {
		err := restore(l, alias)
		if !removed && l.Attrs().Name == subLinkName(ep) {
			removed = true
			removeErr = p.DeleteNetwork(DeleteNetworkRequest{NetworkID: sibling})
		}
		return err
	}
	t.Cleanup(func() { nlLinkSetAlias = restore })

	if _, err := p.CreateEndpoint(t.Context(), CreateEndpointRequest{NetworkID: network, EndpointID: ep,
		Interface: &EndpointInterface{}}); err != nil {
		t.Fatalf("CreateEndpoint, with the sibling removed mid-create (%v): %v", removeErr, err)
	}
	if !removed {
		t.Fatal("the sibling removal never ran inside the create")
	}
	if _, err := netlink.LinkByName(subLinkName(ep)); err != nil {
		t.Errorf("the created endpoint's link is gone (%v)", err)
	}
}
