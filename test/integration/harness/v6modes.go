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
)

// A per-test dual-stack segment whose IPv6 service mode is chosen by
// the test: managed DHCPv6, stateless DHCPv6, SLAAC only, a DHCPv6
// server that sends no router advertisements at all, or a managed
// server that answers nothing.
//
// The mode's observable signature, the RA decoder and the verdict live
// in v6signature.go, untagged, so they can be driven in the fast lane.
// This file is the part that needs root: the bridge, the addresses, the
// server process and the capture.
//
// WHY THIS EXISTS AT ALL, AND WHY HERE. #815 needs exactly one of
// these modes (stateless) to show that the plugin receives an
// INFORMATION-REQUEST reply and then discards it. #816, #820 and #821
// each need a different one. Building a stateless-only fixture for
// #815 and widening it three times would mean the design is reviewed
// as an increment three times and never on its merits; the five modes
// are one mechanism with one parameter, so it lands once, under the
// first issue that needs it.
//
// WHY A SEPARATE L2, AND NOT THE SUITE-STATIC SEGMENTS. This is the
// load-bearing design decision, not an implementation detail. The
// thing that distinguishes these modes is the M and O flags in the
// router advertisement, and RA is a property of the BROADCAST DOMAIN,
// not of a server. Two RA senders with different flags on one segment
// do not produce a conflict the test can see: the client simply acts
// on whichever advertisement it heard last. A test written that way
// measures ARRIVAL ORDER and reports it as protocol behaviour --
// intermittently, and most often correctly, which is the worst
// available failure mode. The suite-static macvlan and bridge
// segments both already run an --enable-ra dnsmasq, so neither can
// host these tests at any price. Hence: own bridge, own prefix, one
// mode at a time, torn down per test. Same rationale as the distinct
// v4 subnets bridge.go documents, one layer down.
//
// WHY DUAL-STACK. The plugin has no IPv6-only network mode -- every
// mode leases v4 and treats v6 as an addition (see network.go). A
// v6-only fixture would therefore be testing a configuration the
// product does not have, so the segment carries a working v4 pool in
// every mode and the v6 half is the part under test.
//
// WHAT EACH TEST OWES. A test on this fixture asserts against ITS OWN
// segment and ITS OWN container. Do not write a sweep that concludes
// "the stateless segment behaves correctly" across several consumers:
// an aggregate over a population is not a claim about its members,
// and "each consumer observes the stateless behaviour" is the claim
// #816, #820 and #821 will actually be relying on.
const (
	// V6BridgeName is this fixture's own Linux bridge. Distinct from
	// BridgeName so the two never share a broadcast domain; still on
	// the dh-itest-* prefix the orphan cleanup keys on, and within
	// IFNAMSIZ.
	V6BridgeName = "dh-itest-br6"

	// The v4 half. 192.168.99/100/101/102/123 are all spoken for by
	// other fixtures; this is the next free one.
	V6BridgeAddr = "192.168.103.1/24"
	V6PoolStart  = "192.168.103.10"
	V6PoolEnd    = "192.168.103.99"
	V6SubnetCIDR = "192.168.103.0/24"

	// The v6 half. fd00:6470:6863::/64 is the macvlan fixture and
	// fd00:6470:6864::/64 is the bridge fixture, so this is the next
	// free ULA prefix.
	V6BridgeAddrV6 = "fd00:6470:6865::1/64"
	V6Prefix       = "fd00:6470:6865::"
	V6PoolStartV6  = "fd00:6470:6865::10"
	V6PoolEndV6    = "fd00:6470:6865::99"
	V6SubnetV6CIDR = "fd00:6470:6865::/64"

	// V6DNSServer and V6SearchDomain are what the server advertises
	// over DHCPv6. Both are deliberately values no other fixture
	// uses, so a test that finds them in a container's resolver
	// configuration knows they came from THIS segment and not from a
	// leaked v4 lease or another fixture's server.
	//
	// The search domain is not decoration: option6:domain-search is
	// the second defect in #815 -- it is carried on every DHCPv6
	// reply, stateful ones included, and the plugin dropped it in
	// both paths. Measured reaching the client as
	// new_dhcp6_domain_search on INFORM6 and on BOUND6 alike.
	V6DNSServer    = "fd00:6470:6865::53"
	V6SearchDomain = "v6mode.example"

	// raLogToken is dnsmasq's own advertisement line, printed
	// verbatim as `RTR-ADVERT(%s) %s` (radv.c:758) rather than through
	// gettext. It is the log column of the mode signature; the wire
	// column is the captured frame.
	raLogToken = "RTR-ADVERT("
)

