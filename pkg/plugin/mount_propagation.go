// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// This process's own mount table, and PID 1's. PID 1 is the host's init
// where the manifest's pidhost grant reaches the host, and a nested
// engine's init where it does not.
const (
	selfMountinfo = "/proc/self/mountinfo"
	selfMountNS   = "/proc/self/ns/mnt"
	initMountNS   = "/proc/1/ns/mnt"
	initMountinfo = "/proc/1/mountinfo"
)

// The three readings of sandbox_netns_propagation.
const (
	// sandboxPropagationUnknown: no mount in this process's mountinfo
	// covers the sandbox netns directory, or mountinfo could not be
	// read.
	sandboxPropagationUnknown int32 = -1
	// sandboxPropagationPrivate: the mount carries no propagation link,
	// so a bind mount the daemon makes under it after this process
	// started can never appear here.
	sandboxPropagationPrivate int32 = 0
	// sandboxPropagationLinked: the mount carries a propagation link,
	// so such a bind mount does appear here.
	sandboxPropagationLinked int32 = 1
)

// sandboxNetnsPropagationIn reports whether the mount carrying the
// sandbox netns directory can receive mounts the daemon makes after
// this process started.
//
// THIS IS THE EXPLANATION SECURITY.md ASSERTS, READ RATHER THAN
// INFERRED. The key route is refused on this lane once per attach, and
// the reason given is that the plugin's /var/run/docker bind is a
// snapshot. A snapshot is not a property of the bind: MEASURED in a
// user and mount namespace built the way runc builds a plugin's
// (default root propagation MS_SLAVE|MS_REC), a bind of a SHARED source
// receives the daemon's later per-sandbox mounts with no mount option
// at all, and a bind of a PRIVATE source receives none of them with
// rslave or rshared either. The propagation belongs to the source, and
// config.json has no lever over it. This reads which of the two this
// host is.
//
// mountinfo's optional fields carry the link: `master:N` is a mount that
// receives from peer group N, `shared:N` one that is a member of it.
// Neither present is a private mount, and a private mount is the arm
// that makes sandbox_key_not_a_namespace inevitable rather than
// incidental.
//
// THE BOUND, and it is why a `shared:` tag is not proof: this process
// cannot see which peer group the daemon's own mount is in, so a
// `shared:` tag names a group it cannot check is the right one. The
// shipped manifest asks for no propagation option, so the tag on a
// stock install is `master:` or nothing, and the ambiguous reading is
// one an operator has to have arranged.
//
// dirs and the mountinfo path are parameters for the same reason
// splitSandboxKeyIn takes its directories: the table is then drivable
// without root and without a live daemon.
func sandboxNetnsPropagationIn(dirs []string, mountinfoPath string) int32 {
	mounts, err := parseMountinfo(mountinfoPath)
	if err != nil {
		return sandboxPropagationUnknown
	}

	for _, dir := range dirs {
		// The permitted directories are two spellings of one place on
		// most hosts, because /var/run is a symlink to /run, and
		// mountinfo carries the resolved one. Resolving here is what
		// keeps the answer from depending on which spelling the
		// caller listed first.
		resolved, ok := resolveThroughMissing(dir)
		if !ok {
			continue
		}
		best := ""
		var bestOptional []string
		for _, m := range mounts {
			if !mountCovers(m.point, resolved) {
				continue
			}
			if len(m.point) >= len(best) {
				best, bestOptional = m.point, m.optional
			}
		}
		if best == "" {
			continue
		}
		for _, o := range bestOptional {
			if strings.HasPrefix(o, "master:") || strings.HasPrefix(o, "shared:") {
				return sandboxPropagationLinked
			}
		}
		return sandboxPropagationPrivate
	}
	return sandboxPropagationUnknown
}

// netnsSources are the paths the three sandbox-netns readings are taken
// from. A zero value means the production ones.
//
// It exists for the reason sandboxNetnsPropagationIn takes its
// directories as a parameter: the readings have to be drivable without
// root and without a live daemon. The parameters made each FUNCTION
// drivable; this makes the hop from the function to the published field
// drivable too, which is where a field can quietly carry its
// neighbour's number (#417 review r1).
//
// Production never sets it. NewPlugin leaves it zero and every reading
// resolves to the constants below.
type netnsSources struct {
	dirs          []string
	mountinfo     string
	selfNS        string
	initNS        string
	initMountinfo string
}

func (p *Plugin) netnsReadingSources() netnsSources {
	s := p.netnsSrc
	if len(s.dirs) == 0 {
		s.dirs = sandboxNetnsDirs
	}
	if s.mountinfo == "" {
		s.mountinfo = selfMountinfo
	}
	if s.selfNS == "" {
		s.selfNS = selfMountNS
	}
	if s.initNS == "" {
		s.initNS = initMountNS
	}
	if s.initMountinfo == "" {
		s.initMountinfo = initMountinfo
	}
	return s
}

