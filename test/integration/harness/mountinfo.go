// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// This file deliberately carries NO `//go:build integration` tag,
// unlike most of the package, for healthfloor.go's reason: the parsing
// below is where every risk in reading a mount table lives, and it has
// to be testable without a live plugin. A parser that has only ever
// been run against one host's /proc/1/mountinfo is not known to work —
// it is known to agree with that host.
//
// The rows this must survive are in mountinfo_test.go: a row with no
// optional fields, one with two, an escaped mount point, and an nsfs
// row that is not any container's sandbox.

package harness

import (
	"bufio"
	"bytes"
	"path/filepath"
	"strings"
)

// Filesystem magic numbers, for telling a real namespace file from the
// placeholder dockerd creates before bind-mounting over it.
const (
	NsfsMagic  = 0x6e736673
	TmpfsMagic = 0x01021994
)

// FSName renders a statfs type for a log line.
func FSName(t uint64) string {
	switch t {
	case NsfsMagic:
		return "nsfs"
	case TmpfsMagic:
		return "tmpfs"
	}
	return "other"
}

// MountRootFor scans a mountinfo blob for the first row whose mount
// POINT satisfies match, and returns that row's mount ROOT — field 4,
// which for an nsfs mount renders as "net:[N]" and is the netns
// identity. It returns "" when no row matches.
//
// # Why it splits on " - " and never counts to the separator
//
// The optional-fields block that precedes the separator is VARIABLE in
// count: rows carry "shared:784", or two entries, or none at all, in
// the same table on the same host. Fields 1-6 are fixed, so reading the
// root and the mount point by index is safe HERE; what is not safe is
// assuming where the block ends. Anything reading past it — CountFSType
// below does — must find the separator rather than count to it, and
// silently, because every column in that region is a plausible-looking
// string. The two are in one file so the rule is stated once.
//
// # Why the mount point is unescaped
//
// The kernel escapes field 5: \040 for space, \011 tab, \012 newline,
// \134 backslash. Container IDs are hex, so a parser that forgot this
// would never be caught by a real sandbox key — which is precisely why
// it needs a fixture rather than a production run to catch it.
func MountRootFor(raw []byte, match func(mountPoint string) bool) string {
	for _, line := range mountinfoLines(raw) {
		i := strings.Index(line, " - ")
		if i < 0 {
			continue
		}
		f := strings.Fields(line[:i])
		if len(f) < 5 {
			continue
		}
		if match(UnescapeMountField(f[4])) {
			return f[3]
		}
	}
	return ""
}

// UnescapeMountField reverses the kernel's escaping of a mountinfo
// path field.
func UnescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

// CountFSType counts rows whose filesystem type — the first token
// AFTER the " - " separator — is fsType.
func CountFSType(raw []byte, fsType string) int {
	n := 0
	for _, line := range mountinfoLines(raw) {
		i := strings.Index(line, " - ")
		if i < 0 {
			continue
		}
		post := strings.Fields(line[i+3:])
		if len(post) > 0 && post[0] == fsType {
			n++
		}
	}
	return n
}

// SandboxKeyMatcher returns a match function for MountRootFor that
// accepts a mount point naming the same sandbox as key: the same bare
// name, under one of dirs.
//
// It matches on the NAME and not on the whole path because the two do
// not spell the same thing. dockerd reports
// /var/run/docker/netns/<id> while the kernel renders the mount point
// as /run/docker/netns/<id> wherever /var/run is a symlink to /run,
// which is Debian and Ubuntu. String equality on the full path finds
// nothing there, and finding nothing is indistinguishable from a
// namespace that is genuinely absent.
//
// dirs is a parameter rather than a copy of pkg/plugin's
// sandboxNetnsDirs kept here: a second transcription of that list
// could go stale against it with nothing to notice, and the caller
// already has the authoritative one.
func SandboxKeyMatcher(key string, dirs []string) func(string) bool {
	name := filepath.Base(filepath.Clean(key))
	return func(mountPoint string) bool {
		if filepath.Base(mountPoint) != name {
			return false
		}
		parent := filepath.Dir(mountPoint)
		for _, d := range dirs {
			if parent == d {
				return true
			}
		}
		return false
	}
}

// MountPointsUnder returns every mount point in raw whose parent
// directory is one of dirs, mapped to that row's mount root (field 4).
//
// A map rather than the first match: the caller watching for a NEW
// sandbox has to compare whole sets, and "the first row that matches"
// cannot express "one more row than before".
func MountPointsUnder(raw []byte, dirs []string) map[string]string {
	out := map[string]string{}
	for _, line := range mountinfoLines(raw) {
		i := strings.Index(line, " - ")
		if i < 0 {
			continue
		}
		f := strings.Fields(line[:i])
		if len(f) < 5 {
			continue
		}
		mp := UnescapeMountField(f[4])
		parent := filepath.Dir(mp)
		for _, d := range dirs {
			if parent == d {
				out[mp] = f[3]
				break
			}
		}
	}
	return out
}

func mountinfoLines(raw []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out
}
