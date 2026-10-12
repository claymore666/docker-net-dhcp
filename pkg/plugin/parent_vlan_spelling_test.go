// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

const mtuTestDownVlan = mtuTestParent + ".200"

func TestCreateNetwork_AMissingDottedParentNamesTheVlanOption(t *testing.T) {
	cases := []struct {
		name, parent string
		remedy       string
		refusal      string
	}{
		{"a missing id on an existing link", mtuTestParent + ".100", "use -o parent=" + mtuTestParent + " -o vlan=100", "failed to lookup parent interface"},
		{"the highest id", mtuTestParent + ".4094", "use -o parent=" + mtuTestParent + " -o vlan=4094", "failed to lookup parent interface"},
		{"id zero", mtuTestParent + ".0", "", "failed to lookup parent interface"},
		{"an id above the range", mtuTestParent + ".4095", "", "failed to lookup parent interface"},
		{"a non-numeric suffix", mtuTestParent + ".abc", "", "failed to lookup parent interface"},
		{"a missing base link", "eth-gone.100", "", "failed to lookup parent interface"},
		{"no dot", "eth-gone", "", "failed to lookup parent interface"},
		{"a dotted parent that exists but is down", mtuTestDownVlan, "", "parent interface is down"},
	}
	for _, mode := range []string{ModeMacvlan, ModeIPvlan} {
		for _, c := range cases {
			t.Run(mode+"/"+c.name, func(t *testing.T) {
				withStateDir(t, t.TempDir())
				installWorkingNotFound(t)
				p := newPluginForTest()
				p.docker = &fakeDocker{}
				err := mtuCreateNetwork(p, map[string]interface{}{"mode": mode, "parent": c.parent})
				if err == nil {
					t.Fatalf("a parent that cannot be used was accepted")
				}
				if !strings.Contains(err.Error(), c.refusal) {
					t.Errorf("the refusal %q lost %q", err, c.refusal)
				}
				if c.remedy != "" && !strings.Contains(err.Error(), c.remedy) {
					t.Errorf("the refusal %q does not say %q", err, c.remedy)
				}
				if c.remedy == "" && strings.Contains(err.Error(), "-o vlan=") {
					t.Errorf("the refusal %q names -o vlan for a parent the remedy does not fit", err)
				}
			})
		}
	}
}

// installWorkingNotFound installs the mtuKernel lookup with a not-found error whose text can be formatted: the zero
// netlink.LinkNotFoundError panics in Error() (#1345).
func installWorkingNotFound(t *testing.T) {
	t.Helper()
	(&mtuKernel{parentMTU: 1500}).install(t)
	byName := nlLinkByName
	nlLinkByName = func(name string) (netlink.Link, error) {
		if name == mtuTestDownVlan {
			return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: name, Index: 9}}, nil
		}
		l, err := byName(name)
		if err != nil {
			return nil, errors.New("Link not found")
		}
		return l, nil
	}
}

func TestCreateNetwork_ABridgeWithAMissingDottedParentIsNotSentToTheVlanOption(t *testing.T) {
	withStateDir(t, t.TempDir())
	installWorkingNotFound(t)
	p := newPluginForTest()
	p.docker = &fakeDocker{}
	err := mtuCreateNetwork(p, map[string]interface{}{"mode": ModeBridge, "bridge": "lanbr", "force_create": "true", "parent": mtuTestParent + ".100"})
	if err == nil || !strings.Contains(err.Error(), "failed to lookup parent interface "+mtuTestParent+".100") {
		t.Fatalf("the bridge create did not reach the parent lookup: %v", err)
	}
	if strings.Contains(err.Error(), "-o vlan=") {
		t.Errorf("the refusal %q sends a bridge network to -o vlan, which bridge mode refuses", err)
	}
}
