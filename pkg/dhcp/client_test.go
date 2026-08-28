// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/claymore666/docker-net-dhcp/pkg/util"
)

// hasArg returns whether args contains exactly target.
func hasArg(args []string, target string) bool {
	for _, a := range args {
		if a == target {
			return true
		}
	}
	return false
}

// newTestClient builds a client and schedules cleanup of its open FIFO
// fd and per-client working directory (NewDHCPClient creates both on
// disk even before Start).
func newTestClient(t *testing.T, iface string, opts *DHCPClientOptions) *DHCPClient {
	t.Helper()
	c, err := NewDHCPClient(iface, opts)
	if err != nil {
		t.Fatalf("NewDHCPClient: %v", err)
	}
	t.Cleanup(func() {
		if c.fifoRead != nil {
			_ = c.fifoRead.Close()
		}
		if c.fifoKeep != nil {
			_ = c.fifoKeep.Close()
		}
		_ = os.RemoveAll(c.workDir)
	})
	return c
}

func readConf(t *testing.T, c *DHCPClient) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.workDir, "dhcpcd.conf"))
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	return string(b)
}

func TestNewDHCPClient_RejectsInvalidIface(t *testing.T) {
	// Names that must be rejected before they can reach the dhcpcd argv
	// that runs under `unshare -m /bin/sh -c …` (go/command-injection):
	// shell metacharacters, whitespace, path separators, flag-shaped
	// leading dashes, over-length names, and the empty string.
	bad := []string{
		"",
		"-rf", // flag-shaped
		// The measured getopt-permutation payload: dhcpcd 10.3.2 reads
		// a trailing positional that looks like an option AS one, and
		// -c names the hook script it runs as root (#706).
		"-c/tmp/evil.sh",
		"-c",
		"-",
		".x",
		"eth0; rm -rf /",     // shell injection attempt
		"eth0 rm",            // whitespace
		"eth/0",              // path separator
		"eth0$(whoami)",      // command substitution
		"`reboot`",           // backticks
		"toolonginterface00", // > 15 chars
	}
	for _, name := range bad {
		if _, err := NewDHCPClient(name, &DHCPClientOptions{MAC: mustMAC(t, "de:ad:be:ef:00:01")}); err == nil {
			t.Errorf("NewDHCPClient(%q) accepted an invalid interface name", name)
		}
	}

	// Realistic names that must be accepted.
	for _, name := range []string{"eth0", "eth0.100", "veth_a1-b2", "en0"} {
		c, err := NewDHCPClient(name, &DHCPClientOptions{MAC: mustMAC(t, "de:ad:be:ef:00:01")})
		if err != nil {
			t.Errorf("NewDHCPClient(%q) rejected a valid interface name: %v", name, err)
			continue
		}
		if c.fifoRead != nil {
			_ = c.fifoRead.Close()
		}
		if c.fifoKeep != nil {
			_ = c.fifoKeep.Close()
		}
		_ = os.RemoveAll(c.workDir)
	}
}

func TestNewDHCPClient_V4CommandAndConfig(t *testing.T) {
	c := newTestClient(t, "eth0", &DHCPClientOptions{
		MAC:         mustMAC(t, "de:ad:be:ef:00:01"),
		Hostname:    "my-container",
		RequestedIP: "192.168.0.50",
		ClientID:    []byte{0x01, 0x23},
	})

	// Command: dhcpcd wrapped in a private mount namespace.
	// The wrapper binary is pinned by ABSOLUTE path: exec.Command
	// resolves a bare name through LookPath against the inherited PATH,
	// which made unshare the one binary in the tree whose identity
	// depended on an environment variable (#707). Asserting the path --
	// like /bin/sh and the handler below -- means moving the binary
	// fails here instead of silently changing which executable runs.
	if c.cmd.Args[0] != unsharePath || !hasArg(c.cmd.Args, "-m") {
		t.Errorf("expected %s -m wrapper; args: %v", unsharePath, c.cmd.Args)
	}
	if !strings.HasPrefix(unsharePath, "/") {
		t.Errorf("unsharePath %q is not absolute; PATH decides which binary runs", unsharePath)
	}
	if c.cmd.Path != unsharePath {
		t.Errorf("cmd.Path = %q, want %q -- a bare name here is resolved through $PATH", c.cmd.Path, unsharePath)
	}
	for _, want := range []string{"/bin/sh", dhcpcdBin, DefaultHandler} {
		if !hasArg(c.cmd.Args, want) {
			t.Errorf("command missing pinned absolute path %q; args: %v", want, c.cmd.Args)
		}
	}
	// dhcpcd reaches execve as $0 of `sh -c '... exec "$0" "$@"'`, and a
	// shell resolves a bare name through PATH exactly as LookPath does,
	// so the absolute form is the assertion (#707).
	//
	// The negative below is the load-bearing half. hasArg compares whole
	// arguments, so it is the reason this case went red when the constant
	// changed — but the bare name IS a substring of the absolute one, and
	// the day someone relaxes this to a strings.Contains over the joined
	// argv, both spellings satisfy it and the case reports green over the
	// exact regression it exists to stop. Keep it exact, and keep the
	// negative.
	if !strings.HasPrefix(dhcpcdBin, "/") {
		t.Errorf("dhcpcdBin %q is not absolute; PATH decides which binary runs", dhcpcdBin)
	}
	if hasArg(c.cmd.Args, "dhcpcd") {
		t.Errorf("argv carries the bare name \"dhcpcd\"; it must be %q, or the shell resolves it through $PATH; args: %v",
			dhcpcdBin, c.cmd.Args)
	}
	for _, want := range []string{"--noconfigure", "-4", "eth0", DefaultHandler} {
		if !hasArg(c.cmd.Args, want) {
			t.Errorf("command missing %q; args: %v", want, c.cmd.Args)
		}
	}
	if hasArg(c.cmd.Args, "-6") {
		t.Errorf("v4 client got -6; args: %v", c.cmd.Args)
	}

	// Config: pinned identity + v4 directives.
	conf := readConf(t, c)
	for _, want := range []string{
		"duid 00:03:00:01:de:ad:be:ef:00:01",
		"interface eth0",
		"iaid 3203334145",
		"request 192.168.0.50",
		"clientid 00:01:23",
		"hostname my-container",
		"vendorclassid " + VendorID, // defaulted
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config missing %q\n---\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "ia_na") {
		t.Errorf("v4 config leaked ia_na:\n%s", conf)
	}
}

