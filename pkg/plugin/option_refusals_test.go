// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/proto"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// Each case is what the daemon passes at `docker network create` (#1240).
func TestCreateNetwork_OptionsThatWouldFailEveryContainerAreRefused(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	cases := []struct {
		name  string
		opts  map[string]interface{}
		names []string // nil means accepted
	}{
		{"gateway that is not an address", map[string]interface{}{"gateway": "not-an-ip"}, []string{"gateway", "not-an-ip"}},
		{"gateway in CIDR form", map[string]interface{}{"gateway": "192.168.0.1/24"}, []string{"gateway", "192.168.0.1/24"}},
		{"gateway with an octet over 255", map[string]interface{}{"gateway": "999.1.1.1"}, []string{"gateway", "999.1.1.1"}},
		{"gateway that is IPv6", map[string]interface{}{"gateway": "2001:db8::1"}, []string{"gateway", "2001:db8::1", "IPv4"}},
		{"gateway that is link-local IPv6", map[string]interface{}{"gateway": "fe80::1"}, []string{"gateway", "fe80::1", "IPv4"}},
		{"gateway that is IPv4-mapped IPv6", map[string]interface{}{"gateway": "::ffff:192.168.0.1"}, []string{"gateway"}},
		{"gateway with a space", map[string]interface{}{"gateway": " 192.168.0.1"}, []string{"gateway"}},
		{"gateway 0.0.0.0", map[string]interface{}{"gateway": "0.0.0.0"}, []string{"gateway", "0.0.0.0"}},
		{"gateway 255.255.255.255", map[string]interface{}{"gateway": "255.255.255.255"}, []string{"gateway"}},
		{"gateway that is multicast", map[string]interface{}{"gateway": "224.0.0.1"}, []string{"gateway"}},
		{"gateway that is loopback", map[string]interface{}{"gateway": "127.0.0.1"}, []string{"gateway"}},
		{"gateway that is a plain IPv4 address", map[string]interface{}{"gateway": "192.168.0.1"}, nil},
		{"gateway that is an IPv4 link-local address", map[string]interface{}{"gateway": "169.254.0.1"}, nil},
		{"gateway left empty is unset", map[string]interface{}{"gateway": ""}, nil},

		{"vendor_class of 256 octets", map[string]interface{}{"vendor_class": long(256)}, []string{"vendor_class", "256", "255"}},
		{"vendor_class of 255 octets", map[string]interface{}{"vendor_class": long(255)}, nil},
		{"client_id of 255 octets, 256 with its type byte", map[string]interface{}{"client_id": long(255)}, []string{"client_id", "255", "254"}},
		{"client_id of 254 octets, 255 with its type byte", map[string]interface{}{"client_id": long(254)}, nil},

		{"negative lease_timeout", map[string]interface{}{"lease_timeout": "-30s"}, []string{"lease_timeout", "-30s"}},
		{"negative lease_timeout of one nanosecond", map[string]interface{}{"lease_timeout": "-1ns"}, []string{"lease_timeout"}},
		{"negative lease_timeout with the probe off", map[string]interface{}{"lease_timeout": "-30s", "conflict_check": "off"}, []string{"lease_timeout"}},
		{"negative lease_timeout with the probe async", map[string]interface{}{"lease_timeout": "-30s", "conflict_check": "async"}, []string{"lease_timeout"}},
		{"lease_timeout of zero is unset", map[string]interface{}{"lease_timeout": "0s"}, nil},
		{"lease_timeout of one minute", map[string]interface{}{"lease_timeout": "1m"}, nil},

		{"mtu 1279 with ipv6_mode=dhcp", map[string]interface{}{"mtu": "1279", "ipv6_mode": "dhcp"}, []string{"mtu=1279", "1280", "ipv6_mode=dhcp"}},
		{"mtu 1000 with ipv6_mode=slaac", map[string]interface{}{"mtu": "1000", "ipv6_mode": "slaac"}, []string{"mtu=1000", "1280", "ipv6_mode=slaac"}},
		{"mtu 1000 with ipv6_mode=auto", map[string]interface{}{"mtu": "1000", "ipv6_mode": "auto"}, []string{"mtu=1000", "1280", "ipv6_mode=auto"}},
		{"mtu 1000 with the ipv6=true spelling", map[string]interface{}{"mtu": "1000", "ipv6": "true"}, []string{"mtu=1000", "1280", "ipv6_mode=dhcp"}},
		{"mtu 1280 with ipv6_mode=dhcp", map[string]interface{}{"mtu": "1280", "ipv6_mode": "dhcp"}, nil},
		{"mtu 1000 with IPv6 off", map[string]interface{}{"mtu": "1000"}, nil},
		{"mtu 1000 with ipv6_mode=off", map[string]interface{}{"mtu": "1000", "ipv6_mode": "off"}, nil},
		{"mtu 68 with IPv6 off", map[string]interface{}{"mtu": "68"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withStateDir(t, t.TempDir())
			(&mtuKernel{parentMTU: 1500}).install(t)
			p := newPluginForTest()
			p.docker = &fakeDocker{}
			opts := map[string]interface{}{"bridge": mtuTestBridge}
			for k, v := range c.opts {
				opts[k] = v
			}
			err := mtuCreateNetwork(p, opts)
			_, loadErr := loadOptions(mtuTestNetwork)
			if c.names == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%v was accepted", c.opts)
			}
			if !errors.Is(err, util.ErrIPAM) {
				t.Errorf("err = %v, want ErrIPAM so the daemon answers 400", err)
			}
			if loadErr == nil {
				t.Errorf("a refused network left a stored record")
			}
			for _, n := range c.names {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("the refusal %q does not name %q", err, n)
				}
			}
		})
	}
}

// The refusals are create-only, so a restart never turns an accepted network into one that cannot load (#1240).
func TestCheckStoredOptions_LoadsNetworksCreatedBeforeTheOptionRefusals(t *testing.T) {
	long := func(n int) string { return strings.Repeat("x", n) }
	for name, opts := range map[string]DHCPNetworkOptions{
		"bad gateway":       {Bridge: "br0", Gateway: "192.168.0.1/24"},
		"v6 gateway":        {Bridge: "br0", Gateway: "fe80::1"},
		"long vendor_class": {Bridge: "br0", VendorClass: long(256)},
		"long client_id":    {Bridge: "br0", ClientID: long(255)},
		"negative timeout":  {Bridge: "br0", LeaseTimeout: -30e9},
		"mtu under 1280":    {Bridge: "br0", MTU: 1000, IPv6Mode: "dhcp"},
	} {
		t.Run(name, func(t *testing.T) {
			p := &Plugin{}
			if err := p.checkStoredOptions("n1", opts); err != nil {
				t.Errorf("checkStoredOptions refused a stored network: %v", err)
			}
			if got := p.networkOptionsRejected.Load(); got != 0 {
				t.Errorf("network_options_rejected = %d, want 0", got)
			}
		})
	}
}

func TestCheckLeaseTimeout_NegativeIsRefusedInEveryMode(t *testing.T) {
	for _, mode := range proto.AllConflictModes() {
		for _, d := range []time.Duration{-1, -30 * time.Second} {
			err := dhcp.CheckLeaseTimeout(d, mode)
			if err == nil || !strings.Contains(err.Error(), "lease_timeout") {
				t.Errorf("CheckLeaseTimeout(%v, %v) = %v, want a refusal naming lease_timeout", d, mode, err)
			}
		}
		if err := dhcp.CheckLeaseTimeout(0, mode); err != nil {
			t.Errorf("CheckLeaseTimeout(0, %v) = %v: zero is the derived default", mode, err)
		}
	}
}
