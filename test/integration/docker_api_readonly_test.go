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

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// The plugin runs logrus' TextFormatter, which quotes a value only when it needs quoting, so both spellings match; RE2
// has no backreferences, so the fields are matched separately (#691).
var (
	dockerAPICallLine   = regexp.MustCompile(`msg="docker-api call"`)
	dockerAPICallMethod = regexp.MustCompile(`\bmethod="?([^"\s]*)"?`)
	dockerAPICallPath   = regexp.MustCompile(`\bpath="?([^"\s]*)"?`)
)

// parseDockerAPICall returns the method and path of one recorded line, failing the test on a line with the message but not both fields.
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

// The unit suite drives the transport directly; only a live plugin shows the transport is in its client's path. The
// plugin logs each method and path once, so the log holds the set of shapes, printed on every run. The domain comes
// first: at least one call, a GET, a network read and a ContainerInspect, which carries the sandbox key and hostname (#691).

// TestDockerAPI_OnlySafeMethodsReachTheDaemon checks that the running plugin sends only GET and HEAD to the daemon (#691).
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

	// The domain checks ask what this test drove, and over the whole log an earlier test would answer them.
	logMark := harness.MarkPluginLog(t, ctx)

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
	// The ContainerInspect is logged as Join returns, so one read after the attach can miss it.
	harness.AwaitPluginLogSince(t, ctx, logMark, 10*time.Second, func(window string) bool {
		observed = map[string]bool{}
		methods = map[string]int{}
		sawContainerInspect = false
		sawNetworkRead = false
		for _, line := range strings.Split(window, "\n") {
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
		return sawContainerInspect && sawNetworkRead
	})

	calls := make([]string, 0, len(observed))
	for c := range observed {
		calls = append(calls, c)
	}
	sort.Strings(calls)
	t.Logf("DOCKER-API SURFACE, as the plugin's own transport recorded it against a live daemon:\n  %s",
		strings.Join(calls, "\n  "))

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

	// The claim is over the whole log: it covers every call the plugin has sent, not only this test's.
	all := map[string]bool{}
	for _, line := range strings.Split(harness.ReadWholePluginLog(t, ctx), "\n") {
		method, path, isCall := parseDockerAPICall(t, line)
		if !isCall {
			continue
		}
		all[method+" "+path] = true
	}
	everyCall := make([]string, 0, len(all))
	for c := range all {
		everyCall = append(everyCall, c)
	}
	sort.Strings(everyCall)
	for _, c := range everyCall {
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