func TestNewDHCPClient_V6CommandAndConfig(t *testing.T) {
	c := newTestClient(t, "eth0", &DHCPClientOptions{
		V6:          true,
		MAC:         mustMAC(t, "de:ad:be:ef:00:01"),
		PreferredV6: "fd00::42",
		// v4-only knobs must be ignored.
		ClientID:    []byte{0x01, 0x23},
		VendorClass: "should-not-appear",
		RequestedIP: "192.168.0.50",
	})

	if !hasArg(c.cmd.Args, "-6") || hasArg(c.cmd.Args, "-4") {
		t.Errorf("v6 client family flags wrong; args: %v", c.cmd.Args)
	}
	conf := readConf(t, c)
	for _, want := range []string{
		"duid 00:03:00:01:de:ad:be:ef:00:01",
		"iaid 3203334145",
		"ia_na 3203334145 / fd00::42",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("v6 config missing %q\n---\n%s", want, conf)
		}
	}
	for _, banned := range []string{"clientid", "vendorclassid", "request", "should-not-appear"} {
		if strings.Contains(conf, banned) {
			t.Errorf("v6 config leaked v4-only directive %q:\n%s", banned, conf)
		}
	}
}

func TestNewDHCPClient_OnceAddsPersistFlags(t *testing.T) {
	c := newTestClient(t, "eth0", &DHCPClientOptions{Once: true, MAC: mustMAC(t, "de:ad:be:ef:00:01")})
	if !hasArg(c.cmd.Args, "-1") || !hasArg(c.cmd.Args, "-p") {
		t.Errorf("one-shot client should have -1 and -p; args: %v", c.cmd.Args)
	}
}

// TestNewDHCPClient_PersistentOmitsTheOneShotFlags: the two clients this
// package builds must stay distinguishable by argv.
//
// The two flags are not equally load-bearing, and until #800 this test
// said they were, for a reason that was never true.
//
//   - -1 has teeth. A persistent client given it exits after its first
//     lease and never renews, so the container's address lapses at the
//     server's deadline with no client left to notice.
//   - -p governs whether dhcpcd DE-CONFIGURES the interface on exit.
//     Under --noconfigure, which renderArgs gives BOTH clients, dhcpcd
//     configured nothing, so there is nothing to keep or drop and the
//     flag is inert here. It never had anything to do with releasing:
//     the release came from the `release` directive, which #800 removed
//     and TestRenderConfig_NothingEverReleases now forbids.
//
// So -p is asserted as a shape guard — the one-shot's argv and the
// persistent client's must not converge — and NOT because omitting it
// releases anything. Nothing in this build releases.
func TestNewDHCPClient_PersistentOmitsTheOneShotFlags(t *testing.T) {
	c := newTestClient(t, "eth0", &DHCPClientOptions{Once: false, MAC: mustMAC(t, "de:ad:be:ef:00:01")})
	if hasArg(c.cmd.Args, "-1") {
		t.Errorf("persistent client got -1; it would exit after its first lease and never "+
			"renew, and the container's address would lapse with nobody watching; args: %v", c.cmd.Args)
	}
	if hasArg(c.cmd.Args, "-p") {
		t.Errorf("persistent client got -p; the one-shot's argv and the persistent client's "+
			"have converged. -p is inert under --noconfigure, so this is a shape guard, not "+
			"a claim that omitting it releases a lease; args: %v", c.cmd.Args)
	}
}

func TestNewDHCPClient_HandlerOverride(t *testing.T) {
	custom := "/tmp/my-handler"
	c := newTestClient(t, "eth0", &DHCPClientOptions{MAC: mustMAC(t, "de:ad:be:ef:00:01"), HandlerScript: custom})
	if !hasArg(c.cmd.Args, custom) {
		t.Errorf("custom handler not used; args: %v", c.cmd.Args)
	}
}

func TestNewDHCPClient_FIFOWiredIntoConfig(t *testing.T) {
	c := newTestClient(t, "eth0", &DHCPClientOptions{MAC: mustMAC(t, "de:ad:be:ef:00:01")})
	conf := readConf(t, c)
	// The env directive must point at the FIFO that actually exists in
	// the work dir, so the handler writes where the client reads.
	wantFIFO := filepath.Join(c.workDir, "events")
	if !strings.Contains(conf, "env "+EventFIFOEnv+"="+wantFIFO) {
		t.Errorf("config missing FIFO env directive for %q\n---\n%s", wantFIFO, conf)
	}
	if _, err := os.Stat(wantFIFO); err != nil {
		t.Errorf("event FIFO not created: %v", err)
	}
}

