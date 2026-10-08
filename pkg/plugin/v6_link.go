// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/claymore666/dhcp-golib/proto"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/dhcp"
	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// libnetwork sets disable_ipv6=1 on a sandbox interface whose endpoint has no AddressIPv6 (#868). Measured on the CI
// engine: while it is set there is no link-local, no router solicitation and no DHCPv6 exchange; cleared, an INFORM6
// carries the server's DNS. Where the endpoint got an address the flag is already 0 and this only reads.
const ipv6DisableSysctlDir = "/proc/sys/net/ipv6/conf"

// procSysMount is the sysctl tree's mount point, read-only in the managed-plugin rootfs (#247, #868).
const procSysMount = "/proc/sys"

// makeProcSysWritable remounts /proc/sys read-write in a private mount namespace of the calling thread (#868). It
// makes the namespace private recursively first, or the remount propagates to the host. Best effort: on a
// --privileged runtime the remount fails and /proc/sys is already writable, so only the disable_ipv6 write decides;
// a failed unshare or MS_PRIVATE skips the remount, never the write. The caller holds its OS thread and restores it.
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

// procSysPrepIsFatal is false: no makeProcSysWritable failure may stop the disable_ipv6 write (#868).
func procSysPrepIsFatal(error) bool { return false }

// ipv6DisablePath is the disable_ipv6 sysctl for iface under dir, a per-netns path; dir is a test seam.
func ipv6DisablePath(dir, iface string) string {
	return filepath.Join(dir, iface, "disable_ipv6")
}

// clearDisableIPv6 turns IPv6 on at path and reports whether it had to write.
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

// prepareV6LinkUnder runs the disable_ipv6 clear, the guard and the purge in that order under dir, so a test drives
// the order (#911). A disable_ipv6 failure returns the zero guard result, since a guard on a link with IPv6 off reads
// back healthy; a zero linkIndex skips the purge, which only happens before Start.
func prepareV6LinkUnder(dir, iface string, linkIndex int) (bool, dhcp.RouterAdvertGuardResult, error) {
	var noGuard dhcp.RouterAdvertGuardResult
	changed, err := clearDisableIPv6(ipv6DisablePath(dir, iface))
	if err != nil {
		return changed, noGuard, err
	}

	guard := dhcp.ApplyRouterAdvertGuard(dir, iface)
	if linkIndex == 0 {
		return changed, guard, nil
	}

	// The purge follows the guard: at accept_ra=1 the next advertisement would put the routes straight back (#821).
	failed, perr := purgeRouterAdvertRoutes(linkIndex)
	guard.Failures += failed
	if perr != nil {
		guard.Err = joinGuardErrors(guard.Err, perr)
	}
	return changed, guard, nil
}

// v6LinkAttempts is #1050's openLinkAttempts, for the same engine: a
// move plus a rename, and one more where the container named its
// interface, so four attempts survive three renames.
const v6LinkAttempts = 4

// errV6LinkNameUnstable ends a link whose name never held still between
// the resolve and the write (#1065).
var errV6LinkNameUnstable = errors.New("the container link kept being renamed while IPv6 was prepared on it")

// v6LinkNameByIndex and v6LinkIndexByName read the calling thread's
// network namespace: the package-level netlink handle opens its socket
// on that thread, and /proc/net would answer for the leader's (#1065).
var (
	v6LinkNameByIndex = func(index int) (string, error) {
		l, err := nlLinkByIndexCurNS(index)
		if err != nil {
			return "", err
		}
		return l.Attrs().Name, nil
	}
	v6LinkIndexByName = func(name string) (int, error) {
		l, err := nlLinkByName(name)
		if err != nil {
			return 0, err
		}
		return l.Attrs().Index, nil
	}
)

