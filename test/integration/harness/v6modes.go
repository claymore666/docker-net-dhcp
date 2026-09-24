// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/claymore666/docker-net-dhcp/v2/pkg/util"
)

// A per-test dual-stack segment whose IPv6 mode the test chooses; the signature and verdict live in v6signature.go.
//
// Own bridge and prefix, one mode at a time (#815, #816, #820, #821): RA flags are a property of the broadcast
// domain, so two RA senders on one segment make a test measure arrival order. The suite-static segments already run
// an --enable-ra dnsmasq. The segment is dual-stack because the plugin has no IPv6-only mode.
const (
	// V6BridgeName is this fixture's own bridge, on the dh-itest-* prefix the orphan cleanup keys on.
	V6BridgeName = "dh-itest-br6"

	// V6BridgePortName is a dummy port enslaved to the bridge so the segment has carrier (#942).
	//
	// Measured on hosted run 34595592138, kernel 6.17.0-1022-azure: a portless bridge has no carrier,
	// netif_carrier_off stops its TX queues, and dnsmasq logs RTR-ADVERT while the wire stays empty. With one enslaved
	// link the same capture took 14 frames including two RAs. The mode is asserted before any container joins.
	V6BridgePortName = "dh-itest-br6p"

	// 192.168.99/100/101/102/123 belong to other fixtures.
	V6BridgeAddr = "192.168.103.1/24"
	V6PoolStart  = "192.168.103.10"
	V6PoolEnd    = "192.168.103.99"
	V6SubnetCIDR = "192.168.103.0/24"

	// fd00:6470:6863::/64 is the macvlan fixture and fd00:6470:6864::/64 the bridge fixture.
	V6BridgeAddrV6 = "fd00:6470:6865::1/64"
	V6Prefix       = "fd00:6470:6865::"
	V6PoolStartV6  = "fd00:6470:6865::10"
	V6PoolEndV6    = "fd00:6470:6865::99"
	V6SubnetV6CIDR = "fd00:6470:6865::/64"

	// The search domain is #815's second defect: carried on every DHCPv6 reply and dropped by the plugin, measured
	// reaching the client as new_dhcp6_domain_search on INFORM6 and BOUND6. V6StaticOnlyAddrV6 is not the pool start
	// because dnsmasq logs a static-only range's address, which would make managed-exhausted's signature managed's (#989).
	V6StaticOnlyAddrV6 = "fd00:6470:6865::ff"

	V6DNSServer    = "fd00:6470:6865::53"
	V6SearchDomain = "v6mode.example"

	V6DHCPOnlyDNSServer    = "fd00:6470:6865::54"
	V6DHCPOnlySearchDomain = "dhcpv6.v6mode.example"

	// raLogToken is printed verbatim as `RTR-ADVERT(%s) %s` (radv.c:758), not through gettext.
	raLogToken = "RTR-ADVERT("
)

// V6FixtureT is the slice of *testing.T the fixture uses, so the drift matrix can observe its failures.
type V6FixtureT interface {
	Helper()
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(f func())
}

