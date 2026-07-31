package plugin

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"

	"encoding/json"
)

// notFoundErr is what the Docker client returns for an inspect of a
// container the daemon has never heard of. Built by wrapping the
// containerd sentinel rather than by hand so the test exercises the
// same errors.Is chain production does — a hand-rolled "not found"
// string would pass while the real error failed to classify.
func notFoundErr() error {
	return fmt.Errorf("Error response from daemon: No such container: deadbeef: %w", cerrdefs.ErrNotFound)
}

func running() dContainer.InspectResponse {
	return dContainer.InspectResponse{
		ContainerJSONBase: &dContainer.ContainerJSONBase{
			State: &dContainer.State{Running: true, Status: "running", Pid: 4242},
		},
	}
}

func stopped(status string) dContainer.InspectResponse {
	return dContainer.InspectResponse{
		ContainerJSONBase: &dContainer.ContainerJSONBase{
			State: &dContainer.State{Running: false, Status: status},
		},
	}
}

// TestContainerGone covers the #376 classifier in both directions.
//
// The asymmetry is the point: "gone" must be positively established,
// never assumed. Every case where the evidence is missing or ambiguous
// has to answer false, because false is what makes the caller count a
// real recovery failure — the safe direction. Answering true on weak
// evidence silently excuses a running container that has lost its
// renewal client, which is the failure mode the counter exists to make
// visible.
func TestContainerGone(t *testing.T) {
	cases := []struct {
		name   string
		id     string
		fake   *fakeDocker
		want   bool
		reason string
	}{
		{
			name:   "running container is not gone",
			id:     "c1",
			fake:   &fakeDocker{containerResult: map[string]dContainer.InspectResponse{"c1": running()}},
			want:   false,
			reason: "a running container that failed recovery has genuinely lost its renewal client",
		},
		{
			name:   "exited container is gone",
			id:     "c1",
			fake:   &fakeDocker{containerResult: map[string]dContainer.InspectResponse{"c1": stopped("exited")}},
			want:   true,
			reason: "the container exited before recovery reached it — nothing is missing a renewal client",
		},
		{
			name: "restarting container is gone",
			id:   "c1",
			fake: &fakeDocker{containerResult: map[string]dContainer.InspectResponse{"c1": stopped("restarting")}},
			want: true,
			// It comes back through Join, which builds its own manager;
			// this recovery attempt has nothing left to recover.
			reason: "a restarting container has no live netns depending on this recovery",
		},
		{
			name:   "removed container is gone",
			id:     "c1",
			fake:   &fakeDocker{containerErr: notFoundErr()},
			want:   true,
			reason: "the daemon has never heard of it, so it cannot be running",
		},
		{
			name:   "an inspect error that is not not-found is no evidence",
			id:     "c1",
			fake:   &fakeDocker{containerErr: errors.New("connection refused")},
			want:   false,
			reason: "an unreachable daemon must degrade to counting a real failure, not to excusing one",
		},
		{
			name: "a container with no State is gone",
			id:   "c1",
			fake: &fakeDocker{containerResult: map[string]dContainer.InspectResponse{
				"c1": {ContainerJSONBase: &dContainer.ContainerJSONBase{}},
			}},
			want:   true,
			reason: "a response carrying no state cannot be reporting a running container",
		},
		{
			name:   "an empty container id is no evidence",
			id:     "",
			fake:   &fakeDocker{},
			want:   false,
			reason: "nothing to ask about; the caller must fall back to counting a real failure",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Plugin{docker: tc.fake}
			if got := p.containerGone(t.Context(), tc.id); got != tc.want {
				t.Errorf("containerGone = %v, want %v — %s", got, tc.want, tc.reason)
			}
		})
	}
}

// TestContainerGone_EmptyIDSkipsTheDaemon pins the short-circuit
// separately from the verdict. An empty ID reaching ContainerInspect
// would be a wasted round-trip on a path that only runs after a
// recovery has already failed, and on a daemon that may be the reason
// it failed.
func TestContainerGone_EmptyIDSkipsTheDaemon(t *testing.T) {
	f := &fakeDocker{}
	p := &Plugin{docker: f}

	p.containerGone(t.Context(), "")

	if f.containerCalls != 0 {
		t.Errorf("ContainerInspect called %d times for an empty id, want 0", f.containerCalls)
	}
}

