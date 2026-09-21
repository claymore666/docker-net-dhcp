// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"errors"
	"fmt"
	"testing"
)

// errNoSuchLink stands in for what the kernel says about a name or an
// index that is not there. The library's own text is reproduced where a
// cell asserts that the caller keeps the real reason.
var errNoSuchLink = errors.New("no such network interface")

// fakeNetns is a namespace's link table with one writer: the test. The
// hooks fire at the instant a client is opened, which is the instant
// the engine's rename lands in the field.
type fakeNetns struct {
	names     map[int]string
	opens     []string
	abandoned []int
	reads     int
	lookups   int
	nameErr   func(call int) error
	onOpen    func(f *fakeNetns)
	afterOpen func(f *fakeNetns)
	nextID    int
}

type fakeClient struct {
	id    int
	index int
}

type fakeClient6 struct {
	id    int
	index int
}

func (f *fakeNetns) nameByIndex(index int) (string, error) {
	f.reads++
	if f.nameErr != nil {
		if err := f.nameErr(f.reads); err != nil {
			return "", err
		}
	}
	name, ok := f.names[index]
	if !ok {
		return "", errNoSuchLink
	}
	return name, nil
}

func (f *fakeNetns) indexByName(name string) (int, error) {
	f.lookups++
	for index, have := range f.names {
		if have == name {
			return index, nil
		}
	}
	return 0, errNoSuchLink
}

func (f *fakeNetns) open(name string) (*fakeClient, error) {
	if f.onOpen != nil {
		hook := f.onOpen
		f.onOpen = nil
		hook(f)
	}
	f.opens = append(f.opens, name)
	index := 0
	for have, nm := range f.names {
		if nm == name {
			index = have
		}
	}
	if index == 0 {
		return nil, fmt.Errorf("runtime: no interface named %q: %w", name, errNoSuchLink)
	}
	if f.afterOpen != nil {
		hook := f.afterOpen
		f.afterOpen = nil
		hook(f)
	}
	f.nextID++
	return &fakeClient{id: f.nextID, index: index}, nil
}

func (f *fakeNetns) abandon(c *fakeClient) { f.abandoned = append(f.abandoned, c.id) }

// install puts this table behind the two package seams for one test.
func (f *fakeNetns) install(t *testing.T) {
	t.Helper()
	prevName, prevIndex := linkNameByIndex, linkIndexByName
	linkNameByIndex = f.nameByIndex
	linkIndexByName = f.indexByName
	t.Cleanup(func() { linkNameByIndex, linkIndexByName = prevName, prevIndex })
}

func newFakeNetns(names map[int]string) *fakeNetns {
	return &fakeNetns{names: names}
}

// TestOpenOnLink_ARenameBetweenTheReadAndTheOpenOpensTheRightLink is
// the field failure, driven at the instant it happens.
//
// The engine moves the container-side link into the sandbox namespace
// and renames it. Every name the plugin reads is therefore already
// stale by the time the open resolves it, which is why narrowing the
// gap is not a fix: the rename here lands INSIDE the open, after the
// name was read and before it was resolved, and the open still has to
// end on the link the caller meant.
func TestOpenOnLink_ARenameBetweenTheReadAndTheOpenOpensTheRightLink(t *testing.T) {
	f := newFakeNetns(map[int]string{3: "dh-abcdef012345"})
	f.onOpen = func(f *fakeNetns) { f.names[3] = "eth0" }
	f.install(t)

	client, name, err := openOnLink("dh-abcdef012345", 3, f.open, f.abandon)
	if err != nil {
		t.Fatalf("the open gave up on a link that is present the whole time, only under another name: %v", err)
	}
	if client.index != 3 {
		t.Errorf("the client opened on link %d and the caller asked for link 3: a renewal client on "+
			"another link renews somebody else's address", client.index)
	}
	if name != "eth0" {
		t.Errorf("the open reports %q as the name it used, and the link is called %q: the failure log "+
			"would name a link that does not exist", name, "eth0")
	}
	if len(f.opens) != 2 {
		t.Errorf("opens were %v, want one that failed on the old name and one that succeeded on the "+
			"new one", f.opens)
	}
	if len(f.abandoned) != 0 {
		t.Errorf("clients %v were abandoned; nothing was opened on a wrong link here", f.abandoned)
	}
}

