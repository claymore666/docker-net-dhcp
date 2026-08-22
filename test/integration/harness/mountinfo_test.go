// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import "testing"

// One table, deliberately holding every shape that has ever been a
// parsing hazard in a mount table. Captured from a real host and then
// extended with the rows a real host does not happen to have:
//
//	1  no optional fields at all
//	2  one optional field
//	3  TWO optional fields — the count is not a constant
//	4  an escaped mount point (\040)
//	5  /run/docker/netns/default — an nsfs row that is NOT a sandbox
//	6  a sandbox rendered under /run while dockerd calls it /var/run
//	7  a same-basename row in a directory that is not a netns dir
//
// Rows 5 and 7 are the ones that make the others mean anything: they
// are what "found an nsfs row" and "found that name" look like when
// the answer is still no.
const mountinfoFixture = `` +
	"25 30 0:23 / /sys rw,nosuid - sysfs sysfs rw\n" +
	"28 29 0:25 / /run rw,nosuid shared:5 - tmpfs tmpfs rw,size=3276800k\n" +
	"31 28 0:44 / /run/snapd/ns rw shared:9 master:3 - tmpfs tmpfs rw\n" +
	"33 28 0:45 / /run/odd\\040name rw - tmpfs tmpfs rw\n" +
	"1186 28 0:4 net:[4026531840] /run/docker/netns/default rw shared:754 - nsfs nsfs rw\n" +
	"1429 28 0:4 net:[4026534269] /run/docker/netns/bbf483df68ca rw shared:949 - nsfs nsfs rw\n" +
	"1500 28 0:4 net:[4026599999] /home/someone/bbf483df68ca rw - nsfs nsfs rw\n"

var netnsDirs = []string{"/var/run/docker/netns", "/run/docker/netns"}

func TestSandboxKeyMatcher_MatchesAcrossTheVarRunSpelling(t *testing.T) {
	// dockerd reports the key through /var/run; the kernel renders the
	// mount point resolved. Measured on a real host: these two strings
	// name one namespace and are not equal.
	const key = "/var/run/docker/netns/bbf483df68ca"

	got := MountRootFor([]byte(mountinfoFixture), SandboxKeyMatcher(key, netnsDirs))
	if want := "net:[4026534269]"; got != want {
		t.Errorf("MountRootFor(%q) = %q, want %q", key, got, want)
	}

	// The equality that was specified first, kept as a control: it
	// finds nothing, and "nothing" is what a genuinely absent namespace
	// also looks like. That is why the rule is the basename and not
	// this.
	if r := MountRootFor([]byte(mountinfoFixture), func(p string) bool { return p == key }); r != "" {
		t.Errorf("full-path equality found %q; the fixture is supposed to reproduce the spelling mismatch", r)
	}
}

func TestSandboxKeyMatcher_RejectsTheSameNameElsewhere(t *testing.T) {
	// Same bare name, wrong directory. Without the directory half, a
	// name match alone would authorize this row.
	const key = "/var/run/docker/netns/bbf483df68ca"
	m := SandboxKeyMatcher(key, netnsDirs)
	if m("/home/someone/bbf483df68ca") {
		t.Error("a row outside the known netns directories matched on its basename alone")
	}
	if !m("/run/docker/netns/bbf483df68ca") {
		t.Error("the real row stopped matching — the premise of the negative above is gone")
	}
}

func TestMountRootFor_DefaultNetnsIsNotASandbox(t *testing.T) {
	// "found an nsfs row" is not "found this sandbox". /run/docker/netns/
	// default is nsfs, is in a known directory, and belongs to no
	// container.
	const key = "/var/run/docker/netns/default"
	got := MountRootFor([]byte(mountinfoFixture), SandboxKeyMatcher(key, netnsDirs))
	if want := "net:[4026531840]"; got != want {
		t.Fatalf("MountRootFor(default) = %q, want %q", got, want)
	}
	// It resolves, and it resolves to something OTHER than the
	// container's namespace — which is the whole reason a caller may
	// not treat "a row was found" as the answer.
	if other := MountRootFor([]byte(mountinfoFixture),
		SandboxKeyMatcher("/var/run/docker/netns/bbf483df68ca", netnsDirs)); other == got {
		t.Errorf("default and a container's sandbox resolved to the same root %q", got)
	}
}