// rangeArgs is the dnsmasq spelling of the mode; "ra-stateless" and "ra-only" are dnsmasq vocabulary.
//
// A bare number as the third v6 --dhcp-range field is parsed as a prefix length, and dnsmasq refuses to start with
// "prefix length must be exactly 64 for RA subnets" (measured 2026-09-05, dnsmasq 2.91, #911). LeaseTime is "2m".
func (m V6Mode) rangeArgs() []string {
	switch m {
	case V6Managed:
		return []string{
			"--dhcp-range=" + V6PoolStartV6 + "," + V6PoolEndV6 + "," + LeaseTime,
			"--enable-ra",
		}
	case V6Stateless:
		return []string{
			"--dhcp-range=" + V6Prefix + ",ra-stateless," + LeaseTime,
			"--enable-ra",
		}
	case V6SLAAC:
		return []string{
			"--dhcp-range=" + V6Prefix + ",ra-only," + LeaseTime,
			"--enable-ra",
		}
	case V6NoRA:
		// No --enable-ra, plus the ignore: without it dnsmasq answers the Solicit the plugin sends after router discovery
		// gives up. Measured on the lane 2026-09-06: the client solicited at 11 s and got fd00:6470:6865::61, so the
		// dhcpv6_no_router_advert arm never reached the absence path (#911).
		return []string{
			"--dhcp-range=" + V6PoolStartV6 + "," + V6PoolEndV6 + "," + LeaseTime,
			"--dhcp-ignore=tag:dhcpv6",
		}
	case V6ManagedSilent:
		// `dhcpv6` is a tag dnsmasq sets on every DHCPv6 request, so the ignore reaches only the v6 half and the v4 pool keeps
		// serving. Measured 2026-08-27 (dnsmasq 2.92rel2, dhcpcd 10.3.2) and 2026-09-05 (2.91): DHCPSOLICIT ignored, no
		// IA_NA, v4 DHCPACK still sent. A `set:` tag on the v6 range with its ignore leaves dnsmasq serving the address (#868).
		return []string{
			"--dhcp-range=" + V6PoolStartV6 + "," + V6PoolEndV6 + "," + LeaseTime,
			"--enable-ra",
			"--dhcp-ignore=tag:dhcpv6",
		}
	case V6ManagedExhausted:
		// Static-only v6 range (#989): `static` parses to CONTEXT_STATIC|CONTEXT_DHCP (option.c:3822), address_allocate
		// skips static contexts (dhcp6.c:497), and a Solicit with no --dhcp-host gets an Advertise with status NoAddrsAvail
		// (rfc3315.c:805-819). --enable-ra sets M and O for a context without CONTEXT_RA (radv.c:368-377). On the wire this is
		// an exhausted pool, without a second client holding the only address.
		return []string{
			"--dhcp-range=" + V6StaticOnlyAddrV6 + ",static," + LeaseTime,
			"--enable-ra",
		}
	case V6AutoFallback:
		// One range carrying both halves (#818): the address fields set CONTEXT_DHCP and `slaac` adds CONTEXT_RA
		// (option.c:3823-3830), so M and O come from the DHCP half (radv.c:627-644) and the A bit from the RA half
		// (radv.c:748) on one prefix. CONTEXT_RA sets doing_ra itself (dnsmasq.c:281-295), so --enable-ra is not needed.
		return []string{
			"--dhcp-range=" + V6PoolStartV6 + "," + V6PoolEndV6 + ",slaac," + LeaseTime,
			"--dhcp-ignore=tag:dhcpv6",
		}
	}
	return nil
}

// V6DeprecatedPrefixArgs is V6SLAAC's segment with its prefix advertised deprecated (RFC 4862 section 5.5.4).
//
// Argv, not a V6Mode: its signature equals V6SLAAC's, since only the lifetimes differ. dnsmasq's `deprecated` keyword
// (option.c:3919), measured 2026-09-16 on dnsmasq 2.91: the option is `03 04 40 c0 ffffffff 00000000`, radv.c:707
// zeroing the preferred lifetime after the valid lifetime is clamped (#819).
func V6DeprecatedPrefixArgs() []string {
	return []string{
		"--dhcp-range=" + V6Prefix + ",ra-only,deprecated",
		"--enable-ra",
	}
}

// V6PoolWithoutRAArgs is V6NoRA's segment with its DHCPv6 pool answering, for a harness RASender to advertise over.
//
// Argv, not a V6Mode: its startup signature equals V6NoRA's, a pool and no advertisement. dnsmasq sends no RA without
// --enable-ra or an ra-* range (radv.c), so every advertisement on the segment is the sender's (#1016).
func V6PoolWithoutRAArgs() []string {
	return []string{"--dhcp-range=" + V6PoolStartV6 + "," + V6PoolEndV6 + "," + LeaseTime}
}

// V6DHCPOnlyDNSArgs gives DHCPv6 replies their own resolver and search domain, which the advertisement does not carry.
//
// radv.c builds RDNSS and DNSSL from options whose tags match the RA context, and `dhcpv6` is set on DHCPv6 requests
// only, so the RA keeps V6DNSServer and V6SearchDomain (RFC 8106 section 5.3.1, #1016).
func V6DHCPOnlyDNSArgs() []string {
	return []string{
		"--dhcp-option=tag:dhcpv6,option6:dns-server,[" + V6DHCPOnlyDNSServer + "]",
		"--dhcp-option=tag:dhcpv6,option6:domain-search," + V6DHCPOnlySearchDomain,
	}
}

