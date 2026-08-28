// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every DHCP server this harness starts must be started through
// withCLocale, and this file is the check that says so.
//
// It exists because the failure it prevents is silent in the expensive
// direction. A fixture server left on the host's locale still starts,
// still leases, and still logs — in another language. What breaks is
// everything that READS the log: waitChallengerReady matched English
// text and failed five server-policy tests against a healthy server,
// and the #800 absence assertions ("zero DHCPRELEASE lines for this
// address") would have gone the other way and passed vacuously, because
// a matcher that recognises nothing and a log that contains nothing
// produce the same number.
//
// A comment saying "remember LC_ALL" would decay. This goes red.

// TestEveryFixtureSubprocessIsPinnedToTheCLocale reads this package's
// own source and requires every exec.Command to go through withCLocale.
//
// EVERY one, with no allowlist, and that is a deliberate choice over the
// narrower rule "every DHCP server". The narrow rule needs a list of
// which binaries count, and that list is exactly the thing that goes
// stale: the first draft of this check classified by binary name,
// flagged an `ip` and a `cat` it had no claim about, and would have
// missed kea entirely because the launch site names a variable rather
// than the constant. A universal with nothing to keep current cannot
// drift, and pinning a locale on a subprocess that does not need one
// costs nothing.
//
// WHY IT EXISTS. The failure it prevents is silent in the expensive
// direction. A fixture server left on the host's locale still starts,
// still leases, and still logs — in another language. What breaks is
// everything that READS the log: waitChallengerReady matches English
// text and failed five server-policy tests against a healthy server on
// a German host, and the #800 absence assertions ("zero DHCPRELEASE
// lines for this address") would have failed the other way and passed
// VACUOUSLY, because a matcher that recognises nothing and a log that
// contains nothing produce the same number. The canned log in
// releasematcher_test.go is English, so the control and its subject
// would have drifted apart by locale alone.
//
// Source inspection rather than behaviour because the behavioural test
// would need root, real interfaces, and a translated locale actually
// installed on the runner. The behavioural half — that the helper this
// insists on does something — is
// TestWithCLocale_SetsTheLocaleAndKeepsTheEnvironment below.
func TestEveryFixtureSubprocessIsPinnedToTheCLocale(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	launch := regexp.MustCompile(`exec\.Command\(`)

	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !launch.MatchString(line) {
				continue
			}
			checked++
			if !strings.Contains(line, "withCLocale(exec.Command(") {
				t.Errorf("%s:%d starts a fixture subprocess without withCLocale:\n\t%s\n"+
					"An unpinned process speaks the host's language. Anything that reads its "+
					"output then matches nothing, which reads as a healthy absence rather "+
					"than as a broken check.", f, i+1, strings.TrimSpace(line))
			}
		}
	}

	// Without this the loop above is satisfied by finding nothing at
	// all: move every launch behind a helper this scan does not
	// recognise, and a green run would mean "nothing is unpinned" only
	// because nothing was seen.
	if checked == 0 {
		t.Fatalf("found no exec.Command call sites in %d source file(s); the check above has "+
			"an empty domain and proves nothing", len(files))
	}
	t.Logf("checked %d fixture subprocess launch site(s)", checked)
}

// TestWithCLocale_SetsTheLocaleAndKeepsTheEnvironment is the other
// half: that the helper the scan insists on actually does something.
//
// A scan for a wrapper that had been quietly turned into a pass-through
// would stay green, which is the same shape of blindness this file was
// written about.
func TestWithCLocale_SetsTheLocaleAndKeepsTheEnvironment(t *testing.T) {
	// A variable the child must still inherit. Set here rather than
	// picked from the ambient environment so the assertion does not
	// depend on what the runner happens to export.
	t.Setenv("DH_ITEST_LOCALE_CANARY", "kept")

	cmd := withCLocale(execCommandForTest())

	var sawLCAll, sawCanary bool
	for _, kv := range cmd.Env {
		switch kv {
		case "LC_ALL=C":
			sawLCAll = true
		case "DH_ITEST_LOCALE_CANARY=kept":
			sawCanary = true
		}
	}
	if !sawLCAll {
		t.Errorf("withCLocale did not set LC_ALL=C; env=%v", cmd.Env)
	}
	if !sawCanary {
		t.Error("withCLocale dropped the inherited environment; a fixture server started " +
			"through it would lose PATH and everything else the runner set")
	}

	// LC_ALL must come after any inherited LC_ALL, since the last
	// assignment wins in execve. A host that exports LC_ALL=de_DE.UTF-8
	// is exactly the case this is for.
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	cmd = withCLocale(execCommandForTest())
	last := ""
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "LC_ALL=") {
			last = kv
		}
	}
	if last != "LC_ALL=C" {
		t.Errorf("with LC_ALL=de_DE.UTF-8 already in the environment, the effective value is "+
			"%q, want \"LC_ALL=C\" — the override must come last", last)
	}
}

// execCommandForTest is a trivial command to hang an environment off.
// /bin/true is never run here — only cmd.Env is inspected.
func execCommandForTest() *exec.Cmd { return exec.Command("/bin/true") }
