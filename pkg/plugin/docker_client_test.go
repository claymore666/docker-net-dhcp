// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
)

const testDHCPDriver = "claymore666/docker-net-dhcp:latest"

// fakeDocker is a programmable dockerClient for exercising the error
// arms of the recovery, option-fallback and hostname-lookup paths,
// which integration cannot reach without a real daemon misbehaving.
type fakeDocker struct {
	listResult []dNetwork.Summary
	listErr    error
	// listErrUntil, when non-zero, is the call number on which
	// NetworkList stops returning listErr and starts answering — a
	// daemon that comes up mid-recovery (#383).
	listErrUntil int

	inspectResult map[string]dNetwork.Inspect
	inspectErr    error
	// inspectErrFromCall, when non-zero, is the 1-based call number at
	// which NetworkInspect starts returning inspectErr. It exists for
	// the netOptions arm of #648: recovery inspects a network, then
	// netOptions inspects it again, and only the second call may see
	// the 404. Without it the two calls cannot be told apart.
	inspectErrFromCall int

	containerResult map[string]dContainer.InspectResponse
	containerErr    error
	// containerDelay models the daemon not answering while it holds the
	// container it is being asked about — the #406 condition. Blocks
	// rather than erroring, because that is what the real daemon does:
	// the connection is accepted and no response header ever arrives.
	containerDelay time.Duration

	closeErr error

	listCalls      int
	inspectCalls   int
	containerCalls int
}

func (f *fakeDocker) NetworkList(_ context.Context, _ dNetwork.ListOptions) ([]dNetwork.Summary, error) {
	f.listCalls++
	// listErrUntil models a daemon that is still starting: fail the
	// first N-1 calls, then answer. Zero means "always fail" (the
	// original behaviour), so existing cases are unaffected.
	if f.listErr != nil && (f.listErrUntil == 0 || f.listCalls < f.listErrUntil) {
		return nil, f.listErr
	}
	return f.listResult, nil
}

func (f *fakeDocker) NetworkInspect(_ context.Context, id string, _ dNetwork.InspectOptions) (dNetwork.Inspect, error) {
	f.inspectCalls++
	if f.inspectErr != nil && (f.inspectErrFromCall == 0 || f.inspectCalls >= f.inspectErrFromCall) {
		return dNetwork.Inspect{}, f.inspectErr
	}
	return f.inspectResult[id], nil
}

func (f *fakeDocker) ContainerInspect(ctx context.Context, id string) (dContainer.InspectResponse, error) {
	f.containerCalls++
	if f.containerDelay > 0 {
		select {
		case <-time.After(f.containerDelay):
		case <-ctx.Done():
			return dContainer.InspectResponse{}, ctx.Err()
		}
	}
	if f.containerErr != nil {
		return dContainer.InspectResponse{}, f.containerErr
	}
	return f.containerResult[id], nil
}

func (f *fakeDocker) Close() error { return f.closeErr }

// testDaemonWait keeps the entry gate's retry loop short. Real waits are
// seconds; the unit suite only needs the loop to terminate.
const testDaemonWait = 100 * time.Millisecond

// fastRetries shrinks the gap between entry-gate attempts so a test can
// observe several of them inside testDaemonWait without paying the real
// half-second spacing.
func fastRetries(t *testing.T) {
	t.Helper()
	prev := recoveryDaemonRetryInterval
	recoveryDaemonRetryInterval = time.Millisecond
	t.Cleanup(func() { recoveryDaemonRetryInterval = prev })
}

