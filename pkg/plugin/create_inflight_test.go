// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dNetwork "github.com/docker/docker/api/types/network"
	"github.com/vishvananda/netlink"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

const (
	inflightParentA = "eth-1187-a"
	inflightParentB = "eth-1187-b"
	inflightBridge  = "br-1187"
	inflightIDA     = "net-1187-a"
	inflightIDB     = "net-1187-b"
)

// gatedList parks the first NetworkList call, so the first create is running and in no list (#1187).
type gatedList struct {
	*fakeDocker
	mu            sync.Mutex
	calls         atomic.Int32
	entered       chan struct{}
	release       chan struct{}
	laterErr      error
	holdSecond    chan struct{}
	secondEntered chan struct{}
}

func newGatedList() *gatedList {
	return &gatedList{fakeDocker: &fakeDocker{}, entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedList) NetworkList(ctx context.Context, o dNetwork.ListOptions) ([]dNetwork.Summary, error) {
	switch n := g.calls.Add(1); {
	case n == 1:
		close(g.entered)
		<-g.release
	case n == 2 && g.holdSecond != nil:
		close(g.secondEntered)
		<-g.holdSecond
	case g.laterErr != nil:
		return nil, g.laterErr
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fakeDocker.NetworkList(ctx, o)
}

func installInflightKernel(t *testing.T) {
	t.Helper()
	prev := nlLinkByName
	t.Cleanup(func() { nlLinkByName = prev })
	nlLinkByName = func(name string) (netlink.Link, error) {
		switch name {
		case inflightParentA, inflightParentB:
			return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name, Index: 8, MTU: 1500, Flags: net.FlagUp}}, nil
		case inflightBridge:
			return &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, Index: 7, MTU: 1500}}, nil
		}
		return nil, netlink.LinkNotFoundError{}
	}
	prevAddr := nlAddrList
	t.Cleanup(func() { nlAddrList = prevAddr })
	nlAddrList = func(netlink.Link, int) ([]netlink.Addr, error) { return nil, nil }
}

func inflightCreate(p *Plugin, id string, opts map[string]interface{}) error {
	return p.CreateNetwork(CreateNetworkRequest{
		NetworkID: id,
		Options:   map[string]interface{}{util.OptionsKeyGeneric: opts},
		IPv4Data:  []*IPAMData{{AddressSpace: "null", Pool: "0.0.0.0/0"}},
	})
}

func startParked(t *testing.T, p *Plugin, g *gatedList, id string, opts map[string]interface{}) <-chan error {
	t.Helper()
	res := make(chan error, 1)
	go func() { res <- inflightCreate(p, id, opts) }()
	select {
	case <-g.entered:
	case err := <-res:
		t.Fatalf("the first create returned before it read the network list: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the first create never read the network list")
	}
	return res
}

func finishParked(t *testing.T, g *gatedList, res <-chan error) {
	t.Helper()
	close(g.release)
	select {
	case err := <-res:
		if err != nil {
			t.Errorf("the first create, released after the second was refused, failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first create did not finish once released")
	}
}

func TestCreateNetwork_RefusesASiblingWhoseCreateIsStillRunning(t *testing.T) {
	macvlan := func(parent string, extra map[string]interface{}) map[string]interface{} {
		o := map[string]interface{}{"mode": "macvlan", "parent": parent}
		for k, v := range extra {
			o[k] = v
		}
		return o
	}
	cases := []struct {
		name         string
		first        map[string]interface{}
		second       map[string]interface{}
		wantErr      string // empty means the second create is accepted
		laterListErr error
	}{
		{"passthru first, bridge second", macvlan(inflightParentA, map[string]interface{}{"macvlan_mode": "passthru"}),
			macvlan(inflightParentA, nil), "another parent", nil},
		{"bridge first, passthru second", macvlan(inflightParentA, nil),
			macvlan(inflightParentA, map[string]interface{}{"macvlan_mode": "passthru"}), "another parent", nil},
		{"the second create's own list fails", macvlan(inflightParentA, map[string]interface{}{"macvlan_mode": "passthru"}),
			macvlan(inflightParentA, nil), "another parent", errors.New("daemon busy")},
		{"two bridge-mode macvlan children share a parent", macvlan(inflightParentA, nil),
			macvlan(inflightParentA, nil), "", nil},
		{"passthru first, another parent second", macvlan(inflightParentA, map[string]interface{}{"macvlan_mode": "passthru"}),
			macvlan(inflightParentB, nil), "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			installInflightKernel(t)
			p := newPluginForTest()
			g := newGatedList()
			g.laterErr = c.laterListErr
			p.docker = g

			res := startParked(t, p, g, inflightIDA, c.first)
			err := inflightCreate(p, inflightIDB, c.second)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("a second create that is no sibling was refused: %v", err)
				}
			} else {
				if err == nil {
					t.Error("accepted while the first create, absent from every list, was still running")
				} else {
					for _, want := range []string{inflightParentA, c.wantErr, "macvlan"} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("the refusal %q does not name %q", err, want)
						}
					}
					if !errors.Is(err, util.ErrModeMismatch) {
						t.Errorf("the refusal is %v, want the sub-mode refusal a stored sibling gets", err)
					}
					if _, lerr := loadOptions(inflightIDB); lerr == nil {
						t.Error("a refused network left state behind")
					}
				}
			}
			finishParked(t, g, res)
		})
	}
}

