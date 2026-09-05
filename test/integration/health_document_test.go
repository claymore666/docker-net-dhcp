// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// TestHealthDocument_EndpointEntryMatchesTheContainer reads one real
// container's entry out of /Plugin.Health's `endpoints` array (O-3) and
// checks the address against the kernel's view from inside that
// container's own namespace.
//
// THE ADDRESS IS CHECKED FROM OUTSIDE THE PLUGIN, which is the whole
// point of the cell (#524). Every other field in the entry is the
// plugin's account of itself and can only be read for shape; the
// address is the one value something else can be asked about, and
// `ip -4 addr show` inside the container is that something else. An
// entry that rendered the wrong endpoint's address -- the mutant this
// cell exists for -- passes every self-consistent check and fails here.
func TestHealthDocument_EndpointEntryMatchesTheContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	netName := "dh-itest-health-doc"
	ctrName := "dh-itest-health-doc-ctr"

	t.Cleanup(func() {
		if t.Failed() {
			fixture.DumpLogs(func(s string) { t.Log(s) })
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	netID := harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	id, ipv4, _ := harness.RunContainer(t, ctx, netName, ctrName)
	harness.AssertIP(t, ipv4)

	// One reading, not a window: nothing here is a delta. The fields
	// below are the document's account of a state that exists right
	// now, and WaitPluginHealth is the suite's sanctioned single read
	// (see counterwindow_guard_test.go) precisely because it makes no
	// claim about counters.
	h := harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)
	if h.Endpoints == nil {
		t.Fatal("this plugin publishes no `endpoints` array, so the per-endpoint document " +
			"cannot be judged — and reading its absence as an empty array is how a plugin " +
			"that publishes nothing would pass this cell")
	}
	if len(h.Endpoints) != h.ActiveEndpoints {
		t.Errorf("`endpoints` has %d entries and active_endpoints is %d. The array is documented as "+
			"bounded by that count; a difference means one of the two is reading a different map",
			len(h.Endpoints), h.ActiveEndpoints)
	}

	wantNet := shortDockerID(netID)
	var e *harness.EndpointHealth
	for i := range h.Endpoints {
		if h.Endpoints[i].Network == wantNet {
			e = &h.Endpoints[i]
			break
		}
	}
	if e == nil {
		t.Fatalf("no entry in `endpoints` carries network %s. The whole array was:\n%s",
			wantNet, formatEndpoints(h.Endpoints))
	}

	// 1. OUTSIDE EVIDENCE. The document says this endpoint holds this
	// address; the kernel inside the container is asked whether it
	// does. `ip` prints "inet <addr>/<prefix> " and the document
	// renders the same CIDR, so the comparison is of the whole thing
	// and not of a prefix of it.
	out := harness.ExecOutput(t, ctx, id, "ip", "-4", "addr", "show")
	if e.Address == "" {
		t.Fatalf("the entry for %s carries no address while the container holds %s:\n%s",
			wantNet, ipv4, out)
	}
	if !strings.Contains(out, "inet "+e.Address+" ") {
		t.Errorf("`endpoints` says %s holds %s; `ip -4 addr show` inside the container does not "+
			"carry it. The document's address is the plugin's word for it and this is the "+
			"kernel's.\n%s", wantNet, e.Address, out)
	}
	// The address Docker reported for the container, independently of
	// both: an entry rendering a DIFFERENT live endpoint's address
	// would still be a real address on some interface somewhere.
	if pfx, perr := netip.ParsePrefix(e.Address); perr != nil {
		t.Errorf("`endpoints` renders %q for %s, which does not parse as a CIDR", e.Address, wantNet)
	} else if pfx.Addr().String() != ipv4 {
		t.Errorf("`endpoints` says %s holds %s; Docker says the container has %s",
			wantNet, e.Address, ipv4)
	}

	// 2. THE REST OF THE FIELDS, each for what it can be checked
	// against. These are the plugin's own account, so each assertion
	// says what would be wrong rather than repeating the value.
	if e.Endpoint == "" {
		t.Error("the entry carries no endpoint id, so nothing in it can be attributed")
	}
	if e.Mode != "macvlan" {
		t.Errorf("`endpoints` says mode=%q for a network created with mode=macvlan", e.Mode)
	}
	if e.LeaseState != "bound" {
		t.Errorf("`endpoints` says lease_state=%q for an endpoint whose container is up with %s; "+
			"a bound client is the only state that carries an address", e.LeaseState, ipv4)
	}
	switch e.ConflictCheck {
	case "wait", "async", "off":
	default:
		t.Errorf("`endpoints` says conflict_check=%q, which is not one of the three D23 modes",
			e.ConflictCheck)
	}
	switch e.ACDPhase {
	case "idle", "probing", "settling", "announcing", "defending":
	default:
		t.Errorf("`endpoints` says acd_phase=%q, which is not one of the RFC 5227 phases",
			e.ACDPhase)
	}
	if e.Server == "" {
		t.Error("`endpoints` carries no server for a bound lease: option 54 identified the server " +
			"that granted it and the document dropped it")
	} else if net.ParseIP(e.Server) == nil {
		t.Errorf("`endpoints` says server=%q, which is not an address", e.Server)
	}
	// The times are absolute by design, so each one is checked for
	// being a time AND for lying on the right side of now. A renewal
	// deadline in the past is the shape a duration rendered as an
	// instant would take.
	now := time.Now()
	for _, ts := range []struct {
		name, value string
		wantFuture  bool
	}{
		{"renew_at", e.RenewAt, true},
		{"rebind_at", e.RebindAt, true},
		{"expires_at", e.ExpiresAt, true},
		{"last_event_at", e.LastEventAt, false},
	} {
		if ts.value == "" {
			t.Errorf("`endpoints` carries no %s for a bound endpoint", ts.name)
			continue
		}
		at, perr := time.Parse(time.RFC3339Nano, ts.value)
		if perr != nil {
			t.Errorf("`endpoints` renders %s=%q, which is not an RFC 3339 instant: %v",
				ts.name, ts.value, perr)
			continue
		}
		if ts.wantFuture && !at.After(now) {
			t.Errorf("`endpoints` renders %s=%s, which is not in the future. A lease deadline in "+
				"the past is what a remaining-seconds value rendered as an instant looks like",
				ts.name, ts.value)
		}
		if !ts.wantFuture && at.After(now.Add(time.Minute)) {
			t.Errorf("`endpoints` renders %s=%s, which is in the future", ts.name, ts.value)
		}
	}
	if e.LastEvent == "" {
		t.Error("`endpoints` carries no last_event for an endpoint that has bound a lease")
	}

	t.Logf("CELL-ENDPOINT endpoint=%s network=%s mode=%s state=%s address=%s server=%s "+
		"conflict_check=%s acd_phase=%s last_event=%s@%s renew=%s rebind=%s expires=%s",
		e.Endpoint, e.Network, e.Mode, e.LeaseState, e.Address, e.Server,
		e.ConflictCheck, e.ACDPhase, e.LastEvent, e.LastEventAt, e.RenewAt, e.RebindAt, e.ExpiresAt)

	// 3. The document's own agreement, which the unit tests cannot
	// reach: this plugin is serving a real endpoint, so `status` and
	// `healthy` have to be saying the same thing about it.
	if h.Status == nil {
		t.Fatal("this plugin publishes no `status`, so pass/warn/fail cannot be judged")
	}
	if *h.Status == "fail" && h.Healthy {
		t.Errorf("status=fail beside healthy=true: the two are derived from one declaration and "+
			"must never disagree. checks: %v", h.Checks)
	}
	if *h.Status != "fail" && !h.Healthy {
		t.Errorf("status=%s beside healthy=false: a latched fault is missing from the checks. "+
			"checks: %v", *h.Status, h.Checks)
	}
}

// TestHealthDocument_BuildInfoIsWhatTheLaneBuilt reads the three build
// identity values (O-4) back out of the plugin this lane built, on both
// surfaces they are published on.
//
// WHY THE SHA COMES FROM THE ENVIRONMENT. The workflow step that builds
// the plugin derives the commit once and exports it (NET_DHCP_EXPECT_COMMIT);
// this cell compares against that value rather than deriving its own,
// because a second derivation would be checked against itself and the
// looser of the two would decide. Where the variable is not set -- a
// hand-installed plugin, a lane that has not been taught to export it --
// the value is still required to be a full revision rather than absent
// or empty, which is the failure that looks like nothing.
func TestHealthDocument_BuildInfoIsWhatTheLaneBuilt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Cleanup(func() {
		if t.Failed() {
			harness.DumpPluginLog(t)
		}
	})

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	// One reading, no delta: see the note in the cell above.
	h := harness.WaitPluginHealth(t, ctx, cli, 15*time.Second)
	if h.Version == nil || h.Commit == nil || h.Library == nil {
		t.Fatalf("this plugin publishes version=%v commit=%v library=%v: an absent field and an "+
			"empty one are different answers and only one of them is a build that did not carry "+
			"its identity", h.Version, h.Commit, h.Library)
	}
	t.Logf("CELL-BUILDINFO version=%q commit=%q library=%q", *h.Version, *h.Commit, *h.Library)

	for _, f := range []struct{ name, value string }{
		{"version", *h.Version},
		{"commit", *h.Commit},
		{"library", *h.Library},
	} {
		if f.value == "" {
			t.Errorf("%s is empty. The build carries a WORD when it does not know the value "+
				"(`dev`, `unknown`); an empty string is the failure that looks like nothing",
				f.name)
		}
	}

	// The library revision is not a build argument: the Dockerfile
	// reads it out of the tree. So the tree is what it is checked
	// against, and this is the one of the three that has an
	// independent source inside the repository.
	if want, rerr := os.ReadFile("../../internal/dhcp-golib/SOURCE"); rerr != nil {
		t.Errorf("read internal/dhcp-golib/SOURCE: %v", rerr)
	} else if got, w := *h.Library, strings.TrimSpace(string(want)); got != w {
		t.Errorf("the plugin reports library=%q and this tree's internal/dhcp-golib/SOURCE says "+
			"%q. The image was built from a different library revision than the one under test",
			got, w)
	}

	if want := os.Getenv("NET_DHCP_EXPECT_COMMIT"); want != "" {
		if *h.Commit != want {
			t.Errorf("the plugin reports commit=%q and this lane built %q: the running plugin is "+
				"not the tree these cells are measuring", *h.Commit, want)
		}
	} else if !fullRevision.MatchString(*h.Commit) && *h.Commit != "unknown" {
		t.Errorf("the plugin reports commit=%q, which is neither a full 40-character revision nor "+
			"the word `unknown`. An abbreviated revision is a value whose length depends on the "+
			"clone that produced it", *h.Commit)
	}

	// The second surface. build_info is a gauge whose only purpose is
	// its labels, so a series rendered with an empty label value is
	// the failure this half exists for -- it scrapes, it graphs, and
	// it identifies nothing.
	body, _, err := harness.PluginMetrics(ctx, cli)
	if err != nil {
		t.Fatalf("/metrics: %v", err)
	}
	var line string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "net_dhcp_build_info{") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("/metrics carries no net_dhcp_build_info series:\n%s", body)
	}
	t.Logf("CELL-BUILDINFO metrics: %s", line)
	if !strings.HasSuffix(line, " 1") {
		t.Errorf("net_dhcp_build_info renders %q: an identity series carries the value 1", line)
	}
	for _, want := range []string{
		fmt.Sprintf("version=%q", *h.Version),
		fmt.Sprintf("commit=%q", *h.Commit),
		fmt.Sprintf("library=%q", *h.Library),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("net_dhcp_build_info does not carry %s. The document and the exposition are "+
				"two renderings of one fact and a difference means one of them is stale:\n%s",
				want, line)
		}
	}
	if strings.Contains(line, `=""`) {
		t.Errorf("net_dhcp_build_info carries an empty label value: %s", line)
	}
}

var fullRevision = regexp.MustCompile(`^[0-9a-f]{40}$`)

// shortDockerID mirrors the plugin's own shortID: the document renders
// ids at 12 characters and the API hands out 64.
func shortDockerID(id string) string {
	if len(id) >= 12 {
		return id[:12]
	}
	return id
}

func formatEndpoints(es []harness.EndpointHealth) string {
	var b strings.Builder
	for _, e := range es {
		fmt.Fprintf(&b, "  endpoint=%s network=%s mode=%s state=%s address=%s\n",
			e.Endpoint, e.Network, e.Mode, e.LeaseState, e.Address)
	}
	return b.String()
}
