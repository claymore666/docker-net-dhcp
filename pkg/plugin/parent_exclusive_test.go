// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// The kernel fact these tests encode, verified directly against it in
// both directions:
//
//	ip link add mv0 link p0 type macvlan   -> ok
//	ip link add iv0 link p0 type ipvlan    -> EBUSY
//	(and the same the other way round)
//
// Many children of one kind are fine; the two kinds cannot share a
// parent, because both claim the parent netdev's single receive
// handler. Both directions are in this repo's CI record — an ipvlan
// endpoint refused while a macvlan child was live, and a macvlan
// validate_dhcp probe refused while an ipvlan child was live (#486).

// fakeParentChild is a netlink.Link whose type and parent are whatever
// the test needs. netlink's concrete types report their own Type(), so
// the two real ones are used directly rather than faked.
func childOn(t *testing.T, kind string, parentIndex int) netlink.Link {
	t.Helper()

	la := netlink.NewLinkAttrs()
	la.Name = "child-" + kind
	la.ParentIndex = parentIndex

	switch kind {
	case ModeMacvlan:
		return &netlink.Macvlan{LinkAttrs: la, Mode: netlink.MACVLAN_MODE_BRIDGE}
	case ModeIPvlan:
		return &netlink.IPVlan{LinkAttrs: la, Mode: netlink.IPVLAN_MODE_L2}
	default:
		t.Fatalf("childOn: unknown kind %q", kind)
		return nil
	}
}

func withLinkList(t *testing.T, links []netlink.Link, err error) {
	t.Helper()

	orig := nlLinkList
	nlLinkList = func() ([]netlink.Link, error) { return links, err }
	t.Cleanup(func() { nlLinkList = orig })
}

// The message an operator actually gets. A bare "device or resource
// busy" gives no reason to suspect a different network on the same NIC,
// which is why #486 was first blamed on the runner image.
func TestExplainChildLinkAdd_NamesTheConflictingKind(t *testing.T) {
	const parentIdx = 7

	cases := []struct {
		name       string
		want       string
		occupant   string
		wantNamed  string
		wantMode   string
		wantParent string
	}{
		{
			name:      "ipvlan refused while macvlan children live",
			wantMode:  ModeIPvlan,
			occupant:  ModeMacvlan,
			wantNamed: ModeMacvlan,
		},
		{
			name:      "macvlan refused while ipvlan children live",
			wantMode:  ModeMacvlan,
			occupant:  ModeIPvlan,
			wantNamed: ModeIPvlan,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withLinkList(t, []netlink.Link{
				childOn(t, tc.occupant, parentIdx),
			}, nil)

			err := explainChildLinkAdd(unix.EBUSY, tc.wantMode, "eth0", parentIdx)
			if err == nil {
				t.Fatal("explainChildLinkAdd returned nil for an EBUSY")
			}

			// The errno has to survive: callers and operators both key
			// off it, and swallowing it would trade one opaque failure
			// for another.
			if !errors.Is(err, unix.EBUSY) {
				t.Errorf("error no longer wraps EBUSY: %v", err)
			}

			msg := err.Error()
			for _, want := range []string{tc.wantNamed, "eth0", "not both"} {
				if !strings.Contains(msg, want) {
					t.Errorf("message does not mention %q: %s", want, msg)
				}
			}
		})
	}
}

// A parent that carries nothing of the other kind by the time we look.
// The blocker went away between the refusal and the lookup, so the
// message must not assert a conflict it cannot see — it says what is
// known and stops.
func TestExplainChildLinkAdd_UnseenBlockerDoesNotInvent(t *testing.T) {
	withLinkList(t, nil, nil)

	err := explainChildLinkAdd(unix.EBUSY, ModeIPvlan, "eth0", 7)
	if !errors.Is(err, unix.EBUSY) {
		t.Fatalf("error no longer wraps EBUSY: %v", err)
	}
	if msg := err.Error(); strings.Contains(msg, "not both") {
		t.Errorf("claimed a mode conflict with no evidence of one: %s", msg)
	}
}

// Children of OTHER parents must never be read as the blocker. The scan
// filters on ParentIndex, and a message naming the wrong network would
// send an operator to change something that is not the problem.
func TestExplainChildLinkAdd_IgnoresChildrenOfOtherParents(t *testing.T) {
	withLinkList(t, []netlink.Link{
		childOn(t, ModeMacvlan, 99),
	}, nil)

	if msg := explainChildLinkAdd(unix.EBUSY, ModeIPvlan, "eth0", 7).Error(); strings.Contains(msg, "not both") {
		t.Errorf("a macvlan child of a different parent was reported as the conflict: %s", msg)
	}
}

// Anything that is not EBUSY keeps the original shape. The explanation
// is specific to one kernel condition and must not be pasted onto
// unrelated failures.
func TestExplainChildLinkAdd_NonBusyIsUnchanged(t *testing.T) {
	boom := errors.New("boom")

	err := explainChildLinkAdd(boom, ModeMacvlan, "eth0", 7)
	if !errors.Is(err, boom) {
		t.Fatalf("original error lost: %v", err)
	}
	if msg := err.Error(); strings.Contains(msg, "not both") || strings.Contains(msg, "torn down") {
		t.Errorf("EBUSY explanation applied to an unrelated error: %s", msg)
	}
}

// The probe attaches as the same kind the network's endpoints will.
//
// This is the whole of #486's product half. A macvlan probe on an ipvlan
// network is refused by the kernel as soon as any ipvlan container is
// running on that parent, and while it runs it blocks every ipvlan
// endpoint there — so `-o mode=ipvlan -o validate_dhcp=true` failed for
// a reason that had nothing to do with DHCP.
func TestNewProbeLink_MatchesTheNetworkMode(t *testing.T) {
	mac, err := net.ParseMAC("02:11:22:33:44:55")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}

	t.Run("ipvlan network gets an ipvlan probe", func(t *testing.T) {
		link := newProbeLink(ModeIPvlan, "dh-probe-a1b2c3", 7, mac)

		if _, ok := link.(*netlink.IPVlan); !ok {
			t.Fatalf("probe link is %T, want *netlink.IPVlan — a macvlan probe "+
				"cannot coexist with the ipvlan endpoints this network will create", link)
		}
		// The kernel rejects a MAC on an ipvlan child outright, so
		// setting one turns the probe into a hard failure.
		if got := link.Attrs().HardwareAddr; got != nil {
			t.Errorf("ipvlan probe link carries HardwareAddr %v, want none", got)
		}
	})

	t.Run("macvlan network gets a macvlan probe with the probe MAC", func(t *testing.T) {
		link := newProbeLink(ModeMacvlan, "dh-probe-a1b2c3", 7, mac)

		if _, ok := link.(*netlink.Macvlan); !ok {
			t.Fatalf("probe link is %T, want *netlink.Macvlan", link)
		}
		// Random and locally administered, so the DISCOVER cannot land
		// on a stable upstream reservation.
		if got := link.Attrs().HardwareAddr; got.String() != mac.String() {
			t.Errorf("macvlan probe MAC = %v, want %v", got, mac)
		}
	})

	t.Run("both attach to the parent given", func(t *testing.T) {
		for _, mode := range []string{ModeMacvlan, ModeIPvlan} {
			if got := newProbeLink(mode, "dh-probe-a1b2c3", 7, mac).Attrs().ParentIndex; got != 7 {
				t.Errorf("%s probe ParentIndex = %d, want 7", mode, got)
			}
		}
	})
}
