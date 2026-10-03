// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"unicode"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
)

var errPIDNotContainer = errors.New("pid no longer belongs to the expected container")

// cgroupNamesContainer is a filter, not an authorization (#688): an unprivileged
// `systemd-run --user --scope --unit=docker-<id>.scope` puts any container ID in a cgroup path, and the match is
// over the whole file. Narrowing to path segments alone buys nothing and ships with the netns identity work (#785);
// TestCgroupNamesContainer_DelegatedSubtreeIsAcceptedByDesign asserts the hole. An empty ctrID never matches.
func cgroupNamesContainer(cgroup, ctrID string) bool {
	if ctrID == "" {
		return false
	}
	return strings.Contains(cgroup, ctrID)
}

// openContainerProc confirms the task at pid still belongs to ctrID and returns a /proc/<pid> directory fd (#688).
// The plugin runs with pidhost, so a recycled PID names an arbitrary host task; procfs invalidates the dentry when
// the task exits, so every openat below the fd reaches the same task or fails with ESRCH.
func openContainerProc(pid int, ctrID string) (*os.File, error) {
	d, err := os.Open(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return nil, fmt.Errorf("open /proc/%d: %w", pid, err)
	}

	// O_CLOEXEC: os/exec does not sweep foreign descriptors, so another goroutine's spawn would inherit this one.
	fd, err := unix.Openat(int(d.Fd()), "cgroup", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("%w: reading the cgroup of pid %d: %v", errPIDNotContainer, pid, err)
	}
	// Closed on every path out (#729): os.NewFile's finalizer bounds a leak only by the next GC.
	cgroupFile := os.NewFile(uintptr(fd), "cgroup")
	defer cgroupFile.Close()

	cgroup, err := io.ReadAll(cgroupFile)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("%w: reading the cgroup of pid %d: %v", errPIDNotContainer, pid, err)
	}

	if !cgroupNamesContainer(string(cgroup), ctrID) {
		d.Close()
		return nil, fmt.Errorf("%w: pid %d is in cgroup %q, which does not name container %s",
			errPIDNotContainer, pid, strings.TrimSpace(string(cgroup)), shortID(ctrID))
	}

	return d, nil
}

// writeContainerResolvConf enters pid's mount namespace and rewrites /etc/resolv.conf (#100): config.json does not
// mount /var/lib/docker/containers, and a new mount prompts every user for a re-grant on upgrade. If setns back fails
// the thread stays locked and retires with its goroutine. Docker rewrites the file on network connect and disconnect,
// and with two net-dhcp networks the last renewal wins. Option 119 takes precedence over option 15 (RFC 3397).
func writeContainerResolvConf(pid int, ctrID string, dns []string, searchList []string, searchDomain, iface string) error {
	// Unusable values are dropped before the emptiness guard, so all-unusable lands on it (#689).
	dns = resolvSafe(dns)
	searchList = resolvSafe(searchList)
	if !dhcp.SafeValue(searchDomain) {
		log.WithField("domain", fmt.Sprintf("%q", searchDomain)).
			Warn("Dropping DHCP domain name: it carries a control character")
		searchDomain = ""
	}
	if trimmed, truncated := dhcp.FirstSearchDomain(searchDomain); truncated {
		log.WithField("domain", fmt.Sprintf("%q", searchDomain)).
			WithField("kept", trimmed).
			Warn("DHCP domain name carried more than one domain; keeping only the first")
		searchDomain = trimmed
	}

	if len(dns) == 0 {
		return fmt.Errorf("refusing to write empty resolv.conf")
	}

	// The PID is confirmed before any thread is locked or namespace touched (#688).
	procDir, err := openContainerProc(pid, ctrID)
	if err != nil {
		return err
	}
	defer procDir.Close()

	runtime.LockOSThread()

	// /proc/self/ns/mnt resolves to the main thread's namespace, so the locked thread's own entry is read.
	origMnt, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/mnt", unix.Gettid()))
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("open self mnt ns: %w", err)
	}
	defer origMnt.Close()

	// Through procDir, not by path, so a recycled PID cannot get back in (#688).
	targetFd, err := unix.Openat(int(procDir.Fd()), "ns/mnt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("open container mnt ns (pid %d): %w", pid, err)
	}
	targetMnt := os.NewFile(uintptr(targetFd), "ns/mnt")
	defer targetMnt.Close()

	// Linux refuses a CLONE_NEWNS setns while the thread shares fs state, which gave "invalid argument" on the first
	// CI run; unshare(CLONE_FS) is per-thread (#100).
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("unshare CLONE_FS: %w", err)
	}

	if err := unix.Setns(int(targetMnt.Fd()), unix.CLONE_NEWNS); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("setns into container mnt ns: %w", err)
	}

	writeErr := writeResolvConfFile(ctrID, "/etc/resolv.conf", buildResolvConf(dns, searchList, searchDomain, iface))

	if err := unix.Setns(int(origMnt.Fd()), unix.CLONE_NEWNS); err != nil {
		// The thread is now in the container's mount namespace, so it is not unlocked.
		return fmt.Errorf("setns back to host mnt ns failed (write was: %v): %w", writeErr, err)
	}
	runtime.UnlockOSThread()
	return writeErr
}

// resolvRewriteStep is a test seam: it runs after the open, after a growing write's extension and after the write (#1188).
var resolvRewriteStep = func(stage string) {}

