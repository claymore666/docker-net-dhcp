//go:build integration

package harness

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
)

const (
	// TestImage is what `docker run` pulls if absent. alpine:3.20 is
	// pinned so a registry blip doesn't suddenly change runtime
	// behaviour mid-test-run; busybox-shipping `ip` is what we need.
	TestImage = "alpine:3.20"
	// IPAcquisitionBudget caps how long Run waits for a container to
	// have a non-empty IP after start. The plugin's CreateEndpoint
	// returns synchronously after udhcpc gets a lease, so the docker
	// inspect should reflect the IP within milliseconds; the budget
	// is generous to absorb real-world dnsmasq RTTs and image pulls.
	IPAcquisitionBudget = 15 * time.Second
)

// EnsureImage pulls TestImage if not already present locally. Run from
// TestMain to amortize the pull across the whole suite.
func EnsureImage(ctx context.Context) error {
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()
	// Try inspect first; pull only on miss.
	if _, err := cli.ImageInspect(ctx, TestImage); err == nil {
		return nil
	}
	rc, err := cli.ImagePull(ctx, TestImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("ImagePull: %w", err)
	}
	defer rc.Close()
	// Drain the stream so the pull completes synchronously.
	buf := make([]byte, 4096)
	for {
		_, err := rc.Read(buf)
		if err != nil {
			break
		}
	}
	return nil
}

// RunContainer starts a long-lived alpine container attached to the
// given plugin-driven network and returns its container ID once the
// IP is available. Registers a t.Cleanup to stop+remove the container.
//
// The container runs `sleep infinity`, so tests can exec into it for
// connectivity checks. cmd is appended to the args if you want a
// different entrypoint shape.
func RunContainer(t *testing.T, ctx context.Context, networkName, containerName string) (id, ipv4, mac string) {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: TestImage,
			Cmd:   []string{"sleep", "infinity"},
			// Hostname surfaces in the tombstone for restart-stability tests.
			Hostname: containerName,
		},
		&container.HostConfig{
			AutoRemove: false, // we remove explicitly in cleanup
		},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				networkName: {},
			},
		},
		nil,
		containerName,
	)
	if err != nil {
		t.Fatalf("ContainerCreate(%s): %v", containerName, err)
	}
	id = create.ID
	t.Cleanup(func() {
		// Best-effort kill+remove; logs the error but doesn't fail the test.
		bg := context.Background()
		_ = cli.ContainerStop(bg, id, container.StopOptions{})
		if err := cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true}); err != nil && !isNotFound(err) {
			t.Logf("WARN: ContainerRemove(%s): %v", id, err)
		}
	})

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart(%s): %v", id, err)
	}

	// Poll docker inspect until the network endpoint reports an IP.
	deadline := time.Now().Add(IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect(%s): %v", id, err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				return id, ep.IPAddress, ep.MacAddress
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("container %s did not get an IP within %v", containerName, IPAcquisitionBudget)
	return // unreachable
}

// ExecOutput runs `docker exec` with the given args and returns
// combined stdout+stderr as a string. Use for quick assertions like
// `ip -4 addr show eth0` from inside a test container.
func ExecOutput(t *testing.T, ctx context.Context, containerID string, cmd ...string) string {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	exec, err := cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		t.Fatalf("ExecCreate: %v", err)
	}
	att, err := cli.ContainerExecAttach(ctx, exec.ID, container.ExecStartOptions{})
	if err != nil {
		t.Fatalf("ExecAttach: %v", err)
	}
	defer att.Close()
	buf := make([]byte, 8192)
	var out strings.Builder
	for {
		n, err := att.Reader.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return out.String()
}

// AssertIP fails the test if got is not a valid IPv4 in the DHCP pool.
// Common helper to keep the assertion phrasing consistent.
func AssertIP(t *testing.T, got string) net.IP {
	t.Helper()
	ip := net.ParseIP(got)
	if ip == nil {
		t.Fatalf("not a valid IP: %q", got)
	}
	if ip.To4() == nil {
		t.Fatalf("not an IPv4: %q", got)
	}
	if !IsInPool(ip) {
		t.Fatalf("IP %q outside DHCP pool [%s, %s]", got, DHCPPoolStart, DHCPPoolEnd)
	}
	return ip
}
