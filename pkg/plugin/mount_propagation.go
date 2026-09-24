// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// PID 1 is the host's init where the pidhost grant reaches the host, and a nested engine's init where it does not
// (#403).
const (
	selfMountinfo = "/proc/self/mountinfo"
	selfMountNS   = "/proc/self/ns/mnt"
	initMountNS   = "/proc/1/ns/mnt"
	initMountinfo = "/proc/1/mountinfo"
)

const (
	sandboxPropagationUnknown int32 = -1
	// sandboxPropagationPrivate: the mount has no propagation link, so a later daemon bind mount never appears here.
	sandboxPropagationPrivate int32 = 0
	// sandboxPropagationLinked: the mount has a propagation link, so a later daemon bind mount appears here.
	sandboxPropagationLinked int32 = 1
)

// sandboxNetnsPropagationIn reads mountinfo's master:N and shared:N fields for the sandbox netns
// mount. Measured in a runc-style namespace, a bind of a shared source receives the daemon's later
// mounts and a private one never does, whatever the bind option, so config.json has no lever (#417).
func sandboxNetnsPropagationIn(dirs []string, mountinfoPath string) int32 {
	mounts, err := parseMountinfo(mountinfoPath)
	if err != nil {
		return sandboxPropagationUnknown
	}

	for _, dir := range dirs {
		// /var/run is usually a symlink to /run, and mountinfo carries the resolved path.
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

// resolveThroughMissing resolves the deepest existing ancestor, since /var/run/docker/netns is absent
// until the first sandbox and a directory created inside a mount belongs to that mount (#417).
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

type mountLine struct {
	point    string
	optional []string
}

// parseMountinfo searches for the " - " separator, since mountinfo's optional fields vary in number (proc(5)).
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

const (
	sandboxInitMountsUnknown int32 = -1
	// sandboxInitMountsSameNS: PID 1 shares this process's mount namespace, so /proc/1/root adds nothing.
	sandboxInitMountsSameNS int32 = -2
)

// sandboxNetnsInitMountsIn counts the sandbox netns mounts in PID 1's table, where /proc/1/root
// resolves; under a nested engine PID 1 is that engine's init, not the host's (#417).
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

// mountCovers counts an ancestor, since entries created under a bind mount show without propagation;
// the netns directory appeared inside a plugin enabled before it existed (#588).
func mountCovers(dest, dir string) bool {
	if dest == dir {
		return true
	}
	return strings.HasPrefix(dir, strings.TrimSuffix(dest, "/")+"/")
}

// unescapeMountField decodes mountinfo's octal escapes for space, tab, newline and backslash (proc(5)).
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