// prepareV6LinkOnLink runs prepareV6LinkUnder on the name the link at
// index has at that instant, with #1050's retry rules for a rename.
func prepareV6LinkOnLink(dir, located string, index int) (bool, dhcp.RouterAdvertGuardResult, error) {
	if index <= 0 {
		return prepareV6LinkUnder(dir, located, index)
	}
	current := func(fallback string) string {
		if name, err := v6LinkNameByIndex(index); err == nil {
			return name
		}
		return fallback
	}

	var noGuard dhcp.RouterAdvertGuardResult
	carried := false
	name := current(located)
	for attempt := 1; ; attempt++ {
		changed, guard, err := prepareV6LinkUnder(dir, name, index)
		if err != nil || guard.Failures > 0 {
			carried = carried || changed
			if attempt >= v6LinkAttempts {
				return carried, guard, err
			}
			next := current(name)
			if next == name {
				return carried, guard, err
			}
			name = next
			continue
		}

		owner, oerr := v6LinkIndexByName(name)
		if oerr != nil || owner == index {
			return carried || changed, guard, nil
		}
		// The sysctls just written belong to link owner, which took the
		// name before the write reached it; ours is retried under its
		// own name, and a link that is gone ends here (#1050, #1065).
		next, rerr := v6LinkNameByIndex(index)
		if rerr != nil {
			return carried, noGuard, rerr
		}
		if attempt >= v6LinkAttempts {
			return carried, noGuard, fmt.Errorf("%w: index %d", errV6LinkNameUnstable, index)
		}
		name = next
	}
}

// prepareIPv6Link enables IPv6, writes the Router-Advertisement guard and purges RA routes on the container link, in
// that order, in one sandbox entry, before the client opens (#868, #911, #821). The link-enable failure has its own
// counter, ipv6_link_enable_failures; the guard and the purge share router_advert_guard_failures. A failed switch
// back keeps the thread locked, as in pkg/dhcp.inNetNS.
func (m *dhcpManager) prepareIPv6Link() (bool, dhcp.RouterAdvertGuardResult, error) {
	// m.ctrLink is nil until locateContainerLink runs, and Attrs() on a nil Link panics.
	var noGuard dhcp.RouterAdvertGuardResult
	if m.ctrLink == nil {
		return false, noGuard, fmt.Errorf("container link not located yet")
	}
	// IsOpen catches a handle Stop closed, not the zero value.
	if !m.nsHandle.IsOpen() {
		return false, noGuard, fmt.Errorf("sandbox network namespace handle is closed")
	}
	located, index := m.ctrLink.Attrs().Name, m.ctrLink.Attrs().Index
	hw := m.ctrLink.Attrs().HardwareAddr
	iid, iidErr := m.opts.ipv6IID()
	purgeEUI64 := iidErr == nil && iid == proto.IIDModeStablePrivacy

	var (
		changed bool
		guard   dhcp.RouterAdvertGuardResult
		err     error
	)
	if eerr := v6EnterSandbox(m, func(dir string) {
		changed, guard, err = prepareV6LinkOnLink(dir, located, index)
		if err != nil || index <= 0 || !purgeEUI64 {
			return
		}
		// After the guard, as the route purge: autoconf=0 keeps an address the kernel already formed (#1268).
		failed, perr := purgeKernelEUI64Addrs(index, hw)
		guard.Failures += failed
		if perr != nil {
			guard.Err = joinGuardErrors(guard.Err, perr)
		}
	}); eerr != nil {
		return false, noGuard, eerr
	}
	return changed, guard, err
}

// v6EnterSandbox runs work inside the sandbox's network namespace with
// /proc/sys writable, handing it the sysctl directory; a var so a unit
// test can run work against a temp tree without CAP_SYS_ADMIN (#1065).
var v6EnterSandbox = (*dhcpManager).enterV6Sandbox

func (m *dhcpManager) enterV6Sandbox(work func(dir string)) error {
	return runInSandboxWritable(m.nsHandle, m.logFields(true), work)
}