// TestOpenOnLink_TwoRenamesAreSurvived keeps the bound honest at the
// shape the engine can actually produce: the move-and-rename, and then
// a second rename where the container asked for an interface name of
// its own.
func TestOpenOnLink_TwoRenamesAreSurvived(t *testing.T) {
	f := newFakeNetns(map[int]string{3: "dh-abcdef012345"})
	f.onOpen = func(f *fakeNetns) {
		f.names[3] = "eth0"
		f.onOpen = func(f *fakeNetns) { f.names[3] = "lan0" }
	}
	f.install(t)

	client, name, err := openOnLink("dh-abcdef012345", 3, f.open, f.abandon)
	if err != nil {
		t.Fatalf("two renames defeated the open: %v", err)
	}
	if client.index != 3 || name != "lan0" {
		t.Errorf("the client is on link %d under the name %q, want link 3 as %q", client.index, name, "lan0")
	}
	if len(f.opens) != 3 {
		t.Errorf("opens were %v, want three: the stale name, the name it was renamed to, and the name "+
			"it was renamed to again", f.opens)
	}
}

// TestOpenOnLink_ALinkThatIsGoneFailsOnceWithItsOwnReason is the
// container that died during the attach.
//
// Two things are asserted and both are the point: the caller hears the
// open's own error and not a rewritten one, and the open is attempted
// exactly once. A retry keyed on "the open failed" instead of on "the
// name moved" would spin here for as long as its bound allows, inside
// the attach budget, for a link that is never coming back.
func TestOpenOnLink_ALinkThatIsGoneFailsOnceWithItsOwnReason(t *testing.T) {
	f := newFakeNetns(map[int]string{3: "dh-abcdef012345"})
	f.onOpen = func(f *fakeNetns) { delete(f.names, 3) }
	f.install(t)

	_, name, err := openOnLink("dh-abcdef012345", 3, f.open, f.abandon)
	if err == nil {
		t.Fatal("a client was opened on a link that had been deleted")
	}
	if !errors.Is(err, errNoSuchLink) {
		t.Errorf("the caller hears %v; the reason the open failed is what an operator acts on", err)
	}
	if errors.Is(err, errLinkNameUnstable) {
		t.Error("a deleted link is reported as an unstable name: the two have different causes and " +
			"different fixes")
	}
	if len(f.opens) != 1 {
		t.Errorf("opens were %v, want exactly one: nothing about a deleted link changes between "+
			"attempts", f.opens)
	}
	if name != "dh-abcdef012345" {
		t.Errorf("the reported name is %q, want the name the open actually tried", name)
	}
}

// TestOpenOnLink_ALinkThatIsGoneWhileItsNameIsTakenKeepsTheRealReason
// is the same dead container, with the one difference that makes the
// two halves of "the link is gone" end in different places: something
// else took the name before the open reached it, so the open SUCCEEDS,
// on a link that is not the caller's.
//
// Without the second question this costs four opens and reports a name
// that kept being renamed, which is a cause an operator would go
// looking for and would not find. The link is gone, that is the
// reason, and it is the reason the caller hears.
func TestOpenOnLink_ALinkThatIsGoneWhileItsNameIsTakenKeepsTheRealReason(t *testing.T) {
	f := newFakeNetns(map[int]string{3: "dh-abcdef012345"})
	f.onOpen = func(f *fakeNetns) {
		delete(f.names, 3)
		f.names[9] = "dh-abcdef012345"
	}
	f.install(t)

	_, _, err := openOnLink("dh-abcdef012345", 3, f.open, f.abandon)
	if err == nil {
		t.Fatal("a client opened on another endpoint's link was returned for a link that is gone")
	}
	if !errors.Is(err, errNoSuchLink) {
		t.Errorf("the caller hears %v, and the link is gone: that is what an operator acts on", err)
	}
	if errors.Is(err, errLinkNameUnstable) {
		t.Error("a deleted link whose name was taken is reported as a name that kept moving: the " +
			"operator is sent after a rename that never happened")
	}
	if len(f.opens) != 1 {
		t.Errorf("opens were %v, want exactly one: a link that is gone does not come back between "+
			"attempts, whoever holds its old name", f.opens)
	}
	if len(f.abandoned) != 1 {
		t.Errorf("clients abandoned: %v, want the one opened on the link that took the name", f.abandoned)
	}
}