func TestMountPrep_RemountsProcSysRW(t *testing.T) {
	script := mountPrep()
	// dhcpcd's interface setup writes /proc/sys, which is ro in the
	// managed-plugin rootfs; the wrapper must flip it rw in the private
	// mount namespace before exec (#247). It must still mount the
	// per-client tmpfs state dir and exec dhcpcd via $0/$@.
	for _, want := range []string{
		mountBin + " -t tmpfs tmpfs " + dhcpcdStateDir,
		// dhcpcd's runtime dir (pidfile + control sockets, keyed by
		// interface name only) must be private per client, or the
		// second same-named-interface client forwards its argv into
		// the first container's dhcpcd and exits without doing DHCP.
		mkdirBin + " -p " + dhcpcdRunDir,
		mountBin + " -t tmpfs tmpfs " + dhcpcdRunDir,
		mountBin + " -o remount,bind,rw " + procSysPath,
		`exec "$0" "$@"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("mountPrep missing %q\n---\n%s", want, script)
		}
	}
	// The remount must precede exec, or dhcpcd starts before /proc/sys is
	// writable.
	if strings.Index(script, "remount,bind,rw") > strings.Index(script, `exec "$0"`) {
		t.Errorf("remount must run before exec\n---\n%s", script)
	}
}

func TestNewDHCPClient_WrapsRemountIntoCommand(t *testing.T) {
	c := newTestClient(t, "eth0", &DHCPClientOptions{MAC: mustMAC(t, "de:ad:be:ef:00:01")})
	// The mount-prep script rides as the `sh -c` argument; assert the
	// /proc/sys remount actually reaches the spawned command.
	if !hasArg(c.cmd.Args, mountPrep()) {
		t.Errorf("mount-prep script not wired into command; args: %v", c.cmd.Args)
	}
}

func TestTailWriter_CapsAndCondenses(t *testing.T) {
	w := &tailWriter{max: 8}
	// Writes beyond max retain only the trailing bytes.
	for _, s := range []string{"hello\n", "world\n"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := string(w.buf); got != "o\nworld\n" {
		t.Errorf("tail not capped to last %d bytes: %q", w.max, got)
	}

	// condense drops blank lines and joins the rest with "; ".
	w2 := &tailWriter{max: stderrTailMax}
	_, _ = w2.Write([]byte("dhcpcd: eth0: if_init: Read-only file system\n\n  \nexiting\n"))
	if got, want := w2.condense(), "dhcpcd: eth0: if_init: Read-only file system; exiting"; got != want {
		t.Errorf("condense() = %q, want %q", got, want)
	}
	if (&tailWriter{max: stderrTailMax}).condense() != "" {
		t.Errorf("empty tail should condense to empty string")
	}
}

// swapCmd replaces the client's dhcpcd command with an arbitrary shell
// script standing in for the real process, so Start/scanner/reaper can
// be exercised without dhcpcd.
func swapCmd(c *DHCPClient, script string) {
	c.cmd = exec.Command("/bin/sh", "-c", script)
}

// TestStart_BoundEventSurvivesFastExit is the regression test for the
// reaper/scanner FIFO race (#325): a one-shot dhcpcd exits immediately
// after its hook writes the bound event, and the reaper's FIFO close
// could discard the event before the scanner read it, surfacing as a
// spurious ErrNoLease under scheduler load. With the keep-alive-only
// close the bound event must survive every time; repeated trials keep
// the race window exercised.
func TestStart_BoundEventSurvivesFastExit(t *testing.T) {
	for i := 0; i < 300; i++ {
		c := newTestClient(t, "eth0", &DHCPClientOptions{Once: true, MAC: mustMAC(t, "de:ad:be:ef:00:01")})
		// Stand-in for dhcpcd -1: write the bound event to the FIFO
		// (as the hook does) and exit immediately.
		swapCmd(c, `printf '{"Type":"bound","Data":{"IP":"10.99.0.2/24","Gateway":"10.99.0.1"}}\n' > `+filepath.Join(c.workDir, "events"))

		events, err := c.Start()
		if err != nil {
			t.Fatalf("trial %d: Start: %v", i, err)
		}

		bound := false
		consumed := make(chan struct{})
		go func() {
			defer close(consumed)
			for event := range events {
				if event.Type == "bound" {
					bound = true
				}
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := c.Wait(ctx); err != nil {
			cancel()
			t.Fatalf("trial %d: Wait: %v", i, err)
		}
		cancel()
		<-consumed

		if !bound {
			t.Fatalf("trial %d: bound event lost between hook write and process exit", i)
		}
	}
}

// stubAttemptGetIP swaps attemptGetIPFunc for the test's duration.
//
// It takes the pre-#868 shape -- (Info, error) -- and reports no router
// advertisement, so the cases written before the observation existed
// keep driving exactly what they drove. stubAttemptGetIPRA is the one
// to use when the advertisement is the subject.
func stubAttemptGetIP(t *testing.T, fn func(context.Context, string, *DHCPClientOptions) (Info, error)) {
	t.Helper()
	stubAttemptGetIPRA(t, func(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, RAObservation, error) {
		info, err := fn(ctx, iface, opts)
		return info, RAObservation{}, err
	})
}

// stubAttemptGetIPRA is stubAttemptGetIP for cases that drive the
// router-advertisement observation (#868).
func stubAttemptGetIPRA(t *testing.T, fn func(context.Context, string, *DHCPClientOptions) (Info, RAObservation, error)) {
	t.Helper()
	prev := attemptGetIPFunc
	attemptGetIPFunc = fn
	t.Cleanup(func() { attemptGetIPFunc = prev })
}

func TestGetIP_RetriesTransientAndSucceeds(t *testing.T) {
	attempts := 0
	stubAttemptGetIP(t, func(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, error) {
		attempts++
		if !opts.Once {
			t.Errorf("attempt %d: opts.Once not set", attempts)
		}
		if attempts < 3 {
			return Info{}, util.ErrNoLease
		}
		return Info{IP: "192.168.1.100/24", Gateway: "192.168.1.1"}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := &DHCPClientOptions{MAC: mustMAC(t, "de:ad:be:ef:00:01")}
	info, _, err := GetIP(ctx, "eth0", opts)
	if err != nil {
		t.Fatalf("GetIP: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts: got %d, want 3", attempts)
	}
	if info.IP != "192.168.1.100/24" || info.Gateway != "192.168.1.1" {
		t.Errorf("lease: got %+v", info)
	}
	if opts.Once {
		t.Errorf("caller's opts.Once was mutated")
	}
}

func TestGetIP_PermanentErrorFailsFast(t *testing.T) {
	attempts := 0
	permanent := errors.New("failed to create DHCP client: invalid interface name")
	stubAttemptGetIP(t, func(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, error) {
		attempts++
		return Info{}, permanent
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, _, err := GetIP(ctx, "eth0", &DHCPClientOptions{MAC: mustMAC(t, "de:ad:be:ef:00:01")})
	if !errors.Is(err, permanent) {
		t.Fatalf("error: got %v, want the permanent error unwrapped", err)
	}
	if attempts != 1 {
		t.Errorf("attempts: got %d, want 1 (no retries on permanent errors)", attempts)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("permanent failure took %v; must fail fast", elapsed)
	}
}

// A non-zero dhcpcd exit (wrapped with its stderr tail, as the reaper
// does) is transient from the caller's perspective and must be retried;
// client-construction errors must not be.
func TestIsRetryableLeaseErr(t *testing.T) {
	exitErr := exec.Command("/bin/sh", "-c", "exit 1").Run()
	if exitErr == nil {
		t.Skip("cannot produce an ExitError on this system")
	}
	wrapped := fmt.Errorf("%w: dhcpcd: timed out", exitErr)

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no lease", util.ErrNoLease, true},
		{"wrapped no lease", fmt.Errorf("x: %w", util.ErrNoLease), true},
		{"dhcpcd exit + stderr tail", wrapped, true},
		{"client setup", errors.New("failed to create DHCP client: x"), false},
		{"context deadline", context.DeadlineExceeded, false},
	} {
		if got := isRetryableLeaseErr(tc.err); got != tc.want {
			t.Errorf("%s: isRetryableLeaseErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// On deadline the error must keep BOTH sentinels intact: the context
// error (probe's "no DHCP OFFER" classification) and the last attempt's
// error (ErrToStatus's 502 mapping for ErrNoLease).
func TestGetIP_DeadlinePreservesErrorChain(t *testing.T) {
	stubAttemptGetIP(t, func(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, error) {
		return Info{}, util.ErrNoLease
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, _, err := GetIP(ctx, "eth0", &DHCPClientOptions{MAC: mustMAC(t, "de:ad:be:ef:00:01")})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error lost context.DeadlineExceeded: %v", err)
	}
	if !errors.Is(err, util.ErrNoLease) {
		t.Errorf("error lost util.ErrNoLease sentinel: %v", err)
	}
	if got := util.ErrToStatus(err); got != http.StatusBadGateway {
		t.Errorf("ErrToStatus: got %d, want 502 (ErrNoLease must survive the retry loop)", got)
	}
}

func TestGetIP_ContextCancelledStopsRetries(t *testing.T) {
	attempts := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stubAttemptGetIP(t, func(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, error) {
		attempts++
		if attempts == 2 {
			cancel()
		}
		return Info{}, util.ErrNoLease
	})

	_, _, err := GetIP(ctx, "eth0", &DHCPClientOptions{MAC: mustMAC(t, "de:ad:be:ef:00:01")})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error: got %v, want context.Canceled", err)
	}
	if attempts != 2 {
		t.Errorf("attempts: got %d, want 2 (must stop on cancellation)", attempts)
	}
}

// TestStart_NoGoroutineLeakPerClient pins the log-pipe lifecycle: each
// client wires dhcpcd's stdout/stderr into logrus via WriterLevel, whose
// pipe-reader goroutines only exit when the writers are closed. The
// reaper must close them after Wait, or every dhcpcd run (including each
// GetIP retry) permanently leaks two goroutines and pipe pairs in the
// long-running plugin daemon.
func TestStart_NoGoroutineLeakPerClient(t *testing.T) {
	const cycles = 50

	baseline := runtime.NumGoroutine()
	for i := 0; i < cycles; i++ {
		c := newTestClient(t, "eth0", &DHCPClientOptions{Once: true, MAC: mustMAC(t, "de:ad:be:ef:00:01")})
		swapCmd(c, "exit 0")
		events, err := c.Start()
		if err != nil {
			t.Fatalf("cycle %d: Start: %v", i, err)
		}
		for range events {
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = c.Wait(ctx)
		cancel()
	}

	// The WriterLevel reader goroutines exit asynchronously after the
	// reaper closes their writers; poll briefly for the count to settle.
	// Pre-fix this leaked 2*cycles goroutines, far above the slack.
	const slack = 10
	deadline := time.Now().Add(5 * time.Second)
	for {
		if growth := runtime.NumGoroutine() - baseline; growth <= slack {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("goroutines grew by %d after %d client cycles (want <= %d): log pipes not closed?", growth, cycles, slack)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A dhcpcd exit whose stderr tail says the interface is gone is
// terminal (link deleted under us), not retryable.
func TestIsRetryableLeaseErr_InterfaceVanished(t *testing.T) {
	exitErr := exec.Command("/bin/sh", "-c", "exit 1").Run()
	if exitErr == nil {
		t.Skip("cannot produce an ExitError on this system")
	}
	vanished := fmt.Errorf("%w: eth0: interface not found or invalid", exitErr)
	if isRetryableLeaseErr(vanished) {
		t.Errorf("interface-not-found exit must be terminal, got retryable")
	}
}

// Finish without Start must not panic (persistent clients would
// otherwise nil-deref cmd.Process on the SIGTERM path) and must clean
// up like await does.
func TestFinish_WithoutStartIsSafe(t *testing.T) {
	c := newTestClient(t, "eth0", &DHCPClientOptions{Once: false, MAC: mustMAC(t, "de:ad:be:ef:00:01")})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Finish(ctx); err != nil {
		t.Fatalf("Finish without Start: %v", err)
	}
}

// TestMountPrep_NamesEveryBinaryAbsolutely pins the property #707 was
// actually about, rather than the one instance of it that got fixed.
//
// The whole mountPrep string is handed to `sh -c`. A shell resolves
// EVERY command word through PATH, not only the one that reaches
// execve — so `$0` was never special, it was just the word the audit
// happened to be looking at. dhcpcdBin was absolutized on exactly that
// reasoning; the four commands in the same string are the same
// exposure by the same mechanism.
//
// Asserting the shape rather than the four spellings is deliberate.
// The literal pins in TestMountPrep_RemountsProcSysRW say the string
// has not drifted; they cannot say it is right, and before this change
// they pinned the bare forms — which is how a test written to stop
// drift ends up holding the wrong value still.
//
// # WHAT THIS TEST UNIQUELY CONTRIBUTES, AND WHAT IT DOES NOT
//
// Not much, and the honest statement is short. Mutate either of the two
// commands mountPrep names today back to a bare `mount` or `mkdir` and
// TestMountPrep_RemountsProcSysRW goes red as well, because its literal
// pins hold those exact spellings. Both tests catch both mutants.
//
// The one thing only this test catches is a command word the file has
// NEVER SEEN: a fifth command, added later, spelled bare. The pins
// cannot go red for a string they were never given, and that is the
// entire margin. It is worth a test because that is exactly how #707
// arrived — three times, each time as a word nobody had looked at yet.
//
// # THE SPLIT IS THE POINT
//
// FieldsFunc over `;`, `&` and `|`, not Split on `;`.
//
// Every one of those characters ENDS a command in sh, so the word after
// one is a fresh command word that the shell will resolve through PATH.
// Splitting on `;` alone reads `a && b` as a single statement and looks
// only at `a` — so a command appended with `&&`, `||`, `|` or `&` is
// invisible to it, and invisible in the specific way that reads as
// PASSING.
//
// That is the same defect as the one this test exists to catch, one
// level up. The audit looked at `$0` because `$0` was the word it was
// looking at; a Split(";") version of this test looks at the first word
// of each `;` because that is the word it is looking at. mountPrep uses
// only `;` today, so this change moves nothing — it removes the way the
// fourth instance would arrive.
//
// `exec` is skipped because it is a shell builtin: there is no PATH
// lookup to pin, and its argument is $0, which is dhcpcdBin.
// fdDupRedirect matches an fd-duplication redirection: `>&2`, `2>&1`,
// `1>&2`. Stripped before the split below.
//
// WHY, and it is not cosmetic. The splitter breaks on `&` — deliberately,
// so a command appended with `&&` or `||` stays visible. `>&2` contains
// one, so the shell operator gets torn in half and the `2` after it
// becomes the first word of a statement that does not exist. #780 added
// `|| /bin/echo '...' >&2` to mountPrep and this test reported four
// PHANTOM command words named "2".
//
// A gate that cries wolf gets discharged, and the discharge here would
// have been to drop the `&` from the splitter — which is exactly the
// blindness the comment above spends two paragraphs arguing against.
// Removing the redirection instead keeps `||` visible and removes only a
// token that cannot be a command word: an fd duplication has no filename
// and no argument, so nothing is hidden behind it.
//
// WHAT THIS STILL CANNOT READ, stated rather than assumed: a redirection
// TO A FILE (`> /tmp/x`) would leave `/tmp/x` as a phantom word. It
// would be reported, not hidden — a false positive, which is the safe
// direction for this gate — and mountPrep uses no such form.
var fdDupRedirect = regexp.MustCompile(`[0-9]*>&[0-9]+`)

// mountPrepCommandWords returns the words the shell would resolve as
// commands in a `sh -c` body.
//
// Extracted so the test below can run it against BOTH the real
// mountPrep() and a deliberately broken string. A scanner that is only
// ever pointed at correct input proves that the input is correct, not
// that the scanner works — and the phantom-word bug above is what a
// scanner nobody had aimed at a mutant looks like.
func mountPrepCommandWords(prep string) []string {
	var words []string
	for _, stmt := range strings.FieldsFunc(fdDupRedirect.ReplaceAllString(prep, ""), func(r rune) bool {
		return r == ';' || r == '&' || r == '|'
	}) {
		fields := strings.Fields(stmt)
		if len(fields) == 0 || fields[0] == "exec" {
			continue
		}
		words = append(words, fields[0])
	}
	return words
}

func TestMountPrep_NamesEveryBinaryAbsolutely(t *testing.T) {
	prep := mountPrep()
	words := mountPrepCommandWords(prep)
	for _, w := range words {
		if !strings.HasPrefix(w, "/") {
			t.Errorf("mountPrep runs %q, resolved through PATH by the shell; "+
				"name it absolutely as dhcpcdBin and unsharePath are\n---\n%s",
				w, prep)
		}
	}
	// The loop body is a rule about command words that exist, so it is
	// satisfied completely by there being none — which is what a
	// mountPrep rewritten into a form this splitter cannot read would
	// look like, and it would look green.
	//
	// Eight now, not four: #780 appends a reporting command to each
	// prepared mount, and a floor that did not move with them would have
	// gone on being satisfied by the four it already knew about.
	if len(words) < 8 {
		t.Errorf("found %d command words in mountPrep, want at least 8; either it stopped "+
			"preparing a mount it used to prepare, or it stopped reporting one that failed, "+
			"or it is now written in a form this test cannot read and is checking less than "+
			"it reports\n---\n%s", len(words), prep)
	}
}

// The scanner, aimed at input it must reject.
//
// Without this, stripping the redirection above is a change that makes a
// red test green with nothing establishing that it can still go red. The
// two cases are the two ways the strip could be wrong: too greedy (it
// eats a real command) and not greedy enough (the phantom survives).
func TestMountPrepCommandWords_SeesBareCommandsAndNotRedirections(t *testing.T) {
	// A bare command appended with `||` and followed by a redirection —
	// the exact shape #780 introduced — must still be reported.
	got := mountPrepCommandWords("/bin/mount -t tmpfs tmpfs /x || echo 'boom' >&2; exec \"$0\"")
	want := []string{"/bin/mount", "echo"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("command words = %q, want %q: the redirection strip must not hide the "+
			"command in front of it", got, want)
	}

	// And the redirection itself must contribute nothing.
	for _, w := range got {
		if w == "2" {
			t.Error("the fd number of a redirection is being read as a command word")
		}
	}
}

// TestMountPrep_DoesNotSwallowDiagnostics is the observer for the thing
// that had none.
//
// Every command in mountPrep carried `2>/dev/null` from the day it was
// written, and NOTHING in this package saw it. The literal pins in
// TestMountPrep_RemountsProcSysRW are `strings.Contains` checks on
// command-and-argument prefixes, so the redirection sat past the end of
// every pin: adding it or removing it left the entire suite green. It
// was removed on measurement, and this test exists so that removal is a
// decision rather than a state that can revert unnoticed.
//
// WHAT IT COSTS TO SWALLOW IT, measured rather than argued. mountPrep's
// four commands are separated by `;`, so a failure does not stop the
// chain, and the `exec` that follows is unconditional, so the exit
// status belongs to dhcpcd. Properties (1) and (2) — the private tmpfs
// over the state and run directories — have no downstream observer at
// all: if they do not land, dhcpcd runs correctly against the SHARED
// directories, which is the collision mountPrep exists to prevent, and
// reports nothing, because from dhcpcd's point of view nothing is
// wrong. Only property (3) has a fallback, since dhcpcd fails loudly on
// a blocked sysctl write.
//
// End to end, on the pinned base image: the shell inherits fd 2 from the
// unshare process and `exec` keeps its descriptors, and NewDHCPClient
// points that fd at io.MultiWriter(logrus-at-debug, the bounded stderr
// tail). With the redirection the parent captured an empty string; with
// it gone the parent captured `mount: can't find /proc/sys in
// /proc/mounts`.
//
// This deliberately does NOT assert that the commands are fatal. The
// remount at (3) legitimately fails under a --privileged runtime, where
// /proc/sys is not a separate mount and is already writable, so `set -e`
// would kill a client on a host that is fine. Audible, not fatal.
func TestMountPrep_DoesNotSwallowDiagnostics(t *testing.T) {
	prep := mountPrep()
	for _, swallow := range []string{
		"2>/dev/null",
		"2> /dev/null",
		"2>&-",
	} {
		if strings.Contains(prep, swallow) {
			t.Errorf("mountPrep redirects a command's stderr away with %q. A failed tmpfs "+
				"there is undetectable: the commands are `;`-separated so the chain "+
				"continues, `exec` is unconditional so the exit status is dhcpcd's, and "+
				"dhcpcd cannot tell it is using the shared state dir instead of a private "+
				"one. Leave the diagnostic on fd 2, which NewDHCPClient already tees into "+
				"the plugin log and the exit-error tail\n---\n%s", swallow, prep)
		}
	}
}

// TestGetIP_UnmanagedAdvertisementStopsRetrying pins the early exit
// #868 added to the retry loop.
//
// On a stateless or SLAAC segment there is no DHCPv6 address to get,
// ever. Retrying until the deadline reaches the same answer and spends
// the container's whole start budget doing it — Docker's client is
// waiting on CreateEndpoint the entire time — so an advertisement
// without the managed flag ends the loop on the spot.
//
// The assertion is the ATTEMPT COUNT, not the returned error: the error
// is the same ErrNoLease either way, so a test asserting only on it
// would pass just as well against a loop that spent the full budget.
func TestGetIP_UnmanagedAdvertisementStopsRetrying(t *testing.T) {
	attempts := 0
	stubAttemptGetIPRA(t, func(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, RAObservation, error) {
		attempts++
		return Info{}, RAObservation{Seen: true}, util.ErrNoLease
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, ra, err := GetIP(ctx, "eth0", &DHCPClientOptions{V6: true})
	if !errors.Is(err, util.ErrNoLease) {
		t.Errorf("err = %v, want ErrNoLease", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 — an advertisement without the managed flag is "+
			"a conclusive answer, and retrying it burns the container-start budget to "+
			"re-learn it", attempts)
	}
	if !ra.Seen || ra.Managed {
		t.Errorf("returned observation = %+v, want Seen without Managed — the caller "+
			"classifies the absence from this, so losing it turns a stateless segment "+
			"into an unexplained failure", ra)
	}
}

// TestGetIP_ManagedAdvertisementKeepsRetrying is the other direction of
// the same branch: a segment that DID advertise DHCPv6 addresses gets
// every attempt the budget allows, because a server that is slow or
// briefly unreachable is exactly what the retry loop exists for (#325).
//
// Without this case, an early exit keyed on `ra.Seen` alone — dropping
// the managed check — passes the test above and quietly turns every
// transient DHCPv6 outage into an immediate endpoint failure.
func TestGetIP_ManagedAdvertisementKeepsRetrying(t *testing.T) {
	attempts := 0
	stubAttemptGetIPRA(t, func(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, RAObservation, error) {
		attempts++
		return Info{}, RAObservation{Seen: true, Managed: true}, util.ErrNoLease
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	_, ra, _ := GetIP(ctx, "eth0", &DHCPClientOptions{V6: true})
	if attempts < 2 {
		t.Errorf("attempts = %d, want more than 1 — the segment advertised managed "+
			"DHCPv6, so silence is a failure worth retrying, not an answer", attempts)
	}
	if !ra.Managed {
		t.Errorf("returned observation = %+v, want Managed — this is the observation "+
			"that keeps a real DHCPv6 outage fatal", ra)
	}
}

// TestGetIP_ObservationSurvivesAnAttemptThatMissedIt pins the merge
// across attempts.
//
// Advertisements are periodic and an attempt can simply fall between
// two of them. What the segment advertised on the first try is still
// true on the third, so the observation accumulates rather than being
// overwritten — and the verdict the caller reaches is about the
// SEGMENT, not about the last attempt that happened to run.
//
// The managed flag is the one that matters here: an attempt that missed
// the advertisement, overwriting rather than merging, would report an
// unadvertised segment and make a real DHCPv6 outage tolerated.
func TestGetIP_ObservationSurvivesAnAttemptThatMissedIt(t *testing.T) {
	attempts := 0
	stubAttemptGetIPRA(t, func(ctx context.Context, iface string, opts *DHCPClientOptions) (Info, RAObservation, error) {
		attempts++
		if attempts == 1 {
			return Info{}, RAObservation{Seen: true, Managed: true}, util.ErrNoLease
		}
		return Info{}, RAObservation{}, util.ErrNoLease
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	_, ra, _ := GetIP(ctx, "eth0", &DHCPClientOptions{V6: true})
	if attempts < 2 {
		t.Fatalf("attempts = %d; this case needs at least two to say anything about "+
			"merging", attempts)
	}
	if !ra.Seen || !ra.Managed {
		t.Errorf("returned observation = %+v after a first attempt that saw a managed "+
			"advertisement and later ones that saw none; want both fields set", ra)
	}
}

// TestRAObservation_MergeIsSymmetricAndAccumulating covers Merge on its
// own, including the receiver-carries-it direction the loop above never
// exercises (the loop always merges INTO the accumulator).
func TestRAObservation_MergeIsSymmetricAndAccumulating(t *testing.T) {
	seen := RAObservation{Seen: true}
	managed := RAObservation{Seen: true, Managed: true}
	var zero RAObservation

	// The operands are stored rather than the computed value, so the
	// direction of each merge stays visible to the guard below. A table
	// holding only results cannot tell "seen into managed" from
	// "managed into seen" -- they produce the same value, which is
	// precisely the symmetry being claimed and therefore precisely what
	// a non-vacuity check must not have to infer.
	cases := []struct {
		name     string
		recv, in RAObservation
		want     RAObservation
	}{
		{"zero into managed", managed, zero, managed},
		{"managed into zero", zero, managed, managed},
		{"seen into managed", managed, seen, managed},
		{"managed into seen", seen, managed, managed},
		{"zero into zero", zero, zero, zero},
	}

	// Non-vacuity. A table is a universal, and shrinking one leaves the
	// lane green with nothing reporting the loss -- not the package, not
	// check-test-weakening.sh.
	//
	// Keyed on the ORDERED OPERAND PAIRS rather than on a row count,
	// because symmetry is the claim: a count of five is equally
	// satisfied by five copies of one direction, and the direction that
	// gets dropped is always the one where the receiver already carries
	// the flag.
	present := map[[2]RAObservation]bool{}
	for _, tc := range cases {
		present[[2]RAObservation{tc.recv, tc.in}] = true
	}
	for _, pair := range [][2]RAObservation{
		{managed, zero}, {zero, managed},
		{managed, seen}, {seen, managed},
		{zero, zero},
	} {
		if !present[pair] {
			t.Fatalf("the Merge table no longer covers %+v.Merge(%+v). Symmetry is "+
				"the claim, so every pair belongs in BOTH orders; a table holding one "+
				"direction asserts nothing about the other", pair[0], pair[1])
		}
	}

	for _, tc := range cases {
		if got := tc.recv.Merge(tc.in); got != tc.want {
			t.Errorf("%s: %+v.Merge(%+v) = %+v, want %+v", tc.name, tc.recv, tc.in, got, tc.want)
		}
	}
}

// TestObserveRA covers the one place a router advertisement's wire
// spelling becomes a verdict (#868).
//
// The flag strings are measured, not invented: dhcpcd 10.3.2 against
// dnsmasq 2.92 exports nd1_flags=MO on a managed segment, O on a
// stateless one and the empty string on plain SLAAC.
//
// The lowercase row is not a hypothetical. The check is a substring
// match, so it is exactly as strong as the case of one letter; pinning
// it says the matcher is deliberate rather than incidental, and a
// future switch to a case-insensitive match has to change this test
// on purpose.
func TestObserveRA(t *testing.T) {
	cases := []struct {
		name  string
		flags string
		want  RAObservation
	}{
		{"managed", "MO", RAObservation{Seen: true, Managed: true}},
		{"managed only", "M", RAObservation{Seen: true, Managed: true}},
		{"stateless", "O", RAObservation{Seen: true}},
		{"slaac", "", RAObservation{Seen: true}},
		{"unknown letters", "XY", RAObservation{Seen: true}},
		{"lowercase is not the flag", "mo", RAObservation{Seen: true}},
	}

	// Non-vacuity, keyed on the two verdicts rather than on the count
	// alone: this is the one place a wire spelling becomes a boolean, so
	// a table left with only managed rows -- or only unmanaged ones --
	// asserts nothing about the discrimination it exists to pin, and
	// nothing else in the package would say so.
	var managed, unmanaged int
	for _, tc := range cases {
		if tc.want.Managed {
			managed++
		} else {
			unmanaged++
		}
	}
	if len(cases) != 6 || managed < 1 || unmanaged < 1 {
		t.Fatalf("the flag table has %d rows (%d managed, %d not), want 6 with both "+
			"verdicts present — a table that reached only one verdict would pass "+
			"against a matcher that always returns it", len(cases), managed, unmanaged)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := observeRA(RAObservation{}, tc.flags); got != tc.want {
				t.Errorf("observeRA(zero, %q) = %+v, want %+v", tc.flags, got, tc.want)
			}
		})
	}
}

// TestObserveRA_DoesNotClearWhatAnEarlierAdvertisementSaid pins the
// accumulation inside one attempt, which the table above cannot see
// because it always starts from the zero value.
//
// A client receives several advertisements per acquisition — three in
// fifteen seconds, measured — and they need not be identical while a
// segment is being reconfigured. An implementation that ASSIGNED
// Managed instead of only setting it would let the last advertisement
// win, so one unflagged RA arriving after a managed one would make a
// real DHCPv6 outage tolerated.
func TestObserveRA_DoesNotClearWhatAnEarlierAdvertisementSaid(t *testing.T) {
	got := observeRA(observeRA(RAObservation{}, "MO"), "O")
	if !got.Managed {
		t.Errorf("got %+v after a managed advertisement followed by an unflagged one; "+
			"the managed observation must survive — it is what keeps a silent DHCPv6 "+
			"server fatal", got)
	}
}

// TestCollectAcquisition covers the fold from one acquisition's event
// stream to the pair the caller acts on: the lease, and what the
// segment advertised.
//
// It is the only unit-level reader of Event.RouterFlags on the
// acquisition path — the retry-loop tests above all stub attemptGetIP
// out, so without this the wire field could be ignored entirely and
// nothing in this package would notice.
func TestCollectAcquisition(t *testing.T) {
	// collect drains the events through the accumulator the production
	// path uses, and reports the same pair the old signature returned.
	collect := func(events ...Event) (*Info, RAObservation) {
		ch := make(chan Event, len(events))
		for _, e := range events {
			ch <- e
		}
		close(ch)
		var a acquisition
		collectAcquisition(ch, &a)
		return a.snapshot()
	}

	t.Run("a managed advertisement and no lease", func(t *testing.T) {
		last, ra := collect(
			Event{Type: "routeradvert", RouterFlags: "MO"},
			Event{Type: "routeradvert", RouterFlags: "MO"},
		)
		if last != nil {
			t.Errorf("lease = %+v, want none", *last)
		}
		if (ra != RAObservation{Seen: true, Managed: true}) {
			t.Errorf("observation = %+v, want a managed advertisement seen", ra)
		}
	})

	t.Run("an unflagged advertisement is still an advertisement", func(t *testing.T) {
		_, ra := collect(Event{Type: "routeradvert"})
		if !ra.Seen || ra.Managed {
			t.Errorf("observation = %+v, want Seen without Managed — a SLAAC segment "+
				"advertised, and reading that as silence sends an operator looking for "+
				"a router that is there", ra)
		}
	})

	t.Run("the last lease wins and the advertisement rides along", func(t *testing.T) {
		last, ra := collect(
			Event{Type: "routeradvert", RouterFlags: "MO"},
			Event{Type: "bound", Data: Info{IP: "2001:db8::1/64"}},
			Event{Type: "renew", Data: Info{IP: "2001:db8::2/64"}},
		)
		if last == nil {
			t.Fatal("lease = none, want the renew's address")
		}
		if last.IP != "2001:db8::2/64" {
			t.Errorf("lease IP = %q, want the LAST event's address", last.IP)
		}
		if !ra.Managed {
			t.Errorf("observation = %+v; a successful acquisition still reports what "+
				"the segment advertised", ra)
		}
	})

	t.Run("unrelated events change nothing", func(t *testing.T) {
		last, ra := collect(
			Event{Type: "config"},
			Event{Type: "leasefail"},
			Event{Type: "nak"},
		)
		if last != nil || ra.Seen || ra.Managed {
			t.Errorf("lease=%v observation=%+v, want neither — none of these events "+
				"says anything about an address or an advertisement", last, ra)
		}
	})
}

// TestAcquisition_ObservationIsReadableBeforeTheStreamCloses is the
// unit-level statement of the fail-open recorded on #868.
//
// The first version of this code accumulated the observation inside the
// collector goroutine and published it by sending on a channel when the
// event stream closed. Everything in this package stayed green, because
// every test fed a CLOSED channel — and the production case that
// decides whether an endpoint is refused is the one where the stream
// has NOT closed: dhcpcd on a managed segment with a silent server
// never exits on its own, so it is still running when the acquisition
// budget expires and the verdict has to be formed.
//
// So the property is not "the fold is correct" — that was already true
// and already tested. It is that the answer is available WITHOUT the
// stream having ended.
func TestAcquisition_ObservationIsReadableBeforeTheStreamCloses(t *testing.T) {
	var a acquisition

	if _, ra := a.snapshot(); ra.Seen || ra.Managed {
		t.Fatalf("a fresh accumulator reports %+v, want nothing observed", ra)
	}

	a.fold(Event{Type: "routeradvert", RouterFlags: "MO"})

	// No channel has been closed and no goroutine has finished. This is
	// exactly the state a managed-but-silent segment is in when the
	// acquisition times out.
	last, ra := a.snapshot()
	if !ra.Seen || !ra.Managed {
		t.Errorf("observation = %+v, want a managed advertisement — reading it as "+
			"absent is what let a container start on a managed network whose "+
			"DHCPv6 server answered nothing (#868)", ra)
	}
	if last != nil {
		t.Errorf("lease = %+v, want none — an advertisement is not a lease", *last)
	}
}

// TestSettleAcquisition_ReportsWhatWasFoldedEvenIfTheStreamNeverEnds
// drives the two ways the drain can go, and the second one is the
// production case.
//
// A stream that never ends is not a hypothetical: it is dhcpcd still
// soliciting when the budget runs out. Returning a zero observation
// there reads as "no router on this segment", which is a TOLERATED
// verdict — so the failure mode of getting this wrong is fail-open on
// the guard whose whole purpose is to stay closed.
func TestSettleAcquisition_ReportsWhatWasFoldedEvenIfTheStreamNeverEnds(t *testing.T) {
	fold := func(a *acquisition) {
		a.fold(Event{Type: "routeradvert", RouterFlags: "MO"})
		a.fold(Event{Type: "bound", Data: Info{IP: "2001:db8::1/64"}})
	}

	t.Run("the collector finished", func(t *testing.T) {
		var a acquisition
		fold(&a)
		collected := make(chan struct{})
		close(collected)

		// A grace long enough that taking it would be visible as a hang
		// rather than as a pass: the closed channel must be what
		// returns, not the timer.
		start := time.Now()
		last, ra := settleAcquisition(collected, &a, time.Minute)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("waited %v for an already-closed channel; the grace is being taken "+
				"unconditionally", elapsed)
		}
		if !ra.Managed || last == nil {
			t.Errorf("observation=%+v lease=%v, want both", ra, last)
		}
	})

	t.Run("the collector never finishes", func(t *testing.T) {
		var a acquisition
		fold(&a)
		never := make(chan struct{}) // deliberately never closed

		last, ra := settleAcquisition(never, &a, 10*time.Millisecond)
		if !ra.Seen || !ra.Managed {
			t.Errorf("observation = %+v, want the managed advertisement that was already "+
				"folded — a stream that has not ended is not evidence that nothing was "+
				"advertised, and treating it as such tolerates a DHCPv6 outage (#868)", ra)
		}
		if last == nil || last.IP != "2001:db8::1/64" {
			t.Errorf("lease = %v, want the folded lease — the same argument applies to "+
				"the address", last)
		}
	})
}

// TestSettleAcquisition_TakesNoContext pins the ABSENCE that the fix
// turns on.
//
// The bug was not a wrong bound, it was a bound that had already
// expired: the observation was read through a select whose other arm
// was ctx.Done(), on the one path that is only reached BECAUSE the
// context expired. Re-introducing a context here would restore that
// exactly, and it would look like a tidy-up.
//
// A signature is the only thing that can carry "must not be able to see
// the deadline", so it is asserted as a signature.
func TestSettleAcquisition_TakesNoContext(t *testing.T) {
	fn := reflect.TypeOf(settleAcquisition)
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	for i := 0; i < fn.NumIn(); i++ {
		if fn.In(i).Implements(ctxType) || fn.In(i) == ctxType {
			t.Fatalf("settleAcquisition takes a %v as argument %d. It must not: it is "+
				"reached with the acquisition context already expired, so a "+
				"context-bounded read there returns the zero observation and a managed "+
				"segment with a silent server becomes a running container (#868)",
				fn.In(i), i)
		}
	}
}