// TestRecoverEndpoints_NetworkListError pins the #383 contract: a daemon
// that never answers is reported to the caller as "not ready" and does
// NOT count a failure here. Counting it at this level is what made a
// routine daemon restart look like a plugin fault — the decision belongs
// to whoever knows whether a retry is still coming (NewPlugin/Listen).
func TestRecoverEndpoints_NetworkListError(t *testing.T) {
	fastRetries(t)
	f := &fakeDocker{listErr: errors.New("list boom")}
	p := &Plugin{docker: f}

	notReady := p.recoverEndpoints(context.Background(), testDaemonWait)

	if !notReady {
		t.Error("recoverEndpoints should report daemonNotReady when the list never succeeds")
	}
	if got := p.recoveryFailed.Load(); got != 0 {
		t.Fatalf("recoveryFailed: got %d want 0 — an unreachable daemon is not a recovery failure here", got)
	}
	if f.inspectCalls != 0 {
		t.Fatalf("NetworkInspect should not be called after list failure (got %d)", f.inspectCalls)
	}
	if f.listCalls < 2 {
		t.Errorf("expected the entry gate to retry; got %d NetworkList calls", f.listCalls)
	}
}

// TestRecoverEndpoints_NetworkListRecoversAfterRetry is the other half:
// a daemon that is merely slow to start must be waited out, not skipped.
func TestRecoverEndpoints_NetworkListRecoversAfterRetry(t *testing.T) {
	fastRetries(t)
	f := &fakeDocker{
		listErr:      errors.New("daemon still starting"),
		listErrUntil: 3, // fail the first two calls, succeed on the third
		listResult:   []dNetwork.Summary{{ID: "n1", Driver: "bridge"}},
	}
	p := &Plugin{docker: f}

	notReady := p.recoverEndpoints(context.Background(), testDaemonWait)

	if notReady {
		t.Error("a daemon that answers on retry must not be reported as not-ready")
	}
	if got := p.recoveryFailed.Load(); got != 0 {
		t.Errorf("recoveryFailed: got %d want 0", got)
	}
	if f.listCalls != 3 {
		t.Errorf("expected exactly 3 NetworkList calls, got %d", f.listCalls)
	}
}

func TestRecoverEndpoints_SkipsNonDHCPNetworks(t *testing.T) {
	f := &fakeDocker{listResult: []dNetwork.Summary{{ID: "n1", Driver: "bridge"}}}
	p := &Plugin{docker: f}

	p.recoverEndpoints(context.Background(), testDaemonWait)

	if got := p.recoveryFailed.Load(); got != 0 {
		t.Fatalf("recoveryFailed: got %d want 0", got)
	}
	if f.inspectCalls != 0 {
		t.Fatalf("non-DHCP network should be skipped before NetworkInspect (got %d)", f.inspectCalls)
	}
}

func TestRecoverEndpoints_NetworkInspectError(t *testing.T) {
	f := &fakeDocker{
		listResult: []dNetwork.Summary{{ID: "n1", Driver: testDHCPDriver}},
		inspectErr: errors.New("inspect boom"),
	}
	p := &Plugin{docker: f}

	p.recoverEndpoints(context.Background(), testDaemonWait)

	if got := p.recoveryFailed.Load(); got != 1 {
		t.Fatalf("recoveryFailed: got %d want 1", got)
	}
}

// #648. The list recovery walks is a snapshot: a `docker network rm`
// landing between it and the detail read answers the second call with a
// 404. That was counted as recovery_failed — fatal, and enough to flip
// healthy — so an ordinary network removal racing a daemon restart
// reported the plugin's most serious fault. A network that is gone has
// no running container left without a renewal client.
//
// The direction matters as much as the count: TestRecoverEndpoints_
// NetworkInspectError above pins that a REAL error is still fatal, and
// both must hold. A fix that swallowed every inspect error would pass
// this test and destroy the counter.
func TestRecoverEndpoints_NetworkGoneIsNotAFailure(t *testing.T) {
	f := &fakeDocker{
		listResult: []dNetwork.Summary{{ID: "n1", Driver: testDHCPDriver}},
		inspectErr: fmt.Errorf("network n1 not found: %w", cerrdefs.ErrNotFound),
	}
	p := &Plugin{docker: f}

	p.recoverEndpoints(context.Background(), testDaemonWait)

	if got := p.recoveryFailed.Load(); got != 0 {
		t.Errorf("recoveryFailed: got %d want 0 — a removed network is not a recovery failure", got)
	}
	if got := p.recoveryNetworkGone.Load(); got != 1 {
		t.Errorf("recoveryNetworkGone: got %d want 1", got)
	}
}