// runInSandboxWritable runs work in namespace ns on a locked thread with /proc/sys writable, restoring both namespaces (#821).
func runInSandboxWritable(ns netns.NsHandle, fields log.Fields, work func(dir string)) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNet, err := netns.Get()
	if err != nil {
		return fmt.Errorf("failed to open current network namespace: %w", err)
	}
	defer func() {
		if err := origNet.Close(); err != nil {
			log.WithError(err).Debug("origNet close failed")
		}
	}()

	// This thread's mount namespace: /proc/self/ns/mnt names the leader's.
	origMnt, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/mnt", unix.Gettid()))
	if err != nil {
		return fmt.Errorf("open self mnt ns: %w", err)
	}
	defer origMnt.Close()

	if err := makeProcSysWritable(); err != nil {
		if procSysPrepIsFatal(err) {
			return err
		}
		// Not a verdict (#868): the write below reports; logged so a host where this always fails is recognisable.
		log.WithError(err).WithFields(fields).
			Debug("Could not make /proc/sys writable; attempting the write anyway")
	}
	defer func() {
		if err := unix.Setns(int(origMnt.Fd()), unix.CLONE_NEWNS); err != nil {
			// A second Lock keeps a thread left in our private mount namespace out of Go's pool.
			log.WithError(err).Error("Failed to restore original mount namespace; pinning thread for kill")
			runtime.LockOSThread()
		}
	}()

	if err := netns.Set(ns); err != nil {
		return fmt.Errorf("failed to enter network namespace: %w", err)
	}
	defer func() {
		if err := netns.Set(origNet); err != nil {
			log.WithError(err).Error("Failed to restore original netns; pinning thread for kill")
			runtime.LockOSThread()
		}
	}()

	work(ipv6DisableSysctlDir)
	return nil
}

// v6SandboxDefaultKnobs are the namespace defaults Join writes (#1145); never keep_addr_on_down, which is per-link state,
// and never all/, which an existing link ignores (#821). Values come from the guard's contract.
var v6SandboxDefaultKnobs = []string{"accept_ra", "autoconf"}

// v6SandboxDefaultsResult is what one write of the sandbox defaults did.
type v6SandboxDefaultsResult struct {
	PriorAcceptRA string
	Failures      int
	Err           error
	// SandboxNotBuilt marks an open that found no namespace file yet: not a failure, not counted (#1145).
	SandboxNotBuilt bool
}

// v6SandboxDefaultsWriter is a var so a Join test runs without a namespace (#1145).
var v6SandboxDefaultsWriter = writeV6SandboxDefaults

// writeV6SandboxDefaultsUnder writes default/accept_ra and default/autoconf under dir and reads each back. Nothing is
// undone at Leave: the defaults die with the sandbox, and restoring 1 would unguard a second endpoint in it (#1145).
func writeV6SandboxDefaultsUnder(dir string) v6SandboxDefaultsResult {
	var (
		res  v6SandboxDefaultsResult
		errs []error
	)
	contract := dhcp.RouterAdvertGuardContract()
	for _, knob := range v6SandboxDefaultKnobs {
		p := filepath.Join(dir, "default", knob)
		want, ok := contract[knob]
		if !ok {
			res.Failures++
			errs = append(errs, fmt.Errorf("the guard contract has no value for %v", knob))
			continue
		}
		if prior, err := os.ReadFile(p); err == nil && knob == "accept_ra" {
			res.PriorAcceptRA = strings.TrimSpace(string(prior))
			log.WithField("accept_ra", res.PriorAcceptRA).Debug("[Join] Sandbox default accept_ra before the write")
		}
		werr := os.WriteFile(p, []byte(want+"\n"), 0o644)
		got, rerr := os.ReadFile(p)
		switch {
		case rerr != nil:
			res.Failures++
			errs = append(errs, fmt.Errorf("write %v=%v: %v; read back: %w", p, want, werr, rerr))
		case strings.TrimSpace(string(got)) != want:
			res.Failures++
			errs = append(errs, fmt.Errorf("%v reads %q after a write of %q (write: %v)",
				p, strings.TrimSpace(string(got)), want, werr))
		}
	}
	res.Err = errors.Join(errs...)
	return res
}