func TestCreateNetwork_RefusesASecondNetworkOnABridgeWhoseFirstCreateIsStillRunning(t *testing.T) {
	cases := []struct {
		name   string
		second map[string]interface{}
		used   bool
	}{
		{"same bridge", map[string]interface{}{"bridge": inflightBridge}, true},
		{"same bridge with ignore_conflicts", map[string]interface{}{"bridge": inflightBridge, "ignore_conflicts": "true"}, false},
		{"another bridge", map[string]interface{}{"bridge": "br-other"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			installInflightKernel(t)
			prev := nlLinkByName
			nlLinkByName = func(name string) (netlink.Link, error) {
				if name == "br-other" {
					return &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, Index: 9, MTU: 1500}}, nil
				}
				return prev(name)
			}
			p := newPluginForTest()
			g := newGatedList()
			p.docker = g

			res := startParked(t, p, g, inflightIDA, map[string]interface{}{"bridge": inflightBridge})
			err := inflightCreate(p, inflightIDB, c.second)
			switch {
			case c.used && !errors.Is(err, util.ErrBridgeUsed):
				t.Errorf("err = %v; want ErrBridgeUsed while the first create on the bridge was still running", err)
			case !c.used && err != nil:
				t.Errorf("refused: %v", err)
			}
			finishParked(t, g, res)
		})
	}
}