// RangeArgsFor exports rangeArgs for the drift matrix.
func RangeArgsFor(m V6Mode) []string { return m.rangeArgs() }

// V6Fixture is a per-test dual-stack segment in one V6Mode.
type V6Fixture struct {
	t    V6FixtureT
	mode V6Mode

	cmd       *exec.Cmd
	tmpDir    string
	leaseFile string
	logFile   string

	startedAt         time.Time
	evidenceStartedAt time.Time
	raCap             *RACapture

	linkUp            bool
	iptablesInstalled bool
}

// NewV6Fixture brings up a segment and returns once it is observed in the requested mode.
func NewV6Fixture(t *testing.T, mode V6Mode) *V6Fixture {
	t.Helper()
	return NewV6FixtureWithArgs(t, mode, mode.rangeArgs())
}

// NewV6FixtureWithArgs is NewV6Fixture with explicit v6 flags, and the only constructor.
func NewV6FixtureWithArgs(t V6FixtureT, name V6Mode, rangeArgs []string) *V6Fixture {
	t.Helper()
	f := newV6Fixture(t, name, rangeArgs, portUp)
	f.assertMode()
	return f
}

// NewV6FixtureWithADeadBridgePort attaches the bridge port and never brings it up, so the bridge has no carrier.
//
// A portless bridge transmits on the pool's kernel but not on hosted ubuntu-latest; with a port, carrier follows the
// port on both (#942).
func NewV6FixtureWithADeadBridgePort(t V6FixtureT, name V6Mode) *V6Fixture {
	t.Helper()
	f := newV6Fixture(t, name, name.rangeArgs(), portDown)
	f.assertMode()
	return f
}

type v6PortState bool

const (
	portUp   v6PortState = true
	portDown v6PortState = false
)