// writeV6SandboxDefaults enters the sandbox at sandboxKey and writes its defaults. Never an error to the caller: a
// failure is a counted step, not a Join verdict (#1145).
func writeV6SandboxDefaults(sandboxKey string) v6SandboxDefaultsResult {
	ns, err := openSandboxNetNSByKeyIn(sandboxNetnsDirs, sandboxKey)
	if errors.Is(err, errSandboxKeyPlaceholder) {
		// Only a placeholder is awaited; an absent entry returns at once above. The context outlasts the bound by one
		// interval, so the await ends on its own bound (#1185).
		ctx, cancel := context.WithTimeout(context.Background(), (placeholderBoundIntervals+1)*pollTime)
		defer cancel()
		ns, err = awaitSandboxNetNSByKeyIn(ctx, sandboxNetnsDirs, sandboxKey, pollTime)
	}
	if err != nil {
		res := v6SandboxDefaultsResult{Err: fmt.Errorf("open the sandbox: %w", err)}
		if errors.Is(err, fs.ErrNotExist) {
			res.SandboxNotBuilt = true
			return res
		}
		res.Failures = 1
		return res
	}
	defer closeNsHandle(ns)

	var res v6SandboxDefaultsResult
	if eerr := runInSandboxWritable(ns, log.Fields{"sandbox": sandboxKey}, func(dir string) {
		res = writeV6SandboxDefaultsUnder(dir)
	}); eerr != nil {
		return v6SandboxDefaultsResult{Failures: 1, Err: fmt.Errorf("enter the sandbox: %w", eerr)}
	}
	return res
}

// guardSandboxDefaults writes the sandbox defaults at Join and counts what failed (#1145).
func (p *Plugin) guardSandboxDefaults(r JoinRequest) {
	res := v6SandboxDefaultsWriter(r.SandboxKey)
	if res.SandboxNotBuilt {
		// Engines 26 and 27 call Join before SetKey creates the namespace; the guard cannot run there (#1145).
		log.WithError(res.Err).WithFields(log.Fields{
			"network":  shortID(r.NetworkID),
			"endpoint": shortID(r.EndpointID),
			"sandbox":  r.SandboxKey,
		}).Debug("[Join] No sandbox namespace yet: this engine builds it after Join, so the Router Advertisement defaults are not written")
		return
	}
	if res.Failures == 0 {
		return
	}
	p.routerAdvertGuardFailures.Add(int32(res.Failures))
	log.WithError(res.Err).WithFields(log.Fields{
		"network":  shortID(r.NetworkID),
		"endpoint": shortID(r.EndpointID),
		"sandbox":  r.SandboxKey,
		"step":     "sandbox_default",
	}).Warn("The Router Advertisement defaults did not take in the sandbox; " +
		"a link that arrives in it before the guard can carry a kernel-installed IPv6 default route")
}

// joinGuardErrors keeps both guard failures in one error, since they share one counter.
func joinGuardErrors(a, b error) error {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return fmt.Errorf("%v; %w", a, b)
}