// The same race one call later: NetworkInspect answers, and netOptions —
// which reaches the daemon only when its on-disk cache misses — gets the
// 404 instead.
func TestRecoverEndpoints_NetOptionsNetworkGoneIsNotAFailure(t *testing.T) {
	withStateDir(t, t.TempDir()) // force the on-disk miss -> docker fallback
	f := &fakeDocker{
		listResult: []dNetwork.Summary{{ID: "n1", Driver: testDHCPDriver}},
		inspectResult: map[string]dNetwork.Inspect{
			"n1": {ID: "n1", Driver: testDHCPDriver},
		},
		inspectErr:         fmt.Errorf("network n1 not found: %w", cerrdefs.ErrNotFound),
		inspectErrFromCall: 2,
	}
	p := &Plugin{docker: f}

	p.recoverEndpoints(context.Background(), testDaemonWait)

	if got := p.recoveryFailed.Load(); got != 0 {
		t.Errorf("recoveryFailed: got %d want 0", got)
	}
	if got := p.recoveryNetworkGone.Load(); got != 1 {
		t.Errorf("recoveryNetworkGone: got %d want 1", got)
	}
	if f.inspectCalls < 2 {
		t.Errorf("inspectCalls: got %d, want the netOptions fallback to have run", f.inspectCalls)
	}
}

// The counter has to reach the wire, and it must NOT move Healthy. A
// counter nobody can read is a log line with extra steps; one that flips
// healthy is the bug this fix exists to undo.
func TestApiHealth_RecoveryNetworkGoneIsNotUnhealthy(t *testing.T) {
	p := newHealthPlugin()

	rec := httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil))
	var clean HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &clean); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !clean.Healthy {
		t.Fatalf("baseline is already unhealthy; the assertion below would pass for the wrong reason")
	}
	if clean.RecoveryNetworkGone != 0 {
		t.Errorf("recovery_network_gone = %d on a fresh plugin, want 0", clean.RecoveryNetworkGone)
	}

	p.recoveryNetworkGone.Add(3)
	rec = httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil))
	var after HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if after.RecoveryNetworkGone != 3 {
		t.Errorf("recovery_network_gone = %d, want 3", after.RecoveryNetworkGone)
	}
	if !after.Healthy {
		t.Error("healthy = false; a removed network leaves no container without a renewal client (#648)")
	}
}

func TestRecoverEndpoints_NetOptionsDecodeError(t *testing.T) {
	withStateDir(t, t.TempDir()) // force the on-disk miss -> docker fallback
	f := &fakeDocker{
		listResult: []dNetwork.Summary{{ID: "n1", Driver: testDHCPDriver}},
		inspectResult: map[string]dNetwork.Inspect{
			"n1": {ID: "n1", Driver: testDHCPDriver, Options: map[string]string{"bogus_unknown_key": "x"}},
		},
	}
	p := &Plugin{docker: f}

	p.recoverEndpoints(context.Background(), testDaemonWait)

	if got := p.recoveryFailed.Load(); got != 1 {
		t.Fatalf("recoveryFailed: got %d want 1 (decode of unknown option should fail)", got)
	}
}

func TestNetOptions_DiskHitSkipsDocker(t *testing.T) {
	withStateDir(t, t.TempDir())
	want := DHCPNetworkOptions{Bridge: "br0"}
	if err := saveOptions("n1", want); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	// Docker errors on any call, proving the disk hit short-circuits it.
	f := &fakeDocker{inspectErr: errors.New("docker must not be called")}
	p := &Plugin{docker: f}

	got, err := p.netOptions(context.Background(), "n1")
	if err != nil {
		t.Fatalf("netOptions: %v", err)
	}
	if got.Bridge != want.Bridge {
		t.Fatalf("opts: got %+v want %+v", got, want)
	}
	if f.inspectCalls != 0 {
		t.Fatalf("disk hit must not call NetworkInspect (got %d)", f.inspectCalls)
	}
}

