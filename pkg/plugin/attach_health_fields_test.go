// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// writeNetnsFixtures builds a directory that the three sandbox-netns
// readings answer with three DIFFERENT numbers, and returns the sources
// that point at it.
//
// Different numbers is the whole point. The readings are computed
// inside healthSnapshot rather than loaded from a counter, so two of
// them wired to each other's field is invisible to any drive where they
// happen to agree — and on a developer box they agree at -1.
//
// dir is EvalSymlinks'd so the mountinfo fixtures can use one spelling:
// the propagation reading resolves the directory before matching, the
// init-mounts reading does not.
func writeNetnsFixtures(t *testing.T) netnsSources {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	netns := filepath.Join(dir, "netns")
	if err := os.Mkdir(netns, 0o755); err != nil {
		t.Fatalf("creating the netns dir: %v", err)
	}
	// Two entries, so sandbox_netns_visible is 2.
	for _, name := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"} {
		if err := os.WriteFile(filepath.Join(netns, name), nil, 0o644); err != nil {
			t.Fatalf("creating a sandbox entry: %v", err)
		}
	}

	// A mount covering the directory and tagged master:9, so
	// sandbox_netns_propagation is sandboxPropagationLinked, which is 1.
	self := filepath.Join(dir, "self-mountinfo")
	if err := os.WriteFile(self, []byte(
		"22 21 0:20 / / rw,relatime - ext4 /dev/sda1 rw\n"+
			"31 22 0:27 / "+netns+" rw,relatime master:9 - tmpfs tmpfs rw\n"), 0o644); err != nil {
		t.Fatalf("writing the self mountinfo: %v", err)
	}

	// Three nsfs mounts under the directory, so
	// sandbox_netns_init_mounts is 3.
	init := filepath.Join(dir, "init-mountinfo")
	if err := os.WriteFile(init, []byte(
		"22 21 0:20 / / rw,relatime - ext4 /dev/sda1 rw\n"+
			"40 31 0:28 / "+netns+"/aaaaaaaaaaaa rw - nsfs nsfs rw\n"+
			"41 31 0:29 / "+netns+"/bbbbbbbbbbbb rw - nsfs nsfs rw\n"+
			"42 31 0:30 / "+netns+"/cccccccccccc rw - nsfs nsfs rw\n"), 0o644); err != nil {
		t.Fatalf("writing the init mountinfo: %v", err)
	}

	// Two different namespace links, so the reading does not stop at
	// sandboxInitMountsSameNS. Dangling on purpose: Readlink reports the
	// target without resolving it, which is what the reading compares.
	selfNS := filepath.Join(dir, "self-ns-mnt")
	if err := os.Symlink("mnt:[4026531840]", selfNS); err != nil {
		t.Fatalf("linking the self mount ns: %v", err)
	}
	initNS := filepath.Join(dir, "init-ns-mnt")
	if err := os.Symlink("mnt:[4026532711]", initNS); err != nil {
		t.Fatalf("linking the init mount ns: %v", err)
	}

	return netnsSources{
		dirs:          []string{netns},
		mountinfo:     self,
		selfNS:        selfNS,
		initNS:        initNS,
		initMountinfo: init,
	}
}

// TestHealthSnapshot_CarriesTheAttachFields is the wiring assertion for
// #403's answer on a host running the shipped log level, and for the
// three sandbox-netns readings beside it.
//
// The counters and the readings were already right when review round 1
// found that nothing observed the hop from them to the published
// document. A field carrying its neighbour's number is not a crash and
// not a red suite: it is a plausible number in the wrong row, which an
// operator reads as a measurement. The lane cannot catch it either --
// its only reader of the four bucket fields is a t.Logf -- and neither
// can the exposition golden, because fixtureSnapshot fills a
// HealthResponse by reflection and never calls healthSnapshot, so it
// proves field-to-series and not counter-to-field.
//
// Every value here is distinct from every other, so any pair of fields
// wired to one source, or to each other, fails.
func TestHealthSnapshot_CarriesTheAttachFields(t *testing.T) {
	p := &Plugin{netnsSrc: writeNetnsFixtures(t)}
	p.joinAttachSlow.Store(11)
	p.joinAttachCompleted.Store(13)
	p.joinAttachUnder1s.Store(17)
	p.joinAttach1sToBudget.Store(19)
	p.joinAttachMsMax.Store(23)

	h := p.healthSnapshot()
	for _, tc := range []struct {
		name string
		got  int32
		want int32
	}{
		{"join_attach_slow", h.JoinAttachSlow, 11},
		{"join_attach_completed", h.JoinAttachCompleted, 13},
		{"join_attach_under_1s", h.JoinAttachUnder1s, 17},
		{"join_attach_1s_to_budget", h.JoinAttach1sToBudget, 19},
		{"join_attach_ms_max", h.JoinAttachMsMax, 23},
		{"sandbox_netns_visible", h.SandboxNetnsVisible, 2},
		{"sandbox_netns_propagation", h.SandboxNetnsPropagation, sandboxPropagationLinked},
		{"sandbox_netns_init_mounts", h.SandboxNetnsInitMounts, 3},
	} {
		if tc.got != tc.want {
			t.Errorf("/Plugin.Health carries %s = %d, want %d. A field carrying its "+
				"neighbour's number is read as a measurement: this is the only observer of the "+
				"hop from the counter to the published document.", tc.name, tc.got, tc.want)
		}
	}
}

// TestNetnsReadingSources_DefaultsToTheProductionPaths keeps the seam
// from becoming the defect it was built to expose. The test above can
// only point the readings somewhere else because healthSnapshot takes
// their paths from a struct field; a plugin that never sets it has to
// read the host.
func TestNetnsReadingSources_DefaultsToTheProductionPaths(t *testing.T) {
	s := (&Plugin{}).netnsReadingSources()
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"mountinfo", s.mountinfo, selfMountinfo},
		{"selfNS", s.selfNS, selfMountNS},
		{"initNS", s.initNS, initMountNS},
		{"initMountinfo", s.initMountinfo, initMountinfo},
	} {
		if tc.got != tc.want {
			t.Errorf("a plugin with no fixtures reads %s from %q, want the production path %q",
				tc.name, tc.got, tc.want)
		}
	}
	if len(s.dirs) != len(sandboxNetnsDirs) {
		t.Fatalf("a plugin with no fixtures reads %d directories, want the %d production ones",
			len(s.dirs), len(sandboxNetnsDirs))
	}
	for i, dir := range sandboxNetnsDirs {
		if s.dirs[i] != dir {
			t.Errorf("directory %d is %q, want the production %q", i, s.dirs[i], dir)
		}
	}
}
