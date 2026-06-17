package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

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

	inspectResult map[string]dNetwork.Inspect
	inspectErr    error

	containerResult map[string]dContainer.InspectResponse
	containerErr    error

	closeErr error

	listCalls      int
	inspectCalls   int
	containerCalls int
}

func (f *fakeDocker) NetworkList(_ context.Context, _ dNetwork.ListOptions) ([]dNetwork.Summary, error) {
	f.listCalls++
	return f.listResult, f.listErr
}

func (f *fakeDocker) NetworkInspect(_ context.Context, id string, _ dNetwork.InspectOptions) (dNetwork.Inspect, error) {
	f.inspectCalls++
	if f.inspectErr != nil {
		return dNetwork.Inspect{}, f.inspectErr
	}
	return f.inspectResult[id], nil
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string) (dContainer.InspectResponse, error) {
	f.containerCalls++
	if f.containerErr != nil {
		return dContainer.InspectResponse{}, f.containerErr
	}
	return f.containerResult[id], nil
}

func (f *fakeDocker) Close() error { return f.closeErr }

func TestRecoverEndpoints_NetworkListError(t *testing.T) {
	f := &fakeDocker{listErr: errors.New("list boom")}
	p := &Plugin{docker: f}

	p.recoverEndpoints(context.Background())

	if got := p.recoveryFailed.Load(); got != 1 {
		t.Fatalf("recoveryFailed: got %d want 1", got)
	}
	if f.inspectCalls != 0 {
		t.Fatalf("NetworkInspect should not be called after list failure (got %d)", f.inspectCalls)
	}
}

func TestRecoverEndpoints_SkipsNonDHCPNetworks(t *testing.T) {
	f := &fakeDocker{listResult: []dNetwork.Summary{{ID: "n1", Driver: "bridge"}}}
	p := &Plugin{docker: f}

	p.recoverEndpoints(context.Background())

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

	p.recoverEndpoints(context.Background())

	if got := p.recoveryFailed.Load(); got != 1 {
		t.Fatalf("recoveryFailed: got %d want 1", got)
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

	p.recoverEndpoints(context.Background())

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

func TestInitialContainerIdentity_HostnameSuccess(t *testing.T) {
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

	if got := p.initialContainerIdentity(context.Background(), netID, epID).Hostname; got != "myhost" {
		t.Fatalf("hostname: got %q want myhost", got)
	}
}

func TestInitialContainerIdentity_EmptyOnFailure(t *testing.T) {
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
			if got := p.initialContainerIdentity(ctx, netID, epID).Hostname; got != "" {
				t.Fatalf("hostname: got %q want empty", got)
			}
		})
	}
}
