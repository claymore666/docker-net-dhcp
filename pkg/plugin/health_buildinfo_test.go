// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"strings"
	"testing"

	"github.com/claymore666/docker-net-dhcp/pkg/buildinfo"
)

// withBuildInfo injects the three values for one test and restores
// them. Restoring matters: they are package variables, and a test that
// left them set would make every later test in this binary assert
// against its fixture rather than against the shipped defaults.
func withBuildInfo(t *testing.T, version, commit, library string) {
	t.Helper()
	v, c, l := buildinfo.Version, buildinfo.Commit, buildinfo.Library
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit, buildinfo.Library = v, c, l })
	buildinfo.Version, buildinfo.Commit, buildinfo.Library = version, commit, library
}

// Direction one: the values the build passed reach both renderings.
// Three distinct values, so a document that rendered one of them three
// times, or read the wrong variable, differs here.
func TestBuildInfo_InjectedValuesReachTheDocumentAndTheMetric(t *testing.T) {
	withBuildInfo(t, "v2.0.0-alpha.1", "0123456789abcdef0123456789abcdef01234567", "fedcba9876543210fedcba9876543210fedcba98")

	p := newHealthPlugin()
	p.instanceID = "inst"
	h := p.healthSnapshot()

	if h.Version != "v2.0.0-alpha.1" {
		t.Errorf("document version = %q", h.Version)
	}
	if h.Commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("document commit = %q", h.Commit)
	}
	if h.Library != "fedcba9876543210fedcba9876543210fedcba98" {
		t.Errorf("document library = %q", h.Library)
	}

	var buf bytes.Buffer
	if err := writeExposition(&buf, h); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	want := `net_dhcp_build_info{instance_id="inst",version="v2.0.0-alpha.1",` +
		`commit="0123456789abcdef0123456789abcdef01234567",` +
		`library="fedcba9876543210fedcba9876543210fedcba98"} 1`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("missing\n\t%s\nin:\n%s", want, buf.String())
	}
	if strings.Contains(buf.String(), `family=`) && strings.Contains(buf.String(), `net_dhcp_build_info{instance_id="inst",family=`) {
		t.Error("build_info carries a family label; it describes the binary, not a lease")
	}
}

// Direction two, and the one that matters: a build that was passed
// NOTHING says so. An unset -X leaves the variable empty, and
// version="" renders as a label that is present and says nothing --
// which is the failure that looks like success. The defaults are words.
func TestBuildInfo_UninjectedValuesAreWordsNotEmptyLabels(t *testing.T) {
	p := newHealthPlugin()
	p.instanceID = "inst"
	h := p.healthSnapshot()

	for _, c := range []struct{ name, got, want string }{
		{"version", h.Version, "dev"},
		{"commit", h.Commit, "unknown"},
		{"library", h.Library, "unknown"},
	} {
		if c.got != c.want {
			t.Errorf("a binary built without -ldflags reports %s=%q; want %q. "+
				"This test binary is that build.", c.name, c.got, c.want)
		}
	}

	var buf bytes.Buffer
	if err := writeExposition(&buf, h); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	for _, label := range []string{"version", "commit", "library"} {
		if strings.Contains(buf.String(), label+`=""`) {
			t.Errorf("exposition carries %s=\"\"; an empty label reads as "+
				"\"nothing is wrong\" rather than \"this build does not know\"", label)
		}
	}
}

// The label set of build_info is a promise: SECURITY.md says the
// exposition carries no per-endpoint identifier, and these five are the
// whole allow-list. A sixth label added here without that decision
// being made is what this catches.
func TestBuildInfo_CarriesExactlyTheFourLabels(t *testing.T) {
	p := newHealthPlugin()
	p.instanceID = "inst"
	var buf bytes.Buffer
	if err := writeExposition(&buf, p.healthSnapshot()); err != nil {
		t.Fatalf("writeExposition: %v", err)
	}
	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(l, "net_dhcp_build_info{") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("no net_dhcp_build_info series in the exposition")
	}
	labels := line[strings.Index(line, "{")+1 : strings.LastIndex(line, "}")]
	var names []string
	for _, kv := range strings.Split(labels, `",`) {
		names = append(names, strings.SplitN(kv, "=", 2)[0])
	}
	want := []string{"instance_id", "version", "commit", "library"}
	if len(names) != len(want) {
		t.Fatalf("build_info labels = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("build_info label %d = %q, want %q", i, names[i], want[i])
		}
	}
}
