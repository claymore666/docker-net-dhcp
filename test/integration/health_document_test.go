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

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// The address is the one field something outside the plugin can confirm: an entry rendering another endpoint's
// address passes every self-consistent check and fails the in-container `ip -4 addr` here (#524).

// TestHealthDocument_EndpointEntryMatchesTheContainer checks a container's /Plugin.Health endpoint entry against the kernel inside the container (#524).
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

	// One reading, no delta. The client binds after Join returns, so the entry reads `acquiring` for a moment, as one CI
	// reading did; lease_state is the precondition, since waiting for the address to match would only prove it eventually did (#524).
	wantNet := shortDockerID(netID)
	h := harness.WaitPluginHealthFor(t, ctx, cli, 30*time.Second,
		"the endpoint on network "+wantNet+" to report lease_state=bound",
		func(h *harness.HealthResponse) bool {
			for _, e := range h.Endpoints {
				if e.Network == wantNet {
					return e.LeaseState == "bound"
				}
			}
			return false
		})
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

	// `ip` prints "inet <addr>/<prefix> " and the document renders the same CIDR, so the whole prefix is compared.
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
	// Docker's own address for the container: another live endpoint's address would still exist on some interface.
	if pfx, perr := netip.ParsePrefix(e.Address); perr != nil {
		t.Errorf("`endpoints` renders %q for %s, which does not parse as a CIDR", e.Address, wantNet)
	} else if pfx.Addr().String() != ipv4 {
		t.Errorf("`endpoints` says %s holds %s; Docker says the container has %s",
			wantNet, e.Address, ipv4)
	}

	if e.Endpoint == "" {
		t.Error("the entry carries no endpoint id, so nothing in it can be attributed")
	}
	if e.Mode != "macvlan" {
		t.Errorf("`endpoints` says mode=%q for a network created with mode=macvlan", e.Mode)
	}
	// lease_state was the precondition of the read and cannot fail here, so it is not asserted.
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
	// A renewal deadline in the past is how a duration rendered as an instant would look.
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

// NET_DHCP_EXPECT_COMMIT is derived once by the build step, since a second derivation would be checked against itself;
// unset, the value must still be a full revision, since an empty one looks like nothing (#910).

// TestHealthDocument_BuildInfoIsWhatTheLaneBuilt checks the three build identity values on both surfaces against what this lane built.
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

	// The Dockerfile reads the library version with the Makefile's `go list -m`, turning a failure into `unknown`, so a
	// failed derivation here is a failure, not a skip (#910).
	w, rerr := harness.CommandStdout(ctx, "go", "list", "-m",
		"-f", "{{.Version}}", "github.com/claymore666/dhcp-golib")
	if rerr != nil {
		t.Errorf("go list -m github.com/claymore666/dhcp-golib: %v", rerr)
	} else if got := *h.Library; got != w {
		t.Errorf("the plugin reports library=%q and this tree's go.mod pins "+
			"%q. The image was built from a different library version than the one under test",
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

	// build_info exists only for its labels, so an empty label value scrapes and graphs and identifies nothing.
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

// shortDockerID mirrors the plugin's shortID: 12 characters of the 64 the API returns.
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
