// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	docker "github.com/docker/docker/client"
)

// DriverClient speaks libnetwork's remote-driver protocol to the plugin socket, to build states that need a call order
// Docker will not give a test, such as an endpoint no container can claim (#568). Anything a container sees still goes
// through Docker. Requires root.
type DriverClient struct {
	t    *testing.T
	sock string
	hc   *http.Client
}

// NewDriverClient returns a client bound to the live plugin's socket, failing the test if the plugin is not enabled.
func NewDriverClient(t *testing.T, ctx context.Context, cli *docker.Client) *DriverClient {
	t.Helper()

	sock, err := PluginSocketPath(ctx, cli)
	if err != nil {
		t.Fatalf("resolve plugin socket: %v", err)
	}
	return &DriverClient{
		t:    t,
		sock: sock,
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
			// CreateEndpoint runs a full DHCP round trip against the fixture.
			Timeout: 60 * time.Second,
		},
	}
}

// driverError is the remote-driver protocol's error body.
type driverError struct {
	Err string `json:"Err"`
}

// call POSTs req to method and decodes the reply into out, which may be nil.
func (d *DriverClient) call(ctx context.Context, method string, req, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", method, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://plugin/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := d.hc.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%s: dial %s: %w", method, d.sock, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e driverError
		if decErr := json.NewDecoder(resp.Body).Decode(&e); decErr == nil && e.Err != "" {
			return fmt.Errorf("%s: %s", method, e.Err)
		}
		return fmt.Errorf("%s returned %s", method, resp.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	return nil
}

// EndpointAddresses is CreateEndpoint's reply: the leased addresses and the MAC they were leased against.
type EndpointAddresses struct {
	Address     string
	AddressIPv6 string
	MacAddress  string
}

// CreateEndpoint runs the plugin's CreateEndpoint, which leases an address and attaches the endpoint's child link.
func (d *DriverClient) CreateEndpoint(ctx context.Context, netID, endpointID string) (EndpointAddresses, error) {
	return d.CreateEndpointWithMAC(ctx, netID, endpointID, "")
}

// CreateEndpointWithMAC is CreateEndpoint with a pinned MAC; the DHCP server keys offers on the MAC, so a repeat lease gets the same address.
func (d *DriverClient) CreateEndpointWithMAC(ctx context.Context, netID, endpointID, mac string) (EndpointAddresses, error) {
	var res struct {
		Interface *EndpointAddresses
	}
	iface := map[string]any{}
	if mac != "" {
		iface["MacAddress"] = mac
	}
	err := d.call(ctx, "NetworkDriver.CreateEndpoint", map[string]any{
		"NetworkID":  netID,
		"EndpointID": endpointID,
		"Interface":  iface,
		"Options":    map[string]any{},
	}, &res)
	if err != nil {
		return EndpointAddresses{}, err
	}
	if res.Interface == nil {
		return EndpointAddresses{}, fmt.Errorf("CreateEndpoint returned no interface for %s", shortID(endpointID))
	}
	return *res.Interface, nil
}

// Join runs the plugin's Join, which attaches the persistent client asynchronously.
func (d *DriverClient) Join(ctx context.Context, netID, endpointID, sandboxKey string) error {
	return d.call(ctx, "NetworkDriver.Join", map[string]any{
		"NetworkID":  netID,
		"EndpointID": endpointID,
		"SandboxKey": sandboxKey,
		"Options":    map[string]any{},
	}, nil)
}

// Leave runs the plugin's Leave, stopping the persistent client.
func (d *DriverClient) Leave(ctx context.Context, netID, endpointID string) error {
	return d.call(ctx, "NetworkDriver.Leave", map[string]any{
		"NetworkID":  netID,
		"EndpointID": endpointID,
	}, nil)
}

// DeleteEndpoint runs the plugin's DeleteEndpoint, which removes the endpoint's child link.
func (d *DriverClient) DeleteEndpoint(ctx context.Context, netID, endpointID string) error {
	return d.call(ctx, "NetworkDriver.DeleteEndpoint", map[string]any{
		"NetworkID":  netID,
		"EndpointID": endpointID,
	}, nil)
}

// CleanupEndpoint runs Leave, ignoring its error, and DeleteEndpoint on a background context, only warning on failure.
func (d *DriverClient) CleanupEndpoint(netID, endpointID string) {
	d.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = d.Leave(ctx, netID, endpointID)
	if err := d.DeleteEndpoint(ctx, netID, endpointID); err != nil {
		d.t.Logf("WARN: DeleteEndpoint(%s): %v", shortID(endpointID), err)
	}
}

// NewEndpointID returns a random 64-hex endpoint ID as libnetwork makes them.
func NewEndpointID(t *testing.T) string {
	t.Helper()

	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate endpoint ID: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// LiveSandboxKey returns a running container's netns path, so Join gets a sandbox that exists while no container
// claims the endpoint; the plugin checks those two facts separately (#573).
func LiveSandboxKey(t *testing.T, ctx context.Context, cli *docker.Client, containerID string) string {
	t.Helper()

	info, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		t.Fatalf("ContainerInspect(%s): %v", containerID[:12], err)
	}
	key := info.NetworkSettings.SandboxKey
	if key == "" {
		t.Fatalf("container %s reports no sandbox key; it must be running for its "+
			"netns to exist, or this construction gives the plugin the wrong answer "+
			"about whether the container vanished", containerID[:12])
	}
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("sandbox key %s does not exist on the host (%v) — the whole point of "+
			"using a live container's key is that the netns is really there", key, err)
	}
	return key
}

// shortID mirrors the plugin's log truncation.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
