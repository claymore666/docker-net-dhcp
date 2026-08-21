// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/pkg/dhcp"
)

// errPIDNotContainer is returned when the PID handed to
// writeContainerResolvConf no longer belongs to the container it was
// resolved from. Callers count it: a rise means the plugin came that
// close to writing DHCP-supplied content into an unrelated process.
var errPIDNotContainer = errors.New("pid no longer belongs to the expected container")

// cgroupNamesContainer reports whether the contents of a
// /proc/<pid>/cgroup file place that task inside container ctrID.
//
// A substring match on the container ID, and that is deliberate: the ID
// is 64 hex characters, so it cannot collide with anything else in the
// path, while the path around it varies by cgroup driver (`/docker/<id>`
// for cgroupfs, `docker-<id>.scope` for systemd), by cgroup version
// (v1 writes one line per controller) and by namespace (a private
// cgroup namespace prefixes the path). Parsing that shape would be
// brittle in the direction that matters -- a parse that failed to
// recognise a valid layout would refuse a legitimate container and
// silently disable DNS propagation.
//
// An empty ctrID is never a match. It is what a future caller that
// forgot to thread the ID through would pass, and "check nothing" is
// not an acceptable reading of it.
func cgroupNamesContainer(cgroup, ctrID string) bool {
	if ctrID == "" {
		return false
	}
	return strings.Contains(cgroup, ctrID)
}

// openContainerProc opens /proc/<pid> and confirms the task behind it
// still belongs to ctrID before anything is done with it (#688).
//
// Both halves matter, and neither is sufficient alone:
//
//   - The cgroup check answers "is this still that container?". The
//     PID is resolved through Docker (NetworkInspect -> ContainerInspect)
//     and nothing between that call and the setns re-checks it. The
//     plugin runs with pidhost: true, so if the container exits in
//     that window and the kernel recycles the PID, the victim is an
//     arbitrary *host* process -- possibly one in the host's root
//     mount namespace. A liveness check would not help: the whole
//     failure mode is that something else is alive at that PID.
//   - The returned directory fd pins the answer. procfs invalidates a
//     /proc/<pid> dentry when the task exits, so every openat below
//     this fd either reaches the same task or fails with ESRCH -- a
//     PID recycled after the check cannot be reached through it.
//     Re-deriving the path as a string afterwards would reopen the
//     window the check just closed.
//
// The container ID appears in the cgroup path under both cgroup
// drivers (`/docker/<id>` for cgroupfs, `docker-<id>.scope` for
// systemd) and survives a private cgroup namespace, which only
// prefixes the path. A substring match on the 64-hex ID is therefore
// both sufficient and unambiguous.
func openContainerProc(pid int, ctrID string) (*os.File, error) {
	d, err := os.Open(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return nil, fmt.Errorf("open /proc/%d: %w", pid, err)
	}

	fd, err := unix.Openat(int(d.Fd()), "cgroup", unix.O_RDONLY, 0)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("%w: reading the cgroup of pid %d: %v", errPIDNotContainer, pid, err)
	}
	cgroup, err := io.ReadAll(os.NewFile(uintptr(fd), "cgroup"))
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

