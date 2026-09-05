// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"reflect"
	"slices"
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
	// makes between them would be wrong. Since O-4 the same line also
	// carries the build identity, and it is checked label by label
	// against the document rather than as one literal: the label ORDER
	// is not a promise to anyone, and an empty value is the failure
	// that looks like nothing -- `commit=""` renders, scrapes and
	// alerts exactly like a healthy build.
	if health.InstanceID == "" {
		t.Fatal("/Plugin.Health reported no instance_id; the identity cross-check below would pass vacuously")
	}
	info, ok := buildInfoLine(body)
	if !ok {
		t.Fatalf("/metrics carries no net_dhcp_build_info series:\n%s", firstLines(body, 8))
	}
	if !strings.HasSuffix(info, "} 1") {
		t.Errorf("build_info is %q; the identity series is always 1", info)
	}
	for _, tag := range harness.HealthFieldsInBuildInfo {
		got, present := labelValue(info, tag)
		want, known := healthFieldString(health, tag)
		switch {
		case !present:
			t.Errorf("build_info carries no %s label; the document reports %q for it.\nline: %s", tag, want, info)
		case got == "":
			t.Errorf("build_info carries %s=\"\"; an empty label scrapes and alerts exactly like a "+
				"populated one, so this is the build-identity failure that looks like nothing.\nline: %s", tag, info)
		case !known:
			t.Errorf("/Plugin.Health published no %s, so the label above is unverifiable", tag)
		case got != want:
			t.Errorf("build_info says %s=%q, /Plugin.Health says %q; the two views are reading different state", tag, got, want)
		}
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
		// The build-identity fields ride as build_info labels and were
		// asserted, value and all, above.
		if slices.Contains(harness.HealthFieldsInBuildInfo, tag) {
			continue
		}
		// Deliberately not exposed, with the reason declared beside the
		// mirror struct. Checked for a REASON, not just for membership:
		// an entry added with an empty one is a silent exemption.
		if why, off := harness.HealthFieldsNotExposed[tag]; off {
			if strings.TrimSpace(why) == "" {
				t.Errorf("%s is exempted from /metrics with no reason given", tag)
			}
			continue
		}
		// A field whose series is not named after it.
		if name, renamed := harness.HealthFieldSeries[tag]; renamed {
			if !series["net_dhcp_"+name] {
				missing = append(missing, tag+" (as "+name+")")
			}
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

// buildInfoLine is the exposition's net_dhcp_build_info sample, without
// its HELP and TYPE comments.
func buildInfoLine(body string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "net_dhcp_build_info{") {
			return line, true
		}
	}
	return "", false
}

// labelValue reads one label out of a sample line. Deliberately not a
// full exposition parser: the values here are ids and revisions, and a
// label value that needed unescaping would itself be the finding.
func labelValue(line, name string) (string, bool) {
	i := strings.Index(line, name+`="`)
	if i < 0 {
		return "", false
	}
	rest := line[i+len(name)+2:]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// healthFieldString reads one string-valued field of the document by
// its json tag, so the labels above are compared against the SAME
// response the scrape is being cross-checked with rather than against a
// second read. The bool is false when the plugin published no value:
// the pointer fields distinguish "" from absent, and a cell that folded
// them together would pass against a plugin that publishes neither.
func healthFieldString(h *harness.HealthResponse, tag string) (string, bool) {
	v := reflect.ValueOf(*h)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if strings.Split(t.Field(i).Tag.Get("json"), ",")[0] != tag {
			continue
		}
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			return f.String(), true
		case reflect.Pointer:
			if f.IsNil() {
				return "", false
			}
			return f.Elem().String(), true
		default:
			return "", false
		}
	}
	return "", false
}