func TestUnescapeMountField(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`/run/odd\040name`, "/run/odd name"},
		{`/a\011b`, "/a\tb"},
		{`/a\012b`, "/a\nb"},
		{`/a\134b`, `/a\b`},
		{"/plain/path", "/plain/path"},
	} {
		if got := UnescapeMountField(tc.in); got != tc.want {
			t.Errorf("UnescapeMountField(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// And it reaches the scan, not just the helper: a caller matching
	// the real path must find the escaped row.
	if r := MountRootFor([]byte(mountinfoFixture), func(p string) bool { return p == "/run/odd name" }); r != "/" {
		t.Errorf("the escaped row was not found by its unescaped path; got root %q", r)
	}
}

// The separator is what makes the filesystem type readable at all. The
// fixture's optional-field counts are 0, 1 and 2, so a parser that
// counted to a fixed column would answer this wrongly for two thirds of
// the table.
func TestCountFSType_SurvivesTheVariableOptionalBlock(t *testing.T) {
	if got, want := CountFSType([]byte(mountinfoFixture), "nsfs"), 3; got != want {
		t.Errorf("CountFSType(nsfs) = %d, want %d", got, want)
	}
	if got, want := CountFSType([]byte(mountinfoFixture), "tmpfs"), 3; got != want {
		t.Errorf("CountFSType(tmpfs) = %d, want %d", got, want)
	}
	if got, want := CountFSType([]byte(mountinfoFixture), "sysfs"), 1; got != want {
		t.Errorf("CountFSType(sysfs) = %d, want %d", got, want)
	}
	// A type nothing carries, so a parser that returned a row count
	// regardless of the type it was asked for does not pass here.
	if got := CountFSType([]byte(mountinfoFixture), "ext4"); got != 0 {
		t.Errorf("CountFSType(ext4) = %d, want 0", got)
	}
}

func TestFSName(t *testing.T) {
	if got := FSName(NsfsMagic); got != "nsfs" {
		t.Errorf("FSName(nsfs magic) = %q", got)
	}
	// The placeholder dockerd creates before bind-mounting over it. A
	// stat of a sandbox key that comes back tmpfs is the file, not the
	// namespace.
	if got := FSName(TmpfsMagic); got != "tmpfs" {
		t.Errorf("FSName(tmpfs magic) = %q", got)
	}
	if got := FSName(0xdeadbeef); got != "other" {
		t.Errorf("FSName(unknown) = %q", got)
	}
}

// The watcher that answers "when does the mapping become observable"
// compares whole sets between polls, so it needs every sandbox row and
// not the first one. A first-match helper cannot express "one more row
// than last time".
func TestMountPointsUnder_ReturnsEveryRowAndItsRoot(t *testing.T) {
	got := MountPointsUnder([]byte(mountinfoFixture), netnsDirs)

	want := map[string]string{
		"/run/docker/netns/default":      "net:[4026531840]",
		"/run/docker/netns/bbf483df68ca": "net:[4026534269]",
	}
	if len(got) != len(want) {
		t.Fatalf("MountPointsUnder returned %d rows (%v), want %d", len(got), got, len(want))
	}
	for mp, root := range want {
		if got[mp] != root {
			t.Errorf("MountPointsUnder[%q] = %q, want %q", mp, got[mp], root)
		}
	}

	// The same-basename row outside the netns directories must not be
	// in the set: a watcher that picked it up would report a new
	// sandbox every time anything appeared anywhere.
	if _, ok := got["/home/someone/bbf483df68ca"]; ok {
		t.Error("a row outside the known netns directories was counted as a sandbox")
	}
	// And neither must the /run tmpfs itself, whose parent is /.
	if _, ok := got["/run"]; ok {
		t.Error("/run was counted as a sandbox")
	}
}