// resolveThroughMissing resolves dir's symlinks, and keeps answering
// once the path stops existing.
//
// EvalSymlinks fails on a path whose leaf is absent, and
// /var/run/docker/netns does not exist until the daemon's first
// sandbox. That is the state of every host where the plugin was
// enabled before anything ran on the network, so a reading that gave
// up there would be unavailable exactly when an operator asks whether
// the route will work. It is also indistinguishable, in the value, from
// a mountinfo that could not be read (#417).
//
// The answer is still well defined: nothing mounts the netns directory
// itself, the daemon mkdirs it inside whatever filesystem already
// covers its parent, and a directory created inside a mount belongs to
// that mount's peer group. So resolve the deepest ancestor that does
// exist and rejoin the rest. On a host where the directory is present
// this is EvalSymlinks and nothing more.
func resolveThroughMissing(dir string) (string, bool) {
	rest := ""
	for p := filepath.Clean(dir); ; {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(resolved, rest), true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", false
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// mountLine is the part of a mountinfo record these readings use.
type mountLine struct {
	point    string
	optional []string
}

// parseMountinfo reads one process's mount table.
//
// A record is `id parent major:minor root mountpoint options
// [optional...] - fstype source superopts`, and the optional fields are
// variable in number, which is why the separator is searched for rather
// than indexed. A record without one is skipped instead of guessed at.
func parseMountinfo(path string) ([]mountLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var mounts []mountLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 7 {
			continue
		}
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 {
			continue
		}
		mounts = append(mounts, mountLine{point: unescapeMountField(fields[4]), optional: fields[6:sep]})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}

// The two non-count readings of sandbox_netns_init_mounts.
const (
	// sandboxInitMountsUnknown: PID 1's mount table could not be read.
	sandboxInitMountsUnknown int32 = -1
	// sandboxInitMountsSameNS: PID 1 is in this process's own mount
	// namespace, so reaching the sandbox key through /proc/1/root
	// reaches the same table this process already has and can add
	// nothing.
	sandboxInitMountsSameNS int32 = -2
)

// sandboxNetnsInitMountsIn counts the sandbox netns mounts that exist in
// PID 1's mount table.
//
// WHICH QUESTION THIS ANSWERS. /proc/1/root resolves in PID 1's mount
// namespace, so a path opened through it sees mounts this process's own
// table does not. That is the route issue #417 names, and whether it
// helps depends on two facts that are invisible from the code: whether
// PID 1 is in a different mount namespace at all, and whether the
// daemon's per-sandbox mounts are in THAT one. A single number carries
// both. -2 is the first fact answered no; a count of zero with
// sandbox_netns_visible non-zero is the second answered no; a count
// tracking sandbox_netns_visible is both answered yes.
//
// IT IS NOT THE SAME NUMBER IN THE TWO ENVIRONMENTS AND THAT IS THE
// POINT. Under a nested engine PID 1 is that engine's init and not the
// outer host's, so a route that works on a systemd host can refuse on
// the lane, and the opposite. Without this reading a red lane and a
// working product look alike.
func sandboxNetnsInitMountsIn(dirs []string, selfNS, initNS, initMountinfo string) int32 {
	self, errSelf := os.Readlink(selfNS)
	init, errInit := os.Readlink(initNS)
	if errSelf == nil && errInit == nil && self == init {
		return sandboxInitMountsSameNS
	}
	mounts, err := parseMountinfo(initMountinfo)
	if err != nil {
		return sandboxInitMountsUnknown
	}
	var n int32
	for _, m := range mounts {
		for _, dir := range dirs {
			if strings.HasPrefix(m.point, strings.TrimSuffix(dir, "/")+"/") {
				n++
				break
			}
		}
	}
	return n
}

// mountCovers reports whether mount point dest contains path dir.
//
// An ancestor counts because a bind mount shares the source's directory
// tree: entries created under it afterwards are visible through the
// mount without any propagation, since creating a directory is not
// creating a mount. That is not a technicality here, it is the fix for
// #588 -- /var/run/docker/netns does not exist until the daemon's first
// sandbox, so the manifest mounts the parent and the netns directory
// appears inside the plugin when libnetwork creates it. Verified with a
// plugin enabled before the directory existed: sandbox_netns_visible
// went -1 -> 1 on the same process, matching the host's entry.
//
// It lived in network_test.go until #417 needed the same question
// answered in production code. One derivation, because two would be
// free to disagree about the one case that matters.
func mountCovers(dest, dir string) bool {
	if dest == dir {
		return true
	}
	return strings.HasPrefix(dir, strings.TrimSuffix(dest, "/")+"/")
}

// unescapeMountField decodes the octal escapes mountinfo uses for space,
// tab, newline and backslash in a path. A mount point under a directory
// with a space in its name is otherwise compared against the wrong
// string, and compares equal to nothing.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+3 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		var v byte
		ok := true
		for _, c := range []byte(s[i+1 : i+4]) {
			if c < '0' || c > '7' {
				ok = false
				break
			}
			v = v*8 + (c - '0')
		}
		if !ok {
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(v)
		i += 3
	}
	return b.String()
}
