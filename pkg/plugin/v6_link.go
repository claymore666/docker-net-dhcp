// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The engine turns IPv6 OFF on a container interface that carries no
// IPv6 address.
//
// libnetwork writes net.ipv6.conf.<iface>.disable_ipv6 = 1 on the
// sandbox interface when the endpoint has no AddressIPv6, and #868 made
// that case reachable for the first time: before it, an endpoint with no
// DHCPv6 address was never created at all. Measured on the CI engine
// (docker run on a network without IPv6: disable_ipv6 reads 1 on eth0
// while conf/all reads 0), and again in an isolated netns against the
// plugin's own persistent-client argv:
//
//	disable_ipv6=1 -> no link-local, no router solicitation, no
//	                 information-request; dhcpcd -6 prints nothing at all
//	disable_ipv6=0 -> link-local appears, RS goes out, dhcpcd reports
//	                 "requesting DHCPv6 information" and the hook fires
//	                 INFORM6 carrying the server's DNS and search domain
//
// So on a stateless or SLAAC segment the endpoint now starts, and then
// has no IPv6 of any kind — the flag the engine set for "no address"
// also forecloses the mechanisms that were supposed to supply one. That
// is the failure the stateless arm of
// TestDHCPv6_Stateless_ConfigurationReachesTheContainer reported: not a
// budget that was too short, but a link on which nothing could ever
// arrive.
//
// Clearing it is therefore part of running a DHCPv6 client at all, not a
// special case of the tolerated path: wherever the plugin is about to
// speak DHCPv6 on a link, IPv6 has to be administratively on. Where the
// endpoint did get an address the flag is already 0 and this is a read
// and no write.
const ipv6DisableSysctlDir = "/proc/sys/net/ipv6/conf"

// procSysMount is the sysctl tree's mount point. It is mounted READ-ONLY
// in the managed-plugin rootfs -- the same fact that made every lease
// fail in #247, documented on pkg/dhcp's procSysPath, and the reason the
// DHCP clients remount it inside their own mount namespace before
// dhcpcd touches net/ipv6/conf/<if>/{autoconf,accept_ra}.
//
// It bit here too, and the counter added with this code is what said so:
// the first CI run reported ipv6_link_enable_failures = 1 with "open ...
// disable_ipv6: read-only file system". Entering the container's NETWORK
// namespace changes which sysctls the path names; it does not change
// whether the filesystem carrying them can be written.
const procSysMount = "/proc/sys"

// makeProcSysWritable takes a private mount namespace for the calling
// thread and remounts the sysctl tree read-write inside it.
//
// Private first, and recursively: an unshared mount namespace still
// inherits shared propagation, so without this the remount could
// propagate back to the host's view. `unshare -m`(1) does this by
// default and the Go call does not, which is exactly the kind of
// difference that makes a shell recipe unsafe to transcribe.
//
// IT IS BEST EFFORT, AND THAT IS THE MEASURED TRADE, NOT A SHRUG. The
// sibling that does the same remount for dhcpcd's argv already carries
// the measurement -- pkg/dhcp.mountPrep, and the "procsys-remount" step
// mountPrepStep names there: on a
// --privileged runtime the remount FAILS -- `can't find /proc/sys in
// /proc/mounts`, because /proc/sys is not a separate mount there -- and
// /proc/sys is already writable, so the failure is correct and
// harmless. That sibling therefore makes it non-fatal deliberately.
//
// This function returned an error on that failure, and the caller took
// it as a verdict: the disable_ipv6 write was then NEVER ATTEMPTED, on
// a host where it would have succeeded. A guard fails in one direction,
// and this one failed in the direction that turns a working host into
// one with no IPv6 -- for a mount that host does not need.
//
// So each step reports separately and only the WRITE decides:
//
//   - unshare and MS_PRIVATE gate the REMOUNT and nothing else. If
//     either fails there is no private namespace, so remounting would
//     propagate to the host's view, and it is skipped. The write still
//     goes ahead in whatever view we have.
//   - the remount failing is not a verdict either. clearDisableIPv6
//     reads and writes the real path, and its own EROFS is the honest
//     report of "the sysctl tree is not writable" -- one observer, at
//     the place the obligation lives, instead of a proxy that can be
//     wrong in both directions.
//
// The caller must already hold its OS thread and must restore the
// original mount namespace afterwards.
func makeProcSysWritable() error {
	if err := unix.Unshare(unix.CLONE_FS | unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("unshare mount namespace: %w", err)
	}
	if err := unix.Mount("none", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mount propagation private: %w", err)
	}
	if err := unix.Mount("", procSysMount, "", unix.MS_REMOUNT|unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("remount %v read-write: %w", procSysMount, err)
	}
	return nil
}

// procSysPrepDisposition says what a makeProcSysWritable error means to
// the caller. It exists so the "keep going" decision is a value a test
// can read, rather than a comment beside a `log.Debug` that nothing
// executes.
//
// There is exactly one disposition today -- CONTINUE -- and that is the
// point: no failure of the preparation step may stop the write. If a
// future step here ever does have to be fatal, it gets a second value
// and this function stops being a constant, which is a change a
// reviewer can see.
func procSysPrepIsFatal(error) bool { return false }

// ipv6DisablePath is the disable_ipv6 sysctl for one interface, as seen
// from inside the network namespace that owns it. /proc/sys/net is
// per-netns: the same path names a different switch depending on the
// reader's netns, which is why the caller enters the sandbox rather than
// reaching in from the host.
func ipv6DisablePath(iface string) string {
	return filepath.Join(ipv6DisableSysctlDir, iface, "disable_ipv6")
}