// V6FixtureT is the slice of *testing.T this fixture uses.
//
// It exists for ONE reason and it is worth stating, because it is the
// first interface of its kind in this harness: the fixture's whole job
// is to FAIL when a segment is not in the mode it was asked for, and a
// check that has never been observed failing is not known to work. The
// drift matrix in v6modes_fixture_test.go drives every ordered pair of
// modes through the real constructor and has to observe the failure
// rather than suffer it. *testing.T satisfies this as it stands, so
// every ordinary caller is unchanged and there is no second code path.
type V6FixtureT interface {
	Helper()
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
	Cleanup(f func())
}

// rangeArgs is the dnsmasq spelling of the mode.
//
// THESE ARE DNSMASQ FLAGS, NOT PROTOCOL NAMES. "ra-stateless" and
// "ra-only" are one server's vocabulary for the M/O flag
// combinations; another server spells them differently and the
// protocol itself has no such words.
//
// The third field of a v6 --dhcp-range is a lease time only when it is
// not a bare number: dnsmasq parses a bare number there as a PREFIX
// LENGTH and refuses to start with "prefix length must be exactly 64
// for RA subnets" (MEASURED 2026-09-05, dnsmasq 2.91). LeaseTime is
// "2m", so this works; a future edit that makes it "120" breaks all
// three RA modes at once, loudly.
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
		// --enable-ra deliberately omitted; that is the whole mode.
		return []string{
			"--dhcp-range=" + V6PoolStartV6 + "," + V6PoolEndV6 + "," + LeaseTime,
		}
	case V6ManagedSilent:
		// Identical to V6Managed plus one directive. `dhcpv6` is a tag
		// dnsmasq sets on its own for every DHCPv6 request, so the
		// ignore reaches the v6 half and only the v6 half -- the v4
		// pool every mode carries keeps serving, which matters because
		// the plugin leases v4 on every network and a broken v4 half
		// would fail the endpoint for the wrong reason.
		//
		// Measured 2026-08-27, dnsmasq 2.92rel2 / dhcpcd 10.3.2, one
		// netns per arm, and again 2026-09-05 on 2.91:
		//
		//   --dhcp-ignore=tag:dhcpv6  -> DHCPSOLICIT ... ignored, no
		//                                IA_NA, v4 DHCPACK still sent
		//
		// The obvious spelling does NOT work and was tried first:
		// tagging the v6 range with `set:` and ignoring that tag leaves
		// dnsmasq serving the address anyway, with the tag visibly in
		// the request's tag set. Do not "simplify" this back to it.
		return []string{
			"--dhcp-range=" + V6PoolStartV6 + "," + V6PoolEndV6 + "," + LeaseTime,
			"--enable-ra",
			"--dhcp-ignore=tag:dhcpv6",
		}
	}
	return nil
}

// RangeArgsFor is rangeArgs, exported for the drift matrix: the contract
// test needs to start one mode's flags under another mode's name, and
// doing that through the same function production uses is what makes
// the matrix a statement about this fixture rather than about a copy of
// it.
func RangeArgsFor(m V6Mode) []string { return m.rangeArgs() }