// writeContainerResolvConf enters the mount namespace of the process
// identified by pid and rewrites /etc/resolv.conf with the
// DHCP-supplied DNS server list.
//
// Why setns into the mount namespace rather than writing the host's
// /var/lib/docker/containers/<id>/resolv.conf bind source directly:
// the plugin's filesystem only bind-mounts /var/run/docker.sock (see
// config.json), so it can't see the host's resolv.conf bind source.
// Adding another bind mount would prompt every existing user for
// re-grant on upgrade. Mount-ns entry is a one-time code cost that
// avoids any plugin-config change.
//
// Threading contract: setns is per-thread, so we lock the goroutine
// to one OS thread for the duration. On the *unhappy* path (setns
// back to host fails) we deliberately do NOT call UnlockOSThread —
// the thread is now in the container's mount namespace and would
// poison the next goroutine that lands on it. Go's runtime retires
// poisoned threads when their owning goroutine exits, so callers
// must run this from a goroutine they're willing to lose. In
// practice this only fires if the container netns vanished mid-write
// (i.e. the container died), which is exactly the case we already
// have to live with on every other namespace operation.
//
// Caveats baked in:
//   - Docker rewrites /etc/resolv.conf on `docker network connect`
//     and `disconnect`. Our write survives between those events but
//     not across them. Operators connecting/disconnecting networks
//     will need to wait for the next DHCP renewal to re-populate.
//   - Multi-network containers: the LAST plugin to write wins.
//     Containers attached to two net-dhcp networks will end up with
//     whichever network's renewal happened most recently.
//   - search-domain handling: prefer the multi-entry DHCP option 119
//     (Domain Search List, dhcpcd env `new_domain_search`). Falls back to the
//     single-entry option 15 (`domain`, dhcpcd env `new_domain_name`) when option 119
//     isn't supplied. RFC 3397 specifies option 119 supersedes option
//     15 when both are present.
func writeContainerResolvConf(pid int, ctrID string, dns []string, searchList []string, searchDomain string) error {
	// Drop anything that would restructure the file before the emptiness
	// guard below, so "every nameserver the server sent was unusable"
	// lands on that guard rather than producing a resolv.conf with no
	// nameserver line at all (#689).
	dns = resolvSafe(dns)
	searchList = resolvSafe(searchList)
	if !dhcp.SafeDirectiveValue(searchDomain) {
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
		// Defensive: caller should have filtered. Writing empty
		// resolv.conf would silently nuke name resolution.
		return fmt.Errorf("refusing to write empty resolv.conf")
	}

	// Before locking a thread or touching a namespace: confirm the PID
	// still belongs to the container it was resolved from, and keep the
	// directory fd that proves it (#688).
	procDir, err := openContainerProc(pid, ctrID)
	if err != nil {
		return err
	}
	defer procDir.Close()

	runtime.LockOSThread()

	// Open self-thread's mnt ns through /proc/self/task/<tid>/ns/mnt
	// rather than /proc/self/ns/mnt: the latter resolves to the main
	// thread's ns, but we just locked to a *different* thread that
	// may already have been moved by an earlier goroutine on this
	// runtime. Always read the current thread's view.
	origMnt, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/mnt", unix.Gettid()))
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("open self mnt ns: %w", err)
	}
	defer origMnt.Close()

	// Through procDir, not by path: see openContainerProc. Reopening
	// /proc/<pid>/ns/mnt as a string would let a recycled PID back in.
	targetFd, err := unix.Openat(int(procDir.Fd()), "ns/mnt", unix.O_RDONLY, 0)
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("open container mnt ns (pid %d): %w", pid, err)
	}
	targetMnt := os.NewFile(uintptr(targetFd), "ns/mnt")
	defer targetMnt.Close()

	// Detach this thread's filesystem state (CWD, root, umask) from
	// the rest of the Go runtime's threads BEFORE setns into the
	// mount namespace. Linux refuses CLONE_NEWNS setns when the
	// caller still shares fs state with another process — that's
	// what produced "invalid argument" on the first CI run. unshare
	// is per-thread and cheap; the locked thread is retired with
	// the goroutine after we return so the side-effect is
	// well-scoped.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("unshare CLONE_FS: %w", err)
	}

	if err := unix.Setns(int(targetMnt.Fd()), unix.CLONE_NEWNS); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("setns into container mnt ns: %w", err)
	}

	writeErr := os.WriteFile("/etc/resolv.conf", buildResolvConf(dns, searchList, searchDomain), 0644)

	if err := unix.Setns(int(origMnt.Fd()), unix.CLONE_NEWNS); err != nil {
		// Thread is now stuck in the container's mnt ns. Don't
		// UnlockOSThread — see threading contract above.
		return fmt.Errorf("setns back to host mnt ns failed (write was: %v): %w", writeErr, err)
	}
	runtime.UnlockOSThread()
	return writeErr
}

// buildResolvConf renders the DHCP-supplied DNS list as a resolv.conf
// file. Marker comment lets operators see at a glance that the file
// is plugin-managed and where the values came from.
//
// Search-line precedence (RFC 3397): a non-empty searchList from
// option 119 wins over the single-domain searchDomain from option 15.
// When both are absent no `search` line is emitted — resolv.conf is
// then equivalent to a no-search-domain configuration.
func buildResolvConf(dns []string, searchList []string, searchDomain string) []byte {
	// Backstop. writeContainerResolvConf already filtered; doing it here
	// too means the renderer itself cannot emit a line it was not asked
	// for, whoever calls it. Same reasoning as dhcp.directive on the
	// config-file side — the format has no escaping, so "drop" is the
	// only available answer.
	dns = resolvSafe(dns)
	searchList = resolvSafe(searchList)
	if !dhcp.SafeDirectiveValue(searchDomain) {
		searchDomain = ""
	}
	searchDomain, _ = dhcp.FirstSearchDomain(searchDomain)

	var b strings.Builder
	b.WriteString("# generated by docker-net-dhcp from DHCP options\n")
	switch {
	case len(searchList) > 0:
		fmt.Fprintf(&b, "search %s\n", strings.Join(searchList, " "))
	case searchDomain != "":
		fmt.Fprintf(&b, "search %s\n", searchDomain)
	}
	for _, ns := range dns {
		fmt.Fprintf(&b, "nameserver %s\n", ns)
	}
	return []byte(b.String())
}

// resolvSafe drops entries that cannot be written to /etc/resolv.conf as
// a single field.
//
// resolv.conf is line-oriented with no quoting, so a value carrying a
// newline does not corrupt its own line — it appends a line the DHCP
// server chose, and `nameserver <attacker>` is a legal one. The DNS server
// list and the search list already reach us through strings.Fields, which
// makes whitespace structurally impossible; option 15 (the single domain)
// does not, and that asymmetry is what #689 records. Filtering all three
// keeps the property from depending on which upstream helper was used.
func resolvSafe(vals []string) []string {
	out := vals[:0:0]
	for _, v := range vals {
		if v == "" || !dhcp.SafeDirectiveValue(v) {
			log.WithField("value", fmt.Sprintf("%q", v)).
				Warn("Dropping DHCP-supplied resolv.conf value: it carries a control character")
			continue
		}
		out = append(out, v)
	}
	return out
}
