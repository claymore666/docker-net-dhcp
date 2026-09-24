// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// errorCases is TestErrors_NetworkCreateValidation's table; opts is the complete driver-options map and wantSubstr a case-insensitive substring.
var errorCases = []struct {
	name       string
	opts       map[string]string
	ipam       *network.IPAM // nil → null IPAM (the supported case)
	wantSubstr string
}{
	{
		name:       "InvalidMode",
		opts:       map[string]string{"mode": "moonbridge"},
		wantSubstr: "invalid mode",
	},
	{
		name:       "MacvlanMissingParent",
		opts:       map[string]string{"mode": "macvlan"},
		wantSubstr: "parent required",
	},
	{
		// The mode check refuses a foreign option for the mode: ErrModeMismatch.
		name: "MacvlanWithBridge",
		opts: map[string]string{
			"mode":   "macvlan",
			"parent": harness.HostVeth,
			"bridge": "foo",
		},
		wantSubstr: "does not apply to selected mode",
	},
	{
		name: "IPAMNotNull",
		opts: map[string]string{
			"mode":   "macvlan",
			"parent": harness.HostVeth,
		},
		ipam:       &network.IPAM{Driver: "default"},
		wantSubstr: "null IPAM driver",
	},
	{
		// dhcp_servers and dhcp_deny_servers are validated before any mode-specific check.
		name: "DHCPServersNotAnIP",
		opts: map[string]string{
			"mode":         "macvlan",
			"parent":       harness.HostVeth,
			"dhcp_servers": "dhcp.example.com",
		},
		wantSubstr: "is not an IP address",
	},
	{
		// A v6 entry is refused, not ignored: dhcpcd's whitelist and blacklist are DHCPv4-only (#111).
		name: "DHCPServersIPv6",
		opts: map[string]string{
			"mode":         "macvlan",
			"parent":       harness.HostVeth,
			"dhcp_servers": "fd00::1",
		},
		wantSubstr: "DHCPv4-only",
	},
	{
		name: "DenyServersNotAnIP",
		opts: map[string]string{
			"mode":              "macvlan",
			"parent":            harness.HostVeth,
			"dhcp_deny_servers": "192.168.100.1/24",
		},
		wantSubstr: "is not an IP address",
	},
	{
		// Denying every preferred server would turn the network into "any server will do" (#669).
		name: "DenyEmptiesPreference",
		opts: map[string]string{
			"mode":              "macvlan",
			"parent":            harness.HostVeth,
			"dhcp_servers":      "192.168.100.1",
			"dhcp_deny_servers": "192.168.100.1",
		},
		wantSubstr: "leaving no server to lease from",
	},
}

// TestErrors_NetworkCreateValidation checks that each row is refused at network create, before any DHCP traffic.
func TestErrors_NetworkCreateValidation(t *testing.T) {
	for _, tc := range errorCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
			if err != nil {
				t.Fatalf("docker client: %v", err)
			}
			defer cli.Close()

			ipam := tc.ipam
			if ipam == nil {
				ipam = &network.IPAM{Driver: "null"}
			}

			netName := "dh-itest-err-" + strings.ToLower(tc.name)
			res, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
				Driver:  harness.DriverName,
				IPAM:    ipam,
				Options: tc.opts,
			})
			if err == nil {
				_ = cli.NetworkRemove(context.Background(), res.ID)
				t.Fatalf("expected NetworkCreate to fail with %q substring, got success", tc.wantSubstr)
			}
			msg := err.Error()
			if !strings.Contains(strings.ToLower(msg), strings.ToLower(tc.wantSubstr)) {
				t.Errorf("error message missing expected substring %q\nactual: %s", tc.wantSubstr, msg)
			} else {
				t.Logf("✓ %s rejected: %s", tc.name, msg)
			}
		})
	}
}