// V6Fixture is a per-test dual-stack segment in one V6Mode. Create it
// with NewV6Fixture; it registers its own teardown.
type V6Fixture struct {
	t    V6FixtureT
	mode V6Mode

	cmd       *exec.Cmd
	tmpDir    string
	leaseFile string
	logFile   string

	// startedAt is the instant dnsmasq was started, and it is the
	// reference every "after" question is asked against.
	startedAt time.Time
	raCap     *RACapture

	linkUp            bool
	iptablesInstalled bool
}

// NewV6Fixture brings up the bridge, the addresses, the FORWARD rules
// and a dnsmasq in the requested mode, and does not return until the
// segment has been observed to be in that mode -- on the wire and in
// the server's own log.
func NewV6Fixture(t *testing.T, mode V6Mode) *V6Fixture {
	t.Helper()
	return NewV6FixtureWithArgs(t, mode, mode.rangeArgs())
}

// NewV6FixtureWithArgs is NewV6Fixture with the server's v6 flags given
// explicitly, so the drift matrix can start one mode's flags under
// another mode's name and require the fixture to notice.
//
// It is the ONLY constructor: NewV6Fixture is two lines over it, and
// the mode assertion happens here, once. That is deliberate. A drift
// test that reached the assertion by a path of its own would prove a
// path production never takes -- two records, one observer -- and a
// mutant that weakened the assertion in one of them would leave the
// other's test green.
func NewV6FixtureWithArgs(t V6FixtureT, name V6Mode, rangeArgs []string) *V6Fixture {
	t.Helper()
	f := newV6Fixture(t, name, rangeArgs)
	f.assertMode()
	return f
}

func newV6Fixture(t V6FixtureT, name V6Mode, rangeArgs []string) *V6Fixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("V6Fixture needs root (got uid=%d)", os.Geteuid())
	}

	// Defensively, so a previous panicked run cannot poison this one.
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

	// Same reason as bridge.go: the kernel's 15s STP forward delay
	// holds the port in LEARNING and no DHCP passes.
	fdPath := filepath.Join("/sys/class/net", V6BridgeName, "bridge/forward_delay")
	if err := os.WriteFile(fdPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("disable STP forward_delay on %s: %v", V6BridgeName, err)
	}
	// Turn duplicate address detection off on this bridge BEFORE any
	// address is added, and before the link comes up.
	//
	// THIS IS NOT A TIDINESS SETTING, IT IS THE RA TIMING. Measured
	// 2026-08-27: with DAD on, dnsmasq starts while the bridge's
	// global address is still `tentative`, cannot send from it, and
	// emits its first router advertisement about NINE seconds later.
	// With the address settled it advertises in about one. The
	// difference is invisible in the log -- dnsmasq says "IPv6 router
	// advertisement enabled" either way -- so the mode check below
	// reads a segment that is merely slow as a segment in the wrong
	// mode, and the obvious repair is to widen the budget until the
	// symptom stops, which buys a fixture that no longer checks the
	// thing it exists to check.
	//
	// DAD has no question to answer here: this bridge is a private
	// segment on a ULA prefix nothing else uses, with exactly one
	// router on it, which is us. All it can do is hold the address
	// unusable for a second.
	dadPath := filepath.Join("/proc/sys/net/ipv6/conf", V6BridgeName, "accept_dad")
	if err := os.WriteFile(dadPath, []byte("0"), 0o644); err != nil {
		t.Fatalf("disable DAD on %s: %v", V6BridgeName, err)
	}

	// ...and add each address with IFA_F_NODAD, which is a stronger
	// statement than the sysctl and answers a different question.
	//
	// accept_dad=0 does not stop the address being CREATED tentative;
	// it stops the probe, and the kernel then clears the flag from
	// addrconf's work queue some time after the link comes up. That
	// window is real: measured 2026-08-27 in run 33096065817, three of
	// the four modes read the address settled and the fourth read it
	// still tentative 60ms after LinkSetUp, on the same code path with
	// the same sysctl written. Nothing about the fourth mode differs
	// here -- the mode only reaches dnsmasq's argv, further down -- so
	// what the guard caught was its own premise, not a mode.
	//
	// NODAD is set at creation, so the flag is never raised at all and
	// there is no window to lose. The sysctl stays because it governs
	// the address this loop does NOT add: the kernel's own link-local,
	// which is what a router advertisement is sent FROM.
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
	awaitNoTentativeAddr(t)

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

	// The signature in v6signature.go was measured against a stated
	// dnsmasq version, and this box's version is not the runner's:
	// 2.92rel2 was measured on 2026-08-27 and 2.91 on 2026-09-05, on
	// the same machine role. Read it, never assume it -- a fixture
	// validated against a version the runner does not run is a
	// measurement that never ran.
	t.Logf("v6 fixture: mode=%s bridge=%s v4=%s-%s v6=%s dnsmasq=%q",
		name, V6BridgeName, V6PoolStart, V6PoolEnd, V6SubnetV6CIDR, dnsmasqVersion())

	// The capture is opened BEFORE dnsmasq starts. The first
	// advertisement arrives about a second later (MEASURED, 12 of 12
	// bring-ups, 0.950s..0.983s), and a capture opened after it would
	// have to wait for the next one, which dnsmasq schedules 5 to 19
	// seconds out.
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
		// The v4 half, present in every mode -- see WHY DUAL-STACK.
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