func TestNetOptions_DockerFallbackSuccessAndBackfill(t *testing.T) {
	withStateDir(t, t.TempDir())
	f := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"n1": {ID: "n1", Driver: testDHCPDriver, Options: map[string]string{"bridge": "br9"}},
		},
	}
	p := &Plugin{docker: f}

	got, err := p.netOptions(context.Background(), "n1")
	if err != nil {
		t.Fatalf("netOptions: %v", err)
	}
	if got.Bridge != "br9" {
		t.Fatalf("opts: got %+v want bridge=br9", got)
	}
	// Backfill: the next load should now hit disk without touching docker.
	if _, err := loadOptions("n1"); err != nil {
		t.Fatalf("expected options backfilled to disk, loadOptions: %v", err)
	}
}

func TestNetOptions_DockerInspectError(t *testing.T) {
	withStateDir(t, t.TempDir())
	f := &fakeDocker{inspectErr: errors.New("inspect boom")}
	p := &Plugin{docker: f}

	if _, err := p.netOptions(context.Background(), "n1"); err == nil {
		t.Fatal("expected error when disk misses and NetworkInspect fails")
	}
}

func TestNetOptions_DockerDecodeError(t *testing.T) {
	withStateDir(t, t.TempDir())
	f := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			"n1": {ID: "n1", Driver: testDHCPDriver, Options: map[string]string{"bogus_unknown_key": "x"}},
		},
	}
	p := &Plugin{docker: f}

	if _, err := p.netOptions(context.Background(), "n1"); err == nil {
		t.Fatal("expected parse error for unknown option key")
	}
}

func TestLookupEndpointMAC(t *testing.T) {
	const netID, epID = "n1", "ep1"
	cases := []struct {
		name    string
		f       *fakeDocker
		wantMAC string
		wantErr bool
	}{
		{
			name:    "inspect_error",
			f:       &fakeDocker{inspectErr: errors.New("boom")},
			wantErr: true,
		},
		{
			name: "endpoint_not_found",
			f: &fakeDocker{inspectResult: map[string]dNetwork.Inspect{
				netID: {Containers: map[string]dNetwork.EndpointResource{
					"c1": {EndpointID: "other", MacAddress: "aa:bb:cc:dd:ee:ff"},
				}},
			}},
			wantErr: true,
		},
		{
			name: "found",
			f: &fakeDocker{inspectResult: map[string]dNetwork.Inspect{
				netID: {Containers: map[string]dNetwork.EndpointResource{
					"c1": {EndpointID: epID, MacAddress: "aa:bb:cc:dd:ee:ff"},
				}},
			}},
			wantMAC: "aa:bb:cc:dd:ee:ff",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Plugin{docker: c.f}
			mac, err := p.lookupEndpointMAC(context.Background(), netID, epID)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got mac=%q", mac)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mac != c.wantMAC {
				t.Fatalf("mac: got %q want %q", mac, c.wantMAC)
			}
		})
	}
}

func TestReacquireEndpoint_MACLookupError(t *testing.T) {
	// Non-ipvlan mode looks up the original MAC first; a docker failure
	// there must abort before the CreateEndpoint replay (which needs a
	// live netns and is integration-covered).
	f := &fakeDocker{inspectErr: errors.New("inspect boom")}
	p := &Plugin{docker: f}

	err := p.reacquireEndpoint(context.Background(),
		JoinRequest{NetworkID: "n1", EndpointID: "ep1"},
		DHCPNetworkOptions{Bridge: "br0"})
	if err == nil {
		t.Fatal("expected error when endpoint MAC lookup fails")
	}
}

func TestInitialDHCPHostname_Success(t *testing.T) {
	const netID, epID = "n1", "ep1"
	f := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			netID: {Containers: map[string]dNetwork.EndpointResource{
				"realctr": {EndpointID: epID},
			}},
		},
		containerResult: map[string]dContainer.InspectResponse{
			"realctr": {Config: &dContainer.Config{Hostname: "myhost"}},
		},
	}
	p := &Plugin{docker: f}

	if got := p.initialDHCPHostname(context.Background(), netID, epID); got.name != "myhost" {
		t.Fatalf("hostname: got %q want myhost", got.name)
	}
}