// recoverAndAwaitCounter drives recoverOneEndpoint to its async Start
// failure and returns once either counter has moved.
//
// Start fails deterministically here: it polls NetworkInspect for a
// container carrying the endpoint, the fake network has none, and
// awaitTimeout is short. That is the same arm production takes when a
// container disappears mid-recovery, which is what makes this a test of
// the wiring rather than of the classifier alone — containerGone can be
// perfectly correct and still be called on the wrong branch.
func recoverAndAwaitCounter(t *testing.T, f *fakeDocker, containerID string) *Plugin {
	t.Helper()
	p := &Plugin{
		docker:         f,
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
		awaitTimeout:   150 * time.Millisecond,
	}

	if err := p.recoverOneEndpoint(t.Context(), containerID, "net-1", "ep-1",
		"02:42:ac:11:00:02", "", "", DHCPNetworkOptions{}); err != nil {
		t.Fatalf("recoverOneEndpoint: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p.recoveryFailed.Load()+p.recoveryAbortedContainerGone.Load() > 0 {
			// One more beat so a second, wrong bump would be visible
			// rather than racing us to the assertion.
			time.Sleep(50 * time.Millisecond)
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("neither recovery counter moved within 10s; Start did not fail as expected")
	return nil
}

func TestRecoverOneEndpoint_ExitedContainerIsNotAFailure(t *testing.T) {
	f := &fakeDocker{
		inspectResult:   map[string]dNetwork.Inspect{"net-1": {}},
		containerResult: map[string]dContainer.InspectResponse{"gone-1": stopped("exited")},
	}
	p := recoverAndAwaitCounter(t, f, "gone-1")

	if got := p.recoveryAbortedContainerGone.Load(); got != 1 {
		t.Errorf("recoveryAbortedContainerGone: got %d, want 1", got)
	}
	if got := p.recoveryFailed.Load(); got != 0 {
		t.Errorf("recoveryFailed: got %d, want 0 — a container that exited before recovery is not a plugin fault (#376)", got)
	}
}

func TestRecoverOneEndpoint_RunningContainerIsAFailure(t *testing.T) {
	f := &fakeDocker{
		inspectResult:   map[string]dNetwork.Inspect{"net-1": {}},
		containerResult: map[string]dContainer.InspectResponse{"live-1": running()},
	}
	p := recoverAndAwaitCounter(t, f, "live-1")

	if got := p.recoveryFailed.Load(); got != 1 {
		t.Errorf("recoveryFailed: got %d, want 1 — a running container really has lost its renewal client", got)
	}
	if got := p.recoveryAbortedContainerGone.Load(); got != 0 {
		t.Errorf("recoveryAbortedContainerGone: got %d, want 0", got)
	}
}

func TestRecoverOneEndpoint_UnreadableDaemonCountsARealFailure(t *testing.T) {
	// The regression this guards: classifying on weak evidence. If an
	// inspect error were read as "gone", a daemon that is failing every
	// inspect — plausibly the same reason recovery failed — would zero
	// out the counter that is supposed to be shouting.
	f := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{"net-1": {}},
		containerErr:  errors.New("dial unix /var/run/docker.sock: connect: connection refused"),
	}
	p := recoverAndAwaitCounter(t, f, "maybe-1")

	if got := p.recoveryFailed.Load(); got != 1 {
		t.Errorf("recoveryFailed: got %d, want 1", got)
	}
	if got := p.recoveryAbortedContainerGone.Load(); got != 0 {
		t.Errorf("recoveryAbortedContainerGone: got %d, want 0 — an unreadable daemon is not proof the container left", got)
	}
}

// TestApiHealth_RecoveryAbortedContainerGoneIsNotUnhealthy is the
// counterpart to TestApiHealth_JoinStartFailureUnhealthy: this counter
// must be visible on the wire and must NOT flip healthy. If it ever
// does, a daemon restart that outlived a container starts paging an
// operator over a normal exit — the exact #376 regression.
func TestApiHealth_RecoveryAbortedContainerGoneIsNotUnhealthy(t *testing.T) {
	p := &Plugin{
		startTime:      time.Now(),
		joinHints:      make(map[string]joinHint),
		persistentDHCP: make(map[string]*dhcpManager),
	}
	p.recoveryAbortedContainerGone.Add(3)

	req := httptest.NewRequest(http.MethodGet, "/Plugin.Health", nil)
	rec := httptest.NewRecorder()
	p.apiHealth(rec, req)

	var got HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Healthy {
		t.Error("a container that exited before recovery must not mark the plugin unhealthy (#376)")
	}
	if got.RecoveryAbortedContainerGone != 3 {
		t.Errorf("recovery_aborted_container_gone: got %d, want 3", got.RecoveryAbortedContainerGone)
	}
	if got.RecoveryFailed != 0 {
		t.Errorf("recovery_failed: got %d, want 0 — the two counters must not share a bump", got.RecoveryFailed)
	}
}