// TestOpenOnLink_AnotherLinkHoldingTheOldNameIsNotAccepted is the one
// way a SUCCESSFUL open is wrong: the rename freed the old name and
// something else took it before the open resolved it. The client that
// comes back leases on a link that belongs to another endpoint, and
// nothing downstream can tell.
func TestOpenOnLink_AnotherLinkHoldingTheOldNameIsNotAccepted(t *testing.T) {
	f := newFakeNetns(map[int]string{3: "dh-abcdef012345"})
	f.onOpen = func(f *fakeNetns) {
		f.names[3] = "eth0"
		f.names[9] = "dh-abcdef012345"
	}
	f.install(t)

	client, name, err := openOnLink("dh-abcdef012345", 3, f.open, f.abandon)
	if err != nil {
		t.Fatalf("openOnLink: %v", err)
	}
	if client.index != 3 {
		t.Errorf("the client opened on link %d, which is the link that took the old name, and not on "+
			"link 3", client.index)
	}
	if name != "eth0" {
		t.Errorf("the client reports the name %q, want %q", name, "eth0")
	}
	if len(f.abandoned) != 1 {
		t.Errorf("clients abandoned: %v, want the one opened on the wrong link. A client dropped "+
			"without being disposed of leaks its sockets", f.abandoned)
	}
}

// TestOpenOnLink_ARenameAfterTheOpenKeepsTheClient. The library's
// sockets are bound to the interface once the name is resolved, so a
// link renamed under a running client keeps leasing. A check that asked
// "is this link still called what I opened" instead of "is this name
// still my link" would throw that client away and open a second one for
// nothing, on every attach.
func TestOpenOnLink_ARenameAfterTheOpenKeepsTheClient(t *testing.T) {
	f := newFakeNetns(map[int]string{3: "dh-abcdef012345"})
	f.afterOpen = func(f *fakeNetns) { f.names[3] = "eth0" }
	f.install(t)

	client, _, err := openOnLink("dh-abcdef012345", 3, f.open, f.abandon)
	if err != nil {
		t.Fatalf("openOnLink: %v", err)
	}
	if client.index != 3 {
		t.Errorf("the client is on link %d, want 3", client.index)
	}
	if len(f.opens) != 1 || len(f.abandoned) != 0 {
		t.Errorf("opens %v, abandoned %v: the open was right and was thrown away anyway", f.opens, f.abandoned)
	}
}

// TestOpenOnLink_AFailedIndexReadFallsBackToTheCallersName. The
// namespace read is an improvement on the caller's name and is never a
// precondition for using it: where it fails, the open proceeds exactly
// as it did before any of this existed.
func TestOpenOnLink_AFailedIndexReadFallsBackToTheCallersName(t *testing.T) {
	f := newFakeNetns(map[int]string{3: "dh-abcdef012345"})
	f.nameErr = func(call int) error {
		if call == 1 {
			return errors.New("netlink is busy")
		}
		return nil
	}
	f.install(t)

	client, name, err := openOnLink("dh-abcdef012345", 3, f.open, f.abandon)
	if err != nil {
		t.Fatalf("a namespace read that failed turned a working open into a failure: %v", err)
	}
	if client.index != 3 || name != "dh-abcdef012345" {
		t.Errorf("client on link %d as %q, want link 3 under the caller's own name", client.index, name)
	}
	if len(f.opens) != 1 {
		t.Errorf("opens were %v, want one", f.opens)
	}
}

