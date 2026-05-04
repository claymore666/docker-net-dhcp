//go:build integration

package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

// CreateNetwork drives `docker network create` with the plugin and
// the per-test options. Registers a t.Cleanup to delete it. Returns
// the network ID.
//
// mode is "bridge", "macvlan", or "ipvlan". For bridge mode pass
// bridge=<name>; for macvlan/ipvlan pass parent=<name>. The harness
// expects HostVeth to be the parent for macvlan/ipvlan — keeps the
// test surface simple.
func CreateNetwork(t *testing.T, ctx context.Context, name, mode string, extraOpts map[string]string) string {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	opts := map[string]string{"mode": mode}
	switch mode {
	case "macvlan", "ipvlan":
		opts["parent"] = HostVeth
	}
	// Bridge mode needs its own fixture — see test/integration/README.md.
	for k, v := range extraOpts {
		opts[k] = v
	}

	res, err := cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver:  DriverName,
		IPAM:    &network.IPAM{Driver: "null"},
		Options: opts,
	})
	if err != nil {
		t.Fatalf("NetworkCreate(%s, mode=%s, opts=%v): %v", name, mode, opts, err)
	}
	t.Cleanup(func() {
		// Use a fresh context so a parent ctx-cancel during a
		// failure doesn't skip cleanup.
		if err := cli.NetworkRemove(context.Background(), res.ID); err != nil && !isNotFound(err) {
			t.Logf("WARN: NetworkRemove(%s): %v", res.ID, err)
		}
	})
	return res.ID
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "No such network")
}