func TestCreateNetwork_CreatesOnDifferentParentsDoNotBlockEachOther(t *testing.T) {
	withStateDir(t, t.TempDir())
	installInflightKernel(t)
	p := newPluginForTest()
	g := newGatedList()
	p.docker = g

	res := startParked(t, p, g, inflightIDA, map[string]interface{}{"mode": "macvlan", "parent": inflightParentA})
	other := make(chan error, 1)
	go func() {
		other <- inflightCreate(p, inflightIDB, map[string]interface{}{"mode": "macvlan", "parent": inflightParentB})
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Errorf("a create on another parent failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("a create on another parent waited for the one parked in its list")
	}
	finishParked(t, g, res)
}

func TestCreateNetwork_ARetryAfterAFailedCreateIsNotItsOwnSibling(t *testing.T) {
	withStateDir(t, t.TempDir())
	installInflightKernel(t)
	p := newPluginForTest()
	p.docker = &fakeDocker{}

	passthru := map[string]interface{}{"mode": "macvlan", "parent": inflightParentA, "macvlan_mode": "passthru"}
	tooBig := map[string]interface{}{"mode": "macvlan", "parent": inflightParentA, "macvlan_mode": "passthru", "mtu": "9000"}
	if err := inflightCreate(p, inflightIDA, tooBig); err == nil {
		t.Fatal("an MTU above the parent's was accepted; the failed first attempt this test needs did not happen")
	}
	if err := inflightCreate(p, inflightIDA, passthru); err != nil {
		t.Errorf("the retry of the same network after a failure was refused: %v", err)
	}
	p.docker = &fakeDocker{listResult: []dNetwork.Summary{
		subModeNet(inflightIDA, "lan-a", testDHCPDriver, map[string]string{"mode": "macvlan", "parent": inflightParentA, "macvlan_mode": "passthru"}),
	}}
	if err := inflightCreate(p, inflightIDB, map[string]interface{}{"mode": "macvlan", "parent": inflightParentA}); err == nil {
		t.Error("a network beside a listed passthru one was accepted")
	}
	if err := inflightCreate(p, inflightIDB, map[string]interface{}{"mode": "macvlan", "parent": inflightParentB}); err != nil {
		t.Errorf("a failed create left something in flight that refused another parent: %v", err)
	}
}

func inflightCount(p *Plugin) int {
	p.createMu.Lock()
	defer p.createMu.Unlock()
	return len(p.creating)
}

func TestCreateNetwork_LeavesNothingInFlightWhateverTheOutcome(t *testing.T) {
	withStateDir(t, t.TempDir())
	installInflightKernel(t)
	p := newPluginForTest()
	p.docker = &fakeDocker{}

	for _, c := range []struct {
		name string
		opts map[string]interface{}
		fail bool
	}{
		{"success", map[string]interface{}{"mode": "macvlan", "parent": inflightParentA}, false},
		{"refused by the MTU check, after the sibling check's entry", map[string]interface{}{"mode": "macvlan", "parent": inflightParentA, "mtu": "9000"}, true},
		{"refused by option validation", map[string]interface{}{"mode": "macvlan", "parent": inflightParentA, "macvlan_mode": "brigde"}, true},
		{"bridge mode, bridge missing", map[string]interface{}{"bridge": "br-missing"}, true},
	} {
		err := inflightCreate(p, inflightIDA, c.opts)
		if (err != nil) != c.fail {
			t.Fatalf("%s: err = %v, want failure %v", c.name, err, c.fail)
		}
		if n := inflightCount(p); n != 0 {
			t.Errorf("%s: %d create(s) still in flight after the return; every later create on the parent would be refused", c.name, n)
		}
	}
}

func TestEarlierCreates_OnlyTheEarlierEntryIsSeenAndNeverTheCallersOwn(t *testing.T) {
	p := newPluginForTest()
	leaveA := p.beginCreate("net-a", DHCPNetworkOptions{Parent: "p"})
	leaveB := p.beginCreate("net-b", DHCPNetworkOptions{Parent: "p"})

	if got := p.earlierCreates("net-a"); len(got) != 0 {
		t.Errorf("the first create saw %v; both would be refused for each other", got)
	}
	if got := p.earlierCreates("net-b"); len(got) != 1 || got["net-a"].Parent != "p" {
		t.Errorf("the second create saw %v, want only net-a", got)
	}
	if got := p.earlierCreates("net-unregistered"); len(got) != 2 {
		t.Errorf("a caller that never entered saw %v, want both", got)
	}

	leaveA2 := p.beginCreate("net-a", DHCPNetworkOptions{Parent: "p"})
	leaveA()
	if got := p.earlierCreates("net-c"); got["net-a"].Parent != "p" {
		t.Errorf("a stale leave removed the retry's entry: %v", got)
	}
	leaveA2()
	leaveB()
	if n := inflightCount(p); n != 0 {
		t.Errorf("%d entries left after every leave", n)
	}
}

// The in-flight set is read before the list, so a create returning between the two reads is still seen (#1187).
func TestCreateNetwork_AFirstCreateThatReturnsDuringTheSecondsListReadIsStillSeen(t *testing.T) {
	cases := []struct {
		name          string
		first, second map[string]interface{}
		want          func(error) bool
	}{
		{"sub-mode", map[string]interface{}{"mode": "macvlan", "parent": inflightParentA, "macvlan_mode": "passthru"},
			map[string]interface{}{"mode": "macvlan", "parent": inflightParentA},
			func(err error) bool { return errors.Is(err, util.ErrModeMismatch) }},
		{"bridge", map[string]interface{}{"bridge": inflightBridge}, map[string]interface{}{"bridge": inflightBridge},
			func(err error) bool { return errors.Is(err, util.ErrBridgeUsed) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			installInflightKernel(t)
			p := newPluginForTest()
			g := newGatedList()
			g.holdSecond, g.secondEntered = make(chan struct{}), make(chan struct{})
			p.docker = g

			first := startParked(t, p, g, inflightIDA, c.first)
			second := make(chan error, 1)
			go func() { second <- inflightCreate(p, inflightIDB, c.second) }()

			var err error
			select {
			case err = <-second:
				close(g.release)
				<-first
			case <-g.secondEntered:
				close(g.release)
				if ferr := <-first; ferr != nil {
					t.Fatalf("the first create failed: %v", ferr)
				}
				close(g.holdSecond)
				err = <-second
			case <-time.After(5 * time.Second):
				t.Fatal("the second create neither returned nor read the list")
			}
			if !c.want(err) {
				t.Errorf("err = %v; the first create returned during the second's list read and was missed", err)
			}
		})
	}
}

// The window after the first create returned and before dockerd lists it: the in-flight entry is gone, the list does
// not show the network yet, and the stored record is the only evidence (#1187).
func TestCreateNetwork_RefusesASiblingWhoseCreateReturnedAndWhoseRecordIsOnDiskBeforeDockerListsIt(t *testing.T) {
	passthru := map[string]interface{}{"mode": "macvlan", "parent": inflightParentA, "macvlan_mode": "passthru"}
	cases := []struct {
		name          string
		first, second map[string]interface{}
		want          error // nil means the second create is accepted
		listFails     bool
	}{
		{"passthru returned, bridge second", passthru, map[string]interface{}{"mode": "macvlan", "parent": inflightParentA}, util.ErrModeMismatch, false},
		{"passthru returned, bridge second, the list fails", passthru, map[string]interface{}{"mode": "macvlan", "parent": inflightParentA}, util.ErrModeMismatch, true},
		{"bridge returned, passthru second", map[string]interface{}{"mode": "macvlan", "parent": inflightParentA},
			map[string]interface{}{"mode": "macvlan", "parent": inflightParentA, "macvlan_mode": "passthru"}, util.ErrModeMismatch, false},
		{"passthru returned, another parent second", passthru, map[string]interface{}{"mode": "macvlan", "parent": inflightParentB}, nil, false},
		{"bridge returned, same bridge second", map[string]interface{}{"bridge": inflightBridge}, map[string]interface{}{"bridge": inflightBridge}, util.ErrBridgeUsed, false},
		{"bridge returned, same bridge second with ignore_conflicts", map[string]interface{}{"bridge": inflightBridge},
			map[string]interface{}{"bridge": inflightBridge, "ignore_conflicts": "true"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			installInflightKernel(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{}

			if err := inflightCreate(p, inflightIDA, c.first); err != nil {
				t.Fatalf("the first create failed: %v", err)
			}
			if n := inflightCount(p); n != 0 {
				t.Fatalf("%d create(s) still in flight; the window this test needs is the one after the return", n)
			}
			if _, err := loadOptions(inflightIDA); err != nil {
				t.Fatalf("the first create returned without its record on disk: %v", err)
			}
			if c.listFails {
				p.docker = &fakeDocker{listErr: errors.New("daemon busy")}
			}
			err := inflightCreate(p, inflightIDB, c.second)
			switch {
			case c.want == nil && err != nil && !c.listFails:
				t.Errorf("a second create that is no sibling was refused: %v", err)
			case c.want != nil && !errors.Is(err, c.want):
				t.Errorf("err = %v; want %v while the first network's record was on disk and Docker did not list it", err, c.want)
			}
		})
	}
}

// A record whose network Docker lists is one sibling: the refusal is the listed network's, named as before (#1187).
func TestCreateNetwork_AStoredRecordOfAListedNetworkIsNotCountedTwice(t *testing.T) {
	withStateDir(t, t.TempDir())
	installInflightKernel(t)
	p := newPluginForTest()
	listedOpts := map[string]string{"mode": "macvlan", "parent": inflightParentA, "macvlan_mode": "passthru"}
	p.docker = &fakeDocker{listResult: []dNetwork.Summary{subModeNet(inflightIDA, "lan-a", testDHCPDriver, listedOpts)}}
	if err := saveOptions(inflightIDA, DHCPNetworkOptions{Mode: ModeMacvlan, Parent: inflightParentA, MacvlanMode: "passthru"}); err != nil {
		t.Fatal(err)
	}

	err := inflightCreate(p, inflightIDB, map[string]interface{}{"mode": "macvlan", "parent": inflightParentA})
	if !errors.Is(err, util.ErrModeMismatch) || !strings.Contains(err.Error(), `"lan-a"`) {
		t.Errorf("err = %v; want the sub-mode refusal naming the listed network lan-a", err)
	}
	if err := inflightCreate(p, inflightIDB, map[string]interface{}{"mode": "macvlan", "parent": inflightParentB}); err != nil {
		t.Errorf("a network on another parent was refused beside a listed and stored one: %v", err)
	}
	if got := storedSiblings(inflightIDB); len(got) != 1 {
		t.Errorf("storedSiblings read %d records, want the one saved", len(got))
	}
}

// After a crash between the save and the return, dockerd retries the same network id; its own record is no sibling (#1187).
func TestCreateNetwork_ARetryOfANetworkWhoseRecordIsOnDiskIsNotItsOwnSibling(t *testing.T) {
	cases := []struct {
		name string
		opts map[string]interface{}
	}{
		{"passthru macvlan", map[string]interface{}{"mode": "macvlan", "parent": inflightParentA, "macvlan_mode": "passthru"}},
		{"bridge", map[string]interface{}{"bridge": inflightBridge}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			installInflightKernel(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			if err := inflightCreate(p, inflightIDA, c.opts); err != nil {
				t.Fatalf("the first attempt failed: %v", err)
			}
			if err := inflightCreate(p, inflightIDA, c.opts); err != nil {
				t.Errorf("the retry of the same id was refused for its own record: %v", err)
			}
			if got := storedSiblings(inflightIDA); len(got) != 0 {
				t.Errorf("storedSiblings returned the network's own record: %v", got)
			}
		})
	}
}

func TestCreateNetwork_ANeverCreatedStateDirectoryIsNoSiblingAndNoError(t *testing.T) {
	withStateDir(t, t.TempDir()+"/not-yet")
	installInflightKernel(t)
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	if err := inflightCreate(p, inflightIDA, map[string]interface{}{"mode": "macvlan", "parent": inflightParentA}); err != nil {
		t.Errorf("the first create on a host with no state directory failed: %v", err)
	}
}
