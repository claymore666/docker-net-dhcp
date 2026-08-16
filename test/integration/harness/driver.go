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
	"path/filepath"
	"testing"
	"time"

	docker "github.com/docker/docker/client"
)

// DriverClient speaks libnetwork's remote-driver protocol to the plugin
// directly, over the same UNIX socket PluginHealth already uses.
//
// # Why a test would want this
//
// Some plugin states are only reachable by controlling the ORDER of
// driver calls, and Docker will not let a test do that. The clearest
// example is an orphaned lease: CreateEndpoint takes an address, and
// the endpoint is orphaned if the persistent client never binds before
// the endpoint goes away. Through Docker the only lever is a container
// that exits quickly — which is a race against dhcpcd's DORA, not a
// construction. The suite lost that race for the first time when #555
// repartitioned the shards, and the losing test could not tell whether
// the code was wrong or the window had simply closed.
//
// Driving the driver directly removes the race: a Join with no
// container behind it cannot find a container to attach to, so the
// attach fails with util.ErrNoContainer and the orphan exists by
// construction. No sleeps, no retries, no widened budgets — the state
// is built rather than waited for.
//
// # What this is NOT
//
// It is not a way to skip Docker where Docker is the thing under test.
// Anything asserting on what a *container* sees — an address inside the
// netns, a renamed interface, a restart — must keep going through
// Docker, because the daemon's own behaviour is part of the claim.
// This is for the narrow case where the daemon is only a sequencer and
// its scheduling is what makes the test flaky.
//
// Requires root, like every other socket user in this harness.
type DriverClient struct {
	t    *testing.T
	sock string
	hc   *http.Client
}

// NewDriverClient resolves the live plugin's socket and returns a client
// bound to it. It fails the test if the plugin is not enabled — a
// caller cannot meaningfully continue without it.
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
			// Generous: CreateEndpoint performs a full DHCP round trip
			// against the fixture, and Join's own work is asynchronous.
			Timeout: 60 * time.Second,
		},
	}
}

// driverError is the remote-driver protocol's error body.
type driverError struct {
	Err string `json:"Err"`
}

// call POSTs req to method and decodes the reply into out (which may be
// nil). A non-2xx reply carries the plugin's own message, which is the
// part a failing test needs to see.
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

// EndpointAddresses is what CreateEndpoint hands back: the addresses the
// one-shot DHCP client leased, and the MAC it leased them against.
type EndpointAddresses struct {
	Address     string
	AddressIPv6 string
	MacAddress  string
}

// CreateEndpoint runs the plugin's CreateEndpoint, which leases an
// address from the real DHCP server and attaches the endpoint's child
// link to the parent NIC.
func (d *DriverClient) CreateEndpoint(ctx context.Context, netID, endpointID string) (EndpointAddresses, error) {
	return d.CreateEndpointWithMAC(ctx, netID, endpointID, "")
}

// CreateEndpointWithMAC is CreateEndpoint with an explicit hardware
// address, the way libnetwork passes one when the caller pinned it.
//
// It exists so a test can lease the SAME address twice on purpose: the
// DHCP server keys its offers on the client's MAC, so a fixed MAC makes
// the second lease land on the first one's address instead of whatever
// the pool hands out next. That turns "hope the address repeats" into a
// property of the request.
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

// Join runs the plugin's Join. It returns as soon as the plugin has
// answered; the persistent client is attached asynchronously, exactly
// as it is for a real container.
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

// DeleteEndpoint runs the plugin's DeleteEndpoint, which removes the
// endpoint's child link from the parent NIC.
func (d *DriverClient) DeleteEndpoint(ctx context.Context, netID, endpointID string) error {
	return d.call(ctx, "NetworkDriver.DeleteEndpoint", map[string]any{
		"NetworkID":  netID,
		"EndpointID": endpointID,
	}, nil)
}

// CleanupEndpoint tears an endpoint down on a background context and
// only warns on failure, for use from t.Cleanup where the test's own
// context may already be cancelled. Leave is attempted first and its
// error ignored: an endpoint that never joined has nothing to leave.
func (d *DriverClient) CleanupEndpoint(netID, endpointID string) {
	d.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = d.Leave(ctx, netID, endpointID)
	if err := d.DeleteEndpoint(ctx, netID, endpointID); err != nil {
		d.t.Logf("WARN: DeleteEndpoint(%s): %v", shortID(endpointID), err)
	}
}

// NewEndpointID returns a random libnetwork-shaped endpoint ID.
//
// libnetwork uses 64 hex characters and the plugin logs a truncated
// form, so tests that read the plugin log can correlate on the prefix.
func NewEndpointID(t *testing.T) string {
	t.Helper()

	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate endpoint ID: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// SyntheticSandboxKey returns a sandbox key for a Join issued without a
// container behind it.
//
// The key itself is nearly irrelevant, and saying so is the point. The
// plugin's own "did the sandbox go away?" check reads the netns
// directory, which is not mounted into the plugin container at all, so
// that check can only ever answer "no usable evidence" (#567). What
// actually makes a socket-driven Join orphan its address is that no
// container claims the endpoint on the network: the attach's container
// lookup retries for the whole budget and then returns
// util.ErrNoContainer, which is the state #566 is about.
//
// So this exists to supply a plausible, non-colliding value for a field
// the plugin logs, not to trigger anything. It is still placed in
// libnetwork's netns directory so the key looks like what the plugin
// sees in production, and so the behaviour does not change if #567 ever
// gives that check real evidence to work with.
func SyntheticSandboxKey(t *testing.T) string {
	t.Helper()

	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate sandbox key: %v", err)
	}
	key := filepath.Join("/var/run/docker/netns", "dh-itest-nocontainer-"+hex.EncodeToString(b[:]))
	if _, err := os.Stat(key); err == nil {
		t.Fatalf("sandbox key %s unexpectedly exists; it must not, or a real "+
			"container could be behind this Join", key)
	}
	return key
}

// shortID mirrors the plugin's own log truncation so harness messages
// line up with plugin log lines.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
