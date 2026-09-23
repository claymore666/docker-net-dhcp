// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package integration

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/v2/test/integration/harness"
	docker "github.com/docker/docker/client"
)

// The unit suite covers the renderer; this asks whether the built and installed plugin serves /metrics on its socket.
// The oracle is harness.HealthResponse, so a counter added later is checked here too (#651).

// TestMetrics_SocketServesTheFullSurface checks that the running plugin's /metrics exposes every health field (#651).
func TestMetrics_SocketServesTheFullSurface(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cli, err := docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	// Readiness only, no counter claim, so no counter window is needed (#405).
	health := harness.WaitPluginHealth(t, ctx, cli, 30*time.Second)

	body, contentType, err := harness.PluginMetrics(ctx, cli)
	if err != nil {
		t.Fatalf("GET /metrics on the plugin socket: %v", err)
	}
	if !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("Content-Type = %q, want the text exposition format", contentType)
	}

	// build_info must carry /Plugin.Health's instance id and build identity, label by label: label order is no promise,
	// and an empty value such as commit="" scrapes like a healthy build (#651).
	if health.InstanceID == "" {
		t.Fatal("/Plugin.Health reported no instance_id; the identity cross-check below would pass vacuously")
	}
	// Every identity series in the mirror: #670 added net_dhcp_engine_info.
	lines := map[string]string{}
	for _, family := range slices.Sorted(maps.Values(harness.HealthFieldsAsLabels)) {
		if _, seen := lines[family]; seen {
			continue
		}
		line, ok := identityLine(body, family)
		if !ok {
			t.Fatalf("/metrics carries no net_dhcp_%s series:\n%s", family, firstLines(body, 8))
		}
		if !strings.HasSuffix(line, "} 1") {
			t.Errorf("%s is %q; an identity series is always 1", family, line)
		}
		lines[family] = line
	}
	for _, tag := range slices.Sorted(maps.Keys(harness.HealthFieldsAsLabels)) {
		family := harness.HealthFieldsAsLabels[tag]
		info := lines[family]
		got, present := labelValue(info, tag)
		want, known := healthFieldString(health, tag)
		switch {
		case !present:
			t.Errorf("%s carries no %s label; the document reports %q for it.\nline: %s", family, tag, want, info)
		case got == "":
			t.Errorf("%s carries %s=\"\"; an empty label scrapes and alerts exactly like a "+
				"populated one, so this is the identity failure that looks like nothing.\nline: %s", family, tag, info)
		case !known:
			t.Errorf("/Plugin.Health published no %s, so the label above is unverifiable", tag)
		case got != want:
			t.Errorf("%s says %s=%q, /Plugin.Health says %q; the two views are reading different state", family, tag, got, want)
		}
	}

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
		if _, isLabel := harness.HealthFieldsAsLabels[tag]; isLabel {
			continue
		}
		// An exemption must carry a reason; an empty one is a silent exemption.
		if why, off := harness.HealthFieldsNotExposed[tag]; off {
			if strings.TrimSpace(why) == "" {
				t.Errorf("%s is exempted from /metrics with no reason given", tag)
			}
			continue
		}
		if name, renamed := harness.HealthFieldSeries[tag]; renamed {
			if !series["net_dhcp_"+name] {
				missing = append(missing, tag+" (as "+name+")")
			}
			continue
		}
		// A family half rides the base series with a family label; both halves are matched, or every v4 half would read as unexposed.
		if base, ok := familyHalfBase(tag); ok {
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

// familyHalfBase returns the base counter of a per-family half, which is exposed as the base series with a family label.
func familyHalfBase(tag string) (string, bool) {
	for _, suffix := range []string{"_v4", "_v6"} {
		if base, ok := strings.CutSuffix(tag, suffix); ok {
			return base, true
		}
	}
	return "", false
}

// seriesNames returns the metric names in an exposition body, without HELP and TYPE lines or labels.
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

// identityLine returns the net_dhcp_<family> sample line, family being build_info or engine_info.
func identityLine(body, family string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "net_dhcp_"+family+"{") {
			return line, true
		}
	}
	return "", false
}

// labelValue returns one label of a sample line; a value that needed unescaping would itself be a finding.
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

// healthFieldString returns a string field of h by json tag, with false when the plugin published none.
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
