// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	docker "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	// TestImage is pinned so a registry change cannot alter runtime behaviour mid-run.
	TestImage = "alpine:3.20"
	// IPAcquisitionBudget caps how long Run waits for a started container to report an IP.
	IPAcquisitionBudget = 15 * time.Second
)

// HostConfig is the HostConfig every test container must be created with.
// Init is set because the kernel discards SIGTERM for PID 1 without a handler, so `sleep infinity` waited out the
// 10 s stop grace: measured 10.16 s per teardown, 0.18 s with init (#367). The container still exits 143, not 137, so
// the graceful Leave path the health and audit tests read is kept; `docker stop -t 0` would bypass it. Since #800 that
// path ends in a DHCPRELEASE only with release_lease set. Callers mutate the returned struct, so each call allocates.
func HostConfig() *container.HostConfig {
	init := true
	return &container.HostConfig{
		AutoRemove: false,
		Init:       &init,
	}
}

// EnsureImage pulls TestImage if it is not present locally.
func EnsureImage(ctx context.Context) error {
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer cli.Close()
	if _, err := cli.ImageInspect(ctx, TestImage); err == nil {
		return nil
	}
	rc, err := cli.ImagePull(ctx, TestImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("ImagePull: %w", err)
	}
	defer rc.Close()
	// Decode each JSON line so a mid-stream errorDetail fails the pull instead of leaving it partial.
	dec := json.NewDecoder(rc)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode pull stream: %w", err)
		}
		if msg.Error != "" {
			return fmt.Errorf("image pull: %s", msg.Error)
		}
		if msg.ErrorDetail.Message != "" {
			return fmt.Errorf("image pull: %s", msg.ErrorDetail.Message)
		}
	}
}

// RunContainer starts a `sleep infinity` container on networkName, registers its removal and returns once it has an IP.
func RunContainer(t *testing.T, ctx context.Context, networkName, containerName string) (id, ipv4, mac string) {
	t.Helper()
	return runContainer(t, ctx, networkName, containerName, "", HostConfig())
}

// RunContainerUser is RunContainer with a container user; a non-root user changes the ptrace check on /proc/<pid>/ns/net
// the plugin's client must pass at Join (#317).
func RunContainerUser(t *testing.T, ctx context.Context, networkName, containerName, user string) (id, ipv4, mac string) {
	t.Helper()
	return runContainer(t, ctx, networkName, containerName, user, HostConfig())
}

func runContainer(t *testing.T, ctx context.Context, networkName, containerName, user string, hostCfg *container.HostConfig) (id, ipv4, mac string) {
	t.Helper()
	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	createStart := time.Now()
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:    TestImage,
			Cmd:      []string{"sleep", "infinity"},
			Hostname: containerName,
			User:     user,
		},
		hostCfg,
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				networkName: {},
			},
		},
		nil,
		containerName,
	)
	EndPhase(t, PhaseContainerCreate, createStart)
	if err != nil {
		t.Fatalf("ContainerCreate(%s): %v", containerName, err)
	}
	id = create.ID
	t.Cleanup(func() {
		// Stop and remove are timed separately (#368): stop is the signal round-trip #367 collapsed, remove is disk work.
		bg := context.Background()
		stopStart := time.Now()
		_ = cli.ContainerStop(bg, id, container.StopOptions{})
		EndPhase(t, PhaseContainerStop, stopStart)

		removeStart := time.Now()
		err := cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
		EndPhase(t, PhaseContainerRemove, removeStart)
		if err != nil && !isNotFound(err) {
			t.Logf("WARN: ContainerRemove(%s): %v", id, err)
		}
	})

	startStart := time.Now()
	err = cli.ContainerStart(ctx, id, container.StartOptions{})
	EndPhase(t, PhaseContainerStart, startStart)
	if err != nil {
		t.Fatalf("ContainerStart(%s): %v", id, err)
	}

	acquireStart := time.Now()
	deadline := time.Now().Add(IPAcquisitionBudget)
	for time.Now().Before(deadline) {
		ins, err := cli.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect(%s): %v", id, err)
		}
		for _, ep := range ins.NetworkSettings.Networks {
			if ep.IPAddress != "" {
				EndPhase(t, PhaseIPAcquisition, acquireStart)
				return id, ep.IPAddress, ep.MacAddress
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Emitted on the timeout path too, since t.Fatalf below stops this goroutine.
	EndPhase(t, PhaseIPAcquisition, acquireStart)
	t.Fatalf("container %s did not get an IP within %v", containerName, IPAcquisitionBudget)
	return
}

// EndpointShortID returns the 12-character endpoint id the plugin logs as `endpoint=` (#278).
func EndpointShortID(t *testing.T, ctx context.Context, cli *docker.Client, containerID, networkName string) string {
	t.Helper()
	ins, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		t.Fatalf("ContainerInspect(%s): %v", containerID, err)
	}
	ep, ok := ins.NetworkSettings.Networks[networkName]
	if !ok {
		t.Fatalf("container %s is not attached to network %q", containerID, networkName)
	}
	if ep.EndpointID == "" {
		t.Fatalf("container %s has no endpoint id on network %q", containerID, networkName)
	}
	if len(ep.EndpointID) < 12 {
		return ep.EndpointID
	}
	return ep.EndpointID[:12]
}

// ExecOutput runs `docker exec` and returns combined stdout and stderr.
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
	// Without a TTY the attach stream carries 8-byte frame headers; StdCopy strips them (#130).
	var out strings.Builder
	if _, err := stdcopy.StdCopy(&out, &out, att.Reader); err != nil {
		t.Fatalf("demux exec output: %v", err)
	}
	return out.String()
}

// AssertIP fails the test unless got is an IPv4 in the macvlan fixture's pool.
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
		t.Fatalf("IP %q outside DHCP pool [%s, %s]. This assert is scoped to the MAIN macvlan "+
			"fixture; a test on the ephemeral fixture wants AssertEphemeralIP and a bridge test "+
			"AssertBridgeIP", got, DHCPPoolStart, DHCPPoolEnd)
	}
	return ip
}

// AssertEphemeralIP is AssertIP for the EphemeralFixture; AssertIP's pool check is fatal, so it cannot be composed.
func AssertEphemeralIP(t *testing.T, got string) net.IP {
	t.Helper()
	ip := net.ParseIP(got)
	if ip == nil {
		t.Fatalf("not a valid IP: %q", got)
	}
	if ip.To4() == nil {
		t.Fatalf("not an IPv4: %q", got)
	}
	if !IsInEphemeralPool(ip) {
		t.Fatalf("IP %q outside the ephemeral fixture's DHCP pool [%s, %s]",
			got, EphemeralPoolStart, EphemeralPoolEnd)
	}
	return ip
}

// AssertBridgeIP is the bridge-fixture analogue of AssertIP.
func AssertBridgeIP(t *testing.T, got string) net.IP {
	t.Helper()
	ip := net.ParseIP(got)
	if ip == nil {
		t.Fatalf("not a valid IP: %q", got)
	}
	if ip.To4() == nil {
		t.Fatalf("not an IPv4: %q", got)
	}
	if !IsInBridgePool(ip) {
		t.Fatalf("IP %q outside bridge DHCP pool [%s, %s]", got, BridgeDHCPPoolStart, BridgeDHCPPoolEnd)
	}
	return ip
}
