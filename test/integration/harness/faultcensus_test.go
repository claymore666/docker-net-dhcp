// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pluginSourceDir is the plugin package relative to this one.
const pluginSourceDir = "../../../pkg/plugin"

func TestFatalFaultSignaturesExistInPluginSource(t *testing.T) {
	src := readPluginSource(t)
	if len(fatalFaultSignatures) == 0 {
		t.Fatal("no signatures defined; FaultCensus would report every run clean")
	}
	for _, sig := range fatalFaultSignatures {
		if !strings.Contains(src, sig.msg) {
			t.Errorf("no line in %s contains %q (signature for %s).\n"+
				"The census matches on this substring, so a reworded log line turns it into a "+
				"permanent zero — and a zero here is reported as a clean whole-run verdict.",
				pluginSourceDir, sig.msg, sig.counter)
		}
	}
}

func TestFatalFaultSignaturesCoverEveryIncrementSite(t *testing.T) {
	src := readPluginSource(t)

	// join_start_failures is counted by JoinFailureCensus, not here.
	want := map[string]int{
		"recoveryFailed.Add(":         3,
		"tombstoneWriteFailures.Add(": 1,
	}
	// recovery_failed has five signatures for three Add sites: one Add is in the recordSyncFailure closure, reached from three log lines.
	for expr, n := range want {
		got := strings.Count(src, expr)
		if got != n {
			t.Errorf("%s appears %d time(s) in %s, expected %d.\n"+
				"If an increment site was added, give it a signature in "+
				"fatalFaultSignatures so the whole-run census can see it; the counter "+
				"alone only covers the stretch since the last plugin restart (#385).",
				expr, got, pluginSourceDir, n)
		}
	}
}

func TestFaultCensus_QuietOnACleanLog(t *testing.T) {
	n, report := FaultCensus([]byte("time=\"...\" level=info msg=\"all fine\"\nmore log\n"))
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
	if report != "" {
		t.Errorf("a clean run should print nothing, got:\n%s", report)
	}
}

func TestFaultCensus_CountsEverySignature(t *testing.T) {
	var lines []string
	for _, sig := range fatalFaultSignatures {
		lines = append(lines, `time="t" level=error msg="`+sig.msg+` something"`)
	}
	n, report := FaultCensus([]byte(strings.Join(lines, "\n")))
	if n != len(fatalFaultSignatures) {
		t.Errorf("count = %d, want %d — one per signature", n, len(fatalFaultSignatures))
	}
	for _, sig := range fatalFaultSignatures {
		if !strings.Contains(report, sig.counter) {
			t.Errorf("report omits the counter %q", sig.counter)
		}
	}
}

func TestFaultCensus_CountsRepeats(t *testing.T) {
	sig := fatalFaultSignatures[0]
	log := strings.Repeat(`msg="`+sig.msg+` x"`+"\n", 3)
	n, _ := FaultCensus([]byte(log))
	if n != 3 {
		t.Errorf("count = %d, want 3; a fault that recurs must not collapse to one", n)
	}
}

func TestFaultCensus_EmptyLogReportsZeroAndSaysNothing(t *testing.T) {
	n, report := FaultCensus(nil)
	if n != 0 || report != "" {
		t.Errorf("FaultCensus(nil) = (%d, %q); want (0, \"\") — the caller, not this "+
			"function, is responsible for noticing there was no log to read", n, report)
	}
}

func readPluginSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(pluginSourceDir)
	if err != nil {
		t.Fatalf("read %s: %v", pluginSourceDir, err)
	}
	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pluginSourceDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		t.Fatalf("no non-test sources found in %s; this guard would pass vacuously", pluginSourceDir)
	}
	return b.String()
}
