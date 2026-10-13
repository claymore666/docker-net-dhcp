// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

func TestCreateNetwork_ConflictCheckOnIpvlan(t *testing.T) {
	cases := []struct {
		name string
		opts map[string]interface{}
		bad  bool
	}{
		{"wait is refused", map[string]interface{}{"mode": ModeIPvlan, "conflict_check": "wait"}, true},
		{"async is accepted", map[string]interface{}{"mode": ModeIPvlan, "conflict_check": "async"}, false},
		{"off is accepted", map[string]interface{}{"mode": ModeIPvlan, "conflict_check": "off"}, false},
		{"absent is accepted", map[string]interface{}{"mode": ModeIPvlan}, false},
		{"absent with a lease_timeout below the wait window is accepted", map[string]interface{}{"mode": ModeIPvlan, "lease_timeout": "3s"}, false},
		{"wait stays accepted on macvlan", map[string]interface{}{"mode": ModeMacvlan, "conflict_check": "wait"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			(&mtuKernel{parentMTU: 1500}).install(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			c.opts["parent"] = mtuTestParent
			err := mtuCreateNetwork(p, c.opts)
			if !c.bad {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted")
			}
			if !errors.Is(err, util.ErrModeMismatch) {
				t.Errorf("%v is not a mode mismatch", err)
			}
			for _, want := range []string{"conflict_check=wait", "ipvlan", "async", "off", "RFC 5227"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
			}
		})
	}
}

func TestConflictWiring_AnAbsentConflictCheckOnIpvlanRunsAsync(t *testing.T) {
	cases := []struct {
		name string
		opts DHCPNetworkOptions
		want proto.ConflictMode
	}{
		{"ipvlan absent", DHCPNetworkOptions{Mode: ModeIPvlan}, proto.ConflictAsync},
		{"ipvlan off", DHCPNetworkOptions{Mode: ModeIPvlan, ConflictCheck: "off"}, proto.ConflictOff},
		{"ipvlan async", DHCPNetworkOptions{Mode: ModeIPvlan, ConflictCheck: "async"}, proto.ConflictAsync},
		{"ipvlan wait stored by an earlier release", DHCPNetworkOptions{Mode: ModeIPvlan, ConflictCheck: "wait"}, proto.ConflictWait},
		{"macvlan absent", DHCPNetworkOptions{Mode: ModeMacvlan}, mustConflictMode(t, dhcp.DefaultConflictCheck)},
		{"bridge absent", DHCPNetworkOptions{}, mustConflictMode(t, dhcp.DefaultConflictCheck)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var o dhcp.DHCPClientOptions
			if err := newHealthPlugin().conflictWiring(&o, c.opts, roleAcquire, "net", "ep", false); err != nil {
				t.Fatal(err)
			}
			if o.ConflictMode != c.want {
				t.Errorf("the client runs %v, want %v", o.ConflictMode, c.want)
			}
		})
	}
}

func mustConflictMode(t *testing.T, v string) proto.ConflictMode {
	t.Helper()
	m, err := dhcp.ParseConflictCheck(v)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
