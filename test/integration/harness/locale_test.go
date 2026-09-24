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

// Every exec.Command, with no allowlist: a list of which binaries count missed kea, whose launch names a variable (#869).
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

	if checked == 0 {
		t.Fatalf("found no exec.Command call sites in %d source file(s); the check above has "+
			"an empty domain and proves nothing", len(files))
	}
	t.Logf("checked %d fixture subprocess launch site(s)", checked)
}

func TestWithCLocale_SetsTheLocaleAndKeepsTheEnvironment(t *testing.T) {
	// Set here so the assertion does not depend on the runner's environment.
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

	// LC_ALL comes after any inherited LC_ALL; os/exec keeps the last duplicate. A host exporting a German LC_ALL is the case (#869).
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

// execCommandForTest returns a command whose Env is inspected and which is never run.
func execCommandForTest() *exec.Cmd { return exec.Command("/bin/true") }
