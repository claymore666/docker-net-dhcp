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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/dhcp-golib/lease"
	dNetwork "github.com/docker/docker/api/types/network"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

// A network removed while the plugin was absent leaves its file and pool binding behind (#1174).

const (
	staleNet = "stalenet0001"
	liveNet  = "livenet00001"
	lateNet  = "latenet00001"
)

func staleBinding(t *testing.T, third int) *ipamBinding {
	t.Helper()
	pool := fmt.Sprintf("192.168.%d.0/24", third)
	poolID, err := ipamPoolID(ipamLocalAddressSpace, pool, nil)
	if err != nil {
		t.Fatalf("ipamPoolID: %v", err)
	}
	return &ipamBinding{PoolID: poolID, Space: ipamLocalAddressSpace, Pool: pool, Gateway: fmt.Sprintf("192.168.%d.1", third)}
}

func persistStale(t *testing.T, id string, third int) *ipamBinding {
	t.Helper()
	b := staleBinding(t, third)
	if err := saveNetwork(id, DHCPNetworkOptions{Mode: ModeBridge, Bridge: "br0"}, b); err != nil {
		t.Fatalf("saveNetwork %s: %v", id, err)
	}
	return b
}

// stalePlugin starts as the real one does: the files are on disk, the index and the snapshot are read from them.
func stalePlugin(t *testing.T, ids ...string) (*Plugin, map[string]*ipamBinding) {
	t.Helper()
	withStateDir(t, t.TempDir())
	bound := map[string]*ipamBinding{}
	for i, id := range ids {
		bound[id] = persistStale(t, id, 90+i)
	}
	p := withRecords(t, &Plugin{ipamIndex: newIPAMIndex()})
	p.persistedAtStart = rebuildIPAMIndex(p.ipamIndex)
	return p, bound
}

func fileExists(t *testing.T, id string) bool {
	t.Helper()
	path, err := stateFilePath(id)
	if err != nil {
		t.Fatalf("stateFilePath: %v", err)
	}
	_, err = os.Stat(path)
	return err == nil
}

func retainedOn(t *testing.T, p *Plugin, network string) string {
	t.Helper()
	mac := deferredMAC(0x02)
	id := p.recordCreated(network, mac, dhcp.ClientIdentity(mac))
	if id == "" {
		t.Fatal("no record was created")
	}
	if err := p.records.Observed(id, acquired("192.168.90.10/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	if err := p.records.Retained(id, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("Retained: %v", err)
	}
	return id
}

func recoverOnce(p *Plugin) {
	p.recoverEndpoints(context.Background(), testDaemonWait)
}

func TestStaleNetworks_ANetworkDockerNoLongerHasIsDroppedWithItsPoolAndRecords(t *testing.T) {
	p, bound := stalePlugin(t, staleNet)
	recID := retainedOn(t, p, staleNet)
	f := &fakeDocker{inspectErr: networkNotFound()}
	p.docker = f

	recoverOnce(p)

	if fileExists(t, staleNet) {
		t.Error("the persisted file of a network Docker answered not-found for is still on disk")
	}
	if n, ok := p.ipamIndex.network(bound[staleNet].PoolID); ok {
		t.Errorf("the pool is still bound to %q, so every create for the subnet is refused", n)
	}
	if got := p.staleNetworksDropped.Load(); got != 1 {
		t.Errorf("stale_networks_dropped = %d, want 1", got)
	}
	if got := recordPhase(t, p, recID); got != lease.PhaseClosed {
		t.Errorf("the dropped network's retained record is %v, want CLOSED", got)
	}
	if f.inspectCalls != 1 {
		t.Errorf("NetworkInspect called %d times, want 1", f.inspectCalls)
	}
}

func TestStaleNetworks_ANetworkInDockersListIsLeftAloneWithoutACall(t *testing.T) {
	p, bound := stalePlugin(t, liveNet)
	f := &fakeDocker{
		listResult:    []dNetwork.Summary{{ID: liveNet, Driver: "bridge"}},
		inspectResult: map[string]dNetwork.Inspect{},
		inspectErr:    networkNotFound(),
	}
	p.docker = f

	recoverOnce(p)

	if !fileExists(t, liveNet) {
		t.Error("the file of a network Docker lists was removed")
	}
	if n, ok := p.ipamIndex.network(bound[liveNet].PoolID); !ok || n != liveNet {
		t.Errorf("the pool binding is (%q, %v), want it kept for %s", n, ok, liveNet)
	}
	if f.inspectCalls != 0 {
		t.Errorf("NetworkInspect called %d times for a listed network, want 0", f.inspectCalls)
	}
	if got := p.staleNetworksDropped.Load(); got != 0 {
		t.Errorf("stale_networks_dropped = %d, want 0", got)
	}
}

func TestStaleNetworks_OnlyNotFoundCountsAsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "server-error", err: errors.New("Error response from daemon: 500 Internal Server Error")},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "unreachable", err: errors.New("Cannot connect to the Docker daemon")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, bound := stalePlugin(t, staleNet)
			recID := retainedOn(t, p, staleNet)
			f := &fakeDocker{inspectErr: tc.err}
			p.docker = f

			recoverOnce(p)

			if !fileExists(t, staleNet) {
				t.Error("the file was removed on an answer that is not not-found")
			}
			if n, ok := p.ipamIndex.network(bound[staleNet].PoolID); !ok || n != staleNet {
				t.Errorf("the pool binding is (%q, %v), want it kept", n, ok)
			}
			if got := p.staleNetworksDropped.Load(); got != 0 {
				t.Errorf("stale_networks_dropped = %d, want 0", got)
			}
			if got := recordPhase(t, p, recID); got != lease.PhaseRetained {
				t.Errorf("the record is %v, want RETAINED: the network may still exist", got)
			}
			if f.inspectCalls != 1 {
				t.Errorf("NetworkInspect called %d times, want 1 (the snapshot is consumed, not retried)", f.inspectCalls)
			}
		})
	}
}

