// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"

	dContainer "github.com/docker/docker/api/types/container"
	dNetwork "github.com/docker/docker/api/types/network"
)

// armCounts is the whole arm surface, read together. Read together
// because the property being asserted is an ACCOUNT of
// sandbox_key_entry_failures, not four independent numbers: a test that
// checks one arm rose passes on a build where two did.
type armCounts struct {
	failures      int32
	notPermitted  int32
	notANamespace int32
	wrongType     int32
	unavailable   int32
}

func readArms(p *Plugin) armCounts {
	return armCounts{
		failures:      p.sandboxKeyEntryFailures.Load(),
		notPermitted:  p.sandboxKeyNotPermitted.Load(),
		notANamespace: p.sandboxKeyNotANamespace.Load(),
		wrongType:     p.sandboxKeyWrongNSType.Load(),
		unavailable:   p.sandboxKeyUnavailable.Load(),
	}
}

func (a armCounts) sum() int32 {
	return a.notPermitted + a.notANamespace + a.wrongType + a.unavailable
}

// TestSandboxKeyRefusal_EachArmIsCountedSeparately is the unit half of
// the finding the review raised: SECURITY.md says the key route is
// refused because the daemon's per-sandbox bind mounts are not
// propagated into this plugin's mount namespace, and before the arms
// existed nothing in the tree — no test, no cell, no log line reaching a
// green run — could tell that apart from a key this plugin declines on
// sight, which is what a daemon with a non-default --exec-root produces.
// Identical aggregate, opposite remedies.
//
// Every case drives the PRODUCTION opener (the one that reads the
// package variable) so the counters are reached the way an attach
// reaches them, and each asserts the whole arm surface rather than its
// own arm: an arm that fires as well is as wrong as an arm that does
// not fire.
func TestSandboxKeyRefusal_EachArmIsCountedSeparately(t *testing.T) {
	// The fallback needs a PID that is NOT the container's, so the PID
	// route refuses too and no case here can pass by taking it.
	for _, tc := range []struct {
		name string
		// entry builds the fixture inside dir and returns the key to
		// hand the opener. An empty return means "a key outside dir".
		entry func(t *testing.T, dir string) string
		want  armCounts
	}{
		{
			name:  "a key outside the permitted directories",
			entry: func(t *testing.T, dir string) string { return "/tmp/not-a-sandbox-key" },
			want:  armCounts{failures: 1, notPermitted: 1},
		},
		{
			name: "the placeholder file libnetwork leaves under the mount",
			entry: func(t *testing.T, dir string) string {
				key := filepath.Join(dir, "aa11bb22")
				if err := os.WriteFile(key, []byte{}, 0o600); err != nil {
					t.Fatalf("write fixture entry: %v", err)
				}
				return key
			},
			want: armCounts{failures: 1, notANamespace: 1},
		},
		{
			name: "a namespace of the wrong type",
			entry: func(t *testing.T, dir string) string {
				key := filepath.Join(dir, "cc33dd44")
				if err := os.Symlink("/proc/self/ns/uts", key); err != nil {
					t.Fatalf("link fixture entry: %v", err)
				}
				return key
			},
			want: armCounts{failures: 1, wrongType: 1},
		},
		{
			name: "an entry that never appears",
			entry: func(t *testing.T, dir string) string {
				return filepath.Join(dir, "never-appears")
			},
			want: armCounts{failures: 1, unavailable: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			key := tc.entry(t, dir)
			withSandboxNetnsDirs(t, []string{dir})

			p := &Plugin{}
			m := &dhcpManager{plugin: p}

			// Short: the "never appears" case is the only one that
			// spends the budget, and it has to spend all of it.
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
			defer cancel()

			ns, err := m.openSandboxNetNS(ctx, key, os.Getpid(), foreignCtrID, 10*time.Millisecond)
			if err == nil {
				closeNsHandle(ns)
				t.Fatal("both routes were expected to fail: the PID handed in does not name a container")
			}

			got := readArms(p)
			if got != tc.want {
				t.Errorf("arms = %+v, want %+v.\nAn arm that fires as well as the right one is as "+
					"wrong as one that does not fire: the four are an account of "+
					"sandbox_key_entry_failures, and a cell reads them as one.", got, tc.want)
			}
			// Stated separately from the comparison above so a future
			// case that changes `want` cannot drop the invariant with it.
			if got.sum() != got.failures {
				t.Errorf("the arms sum to %d and sandbox_key_entry_failures is %d: a refusal was "+
					"counted in the aggregate and attributed to no arm", got.sum(), got.failures)
			}
		})
	}
}