func newV6Fixture(t V6FixtureT, name V6Mode, rangeArgs []string, port v6PortState) *V6Fixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("V6Fixture needs root (got uid=%d)", os.Geteuid())
	}

	cleanupV6Links()

	f := &V6Fixture{t: t, mode: name}
	t.Cleanup(f.teardown)

	la := netlink.NewLinkAttrs()
	la.Name = V6BridgeName
	if err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: la}); err != nil {
		t.Fatalf("LinkAdd bridge %s: %v", V6BridgeName, err)
	}
	f.linkUp = true
	link, err := netlink.LinkByName(V6BridgeName)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", V6BridgeName, err)
	}

	// The kernel's 15 s STP forward delay holds the port in LEARNING and no DHCP passes.
	fdPath := filepath.Join("/sys/class/net", V6BridgeName, "bridge/forward_delay")
	if err := os.WriteFile(fdPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("disable STP forward_delay on %s: %v", V6BridgeName, err)
	}
	// Measured 2026-08-27: with DAD on, dnsmasq starts while the global address is tentative and sends its first RA about
	// nine seconds later, against about one second with the address settled; the log reads the same either way (#815).
	dadPath := filepath.Join("/proc/sys/net/ipv6/conf", V6BridgeName, "accept_dad")
	if err := os.WriteFile(dadPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("disable DAD on %s: %v", V6BridgeName, err)
	}

	// accept_dad=0 still creates the address tentative: measured 2026-08-27 in run 33096065817, one mode of four read it
	// tentative 60 ms after LinkSetUp. IFA_F_NODAD never raises the flag; the sysctl covers the kernel's link-local (#864).
	for _, cidr := range []string{V6BridgeAddr, V6BridgeAddrV6} {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", cidr, err)
		}
		addr.Flags |= unix.IFA_F_NODAD
		if err := netlink.AddrAdd(link, addr); err != nil {
			t.Fatalf("AddrAdd %s on %s: %v", cidr, V6BridgeName, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("LinkSetUp %s: %v", V6BridgeName, err)
	}
	attachV6BridgePort(t, link, port)
	awaitNoTentativeAddr(t)
	awaitBridgeCarrier(t)

	if err := installBridgeForward(V6BridgeName); err != nil {
		t.Fatalf("install FORWARD rules for %s: %v", V6BridgeName, err)
	}
	f.iptablesInstalled = true

	tmp, err := os.MkdirTemp("", "dh-itest-v6-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	f.tmpDir = tmp
	f.leaseFile = filepath.Join(tmp, "leases")
	f.logFile = filepath.Join(tmp, "dnsmasq.log")

	// The v6 signature was measured on dnsmasq 2.92rel2 (2026-08-27) and 2.91 (2026-09-05) on the same runner role.
	probe := dnsmasqVersion()
	t.Logf("v6 fixture: mode=%s bridge=%s v4=%s-%s v6=%s dnsmasq=%q",
		name, V6BridgeName, V6PoolStart, V6PoolEnd, V6SubnetV6CIDR, probe)
	// A runner on another dnsmasq silently drops renamed v6 message names from the derived exchange contract.
	if findings := DnsmasqVersionFindings(probe); len(findings) > 0 {
		t.Fatalf("v6 fixture mode=%s: %s", name, strings.Join(findings, "; "))
	}

	// The first RA arrives about a second after start (97 of 99 observations), the next 5 to 19 s later (#911).
	f.raCap = StartRACapture(t, V6BridgeName)

	f.start(rangeArgs)
	return f
}

func (f *V6Fixture) start(rangeArgs []string) {
	f.t.Helper()
	logF, err := os.Create(f.logFile)
	if err != nil {
		f.t.Fatalf("create v6 dnsmasq log: %v", err)
	}
	defer logF.Close()

	args := []string{
		"--no-daemon",
		"--conf-file=/dev/null",
		"--port=0",
		"--interface=" + V6BridgeName,
		"--bind-interfaces",
		"--except-interface=lo",
		"--dhcp-range=" + V6PoolStart + "," + V6PoolEnd + "," + LeaseTime,
	}
	args = append(args, rangeArgs...)
	args = append(args,
		"--dhcp-option=option6:dns-server,["+V6DNSServer+"]",
		"--dhcp-option=option6:domain-search,"+V6SearchDomain,
		"--dhcp-leasefile="+f.leaseFile,
		"--dhcp-no-override",
		"--dhcp-broadcast",
		"--log-dhcp",
		"--log-facility=-",
	)

	f.cmd = withCLocale(exec.Command("/usr/sbin/dnsmasq", args...))
	f.cmd.Stdout = logF
	f.cmd.Stderr = logF
	f.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	f.startedAt = time.Now()
	if err := f.cmd.Start(); err != nil {
		f.t.Fatalf("start v6 dnsmasq: %v", err)
	}

	f.waitReady()
}

// dnsmasqVersion is the first line of `dnsmasq --version`, or why it could not be read.
func dnsmasqVersion() string {
	out, err := withCLocale(exec.Command("/usr/sbin/dnsmasq", "--version")).Output()
	if err != nil {
		return fmt.Sprintf("(could not read dnsmasq --version: %v)", err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

// Keyed on the pool's start address for the locale reason in V6Signature.
func (f *V6Fixture) waitReady() {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(f.readLog(), V6PoolStart) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.t.Fatalf("v6 fixture dnsmasq (mode=%s) did not become ready; log:\n%s",
		f.mode, f.readLog())
}

// A mode that expects no RA always spends the full window.
func (f *V6Fixture) evidence() V6Evidence {
	f.t.Helper()
	want := f.mode.Signature()
	budget := RABudget()
	if !want.RA {
		budget = V6NoRAWindow()
	}
	f.evidenceStartedAt = time.Now()
	deadline := f.evidenceStartedAt.Add(budget)
	for {
		log := f.readLog()
		ev := V6Evidence{
			PoolLogged: strings.Contains(log, V6PoolStartV6),
			RALogged:   strings.Contains(log, raLogToken),
			Frames:     f.raCap.FramesAfter(f.startedAt),
		}
		if V6EvidenceSettled(ev) {
			return ev
		}
		if time.Now().After(deadline) {
			return ev
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// assertOwnsTheSegment asserts this bridge carries only this fixture's advertisements (#821, run 35141032546).
func (f *V6Fixture) assertOwnsTheSegment() {
	f.t.Helper()

	var findings []string

	var foreign []RAFrame
	for _, fr := range f.raCap.Frames() {
		if fr.At.Before(f.startedAt) {
			foreign = append(foreign, fr)
		}
	}
	if len(foreign) > 0 {
		findings = append(findings, fmt.Sprintf(
			"%d advertisement(s) reached this bridge BEFORE this fixture's dnsmasq started, "+
				"so another router is live on %s and this segment's mode is not this "+
				"fixture's to decide:\n%s",
			len(foreign), V6BridgeName, formatRAFrames(foreign)))
	}

	mine := f.raCap.FramesAfter(f.startedAt)
	srcs := make(map[string]int)
	var order []string
	for _, fr := range mine {
		k := fr.SourceMAC.String()
		if _, seen := srcs[k]; !seen {
			order = append(order, k)
		}
		srcs[k]++
	}
	// Reported, not failed: a bridge takes its lowest port MAC, so attaching a container's veth changes the RA source
	// address mid-fixture. Telling that from a second router needs the port timeline (#821).
	if len(order) > 1 {
		var parts []string
		for _, k := range order {
			parts = append(parts, fmt.Sprintf("%s x%d", k, srcs[k]))
		}
		f.t.Logf("v6 fixture mode=%s: advertisements from %d source address(es) on %s (%s). "+
			"Expected when a port joins the bridge and it adopts a new MAC; a second "+
			"ROUTER would also look like this.",
			f.mode, len(order), V6BridgeName, strings.Join(parts, ", "))
	}

	if len(findings) > 0 {
		f.t.Fatalf("v6 fixture mode=%s does not own its segment: %s\non the wire: %s\nlog:\n%s",
			f.mode, strings.Join(findings, "; "), f.raCap.SeenTally(), f.readLog())
	}
}

func (f *V6Fixture) assertMode() {
	f.t.Helper()
	ev := f.evidence()
	f.assertOwnsTheSegment()
	if findings := V6ModeFindings(f.mode, ev); len(findings) > 0 {
		f.t.Fatalf("v6 fixture mode=%s: %s\ncaptured %d router advertisement(s):\n%s\non the wire: %s\nlog:\n%s",
			f.mode, strings.Join(findings, "; "), len(ev.Frames),
			formatRAFrames(ev.Frames), f.raCap.SeenTally(), f.readLog())
	}
}

func formatRAFrames(frames []RAFrame) string {
	if len(frames) == 0 {
		return "  (none)"
	}
	var b strings.Builder
	for _, fr := range frames {
		b.WriteString("  " + fr.String() + "\n")
	}
	return b.String()
}

// ModeFindings is assertMode's verdict without the failure.
func (f *V6Fixture) ModeFindings() []string {
	return V6ModeFindings(f.mode, f.evidence())
}

// Mode is the mode this segment was brought up in.
func (f *V6Fixture) Mode() V6Mode { return f.mode }

// Bridge is the bridge name to hand the driver as `bridge=`.
func (f *V6Fixture) Bridge() string { return V6BridgeName }

// StartedAt is when the server process started.
func (f *V6Fixture) StartedAt() time.Time { return f.startedAt }

// EvidenceStartedAt is when assertMode's budget began, after the readiness wait.
func (f *V6Fixture) EvidenceStartedAt() time.Time { return f.evidenceStartedAt }

// RACapture is the segment's router-advertisement capture.
func (f *V6Fixture) RACapture() *RACapture { return f.raCap }

// AwaitRAAfter fails the test unless this server logged an RA and the wire carried one after since.
//
// The syslog stamp has one-second resolution and a locale-dependent format, so only the wire column can carry
// "after since" (#821).
func (f *V6Fixture) AwaitRAAfter(since time.Time, budget time.Duration) []RAFrame {
	f.t.Helper()
	deadline := time.Now().Add(budget)
	for {
		frames := f.raCap.FramesAfter(since)
		logged := strings.Contains(f.readLog(), raLogToken)
		if logged && len(frames) > 0 {
			return frames
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("v6 fixture mode=%s: no router advertisement after %s within %v — "+
				"the wire, which is the column that answers \"after\", has %d frame(s) after "+
				"that instant; the server log has an %s line: %v, which says only that this "+
				"server advertises at some point and never that it did so after %s. "+
				"Every v6 assertion downstream of this rests on an advertisement the client "+
				"could actually have heard; log:\n%s",
				f.mode, since.Format("15:04:05.000"), budget, len(frames), raLogToken, logged,
				since.Format("15:04:05.000"), f.readLog())
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertNoRAWithin fails the test if an RA is seen within window, which callers take from V6NoRAWindow.
func (f *V6Fixture) AssertNoRAWithin(window time.Duration) {
	f.t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if frames := f.raCap.Frames(); len(frames) > 0 {
			f.t.Fatalf("v6 fixture mode=%s: a router advertisement was captured on a segment "+
				"that must not advertise:\n%s", f.mode, formatRAFrames(frames))
		}
		if strings.Contains(f.readLog(), raLogToken) {
			f.t.Fatalf("v6 fixture mode=%s: the server logged %s on a segment that must not "+
				"advertise; log:\n%s", f.mode, raLogToken, f.readLog())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertExchange fails the test unless the server's log shows the exchange this mode is defined by.
func (f *V6Fixture) AssertExchange(budget time.Duration) {
	f.t.Helper()
	deadline := time.Now().Add(budget)
	for {
		findings := V6ExchangeFindings(f.mode, f.readLog())
		if len(findings) == 0 {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("v6 fixture mode=%s: the exchange this mode is defined by is not in the "+
				"server's log after %v: %s\nlog:\n%s",
				f.mode, budget, strings.Join(findings, "; "), f.readLog())
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ExchangeFindings is AssertExchange's verdict without the failure.
func (f *V6Fixture) ExchangeFindings() []string {
	return V6ExchangeFindings(f.mode, f.readLog())
}

// CountLogLines counts server log lines containing all of substrings.
func (f *V6Fixture) CountLogLines(substrings ...string) int {
	return countLinesWithAll(f.readLog(), substrings)
}

// AwaitIgnoredSolicit blocks until the server logs a refused DHCPv6 request, and fails the test if none arrives.
//
// V6ManagedSilent's startup signature equals V6Managed's (measured 2026-09-05, #868). dnsmasq writes "ignored" through
// gettext (rfc3315.c:652; de.po "ignoriert"), so the needle depends on withCLocale's LC_ALL=C.
func (f *V6Fixture) AwaitIgnoredSolicit(budget time.Duration) {
	f.t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if f.CountLogLines("DHCPSOLICIT", "ignored") > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	f.t.Fatalf("v6 fixture mode=%s: no DHCPv6 solicit was refused within %v — "+
		"either no client asked, or the server answered and this segment is not "+
		"silent at all; log:\n%s", f.mode, budget, f.readLog())
}

func (f *V6Fixture) readLog() string {
	data, err := os.ReadFile(f.logFile)
	if err != nil {
		return fmt.Sprintf("(could not read v6 fixture log: %v)", err)
	}
	return string(data)
}

// DumpLogs writes the server log and the captured frames for failure diagnostics.
func (f *V6Fixture) DumpLogs(write func(string)) {
	write(fmt.Sprintf("--- v6 fixture dnsmasq log (mode=%s) ---\n%s", f.mode, f.readLog()))
	if f.raCap != nil {
		f.raCap.Dump(write)
	}
}

// Reannounce restarts dnsmasq with new range arguments on the same bridge and returns once it serves (#821).
//
// The restart truncates the log file.
func (f *V6Fixture) Reannounce(rangeArgs []string) {
	f.t.Helper()
	f.stopServer()
	f.start(rangeArgs)
}

// stopServer ends the dnsmasq process and waits for it, so Reannounce never leaves two servers on one bridge.
func (f *V6Fixture) stopServer() {
	if f.cmd == nil || f.cmd.Process == nil {
		return
	}
	_ = f.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = f.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = f.cmd.Process.Kill()
		<-done
	}
	f.cmd = nil
}

func (f *V6Fixture) teardown() {
	if f.raCap != nil {
		f.raCap.Stop()
	}
	f.stopServer()
	if f.tmpDir != "" {
		_ = os.RemoveAll(f.tmpDir)
	}
	if f.iptablesInstalled {
		removeBridgeForward(V6BridgeName)
		f.iptablesInstalled = false
	}
	cleanupV6Links()
	f.linkUp = false
}

// tentativeBudget is shorter than any real DAD (up to 1 s delay plus a 1 s RetransTimer) and longer than addrconf
// clearing the flag with DAD off. Measured 2026-08-27, 200 bring-ups per arm, link-local readings (#815):
// accept_dad=0 no NODAD no wait 15/200 tentative; NODAD no wait 11/200; NODAD and wait 0/200, longest 2 ms;
// accept_dad=1 NODAD wait 200/200 red at the budget. At 40 runs NODAD alone read 0/40 against 6/40.
const tentativeBudget = 250 * time.Millisecond

// awaitNoTentativeAddr checks every v6 address on the link, including the kernel's link-local that RAs are sent from.
func awaitNoTentativeAddr(t V6FixtureT) {
	t.Helper()
	link, err := netlink.LinkByName(V6BridgeName)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", V6BridgeName, err)
	}
	deadline := time.Now().Add(tentativeBudget)
	for {
		addrs, err := util.DumpResult(netlink.AddrList(link, netlink.FAMILY_V6))
		if err != nil {
			t.Fatalf("AddrList %s: %v", V6BridgeName, err)
		}
		var stuck []string
		for _, a := range addrs {
			if a.Flags&unix.IFA_F_TENTATIVE != 0 {
				stuck = append(stuck, a.IPNet.String())
			}
		}
		if len(stuck) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still tentative on %s after %s, with accept_dad=0 "+
				"and IFA_F_NODAD set — router advertisements will be "+
				"delayed and the mode check will misreport. This budget is "+
				"too short for a real DAD probe to clear, so DAD is on: a "+
				"configured address here means NODAD was not honoured, the "+
				"link-local means the sysctl did not take",
				strings.Join(stuck, ", "), V6BridgeName, tentativeBudget)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// cleanupV6Links removes the fixture's links on teardown and at setup.
func cleanupV6Links() {
	// The port first: an enslaved dummy outlives its bridge and the next LinkAdd fails on the name.
	for _, name := range []string{V6BridgePortName, V6BridgeName} {
		if link, err := netlink.LinkByName(name); err == nil {
			_ = netlink.LinkDel(link)
		}
	}
}

// attachV6BridgePort gives the bridge a dummy port with IPv6 off, which adds no second host to the segment.
func attachV6BridgePort(t V6FixtureT, bridge netlink.Link, state v6PortState) {
	t.Helper()
	la := netlink.NewLinkAttrs()
	la.Name = V6BridgePortName
	if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: la}); err != nil {
		t.Fatalf("LinkAdd %s: %v", V6BridgePortName, err)
	}
	port, err := netlink.LinkByName(V6BridgePortName)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", V6BridgePortName, err)
	}
	disable := filepath.Join("/proc/sys/net/ipv6/conf", V6BridgePortName, "disable_ipv6")
	if err := os.WriteFile(disable, []byte("1"), 0o644); err != nil {
		t.Fatalf("disable IPv6 on %s: %v", V6BridgePortName, err)
	}
	if err := netlink.LinkSetMaster(port, bridge); err != nil {
		t.Fatalf("enslave %s to %s: %v", V6BridgePortName, V6BridgeName, err)
	}
	if state == portDown {
		return
	}
	if err := netlink.LinkSetUp(port); err != nil {
		t.Fatalf("LinkSetUp %s: %v", V6BridgePortName, err)
	}
}

// carrierBudget bounds awaitBridgeCarrier; carrier follows the port through the linkwatch work queue.
const carrierBudget = 2 * time.Second

// awaitBridgeCarrier refuses a segment whose transmit queues are stopped, before dnsmasq starts (#942).
//
// It reads IFF_LOWER_UP (netif_carrier_ok). IFF_RUNNING also reports IF_OPER_UNKNOWN, which a just-up bridge shows
// until linkwatch runs: measured in run 34603031325, three of five modes read a carrier-0 bridge as running and the
// no-RA mode passed.
func awaitBridgeCarrier(t V6FixtureT) {
	t.Helper()
	deadline := time.Now().Add(carrierBudget)
	for {
		link, err := netlink.LinkByName(V6BridgeName)
		if err != nil {
			t.Fatalf("LinkByName %s: %v", V6BridgeName, err)
			return
		}
		if link.Attrs().RawFlags&unix.IFF_LOWER_UP != 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still has no carrier %s after %s was attached to it. A bridge in this "+
				"state has its transmit queues stopped, so the server's advertisements never "+
				"reach the wire and every wire assertion about this segment -- including the "+
				"ones that assert an ABSENCE -- is about a link nothing can speak on (#942)",
				V6BridgeName, carrierBudget, V6BridgePortName)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
