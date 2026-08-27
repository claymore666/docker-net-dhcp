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

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return false, fmt.Errorf("failed to open current network namespace: %w", err)
	}
	defer func() {
		if err := origNS.Close(); err != nil {
			log.WithError(err).Debug("origNS close failed")
		}
	}()

	if err := netns.Set(m.nsHandle); err != nil {
		return false, fmt.Errorf("failed to enter network namespace: %w", err)
	}
	defer func() {
		if err := netns.Set(origNS); err != nil {
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