func TestInitialDHCPHostname_EmptyOnFailure(t *testing.T) {
	const netID, epID = "n1", "ep1"
	cases := []struct {
		name string
		f    *fakeDocker
	}{
		{
			name: "network_inspect_error",
			f:    &fakeDocker{inspectErr: errors.New("boom")},
		},
		{
			name: "container_inspect_error",
			f: &fakeDocker{
				inspectResult: map[string]dNetwork.Inspect{
					netID: {Containers: map[string]dNetwork.EndpointResource{"realctr": {EndpointID: epID}}},
				},
				containerErr: errors.New("boom"),
			},
		},
		{
			name: "endpoint_placeholder_not_yet_bound",
			f: &fakeDocker{
				inspectResult: map[string]dNetwork.Inspect{
					netID: {Containers: map[string]dNetwork.EndpointResource{"ep-" + epID: {EndpointID: epID}}},
				},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Short deadline so the poll loop gives up quickly instead of
			// waiting the full initialDHCPHostnameLookupTimeout.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			p := &Plugin{docker: c.f}
			if got := p.initialDHCPHostname(ctx, netID, epID); got.name != "" {
				t.Fatalf("hostname: got %q want empty", got.name)
			}
		})
	}
}

// TestInitialDHCPHostname_RefusalIsNotAnAbsence pins the SECOND return
// value, which every other test here discards with `_`.
//
// Both outcomes produce an empty hostname and they mean opposite things
// (#726). An absent hostname is an honest unknown, and tombstone
// matching treats it as a wildcard on purpose — that is the v0.5.0
// contract. A REFUSED hostname is attacker-supplied, and if it arrives
// at the tombstone store looking like an absence it buys that wildcard:
// one container with a control character in its hostname could then
// consume the tombstone of any container on the network and inherit its
// MAC and address.
//
// So the flag is the whole fix, and until this test it was asserted
// nowhere at its source: TestInitialDHCPHostname_Success and
// _EmptyOnFailure both read `got, _`.
func TestInitialDHCPHostname_RefusalIsNotAnAbsence(t *testing.T) {
	const netID, epID = "n1", "ep1"

	tests := []struct {
		name        string
		hostname    string
		wantTrusted bool
		why         string
	}{
		{
			name:        "an ordinary hostname is trusted",
			hostname:    "myhost",
			wantTrusted: true,
			why:         "nothing was refused",
		},
		{
			name:        "no hostname set at all is an honest absence",
			hostname:    "",
			wantTrusted: true,
			why:         "an empty Config.Hostname is a container that named nothing, not one whose name was rejected",
		},
		{
			name:        "a control character is a refusal, not an absence",
			hostname:    "attacker-host\x01",
			wantTrusted: false,
			why:         "it reaches the store as an empty Hostname, which is the tombstone matcher's wildcard",
		},
		{
			name:        "a NUL is a refusal",
			hostname:    "web\x00evil",
			wantTrusted: false,
			why:         "a NUL survives dockerd and truncates in the C string on the other side",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeDocker{
				inspectResult: map[string]dNetwork.Inspect{
					netID: {Containers: map[string]dNetwork.EndpointResource{
						"realctr": {EndpointID: epID},
					}},
				},
				containerResult: map[string]dContainer.InspectResponse{
					"realctr": {Config: &dContainer.Config{Hostname: tc.hostname}},
				},
			}
			p := &Plugin{docker: f}

			got := p.initialDHCPHostname(context.Background(), netID, epID)
			if got.trusted() != tc.wantTrusted {
				t.Errorf("trusted = %v, want %v — %s", got.trusted(), tc.wantTrusted, tc.why)
			}
			if !tc.wantTrusted && got.name != "" {
				t.Errorf("a refused hostname came back as %q; it must not reach the DHCP client config at all", got.name)
			}
		})
	}
}
