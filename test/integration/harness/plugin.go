// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/docker/docker/api/types/filters"
	docker "github.com/docker/docker/client"
)

// PluginRef is the plugin the run's pre-test step installed; the harness never installs one. INTEGRATION_PLUGIN_REF overrides it.
var PluginRef = func() string {
	if v := os.Getenv("INTEGRATION_PLUGIN_REF"); v != "" {
		return v
	}
	return "ghcr.io/claymore666/docker-net-dhcp:golang"
}()

// VerifyPluginEnabled checks that PluginRef is installed and enabled.
func VerifyPluginEnabled(ctx context.Context) error {
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()
	plugins, err := cli.PluginList(ctx, filters.NewArgs())
	if err != nil {
		return fmt.Errorf("PluginList: %w", err)
	}
	for _, p := range plugins {
		if p.Name == PluginRef && p.Enabled {
			return nil
		}
	}
	available := []string{}
	for _, p := range plugins {
		available = append(available, fmt.Sprintf("%s(enabled=%v)", p.Name, p.Enabled))
	}
	return fmt.Errorf("plugin %q is not enabled. Available: %s. Install/enable it before running integration tests", PluginRef, strings.Join(available, ", "))
}

// DriverName is the network driver name, the same as PluginRef.
var DriverName = PluginRef
