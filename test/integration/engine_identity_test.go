// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	docker "github.com/docker/docker/client"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
)

// TestEngineIdentity_HealthMatchesTheDaemonsOwnAnswer is the outside
// evidence for #670's published fields.
//
// THE PLUGIN'S OWN VALUE IS NOT EVIDENCE FOR THE PLUGIN'S OWN VALUE. A
// cell that read `engine_version` and checked it looked like a version
// would pass against a plugin that reported the version it was COMPILED
// against, or the floor constant, or the last value it saw on another
// host. The daemon is asked the same question here, through this test's
// own client, and the two answers have to be the same string.
func TestEngineIdentity_HealthMatchesTheDaemonsOwnAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer cli.Close()

	srv, err := cli.ServerVersion(ctx)
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	if srv.Version == "" {
		t.Fatal("the daemon reported an empty version; there is nothing to compare against")
	}

	h := harness.WaitPluginHealth(t, ctx, cli, 30*time.Second)

	if h.EngineVersion == nil {
		t.Fatal("the health document carries no engine_version field")
	}
	if *h.EngineVersion != srv.Version {
		t.Errorf("engine_version = %q, the daemon says %q", *h.EngineVersion, srv.Version)
	}

	if h.APIVersion == nil {
		t.Fatal("the health document carries no api_version field")
	}
	api := *h.APIVersion
	if api == "unknown" || api == "" {
		t.Fatalf("api_version = %q on a run where the daemon answered", api)
	}

	// The published API version is NEGOTIATED: the lower of what the
	// client library can speak and what this daemon can. So it must not
	// be above the daemon's own maximum, and a plugin publishing its
	// library's maximum instead of the negotiated value fails here on
	// any daemon older than the library.
	if apiAbove(api, srv.APIVersion) {
		t.Errorf("api_version = %q, above the daemon's maximum %q; that is not a negotiated version",
			api, srv.APIVersion)
	}

	// The same identity on the metrics surface, because that is where
	// an operator's dashboard reads it and the two renderings are
	// separate code.
	body, _, err := harness.PluginMetrics(ctx, cli)
	if err != nil {
		t.Fatalf("PluginMetrics: %v", err)
	}
	want := `net_dhcp_engine_info{engine_version="` + srv.Version + `",api_version="` + api + `"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("/metrics does not carry %s", want)
	}
}

// apiAbove reports whether a is a higher API version than b. Docker API
// versions are major.minor and are not decimals: 1.41 is above 1.9.
func apiAbove(a, b string) bool {
	aMaj, aMin, aOK := apiParts(a)
	bMaj, bMin, bOK := apiParts(b)
	if !aOK || !bOK {
		return false
	}
	if aMaj != bMaj {
		return aMaj > bMaj
	}
	return aMin > bMin
}

func apiParts(v string) (major, minor int, ok bool) {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}