func TestStaleNetworks_AnUnlistedNetworkDockerStillAnswersForIsKept(t *testing.T) {
	p, bound := stalePlugin(t, staleNet)
	recID := retainedOn(t, p, staleNet)
	f := &fakeDocker{}
	p.docker = f

	recoverOnce(p)

	if f.inspectCalls != 1 {
		t.Fatalf("NetworkInspect called %d times, want 1: the list lacked the network, so it is asked", f.inspectCalls)
	}
	if !fileExists(t, staleNet) {
		t.Error("the file of a network whose inspect succeeded was removed")
	}
	if n, ok := p.ipamIndex.network(bound[staleNet].PoolID); !ok || n != staleNet {
		t.Errorf("the pool binding is (%q, %v), want it kept", n, ok)
	}
	if got := recordPhase(t, p, recID); got != lease.PhaseRetained {
		t.Errorf("the record is %v, want RETAINED: the daemon still has the network", got)
	}
	if got := p.staleNetworksDropped.Load(); got != 0 {
		t.Errorf("stale_networks_dropped = %d, want 0", got)
	}
}

func TestStaleNetworks_OnlyTheDroppedNetworksRetainedRecordsAreClosed(t *testing.T) {
	p, _ := stalePlugin(t, staleNet, liveNet)
	staleRetained := retainedOn(t, p, staleNet)
	liveRetained := retainedOn(t, p, liveNet)
	mac := deferredMAC(0x03)
	staleCreated := p.recordCreated(staleNet, mac, dhcp.ClientIdentity(mac))
	if staleCreated == "" {
		t.Fatal("no record was created")
	}
	if err := p.records.Observed(staleCreated, acquired("192.168.90.11/24", time.Hour), nil); err != nil {
		t.Fatalf("Observed: %v", err)
	}
	p.docker = &fakeDocker{
		listResult: []dNetwork.Summary{{ID: liveNet, Driver: "bridge"}},
		inspectErr: networkNotFound(),
	}

	recoverOnce(p)

	if got := recordPhase(t, p, staleRetained); got != lease.PhaseClosed {
		t.Fatalf("the dropped network's retained record is %v, want CLOSED", got)
	}
	if got := recordPhase(t, p, liveRetained); got != lease.PhaseRetained {
		t.Errorf("a listed network's retained record is %v, want RETAINED", got)
	}
	if got := recordPhase(t, p, staleCreated); got != lease.PhaseCreated {
		t.Errorf("the dropped network's created record is %v, want CREATED: only RETAINED records are closed", got)
	}
}