// clearDisableIPv6 turns IPv6 on for the interface whose disable_ipv6
// sysctl is at path, reporting whether it had to write anything.
//
// Split out from the namespace entry below it so the read-before-write
// and its two outcomes are reachable from a test with a temp file; the
// caller supplies the namespace and the path.
func clearDisableIPv6(path string) (bool, error) {
	cur, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %v: %w", path, err)
	}
	if strings.TrimSpace(string(cur)) == "0" {
		return false, nil
	}
	if err := os.WriteFile(path, []byte("0\n"), 0o644); err != nil {
		return false, fmt.Errorf("write %v: %w", path, err)
	}
	return true, nil
}

// enableIPv6OnContainerLink clears disable_ipv6 on the container side of
// this endpoint's link, inside the sandbox network namespace.
//
// Concurrency contract is pkg/dhcp.DHCPClient.Start's, for the same
// reason and with the same failure handling: the goroutine is locked to
// its OS thread for the switch, and if the switch back fails the thread
// is deliberately kept locked so a wrong-netns thread never re-enters
// Go's pool.
func (m *dhcpManager) enableIPv6OnContainerLink() (bool, error) {
	// Both preconditions are read BEFORE any thread is locked or any
	// namespace entered, because neither needs the namespace and a
	// failure after the switch is a failure with a thread to unwind.
	//
	// The link check is not decoration: m.ctrLink is nil until
	// locateContainerLink has run, and Attrs() on a nil Link panics.
	if m.ctrLink == nil {
		return false, fmt.Errorf("container link not located yet")
	}
	// NsHandle.IsOpen() is `ns != -1`, so it catches a CLOSED handle
	// and NOT the zero value -- an unset handle is 0, which is stdin.
	// Checked here anyway because a closed handle is the reachable
	// case (Stop closes it), and the zero value only occurs in a
	// manager that never reached openSandboxNetNS, which cannot reach
	// this call either.
	if !m.nsHandle.IsOpen() {
		return false, fmt.Errorf("sandbox network namespace handle is closed")
	}
	iface := m.ctrLink.Attrs().Name

	// Two namespaces are needed and they are needed for different
	// reasons: the NETWORK namespace decides which interface the path
	// names, and the MOUNT namespace decides whether it can be written
	// at all.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNet, err := netns.Get()
	if err != nil {
		return false, fmt.Errorf("failed to open current network namespace: %w", err)
	}
	defer func() {
		if err := origNet.Close(); err != nil {
			log.WithError(err).Debug("origNet close failed")
		}
	}()

	// Read the CURRENT thread's mount namespace, not /proc/self/ns/mnt:
	// that resolves to the main thread's, and this goroutine has just
	// locked to a different one which an earlier goroutine may already
	// have moved. Same reasoning as propagateDNS.
	origMnt, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/mnt", unix.Gettid()))
	if err != nil {
		return false, fmt.Errorf("open self mnt ns: %w", err)
	}
	defer origMnt.Close()

	if err := makeProcSysWritable(); err != nil {
		if procSysPrepIsFatal(err) {
			return false, err
		}
		// Not a verdict -- see makeProcSysWritable. The write below is
		// the observer, and its own error is the honest report. Audible
		// rather than silent, because a host where this fails every
		// time and the write succeeds anyway is worth being able to
		// recognise in a log.
		log.WithError(err).WithFields(m.logFields(true)).
			Debug("Could not make /proc/sys writable; attempting the disable_ipv6 write anyway")
	}
	defer func() {
		if err := unix.Setns(int(origMnt.Fd()), unix.CLONE_NEWNS); err != nil {
			// The thread is now stuck in a mount namespace of our own
			// making. Keep it locked (a second Lock so the deferred
			// Unlock does not pair) so it dies with the goroutine
			// rather than returning to Go's pool with a private
			// /proc/sys view.
			log.WithError(err).Error("Failed to restore original mount namespace; pinning thread for kill")
			runtime.LockOSThread()
		}
	}()

	if err := netns.Set(m.nsHandle); err != nil {
		return false, fmt.Errorf("failed to enter network namespace: %w", err)
	}
	defer func() {
		if err := netns.Set(origNet); err != nil {
			log.WithError(err).Error("Failed to restore original netns; pinning thread for kill")
			runtime.LockOSThread()
		}
	}()

	return clearDisableIPv6(ipv6DisablePath(iface))
}

// ensureIPv6Enabled is the call site's view: enable IPv6 on the link,
// and treat a failure as degraded rather than fatal.
//
// Degraded and not fatal because the v4 client is already running and
// keeping the container's IPv4 lease is worth more than refusing the
// endpoint over the v6 half. The consequence is visible without reading
// the log — the link-local wait that follows will time out and every
// DHCPv6 exchange will fail — but it gets a counter of its own so the
// cause is distinguishable from a segment that is merely quiet.
func (m *dhcpManager) ensureIPv6Enabled() {
	changed, err := m.enableIPv6OnContainerLink()
	if err != nil {
		// Nil plugin, not nil error: unit tests that do not stand up a
		// Plugin leave it nil, and the failure is still a failure when
		// there is no counter to bump (see dhcpManager.plugin).
		if m.plugin != nil {
			m.plugin.ipv6LinkEnableFailures.Add(1)
		}
		log.WithError(err).WithFields(m.logFields(true)).
			Warn("Failed to enable IPv6 on the container link; DHCPv6 cannot work on it")
		return
	}
	if changed {
		log.WithFields(m.logFields(true)).
			Info("Enabled IPv6 on the container link; the engine had disabled it for an endpoint with no IPv6 address")
	}
}
