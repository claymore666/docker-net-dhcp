// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// TestMetrics_SocketServesTheFullSurface is the outside-evidence half of
// #651.
//
// The unit suite proves the renderer is correct and that its table
// covers every HealthResponse field. Neither of those touches the
// question this test asks: does the plugin we actually BUILT AND
// INSTALLED serve /metrics on the socket an operator will scrape?
// Routing, the shipped manifest and the real binary are all outside what
// a unit test can see — and a route registered in code but lost in the
// build is precisely the class of failure this project keeps finding.
//
// The oracle is the harness's own HealthResponse. Driving the assertion
// from the struct rather than a literal list means a counter added later
// is checked here automatically, across the process boundary, against
// whatever the running plugin really exposes.
func TestMetrics_SocketServesTheFullSurface(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	// Readiness only. This makes no claim about counters, which is why
	// it is the right call here and a bare health read would not be
	// (#405) — all we need from it is the identity of the process that
	// is about to answer the scrape.
	health := harness.WaitPluginHealth(t, ctx, cli, 30*time.Second)

	body, contentType, err := harness.PluginMetrics(ctx, cli)
	if err != nil {
		t.Fatalf("GET /metrics on the plugin socket: %v", err)
	}
	if !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("Content-Type = %q, want the text exposition format", contentType)
	}

	// Same process, same snapshot source. If build_info carried a
	// different id than /Plugin.Health reports, the two views would be
	// reading different state and every cross-reference an operator
	// makes between them would be wrong.
	if health.InstanceID == "" {
		t.Fatal("/Plugin.Health reported no instance_id; the identity cross-check below would pass vacuously")
	}
	wantInfo := `net_dhcp_build_info{instance_id="` + health.InstanceID + `"} 1`
	if !strings.Contains(body, wantInfo) {
		t.Errorf("/metrics does not identify the process /Plugin.Health just answered from.\nwant line: %s\ngot:\n%s",
			wantInfo, firstLines(body, 8))
	}

	// Every field of the health surface must be reachable from the
	// exposition.
	series := seriesNames(body)
	if len(series) == 0 {
		t.Fatal("no series parsed out of the exposition; the check below would pass vacuously")
	}

	typ := reflect.TypeOf(harness.HealthResponse{})
	var missing []string
	for i := 0; i < typ.NumField(); i++ {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		// instance_id rides as a build_info label, already asserted above.
		if tag == "instance_id" {
			continue
		}
		// The _v6 counters are exposed as family="ipv6" on the base
		// name rather than as series of their own; the base name is
		// checked on its own iteration.
		if base, ok := strings.CutSuffix(tag, "_v6"); ok {
			if !series["net_dhcp_"+base+"_total"] {
				missing = append(missing, tag+" (via "+base+")")
			}
			continue
		}
		if series["net_dhcp_"+tag] || series["net_dhcp_"+tag+"_total"] {
			continue
		}
		missing = append(missing, tag)
	}
	if len(missing) > 0 {
		t.Errorf("the running plugin's /metrics does not expose %d health field(s): %s\n"+
			"An operator alerting on these would get no series and no error — silence that looks like zero.",
			len(missing), strings.Join(missing, ", "))
	}
}

// seriesNames pulls the metric names out of an exposition body, ignoring
// HELP/TYPE comments and any labels.
func seriesNames(body string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		out[name] = true
	}
	return out
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