// TestSandboxKeyRefusal_ClassificationIsTotal drives the classifier with
// an error carrying none of the three sentinels.
//
// The residual arm is the reason the sum invariant holds by
// construction rather than by everybody remembering to add a counter
// when they add a refusal. Drive the absence: with the default arm gone,
// this case increments nothing and the invariant breaks.
func TestSandboxKeyRefusal_ClassificationIsTotal(t *testing.T) {
	p := &Plugin{}
	p.countSandboxKeyRefusal(context.DeadlineExceeded)

	got := readArms(p)
	if got.unavailable != 1 {
		t.Errorf("sandbox_key_unavailable = %d after a refusal carrying no arm sentinel, want 1: "+
			"an unclassified refusal must land in the residual arm, where it is visible, rather "+
			"than nowhere", got.unavailable)
	}
	if got.notPermitted+got.notANamespace+got.wrongType != 0 {
		t.Errorf("a refusal carrying no arm sentinel was attributed to a named arm: %+v", got)
	}
}

// TestSandboxKeyFallback_IsNotAWarning is finding 3.
//
// On a stock engine this fallback happens on EVERY attach, of every
// container, forever. A warning asks its reader to do something, and
// there is nothing to do — so a line that is correct and unactionable
// on every attach of a healthy host is training an operator to filter
// the level. The counters carry the signal at every log level; the line
// carries the detail.
//
// Both directions are asserted. Only the first would be satisfied by
// deleting the line altogether.
func TestSandboxKeyFallback_IsNotAWarning(t *testing.T) {
	withSandboxNetnsDirs(t, []string{"/var/run/docker/netns"})

	p := &Plugin{}
	m := &dhcpManager{plugin: p}
	pid := os.Getpid()
	ctrID := selfCgroupLeaf(t, pid)

	warned := captureWarnings(t, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ns, err := m.openSandboxNetNS(ctx, "/tmp/not-a-sandbox-key", pid, ctrID, time.Millisecond)
		if err != nil {
			t.Fatalf("the fallback did not carry the open: %v", err)
		}
		closeNsHandle(ns)
	})
	if len(warned) != 0 {
		t.Errorf("the ordinary key-route fallback logged %d WARN-or-above line(s):\n%s\n"+
			"This happens once per attach on a stock engine and there is nothing an operator "+
			"should do about it. sandbox_key_entry_failures and its arms are the record; the "+
			"level is what says whether to look.", len(warned), strings.Join(warned, "\n"))
	}

	var buf strings.Builder
	prevOut, prevLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetLevel(prevLevel)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ns, err := m.openSandboxNetNS(ctx, "/tmp/not-a-sandbox-key", pid, ctrID, time.Millisecond)
	if err != nil {
		t.Fatalf("the fallback did not carry the open: %v", err)
	}
	closeNsHandle(ns)

	out := buf.String()
	if !strings.Contains(out, "level=debug") {
		t.Errorf("no debug line was emitted for the fallback:\n%s\nLowering the level must not mean "+
			"deleting the evidence — the reason the key was refused is only in this line.", out)
	}
	// The reason is taken from the sentinel rather than typed, so a
	// reworded refusal cannot leave this asserting a string nothing
	// emits any more.
	for _, want := range []string{"the container PID route carries this attach", errSandboxKeyNotPermitted.Error()} {
		if !strings.Contains(out, want) {
			t.Errorf("the fallback line does not carry %q:\n%s\n"+
				"Lowering the level must not mean losing WHICH refusal happened.", want, out)
		}
	}
}

// TestMetricHelp_NamesTheExpectedStateAsExpected is finding 3's other
// half, and the surface an operator actually reads: the HELP string
// served on /metrics, which a dashboard shows beside the number.
//
// The counter's help used to say a sustained rise "means the
// /var/run/docker mount is not carrying the daemon's sandbox netns
// entries on this host" — true, and phrased as a diagnosis of a fault,
// for a state that is normal on every stock engine. The prose is not
// checkable in general; that it does not call the normal state abnormal
// without saying so is.
func TestMetricHelp_NamesTheExpectedStateAsExpected(t *testing.T) {
	help := map[string]string{}
	for _, d := range metricDefs() {
		help[d.name] = d.help
	}
	for _, name := range []string{"sandbox_key_entry_failures", "sandbox_key_not_a_namespace"} {
		h, ok := help[name]
		if !ok {
			t.Errorf("%s has no metric definition, so it is not on /metrics at all", name)
			continue
		}
		if !strings.Contains(h, "EXPECTED") {
			t.Errorf("the /metrics help for %s does not say the state it counts is expected:\n%s\n"+
				"On a stock engine it rises once per container attach forever. docs/reference.md "+
				"says so; an operator reading a dashboard sees this string instead.", name, h)
		}
	}
	// The other direction: the arm that is NOT expected must not be
	// described as if it were, or the pair says nothing.
	if h := help["sandbox_key_not_permitted"]; !strings.Contains(strings.ToLower(h), "not expected") {
		t.Errorf("the /metrics help for sandbox_key_not_permitted does not mark it as the "+
			"unexpected arm:\n%s\nIt is the one that shares an aggregate with the ordinary case "+
			"and wants a different remedy.", h)
	}
}