func TestStaleNetworks_ANetworkPersistedAfterStartIsNeverACandidate(t *testing.T) {
	p, _ := stalePlugin(t)
	late := persistStale(t, lateNet, 95)
	p.ipamIndex.bind(late.PoolID, lateNet)
	f := &fakeDocker{inspectErr: networkNotFound()}
	p.docker = f

	recoverOnce(p)

	if !fileExists(t, lateNet) {
		t.Error("a network this process created after start was dropped; Docker's list had not caught up with it")
	}
	if n, ok := p.ipamIndex.network(late.PoolID); !ok || n != lateNet {
		t.Errorf("the pool binding is (%q, %v), want it kept", n, ok)
	}
	if f.inspectCalls != 0 {
		t.Errorf("NetworkInspect called %d times, want 0", f.inspectCalls)
	}
}

func TestStaleNetworks_TheSecondPassMakesNoDockerCall(t *testing.T) {
	p, bound := stalePlugin(t, staleNet)
	f := &fakeDocker{inspectErr: networkNotFound()}
	p.docker = f

	recoverOnce(p)
	if got := p.staleNetworksDropped.Load(); got != 1 || f.inspectCalls != 1 {
		t.Fatalf("first pass: dropped=%d inspects=%d, want 1 and 1", got, f.inspectCalls)
	}

	// A create the daemon has not committed yet, and a stale file written back: neither may be touched.
	late := persistStale(t, lateNet, 95)
	p.ipamIndex.bind(late.PoolID, lateNet)
	persistStale(t, staleNet, 90)
	p.ipamIndex.bind(bound[staleNet].PoolID, staleNet)

	recoverOnce(p)

	if f.inspectCalls != 1 {
		t.Errorf("NetworkInspect called %d times after the second pass, want 1: the snapshot was consumed", f.inspectCalls)
	}
	if !fileExists(t, lateNet) || !fileExists(t, staleNet) {
		t.Error("the second pass removed a file")
	}
	if got := p.staleNetworksDropped.Load(); got != 1 {
		t.Errorf("stale_networks_dropped = %d, want 1", got)
	}
}

func TestStaleNetworks_TheCounterIsOnHealthAndDoesNotMakeThePluginUnhealthy(t *testing.T) {
	p := newHealthPlugin()
	p.staleNetworksDropped.Add(2)

	rec := httptest.NewRecorder()
	p.apiHealth(rec, httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil))
	var h HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.StaleNetworksDropped != 2 {
		t.Errorf("stale_networks_dropped = %d, want 2", h.StaleNetworksDropped)
	}
	if !h.Healthy {
		t.Error("healthy = false; a removed network's leftover is not a fault")
	}
}

// fakeDaemon answers like an engine whose network store holds nothing, through the real client and its 404 mapping.
func fakeDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("API-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/version"):
			_, _ = w.Write([]byte(`{"Version":"27.0.0","ApiVersion":"1.46"}`))
		case strings.HasSuffix(r.URL.Path, "/networks"):
			_, _ = w.Write([]byte("[]"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"network not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStaleNetworks_StartDropsWhatTheDaemonNoLongerHas(t *testing.T) {
	withStateDir(t, t.TempDir())
	b := persistStale(t, staleNet, 90)
	t.Setenv(envDockerHost, "tcp://"+strings.TrimPrefix(fakeDaemon(t).URL, "http://"))

	p, err := NewPlugin(Options{})
	if err != nil {
		t.Fatalf("NewPlugin: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	if fileExists(t, staleNet) {
		t.Error("start left the file of a network the daemon answered 404 for")
	}
	if n, ok := p.ipamIndex.network(b.PoolID); ok {
		t.Errorf("start left the pool bound to %q", n)
	}
	if got := p.staleNetworksDropped.Load(); got != 1 {
		t.Errorf("stale_networks_dropped = %d, want 1", got)
	}
}
