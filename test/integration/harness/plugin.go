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

// PluginRef is the docker plugin reference the harness expects to be
// installed and enabled before the test run starts. We deliberately
// don't install/enable from within tests — that's a global daemon
// mutation and conflicts with whatever the operator already has set
// up. The runner's pre-test step handles install (or not).
//
// The default ":golang" matches the production install. A run can
// point the harness at a different tag (e.g. ":dev" for a code
// change being verified before the ":golang" slot is bumped) by
// setting INTEGRATION_PLUGIN_REF in the environment.
var PluginRef = func() string {
	if v := os.Getenv("INTEGRATION_PLUGIN_REF"); v != "" {
		return v
	}
	return "ghcr.io/claymore666/docker-net-dhcp:golang"
}()

// VerifyPluginEnabled checks that PluginRef is installed and currently
// enabled in the local Docker daemon. Use from TestMain so the suite
// fails fast with a clear message instead of every test failing
// downstream when network create can't find the driver.
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

// DriverName is the network driver name to pass to docker network
// create — same as PluginRef. Aliased for readability.
var DriverName = PluginRef