// purgeRouterAdvertRoutes deletes the RTPROT_RA routes on the link at linkIndex and returns the failure count (#821).
// accept_ra=0 stops the next advertisement but keeps installed routes for their lifetime, up to 65535 s (RFC 4861
// section 4.2), and the link comes up at accept_ra=1 before the guard runs. Addresses formed from an RA are #818's.
func purgeRouterAdvertRoutes(linkIndex int) (int, error) {
	routes, err := util.DumpResult(nlRouteListFiltered(unix.AF_INET6, &netlink.Route{
		LinkIndex: linkIndex,
		Protocol:  unix.RTPROT_RA,
	}, netlink.RT_FILTER_OIF|netlink.RT_FILTER_PROTOCOL))
	if err != nil {
		return 1, fmt.Errorf("list kernel router-advertisement routes: %w", err)
	}

	failed := 0
	var firstErr error
	for i := range routes {
		if err := nlRouteDel(&routes[i]); err != nil {
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("delete kernel router-advertisement route %v: %w",
					describeRoute(routes[i]), err)
			}
			// Keep going: the default route is not reliably first in the list.
			continue
		}
		log.WithField("route", describeRoute(routes[i])).
			Info("Removed a route the kernel installed from a Router Advertisement before the guard took")
	}
	return failed, firstErr
}

// An advertisement between the engine's link-up and the guard lets the kernel form the modified EUI-64 address, the one
// ipv6_iid=stable-privacy exists to avoid; the sandbox defaults cannot cover engines 24 to 27 (#1145). Measured on kernel
// 6.12.111: autoconf=0 and accept_ra=0 keep that address for its valid lifetime, a delete removes it for good, and a
// per-link autoconf=0 written before the netns move reads 1 after it (#1268).

// purgeKernelEUI64Addrs deletes the link's non-link-local addresses whose identifier is hw's modified EUI-64.
func purgeKernelEUI64Addrs(linkIndex int, hw net.HardwareAddr) (int, error) {
	iid, err := proto.ModifiedEUI64(hw)
	if err != nil {
		// A link address that is no EUI-48 or EUI-64 gives the kernel no MAC to form an identifier from.
		return 0, nil
	}
	link := &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: linkIndex}}
	addrs, err := util.DumpResult(nlAddrList(link, unix.AF_INET6))
	if err != nil {
		return 1, fmt.Errorf("list the link's IPv6 addresses: %w", err)
	}

	failed := 0
	var firstErr error
	for i := range addrs {
		ip := addrs[i].IP.To16()
		if ip == nil || ip.IsLinkLocalUnicast() || !bytes.Equal(ip[8:], iid[:]) {
			continue
		}
		// EADDRNOTAVAIL: the kernel expired it between the list and the delete.
		if err := nlAddrDelCurNS(link, &addrs[i]); err != nil && !errors.Is(err, unix.EADDRNOTAVAIL) {
			failed++
			if firstErr == nil {
				firstErr = fmt.Errorf("delete the kernel-formed EUI-64 address %v: %w", addrs[i].IPNet, err)
			}
			continue
		}
		log.WithField("address", addrs[i].IPNet.String()).
			Info("Removed the MAC-derived IPv6 address the kernel formed from a Router Advertisement before the guard took, " +
				"since this network sets ipv6_iid=stable-privacy")
	}
	return failed, firstErr
}

// describeRoute renders one route as "dest via gw" for a log field.
func describeRoute(r netlink.Route) string {
	dst := "default"
	if r.Dst != nil {
		dst = r.Dst.String()
	}
	if r.Gw == nil {
		return dst
	}
	return dst + " via " + r.Gw.String()
}

// ensureIPv6Enabled prepares the link for DHCPv6 and treats every failure as degraded, keeping the IPv4 lease (#868).
func (m *dhcpManager) ensureIPv6Enabled() {
	changed, guard, err := m.prepareIPv6Link()
	if err != nil {
		// A nil plugin is a unit test without one.
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
	if guard.Failures > 0 {
		if m.plugin != nil {
			m.plugin.routerAdvertGuardFailures.Add(int32(guard.Failures))
		}
		// Loud: the kernel RA route may outlive this for a Router Lifetime of up to 65535 s (RFC 4861 section 4.2).
		log.WithError(guard.Err).WithFields(m.logFields(true)).
			WithField("failed_steps", guard.Failures).
			Warn("The Router Advertisement guard did not take on the container link; " +
				"the container may carry a second IPv6 default route or an address the kernel formed")
	}
}