// TestStart_RecoveryTakesTheKeyFromTheInspect is finding 4.
//
// recoverOneEndpoint synthesises a JoinRequest with NO SandboxKey — a
// re-adoption has no Join to carry one — so the only production route to
// the key on that path is the ContainerInspect that Start already makes.
// A reviewer's mutant deleting that assignment SURVIVED `go test
// ./pkg/...`: the sole observer was one integration cell on one shard.
//
// This drives Start with the recovery SHAPE (empty joinReq.SandboxKey)
// and a fake daemon whose inspect carries a key naming a real network
// namespace, and asserts the key route was the one taken. It exercises
// the LINE, not a helper called by it, which is the only version that
// kills that mutant.
func TestStart_RecoveryTakesTheKeyFromTheInspect(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "ee55ff66")
	if err := linkANetnsEntry(key); err != nil {
		t.Fatalf("link fixture entry: %v", err)
	}
	withSandboxNetnsDirs(t, []string{dir})

	const netID = "netid-recovery-key-source"
	const epID = "epid-recovery-key-source"
	const ctrID = "ctrid-recovery-key-source"

	// The key lives here and NOWHERE else in this test: the JoinRequest
	// below carries none, exactly as a re-adoption's synthesised one
	// does. Assigned through the promoted field rather than through a
	// composite literal, because the struct that declares it is
	// deprecated and naming it fails staticcheck — the production read
	// in dhcp_manager.go goes through the same promoted field.
	settings := &dContainer.NetworkSettings{}
	settings.SandboxKey = key

	docker := &fakeDocker{
		inspectResult: map[string]dNetwork.Inspect{
			netID: {Containers: map[string]dNetwork.EndpointResource{
				ctrID: {EndpointID: epID},
			}},
		},
		containerResult: map[string]dContainer.InspectResponse{
			ctrID: {
				ContainerJSONBase: &dContainer.ContainerJSONBase{
					State: &dContainer.State{Pid: os.Getpid()},
				},
				Config:          &dContainer.Config{Hostname: "recovered"},
				NetworkSettings: settings,
			},
		},
	}

	p := &Plugin{}
	m := newDHCPManager(docker, JoinRequest{NetworkID: netID, EndpointID: epID}, DHCPNetworkOptions{}).withPlugin(p)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start is EXPECTED to fail: there is no container link in this
	// process's namespace to locate. Everything asserted below happens
	// before that point, and asserting the counters rather than the
	// error is what keeps this test about the key source.
	_ = m.Start(ctx)
	closeNsHandle(m.nsHandle)
	closeNetHandle(m.netHandle)

	if got := p.sandboxKeyEntries.Load(); got != 1 {
		t.Errorf("sandbox_key_entries = %d, want 1. Recovery did not enter through the sandbox key, "+
			"and the only place it can come from on that path is ContainerInspect's "+
			"NetworkSettings.SandboxKey — the JoinRequest here carries none, exactly as a "+
			"re-adoption's synthesised one does.", got)
	}
	if got := p.sandboxPIDFallbacks.Load(); got != 0 {
		t.Errorf("sandbox_pid_fallbacks = %d, want 0: the key was available and the PID route was "+
			"taken anyway", got)
	}
	if got := p.sandboxKeyEntryFailures.Load(); got != 0 {
		t.Errorf("sandbox_key_entry_failures = %d, want 0", got)
	}
}

// TestHealthSnapshot_CarriesTheRefusalArms is the wiring assertion for
// the arms, and it is not decoration: an arm that is counted in the
// process and not published is an arm no cell can read, which is the
// exact shape of the finding it answers. The counters were already
// right when the review found nothing could see them.
//
// Distinct values per arm, so a snapshot that populates every field
// from one counter — or wires two fields to the same one — fails here
// rather than agreeing with itself.
func TestHealthSnapshot_CarriesTheRefusalArms(t *testing.T) {
	p := &Plugin{}
	p.sandboxKeyNotPermitted.Store(3)
	p.sandboxKeyNotANamespace.Store(5)
	p.sandboxKeyWrongNSType.Store(7)
	p.sandboxKeyUnavailable.Store(11)

	h := p.healthSnapshot()
	for _, tc := range []struct {
		name string
		got  int32
		want int32
	}{
		{"sandbox_key_not_permitted", h.SandboxKeyNotPermitted, 3},
		{"sandbox_key_not_a_namespace", h.SandboxKeyNotANamespace, 5},
		{"sandbox_key_wrong_ns_type", h.SandboxKeyWrongNSType, 7},
		{"sandbox_key_unavailable", h.SandboxKeyUnavailable, 11},
	} {
		if tc.got != tc.want {
			t.Errorf("/Plugin.Health carries %s = %d, want %d. The integration cells read this "+
				"endpoint; an arm counted in the process and not published here is an arm no cell "+
				"can assert on.", tc.name, tc.got, tc.want)
		}
	}
}
