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

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
)

// TestEngineIdentity_HealthMatchesTheDaemonsOwnAnswer checks the #670 engine fields against the daemon's own answer to this client.
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

	// The published API version is negotiated, the lower of the library's and the daemon's, so it may not exceed the daemon's maximum (#670).
	if apiAbove(api, srv.APIVersion) {
		t.Errorf("api_version = %q, above the daemon's maximum %q; that is not a negotiated version",
			api, srv.APIVersion)
	}

	// The metrics surface renders the identity in separate code (#670).
	body, _, err := harness.PluginMetrics(ctx, cli)
	if err != nil {
		t.Fatalf("PluginMetrics: %v", err)
	}
	want := `net_dhcp_engine_info{engine_version="` + srv.Version + `",api_version="` + api + `"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("/metrics does not carry %s", want)
	}
}

// apiAbove reports whether a is a higher Docker API version than b, compared as major.minor: 1.41 is above 1.9.
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
