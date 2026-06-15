package udhcpc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		if c.fifo != nil {
			_ = c.fifo.Close()
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
		"-rf",                // flag-shaped
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
		if c.fifo != nil {
			_ = c.fifo.Close()
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
	if c.cmd.Args[0] != "unshare" || !hasArg(c.cmd.Args, "-m") {
		t.Errorf("expected unshare -m wrapper; args: %v", c.cmd.Args)
	}
	for _, want := range []string{"dhcpcd", "--noconfigure", "-4", "eth0", DefaultHandler} {
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

func TestNewDHCPClient_PersistentReleasesOnStop(t *testing.T) {
	c := newTestClient(t, "eth0", &DHCPClientOptions{Once: false, MAC: mustMAC(t, "de:ad:be:ef:00:01")})
	if hasArg(c.cmd.Args, "-1") || hasArg(c.cmd.Args, "-p") {
		t.Errorf("persistent client must omit -1/-p (releases on stop); args: %v", c.cmd.Args)
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
		"mount -t tmpfs tmpfs " + dhcpcdStateDir,
		"mount -o remount,bind,rw " + procSysPath,
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
