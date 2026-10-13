// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// Docker's null IPAM answers a v6 RequestPool with ::/0 and a RequestAddress with no address, and the engine then
// sets disable_ipv6=1 on an endpoint link with no v6 address, so the flag alone gives no IPv6 (#1343).
func nullIPAMIPv6Request(generic map[string]interface{}, engineIPv6 interface{}) CreateNetworkRequest {
	r := CreateNetworkRequest{
		NetworkID: mtuTestNetwork,
		Options: map[string]interface{}{
			util.OptionsKeyGeneric:           generic,
			"com.docker.network.enable_ipv4": true,
		},
		IPv4Data: []*IPAMData{{AddressSpace: "null", Pool: "0.0.0.0/0"}},
	}
	if engineIPv6 != nil {
		r.Options["com.docker.network.enable_ipv6"] = engineIPv6
	}
	if b, ok := engineIPv6.(bool); ok && b {
		r.IPv6Data = []*IPAMData{{AddressSpace: "null", Pool: "::/0"}}
	}
	return r
}

func TestCreateNetwork_DockerIPv6OnNullIPAMEnablesIPv6(t *testing.T) {
	for _, c := range []struct {
		name       string
		opts       map[string]interface{}
		engineIPv6 interface{}
		wantV6     bool
		wantRefuse string
	}{
		{"--ipv6 alone", map[string]interface{}{}, true, true, ""},
		{"--ipv6 as the string true", map[string]interface{}{}, "true", true, ""},
		{"--ipv6 with ipv6_mode=dhcp", map[string]interface{}{"ipv6_mode": "dhcp"}, true, true, ""},
		{"--ipv6 with ipv6=true", map[string]interface{}{"ipv6": "true"}, true, true, ""},
		{"--ipv6 with ipv6_mode=off", map[string]interface{}{"ipv6_mode": "off"}, true, false, "ipv6_mode=off"},
		{"--ipv6 as something that is not a boolean", map[string]interface{}{}, "maybe", false, "enable_ipv6"},
		{"no --ipv6 and no option", map[string]interface{}{}, false, false, ""},
		{"the flag absent", map[string]interface{}{}, nil, false, ""},
		{"--ipv6 with an explicit ipv6=false", map[string]interface{}{"ipv6": "false"}, true, false, ""},
		{"--ipv6 on ipvlan", map[string]interface{}{"mode": "ipvlan", "bridge": "", "parent": mtuTestParent}, true, true, ""},
		{"no --ipv6 with ipv6=true", map[string]interface{}{"ipv6": "true"}, false, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			(&mtuKernel{parentMTU: 1500}).install(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			generic := map[string]interface{}{"bridge": mtuTestBridge}
			for k, v := range c.opts {
				generic[k] = v
				if v == "" {
					delete(generic, k)
				}
			}
			err := p.CreateNetwork(nullIPAMIPv6Request(generic, c.engineIPv6))
			if c.wantRefuse != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantRefuse) {
					t.Fatalf("want a refusal naming %q, got %v", c.wantRefuse, err)
				}
				if !errors.Is(err, util.ErrIPAM) {
					t.Errorf("the refusal %v is not an ErrIPAM", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			stored, lerr := loadOptions(mtuTestNetwork)
			if lerr != nil {
				t.Fatalf("loadOptions: %v", lerr)
			}
			if got := stored.ipv6Enabled(); got != c.wantV6 {
				t.Errorf("stored network reports IPv6 %v, want %v (opts %+v)", got, c.wantV6, stored)
			}
			if mode, merr := stored.ipv6Mode(); c.wantV6 && (merr != nil || mode != proto.Mode6DHCP) {
				t.Errorf("the flag resolved to IPv6 mode %v (%v), want dhcp, the one mode every driver mode takes", mode, merr)
			}
		})
	}
}
