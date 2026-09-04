// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// A recorded call is one log line carrying the transport's message and
// two logrus fields. logrus' TextFormatter is what the plugin runs
// with, so fields arrive as key=value with the value quoted only when
// the value needs it; both spellings are matched, because which one
// appears is a property of the value and not of the plugin.
//
// Three expressions rather than one: Go's regexp is RE2 and has no
// backreferences, so "the same quote on both sides" cannot be written
// as a pattern. Matching the fields separately says the same thing
// without pretending otherwise.
var (
	dockerAPICallLine   = regexp.MustCompile(`msg="docker-api call"`)
	dockerAPICallMethod = regexp.MustCompile(`\bmethod="?([^"\s]*)"?`)
	dockerAPICallPath   = regexp.MustCompile(`\bpath="?([^"\s]*)"?`)
)

// parseDockerAPICall pulls the method and path out of one recorded
// line. A line that carries the message but not both fields is a
// FAILURE to report, not a line to skip: it means the record changed
// shape and this test would otherwise quietly judge a smaller set than
// the plugin actually made.
func parseDockerAPICall(t *testing.T, line string) (method, path string, isCall bool) {
	t.Helper()
	if !dockerAPICallLine.MatchString(line) {
		return "", "", false
	}
	m := dockerAPICallMethod.FindStringSubmatch(line)
	p := dockerAPICallPath.FindStringSubmatch(line)
	if m == nil || p == nil {
		t.Errorf("a `docker-api call` line carries no method and path pair, so the recorded set "+
			"cannot be read off the log any more: %s", line)
		return "", "", false
	}
	return m[1], p[1], true
}

// TestDockerAPI_OnlySafeMethodsReachTheDaemon is the live half of
// #691's read-only socket contract.
//
// WHAT THE UNIT SUITE ALREADY PROVES, AND WHY IT IS NOT ENOUGH.
// docker_transport_test.go drives the round tripper directly: POST,
// PUT, PATCH, DELETE and a lowercase "get" are refused, GET and HEAD
// pass, and the refusal counter moves. All of that is a statement about
// a transport in a test process. It says nothing about whether the
// transport is installed in the client the RUNNING plugin uses, nor
// what that plugin actually sends to a real daemon across real
// container lifecycles. The interesting failure is not "the wrapper is
// wrong" — it is "the wrapper is not in the path", and only a live
// plugin can be asked that.
//
// THE ASSERTION IS THE SET, NOT A SAMPLE. The plugin logs each distinct
// method+path once, so its log carries the SET of shapes the daemon saw
// since the plugin started — every network create and every container
// attach this shard has run. That set is printed into the run whether
// the test passes or fails: the handover's claim is about what the
// plugin calls, and a claim like that should be readable off a green
// run rather than reconstructed from a red one. It is also how #691's
// three-call list gets RE-MEASURED at this base instead of quoted.
//
// THE DOMAIN IS ASSERTED FIRST, IN THREE PARTS. "No unsafe method was
// seen" is true of a plugin that made no calls at all, of a log that
// was never written, and of a build with the recording removed. So this
// requires at least one recorded call, at least one GET among them, and
// the two shapes the plugin cannot work without: a network read and a
// ContainerInspect (the sandbox key and the hostname both come from it
// — seam design D-5). Without those, "every call was safe" is a
// statement about a set the test itself emptied.
func TestDockerAPI_OnlySafeMethodsReachTheDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const (
		netName = "dh-itest-roapi"
		ctrName = "dh-itest-roapi-ctr"
	)

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

	w := harness.BeginCounterWindow(t, ctx, cli, "docker_api_non_get_refusals")

	// Exercise the plugin so the window covers real API traffic rather
	// than only whatever startup left behind: CreateNetwork drives the
	// network reads, and attaching a container drives Join's
	// ContainerInspect.
	harness.CreateNetwork(t, ctx, netName, "macvlan", nil)
	_, ipv4, _ := harness.RunContainer(t, ctx, netName, ctrName)
	harness.AssertIP(t, ipv4)

	before, after := w.End()
	refusals, ok := counterDelta(t, "docker_api_non_get_refusals",
		before.DockerAPINonGETRefusals, after.DockerAPINonGETRefusals)
	if !ok {
		return
	}

	observed := map[string]bool{}
	methods := map[string]int{}
	sawContainerInspect := false
	sawNetworkRead := false
	for _, line := range strings.Split(harness.ReadPluginLog(t, ctx), "\n") {
		method, path, isCall := parseDockerAPICall(t, line)
		if !isCall {
			continue
		}
		observed[method+" "+path] = true
		methods[method]++
		if strings.Contains(path, "/containers/") && strings.HasSuffix(path, "/json") {
			sawContainerInspect = true
		}
		if strings.Contains(path, "/networks") {
			sawNetworkRead = true
		}
	}

	calls := make([]string, 0, len(observed))
	for c := range observed {
		calls = append(calls, c)
	}
	sort.Strings(calls)
	t.Logf("DOCKER-API SURFACE, as the plugin's own transport recorded it against a live daemon:\n  %s",
		strings.Join(calls, "\n  "))

	// The domain.
	if len(calls) == 0 {
		t.Fatalf("the plugin's log records no `docker-api call` line at all, so there is no set to "+
			"judge. Either the read-only transport is not installed in the client the plugin uses, or "+
			"the lane no longer runs it at a level that emits the record (the Makefile sets "+
			"LOG_LEVEL=trace). Refusals counted in the same window: %d", refusals)
	}
	if methods["GET"] == 0 {
		t.Errorf("not one recorded call is a GET, yet the plugin cannot resolve a network or a "+
			"container hostname without one: %v", calls)
	}
	if !sawNetworkRead {
		t.Errorf("no /networks call was recorded across a network create, so the record does not "+
			"cover NetworkList/NetworkInspect and the set below is short by them: %v", calls)
	}
	if !sawContainerInspect {
		t.Errorf("no ContainerInspect shape (GET /containers/<id>/json) was recorded across a full "+
			"container attach, so the record does not cover the call the sandbox key and the hostname "+
			"come from: %v", calls)
	}

	// The claim.
	for _, c := range calls {
		method := strings.SplitN(c, " ", 2)[0]
		if method != "GET" && method != "HEAD" {
			t.Errorf("the plugin sent %q to the daemon. Only GET and HEAD are safe and body-less "+
				"(RFC 9110 sections 9.3.1, 9.3.2); everything else is what makes the socket mount "+
				"equivalent to root on the host (#691)", c)
		}
	}
	if refusals != 0 {
		t.Errorf("docker_api_non_get_refusals rose by %d: the transport refused a call the plugin "+
			"tried to make. The refusal held, so nothing unsafe reached the daemon — but a call site "+
			"now exists that believes it may write, and it must be removed rather than left refused. "+
			"Recorded shapes: %v", refusals, calls)
	}
}