// TestOpenOnLink_WithoutAnIndexNothingIsAsked is the CreateEndpoint
// one-shot: a link in this namespace that nobody is renaming. It must
// cost exactly one open and not one namespace read, because that is
// what it costs today and this change is not entitled to make it
// slower or to give it a new way to fail.
func TestOpenOnLink_WithoutAnIndexNothingIsAsked(t *testing.T) {
	f := newFakeNetns(map[int]string{3: "dh-abcdef012345"})
	f.install(t)

	client, name, err := openOnLink("dh-abcdef012345", 0, f.open, f.abandon)
	if err != nil {
		t.Fatalf("openOnLink: %v", err)
	}
	if client.index != 3 || name != "dh-abcdef012345" {
		t.Errorf("client on link %d as %q, want link 3 under the name it was given", client.index, name)
	}
	if f.reads != 0 || f.lookups != 0 {
		t.Errorf("the namespace was asked %d times by index and %d times by name; a caller with no "+
			"index has nothing to resolve", f.reads, f.lookups)
	}
	if len(f.opens) != 1 {
		t.Errorf("opens were %v, want one", f.opens)
	}
}

// TestOpenOnLink_ANameThatNeverSettlesIsBoundedAndHonest. The last
// resort has to be an error and not a client: handing back a client
// opened on a link that is not the caller's is the failure this whole
// change is about, and it is worse when it is silent. Bounded, every
// client disposed of, and an error that says what happened.
func TestOpenOnLink_ANameThatNeverSettlesIsBoundedAndHonest(t *testing.T) {
	f := newFakeNetns(map[int]string{3: "dh-abcdef012345"})
	f.names[9] = "taken"
	// Every read answers with a name another link holds, so every open
	// succeeds on the wrong link.
	prevName, prevIndex := linkNameByIndex, linkIndexByName
	linkNameByIndex = func(int) (string, error) { f.reads++; return "taken", nil }
	linkIndexByName = func(string) (int, error) { f.lookups++; return 9, nil }
	t.Cleanup(func() { linkNameByIndex, linkIndexByName = prevName, prevIndex })

	_, _, err := openOnLink("dh-abcdef012345", 3, f.open, f.abandon)
	if !errors.Is(err, errLinkNameUnstable) {
		t.Fatalf("openOnLink returned %v; a name that never settles must end as an error and never as "+
			"a client on another endpoint's link", err)
	}
	if len(f.opens) != openLinkAttempts {
		t.Errorf("opens were %v, want %d: the retry is bounded and pays no wait", f.opens, openLinkAttempts)
	}
	if len(f.abandoned) != openLinkAttempts {
		t.Errorf("clients abandoned: %v, want every one of the %d opened. A library client dropped "+
			"without being run leaks its sockets", f.abandoned, openLinkAttempts)
	}
}

// TestOpenOnLink_TheV6FamilyOpensTheSameWay. The two families are one
// function on purpose: the v6 client opens seconds after the v4 one, in
// the same attach, and a rename between them is the same rename. A
// second hand-written copy is where one family gets the retry and the
// other does not.
func TestOpenOnLink_TheV6FamilyOpensTheSameWay(t *testing.T) {
	f := newFakeNetns(map[int]string{3: "dh-abcdef012345"})
	f.onOpen = func(f *fakeNetns) { f.names[3] = "eth0" }
	f.install(t)

	open6 := func(name string) (*fakeClient6, error) {
		client, err := f.open(name)
		if err != nil {
			return nil, err
		}
		return &fakeClient6{id: client.id, index: client.index}, nil
	}
	abandon6 := func(c *fakeClient6) { f.abandoned = append(f.abandoned, c.id) }

	client, name, err := openOnLink("dh-abcdef012345", 3, open6, abandon6)
	if err != nil {
		t.Fatalf("the v6 open gave up on a link that was renamed under it: %v", err)
	}
	if client.index != 3 || name != "eth0" {
		t.Errorf("the v6 client is on link %d as %q, want link 3 as %q", client.index, name, "eth0")
	}
}