// resolvPadTo extends content with blank or comment filler to size bytes, so a half-rewritten file is never the new
// head over an old tail (#1188). Content that does not end in a newline is returned as it is.
func resolvPadTo(content []byte, size int) []byte {
	n := size - len(content)
	if n <= 0 || len(content) == 0 || content[len(content)-1] != '\n' {
		return content
	}
	out := make([]byte, 0, size)
	out = append(out, content...)
	if n > 1 {
		out = append(out, strings.Repeat("#", n-1)...)
	}
	return append(out, '\n')
}

// resolvLocks holds one mutex per container with a writer in flight (#1188). The path is /etc/resolv.conf in every
// mount namespace, so the container id is the key; a write that never returns stalls only its own container.
var resolvLocks = struct {
	mu sync.Mutex
	m  map[string]*resolvLock
}{m: map[string]*resolvLock{}}

type resolvLock struct {
	mu   sync.Mutex
	refs int
}

// lockResolv takes ctrID's lock and returns its release; the entry is dropped with its last holder or waiter.
func lockResolv(ctrID string) func() {
	resolvLocks.mu.Lock()
	l := resolvLocks.m[ctrID]
	if l == nil {
		l = &resolvLock{}
		resolvLocks.m[ctrID] = l
	}
	l.refs++
	resolvLocks.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		resolvLocks.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(resolvLocks.m, ctrID)
		}
		resolvLocks.mu.Unlock()
	}
}

// writeResolvConfFile rewrites path without an empty window (#1188). Docker bind-mounts the file, so rename is out and
// os.WriteFile truncates first. Equal bytes are not written. Otherwise the new bytes go in at offset 0, then Truncate;
// until then the file is never shorter than old or new: a growing write extends it with filler first, a shrinking one
// carries filler (resolvPadTo). A reader's own copy can still interleave with a write's.
func writeResolvConfFile(ctrID, path string, content []byte) error {
	defer lockResolv(ctrID)()

	old, readErr := os.ReadFile(path)
	if readErr == nil && bytes.Equal(old, content) {
		return nil
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	resolvRewriteStep("opened")

	buf := content
	if fi, err := f.Stat(); err == nil {
		size := int(fi.Size())
		if readErr == nil && size == len(old) && len(content) > size {
			if ext := resolvPadTo(old, len(content)); len(ext) > size {
				if _, err := f.WriteAt(ext[size:], int64(size)); err != nil {
					f.Close()
					return err
				}
				resolvRewriteStep("extended")
				size = len(ext)
			}
		}
		buf = resolvPadTo(content, size)
	}
	if _, err := f.Write(buf); err != nil {
		f.Close()
		return err
	}
	resolvRewriteStep("written")
	if err := f.Truncate(int64(len(content))); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// buildResolvConf renders the DNS list; option 119 takes precedence over option 15 (RFC 3397, #101).
func buildResolvConf(dns []string, searchList []string, searchDomain, iface string) []byte {
	dns = resolvSafe(dns)
	searchList = resolvSafe(searchList)
	if !dhcp.SafeValue(searchDomain) {
		searchDomain = ""
	}
	searchDomain, _ = dhcp.FirstSearchDomain(searchDomain)
	if !resolvOneField(iface) {
		iface = ""
	}

	var b strings.Builder
	b.WriteString("# generated by docker-net-dhcp from DHCP options\n")
	switch {
	case len(searchList) > 0:
		fmt.Fprintf(&b, "search %s\n", strings.Join(searchList, " "))
	case searchDomain != "":
		fmt.Fprintf(&b, "search %s\n", searchDomain)
	}
	for _, ns := range dns {
		fmt.Fprintf(&b, "nameserver %s\n", zonedNameserver(ns, iface))
	}
	return []byte(b.String())
}

// zonedNameserver zones a link-local resolver with the container's interface name (RFC 4007 section 11, #821).
// Routers send link-local RDNSS entries (RFC 8106 section 5.1), and glibc fails every lookup on one without a zone.
// musl parses resolv.conf with inet_pton and drops a zoned line, so no form works for both.
func zonedNameserver(ns, iface string) string {
	if iface == "" || strings.Contains(ns, "%") {
		return ns
	}
	ip := net.ParseIP(ns)
	if ip == nil || ip.To4() != nil || !ip.IsLinkLocalUnicast() {
		return ns
	}
	return ns + "%" + iface
}

// resolvOneField also refuses a space: dhcp.SafeValue stops at 0x1f, and a space splits one entry into two (#1010,
// #704).
func resolvOneField(v string) bool {
	return v != "" && dhcp.SafeValue(v) && !strings.ContainsFunc(v, unicode.IsSpace)
}

// resolvSafe drops values carrying a newline, which would add a line the server chose (#689).
func resolvSafe(vals []string) []string {
	out := vals[:0:0]
	for _, v := range vals {
		if v == "" {
			continue
		}
		if !dhcp.SafeValue(v) {
			log.WithField("value", fmt.Sprintf("%q", v)).
				Warn("Dropping DHCP-supplied resolv.conf value: it carries a control character")
			continue
		}
		if !resolvOneField(v) {
			log.WithField("value", fmt.Sprintf("%q", v)).
				Warn("Dropping DHCP-supplied resolv.conf value: it carries whitespace and would render as more than one")
			continue
		}
		out = append(out, v)
	}
	return out
}
