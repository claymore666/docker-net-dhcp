// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"
)

// NewDockerClient is the one engine client the tests dial with; negotiation is the default (#178).
func NewDockerClient() (*docker.Client, error) {
	return docker.New(docker.FromEnv)
}

// AddrString and PrefixString give "" for an unset value, which String prints "invalid IP" (#178).
func AddrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func PrefixString(p netip.Prefix) string {
	if !p.IsValid() {
		return ""
	}
	return p.String()
}

func MustMAC(s string) network.HardwareAddr {
	hw, err := net.ParseMAC(s)
	if err != nil {
		panic(fmt.Sprintf("MustMAC(%q): %v", s, err))
	}
	return network.HardwareAddr(hw)
}
