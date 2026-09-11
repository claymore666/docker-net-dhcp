// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// writeMountinfo puts a mountinfo table in a temp file and returns its
// path. The records are real /proc/self/mountinfo shapes: the optional
// field count varies per record, which is the part a reader gets wrong.
func writeMountinfo(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("writing mountinfo: %v", err)
	}
	return p
}

// TestSandboxNetnsPropagation_ReadsTheLinkFromMountinfo drives every
// reading of the gauge that carries SECURITY.md's causal sentence.
//
// The sentence says the plugin's /var/run/docker bind is a snapshot.
// MEASURED in a user and mount namespace built the way runc builds a
// plugin's: a bind of a SHARED source receives the daemon's later
// per-sandbox mounts with no mount option at all, and a bind of a
// PRIVATE source receives none with rslave or rshared either. The
// snapshot is therefore a property of the source mount, so the gauge
// reads which of the two this host is.
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

// TestSandboxNetnsPropagation_UnreadableMountinfoIsNotPrivate is the
// direction check. Zero is the reading an operator acts on: it says
// every attach is carried by the container PID and the netns half of
// the pidhost grant is load-bearing. A file that could not be read must
// not produce it, or the absence of a measurement is served as a
// measurement.
func TestSandboxNetnsPropagation_UnreadableMountinfoIsNotPrivate(t *testing.T) {
	got := sandboxNetnsPropagationIn([]string{"/var/run/docker/netns"}, filepath.Join(t.TempDir(), "absent"))
	if got != sandboxPropagationUnknown {
		t.Errorf("an unreadable mountinfo read as %d, want %d (unknown)", got, sandboxPropagationUnknown)
	}
}

// TestSandboxNetnsPropagation_AnswersBeforeTheDirectoryExists is the
// reading an operator asks for FIRST and the one the gauge would have
// been unable to give.
//
// /var/run/docker/netns does not exist until the daemon's first
// sandbox, so on a host where the plugin was enabled before anything
// ran on the network the whole question is "will the route work when a
// container arrives". Giving up there returned the same value as an
// unreadable mountinfo, which is a measurement's absence served as a
// measurement (#417).
//
// It is answerable because nothing mounts that directory: the daemon
// mkdirs it inside the mount that already covers its parent, and a
// directory created inside a mount is in that mount's peer group. So
// the parent's propagation IS the answer, before and after.
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

	// The other direction, or the reading above is produced by
	// answering "linked" whenever the path is missing.
	privateBody := "40 30 0:32 / " + base + " rw,relatime - tmpfs tmpfs rw\n"
	if got := sandboxNetnsPropagationIn([]string{absent}, writeMountinfo(t, privateBody)); got != sandboxPropagationPrivate {
		t.Errorf("an absent directory under a PRIVATE parent read as %d, want %d", got, sandboxPropagationPrivate)
	}
}

// TestSandboxNetnsPropagation_DecodesEscapedMountPoints keeps the
// comparison honest for a mount point mountinfo had to escape. A path
// with a space in it is carried as \040 and compares equal to nothing,
// so the covering mount is missed and a linked host reports unknown.
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

// TestSandboxNetnsInitMounts_SaysWhichWorldPidOneIsIn drives the gauge
// that separates the two environments this change is judged in. Under a
// nested engine PID 1 is that engine's init and not the outer host's,
// so a route through /proc/1/root can work on one and refuse on the
// other, and a lane result read without this number is read against the
// wrong world.
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
	// Two entries UNDER the directory; the directory's own mount is not
	// one of them, which is the off-by-one a prefix match invites.
	if got := sandboxNetnsInitMountsIn(dirs, same, other, writeMountinfo(t, body)); got != 2 {
		t.Errorf("sandbox netns mounts in PID 1's table read as %d, want 2", got)
	}

	if got := sandboxNetnsInitMountsIn(dirs, same, other, filepath.Join(t.TempDir(), "absent")); got != sandboxInitMountsUnknown {
		t.Errorf("an unreadable PID 1 mount table read as %d, want %d", got, sandboxInitMountsUnknown)
	}
}