// dnsmasqVersion is the first line of `dnsmasq --version`, or a string
// saying why it could not be read. It is never a silent empty value:
// the whole point is that the version is recorded rather than assumed.
func dnsmasqVersion() string {
	out, err := exec.Command("/usr/sbin/dnsmasq", "--version").Output()
	if err != nil {
		return fmt.Sprintf("(could not read dnsmasq --version: %v)", err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

// waitReady blocks until the v4 pool is logged, which every mode has
// and which dnsmasq prints once its ranges are configured. Keyed on
// the pool's start ADDRESS for the locale reason in V6Signature.
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

// evidence gathers both columns of the mode signature: the two facts
// from the server's own log, and every advertisement seen on the wire.
//
// The positive case polls and returns as soon as the whole signature is
// satisfied. The negative case -- a mode that expects NO advertisement
// -- always spends the full window, because "no RTR-ADVERT yet" and "no
// RTR-ADVERT ever" are the same evidence until enough time has passed,
// and a negative that returns early passes because it did not wait.
func (f *V6Fixture) evidence() V6Evidence {
	f.t.Helper()
	want := f.mode.Signature()
	budget := raBudget
	if !want.RA {
		budget = V6NoRAWindow()
	}
	deadline := time.Now().Add(budget)
	for {
		log := f.readLog()
		ev := V6Evidence{
			PoolLogged: strings.Contains(log, V6PoolStartV6),
			RALogged:   strings.Contains(log, raLogToken),
			Frames:     f.raCap.FramesAfter(f.startedAt),
		}
		// The wait exists only to tell "no advertisement yet" from "no
		// advertisement ever". Once one has been captured that question
		// is answered and every remaining disagreement is decidable, so
		// spending the rest of the budget would only collect the same
		// frame again -- and the drift matrix starts twenty-five of
		// these.
		if want.RA && ev.Observed().RA {
			return ev
		}
		if time.Now().After(deadline) {
			return ev
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// assertMode checks the segment is in the mode the test asked for,
// rather than assuming the flags did what they did the day they were
// measured. It is the fixture's reason to exist and the single place
// the verdict is turned into a failure.
//
// The verdict itself is V6ModeFindings, which is pure and lives in the
// fast lane, so both directions of it are driven without a bridge.
func (f *V6Fixture) assertMode() {
	f.t.Helper()
	ev := f.evidence()
	if findings := V6ModeFindings(f.mode, ev); len(findings) > 0 {
		f.t.Fatalf("v6 fixture mode=%s: %s\ncaptured %d router advertisement(s):\n%s\nlog:\n%s",
			f.mode, strings.Join(findings, "; "), len(ev.Frames),
			formatRAFrames(ev.Frames), f.readLog())
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

// ModeFindings is assertMode's verdict without the failure, for the
// contract test that has to observe the check working rather than
// suffer it.
func (f *V6Fixture) ModeFindings() []string {
	return V6ModeFindings(f.mode, f.evidence())
}

// Mode is the mode this segment was brought up in.
func (f *V6Fixture) Mode() V6Mode { return f.mode }

// Bridge is the bridge name to hand the driver as `bridge=`.
func (f *V6Fixture) Bridge() string { return V6BridgeName }

// StartedAt is when the server process started, which is the reference
// for every "after" question about this segment.
func (f *V6Fixture) StartedAt() time.Time { return f.startedAt }

// RACapture is the segment's router-advertisement capture.
func (f *V6Fixture) RACapture() *RACapture { return f.raCap }

// AwaitRAAfter fails the test unless a router advertisement arrived
// after since, in BOTH columns: the server logged an RTR-ADVERT line
// and the capture holds a frame with a later timestamp.
//
// This is trap 1's observer, and the reason it takes an instant rather
// than just counting is that the trap is a test passing because the
// advertisement it depends on arrived BEFORE the client started, or
// never, while the client reported "no router" and the test only
// asserted that the endpoint came up. "An RA existed" and "an RA
// arrived after this point" are different claims and only the second
// one is the premise those tests rest on.
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
				"server log has an %s line: %v, capture has %d frame(s) after that instant. "+
				"Every v6 assertion downstream of this rests on an advertisement the client "+
				"could actually have heard; log:\n%s",
				f.mode, since.Format("15:04:05.000"), budget, raLogToken, logged,
				len(frames), f.readLog())
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertNoRAWithin fails the test if a router advertisement is seen at
// all within window, in either column.
//
// The window is not a parameter with a default: pass V6NoRAWindow(),
// which is derived from dnsmasq's own scheduling. A window shorter than
// that interval passes because it did not wait, which is the negative
// half of trap 1 and the reason this function exists rather than a
// bare `if len(frames) != 0`.
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

// AssertExchange fails the test unless the server's log says a client
// completed the exchange this mode is defined by.
//
// It is the client-dependent half of the mode signature, split out
// because it can only be asked once a client has run: the fixture
// checks the client-INdependent half (the pool line, the advertisement
// and its flags) at construction, and this is what M7d's scenarios call
// afterwards. The contract is a table in v6signature.go and the verdict
// is a pure function over the log, driven in the fast lane against
// captured server logs, one per mode.
//
// Nothing in this round can drive it positively on a live segment: the
// 2.x branch refuses ipv6=true at network creation, so no run on this
// branch constructs a DHCPv6 client. The live drive is M7d's.
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
// Mirrors Fixture.CountBridgeLogLines: this is the outside evidence a
// test should assert on, the plugin's own counters being evidence of
// intent rather than of effect.
func (f *V6Fixture) CountLogLines(substrings ...string) int {
	return countLinesWithAll(f.readLog(), substrings)
}

// AwaitIgnoredSolicit blocks until the server has logged a DHCPv6
// request it refused to answer, and fails the test if none arrives.
//
// It exists because V6ManagedSilent has the same startup signature as
// V6Managed -- same pool line, same RA, same M and O flags, same prefix
// option (MEASURED 2026-09-05; V6IndistinguishableModes derives that
// pair from the table rather than asserting it here) -- so no
// fixture-time check can tell them apart, and the thing that separates
// them only becomes visible once a client actually solicits. Without
// this a mistyped ignore directive would produce a segment that quietly
// serves addresses while the test believed the server was silent, which
// turns the strongest assertion in the #868 set into a tautology.
//
// "ignored" is NOT a protocol token, and an earlier version of this
// comment said it was. dnsmasq writes it through gettext as
// `_("ignored")` (rfc3315.c:652) and its own po/de.po renders it
// "ignoriert" -- on the German locale the integration runner speaks,
// this needle would match nothing and the wait would time out. What
// makes it safe is withCLocale, which pins every fixture server to
// LC_ALL=C; the dependency is on that helper, and it is named here so a
// change to it is understood to reach this function. DHCPSOLICIT, by
// contrast, really is printed verbatim.
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

// DumpLogs mirrors Fixture.DumpBridgeLogs for failure diagnostics, and
// dumps the wire alongside the log because half the mode signature is
// only in the frames.
func (f *V6Fixture) DumpLogs(write func(string)) {
	write(fmt.Sprintf("--- v6 fixture dnsmasq log (mode=%s) ---\n%s", f.mode, f.readLog()))
	if f.raCap != nil {
		f.raCap.Dump(write)
	}
}

func (f *V6Fixture) teardown() {
	if f.raCap != nil {
		f.raCap.Stop()
	}
	if f.cmd != nil && f.cmd.Process != nil {
		_ = f.cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = f.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = f.cmd.Process.Kill()
			<-done
		}
	}
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

// tentativeBudget bounds awaitNoTentativeAddr below.
//
// IT IS DELIBERATELY SHORTER THAN THE FASTEST POSSIBLE REAL DAD, and
// that is the whole design. Duplicate address detection sends its
// probe after a random delay of up to a second and waits a further
// RetransTimer -- a second by default -- so a segment on which DAD is
// genuinely running cannot clear inside 250ms and this still fails.
// What it absorbs is the other thing: with DAD off the kernel does not
// skip the tentative flag, it clears it from addrconf's work queue
// shortly after the link comes up, and that is sub-millisecond.
//
// Measured 2026-08-27, 200 bring-ups per arm, one variable each, all
// tentative readings on the kernel's own link-local:
//
//	accept_dad=0, no NODAD, no wait  -> 15/200 tentative
//	accept_dad=0, NODAD,    no wait  -> 11/200 tentative
//	accept_dad=0, NODAD,    wait     ->  0/200, longest wait 2ms
//	accept_dad=1, NODAD,    wait     -> 200/200 failed at the budget
//
// The last line is the control, and it is why this is a discriminator
// rather than a timeout: turn DAD back on and the guard still goes
// red on every single run. The third line is why the budget costs
// nothing in the healthy case.
//
// The second line is why the wait is here at all, and it is worth
// reading twice: NODAD alone is NOT enough. At 40 runs per arm it
// looked like it was -- 0/40, against 6/40 without it -- and that
// reading would have shipped a wait-free fixture that flaked twice a
// week. Four hundred more bring-ups put both arms at the same rate.
// NODAD closes the two addresses this fixture ADDS; nothing it can
// set reaches the link-local the kernel generates.
const tentativeBudget = 250 * time.Millisecond

// awaitNoTentativeAddr checks the two settings above actually took,
// rather than trusting that writing them was enough. An address left
// tentative cannot be a send source, and the only symptom downstream
// is a router advertisement that arrives late -- which presents as the
// wrong mode. Naming the cause here costs one netlink call and saves
// that diagnosis.
//
// It reads EVERY v6 address on the link, not just the two added above.
// The kernel's own link-local is the one this fixture cannot set
// NODAD on -- it does not add it -- and it is also the address a
// router advertisement is sent FROM, so it is the one that matters
// most and the only one left racing the work queue.
func awaitNoTentativeAddr(t V6FixtureT) {
	t.Helper()
	link, err := netlink.LinkByName(V6BridgeName)
	if err != nil {
		t.Fatalf("LinkByName %s: %v", V6BridgeName, err)
	}
	deadline := time.Now().Add(tentativeBudget)
	for {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V6)
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

// cleanupV6Links removes the fixture's bridge, on teardown and
// defensively at setup.
func cleanupV6Links() {
	if link, err := netlink.LinkByName(V6BridgeName); err == nil {
		_ = netlink.LinkDel(link)
	}
}
