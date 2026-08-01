package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pluginSourceDir is where the plugin's own code lives, relative to
// this package.
const pluginSourceDir = "../../../pkg/plugin"

// TestFatalFaultSignaturesExistInPluginSource is the load-bearing test
// in this file.
//
// FaultCensus recognises a fault by a substring of the line the plugin
// logs. Reword that line and the census silently returns zero — and a
// zero from this census is read as "the run was clean over its whole
// length", which is the strongest claim the floor makes. Absence of
// evidence would arrive wearing the costume of evidence of absence,
// which is the exact failure #385 and #377 are both about.
//
// So every signature is pinned against the plugin's source. If this
// fails, either the log line moved (update the signature) or the fault
// path was removed (drop it) — but it can no longer happen quietly.
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

// TestFatalFaultSignaturesCoverEveryIncrementSite checks the other
// direction: that no healthy-affecting counter gained an increment the
// census cannot see.
//
// The signature list is a claim about how many distinct ways each
// counter can move. A new increment site with a new log line would
// leave the census under-reporting while still looking healthy.
func TestFatalFaultSignaturesCoverEveryIncrementSite(t *testing.T) {
	src := readPluginSource(t)

	// join_start_failures is counted by JoinFailureCensus, not here.
	want := map[string]int{
		"recoveryFailed.Add(":         3,
		"tombstoneWriteFailures.Add(": 1,
	}
	// recoveryFailed has three literal Add sites, but one of them is
	// inside the recordSyncFailure closure, reached from three call
	// sites with three distinct log lines — hence five recovery_failed
	// signatures against three Add sites. Spelled out because the
	// mismatch looks like a bug otherwise.
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

// An empty log is not a clean run. It is a run whose evidence is
// missing, and the caller has to be able to tell those apart — the
// count alone cannot, so the floor treats an unreadable log as a fault
// before it ever gets here. This pins the boundary of what FaultCensus
// itself promises.
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
