// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	docker "github.com/moby/moby/client"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The ipvlan child sits in its container's namespace, out of the host's link table, so the kernel's EBUSY for a
// macvlan child on that parent names no occupant; the plugin's own endpoint list is the only attribution (#1372).

// TestEBUSYCensus_NamesTheOtherKindsEndpointOnTheParent checks that a macvlan child refused beside a running ipvlan container names that container's endpoint and network.
func TestEBUSYCensus_NamesTheOtherKindsEndpointOnTheParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		occupantNet = "dh-itest-census-ipv"
		occupantCtr = "dh-itest-census-ipv-ctr"
		refusedNet  = "dh-itest-census-mv"
		refusedCtr  = "dh-itest-census-mv-ctr"
	)

	t.Cleanup(func() {
		if t.Failed() {
			harness.DumpPluginLog(t)
		}
	})

	cli, err := harness.NewDockerClient()
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	occupantNetID := harness.CreateNetwork(t, ctx, occupantNet, "ipvlan", nil)
	occupantID, _, _ := harness.RunContainer(t, ctx, occupantNet, occupantCtr)
	occupantEndpoint := harness.EndpointShortID(t, ctx, cli, occupantID, occupantNet)

	// CreateNetwork asserts the parent shows no ipvlan child in the host namespace before the macvlan network exists.
	harness.CreateNetwork(t, ctx, refusedNet, "macvlan", map[string]string{"parent": harness.IpvlanParent})

	create, err := cli.ContainerCreate(ctx, docker.ContainerCreateOptions{
		Config:     &container.Config{Image: harness.TestImage, Cmd: []string{"sleep", "infinity"}, Hostname: refusedCtr},
		HostConfig: harness.HostConfig(),
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			refusedNet: {},
		}},
		Name: refusedCtr,
	})
	if err != nil {
		t.Fatalf("ContainerCreate %s: %v", refusedCtr, err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), create.ID, docker.ContainerRemoveOptions{Force: true})
	})

	_, err = cli.ContainerStart(ctx, create.ID, docker.ContainerStartOptions{})
	if err == nil {
		t.Fatalf("a macvlan child started on %s beside the running ipvlan container %s; the kernel refuses that "+
			"with EBUSY (#486), so the fixture no longer produces the case", harness.IpvlanParent, occupantCtr)
	}
	msg := err.Error()
	for _, want := range []string{
		"device or resource busy",
		"endpoint " + occupantEndpoint + " (network " + occupantNetID[:12] + ", ipvlan, ",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not carry %q, so the running ipvlan endpoint the daemon reports on %s is not "+
				"named as the parent's occupant: %s", want, harness.IpvlanParent, msg)
		}
	}
}
