// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

func writeMountinfo(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("writing mountinfo: %v", err)
	}
	return p
}

func TestSandboxNetnsPropagation_ReadsTheLinkFromMountinfo(t *testing.T) {
	dir := t.TempDir()
	netns := filepath.Join(dir, "docker", "netns")
	if err := os.MkdirAll(netns, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dirs := []string{netns}

	for _, tc := range []struct {
		name string
		body string
		want int32
	}{
		{
			name: "a slave mount receives the daemon's later mounts",
			body: "40 30 0:32 / " + dir + "/docker rw,relatime master:9 - tmpfs tmpfs rw\n",
			want: sandboxPropagationLinked,
		},
		{
			name: "a shared mount is in a peer group",
			body: "40 30 0:32 / " + dir + " rw,relatime shared:9 - tmpfs tmpfs rw\n",
			want: sandboxPropagationLinked,
		},
		{
			name: "a private mount receives nothing the daemon mounts later",
			body: "40 30 0:32 / " + dir + "/docker rw,relatime - tmpfs tmpfs rw\n",
			want: sandboxPropagationPrivate,
		},
		{
			name: "the deepest covering mount decides, not the first",
			body: "30 1 0:20 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
				"40 30 0:32 / " + dir + "/docker rw,relatime - tmpfs tmpfs rw\n",
			want: sandboxPropagationPrivate,
		},
		{
			name: "no mount covers the directory",
			body: "40 30 0:32 / /somewhere/else rw,relatime shared:9 - tmpfs tmpfs rw\n",
			want: sandboxPropagationUnknown,
		},
		{
			name: "a record with no separator is skipped, not guessed at",
			body: "40 30 0:32 / " + dir + "/docker rw,relatime master:9 tmpfs tmpfs rw\n",
			want: sandboxPropagationUnknown,
		},
		{
			name: "propagate_from is not a link on its own",
			body: "40 30 0:32 / " + dir + "/docker rw,relatime propagate_from:9 - tmpfs tmpfs rw\n",
			want: sandboxPropagationPrivate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sandboxNetnsPropagationIn(dirs, writeMountinfo(t, tc.body))
			if got != tc.want {
				t.Errorf("sandboxNetnsPropagationIn = %d, want %d.\nmountinfo:\n%s", got, tc.want, tc.body)
			}
		})
	}
}

func TestSandboxNetnsPropagation_UnreadableMountinfoIsNotPrivate(t *testing.T) {
	got := sandboxNetnsPropagationIn([]string{"/var/run/docker/netns"}, filepath.Join(t.TempDir(), "absent"))
	if got != sandboxPropagationUnknown {
		t.Errorf("an unreadable mountinfo read as %d, want %d (unknown)", got, sandboxPropagationUnknown)
	}
}

func TestSandboxNetnsPropagation_AnswersBeforeTheDirectoryExists(t *testing.T) {
	base := t.TempDir()
	absent := filepath.Join(base, "docker", "netns")
	body := "40 30 0:32 / " + base + " rw,relatime master:9 - tmpfs tmpfs rw\n"

	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatalf("the case needs %s to be absent, Stat said %v", absent, err)
	}
	if got := sandboxNetnsPropagationIn([]string{absent}, writeMountinfo(t, body)); got != sandboxPropagationLinked {
		t.Errorf("a directory the daemon has not created yet read as %d, want %d (linked).\n"+
			"Its parent is covered by a mount carrying master:9, and that mount is the one the "+
			"directory will be created in, so the propagation is known before the directory is",
			got, sandboxPropagationLinked)
	}

	privateBody := "40 30 0:32 / " + base + " rw,relatime - tmpfs tmpfs rw\n"
	if got := sandboxNetnsPropagationIn([]string{absent}, writeMountinfo(t, privateBody)); got != sandboxPropagationPrivate {
		t.Errorf("an absent directory under a PRIVATE parent read as %d, want %d", got, sandboxPropagationPrivate)
	}
}

func TestSandboxNetnsPropagation_DecodesEscapedMountPoints(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "doc ker", "netns")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "40 30 0:32 / " + filepath.Join(base, `doc\040ker`) + " rw,relatime master:9 - tmpfs tmpfs rw\n"
	if got := sandboxNetnsPropagationIn([]string{dir}, writeMountinfo(t, body)); got != sandboxPropagationLinked {
		t.Errorf("escaped mount point read as %d, want %d", got, sandboxPropagationLinked)
	}
}

func TestSandboxNetnsInitMounts_SaysWhichWorldPidOneIsIn(t *testing.T) {
	dirs := []string{"/run/docker/netns"}
	body := "40 30 0:32 / /run/docker/netns/abc rw,relatime - nsfs nsfs rw\n" +
		"41 30 0:32 / /run/docker/netns/def rw,relatime - nsfs nsfs rw\n" +
		"42 30 0:32 / /run/docker rw,relatime - tmpfs tmpfs rw\n"

	same := filepath.Join(t.TempDir(), "mnt")
	if err := os.Symlink("mnt:[4026531840]", same); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if got := sandboxNetnsInitMountsIn(dirs, same, same, writeMountinfo(t, body)); got != sandboxInitMountsSameNS {
		t.Errorf("PID 1 in this process's own mount namespace read as %d, want %d", got, sandboxInitMountsSameNS)
	}

	other := filepath.Join(t.TempDir(), "mnt")
	if err := os.Symlink("mnt:[4026532000]", other); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if got := sandboxNetnsInitMountsIn(dirs, same, other, writeMountinfo(t, body)); got != 2 {
		t.Errorf("sandbox netns mounts in PID 1's table read as %d, want 2", got)
	}

	if got := sandboxNetnsInitMountsIn(dirs, same, other, filepath.Join(t.TempDir(), "absent")); got != sandboxInitMountsUnknown {
		t.Errorf("an unreadable PID 1 mount table read as %d, want %d", got, sandboxInitMountsUnknown)
	}
}
