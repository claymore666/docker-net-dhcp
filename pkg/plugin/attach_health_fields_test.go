// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func writeNetnsFixtures(t *testing.T, linked bool) netnsSources {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving the temp dir: %v", err)
	}
	netns := filepath.Join(dir, "netns")
	if err := os.Mkdir(netns, 0o755); err != nil {
		t.Fatalf("creating the netns dir: %v", err)
	}
	for _, name := range []string{"aaaaaaaaaaaa", "bbbbbbbbbbbb"} {
		if err := os.WriteFile(filepath.Join(netns, name), nil, 0o644); err != nil {
			t.Fatalf("creating a sandbox entry: %v", err)
		}
	}

	optional := ""
	if linked {
		optional = "master:9 "
	}
	self := filepath.Join(dir, "self-mountinfo")
	if err := os.WriteFile(self, []byte(
		"22 21 0:20 / / rw,relatime - ext4 /dev/sda1 rw\n"+
			"31 22 0:27 / "+netns+" rw,relatime "+optional+"- tmpfs tmpfs rw\n"), 0o644); err != nil {
		t.Fatalf("writing the self mountinfo: %v", err)
	}

	init := filepath.Join(dir, "init-mountinfo")
	if err := os.WriteFile(init, []byte(
		"22 21 0:20 / / rw,relatime - ext4 /dev/sda1 rw\n"+
			"40 31 0:28 / "+netns+"/aaaaaaaaaaaa rw - nsfs nsfs rw\n"+
			"41 31 0:29 / "+netns+"/bbbbbbbbbbbb rw - nsfs nsfs rw\n"+
			"42 31 0:30 / "+netns+"/cccccccccccc rw - nsfs nsfs rw\n"), 0o644); err != nil {
		t.Fatalf("writing the init mountinfo: %v", err)
	}

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

func TestHealthSnapshot_CarriesTheAttachFields(t *testing.T) {
	for _, source := range []struct {
		name        string
		linked      bool
		propagation int32
	}{
		{"linked source mount", true, sandboxPropagationLinked},
		{"private source mount", false, sandboxPropagationPrivate},
	} {
		t.Run(source.name, func(t *testing.T) {
			p := &Plugin{netnsSrc: writeNetnsFixtures(t, source.linked)}
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
				{"sandbox_netns_propagation", h.SandboxNetnsPropagation, source.propagation},
				{"sandbox_netns_init_mounts", h.SandboxNetnsInitMounts, 3},
			} {
				if tc.got != tc.want {
					t.Errorf("/Plugin.Health carries %s = %d, want %d. A field carrying its "+
						"neighbour's number is read as a measurement: this is the only observer "+
						"of the hop from the counter to the published document.",
						tc.name, tc.got, tc.want)
				}
			}
		})
	}
}

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
