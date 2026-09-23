// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: this guard reads source and runs in the unit job.

package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// floorReader is the one file allowed to call PluginHealth: the floor's single end-of-run reading is not a delta (#405).
const floorReader = "healthfloor_test.go"

// Every other health read goes through CounterWindow: a before/after pair subtracts counters from two plugin processes when a recycle lands in between (#405).
func TestCounterWindow_NoDirectHealthReadsInSuite(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "*_test.go"))
	if err != nil {
		t.Fatalf("glob suite sources: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no ../*_test.go found; this guard would pass vacuously")
	}

	var sawFloor bool
	for _, f := range files {
		if filepath.Base(f) == floorReader {
			sawFloor = true
		}
	}
	if !sawFloor {
		t.Fatalf("the exempted file %s no longer exists under ../ — "+
			"either it moved (update floorReader) or the floor's read went away "+
			"(drop the exemption). A stale exemption hides whatever takes its place.", floorReader)
	}

	const direct = "harness.PluginHealth("
	for _, f := range files {
		if filepath.Base(f) == floorReader {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, direct) {
				continue
			}
			t.Errorf("%s:%d reads plugin health directly:\n\t%s\n"+
				"Use harness.BeginCounterWindow(...) and End(), or Await() for a "+
				"poll-until-condition. A hand-rolled before/after pair subtracts two "+
				"numbers that may come from different plugin processes: the counters "+
				"are in-memory and reset with the plugin, and three tests in this suite "+
				"end it on purpose. Such a delta reads as \"no change\" (#405). "+
				"For a bare readiness poll after a deliberate recycle, use "+
				"harness.WaitPluginHealth, which makes no claim about counters.",
				filepath.Base(f), i+1, strings.TrimSpace(line))
		}
	}
}

func TestCounterWindow_GuardWouldCatchTheOldPattern(t *testing.T) {
	const direct = "harness.PluginHealth("

	shouldFlag := []string{
		"\tbefore, err := harness.PluginHealth(ctx, cli)",
		"\t\th, err := harness.PluginHealth(ctx, cli2)",
		"\tif _, err := harness.PluginHealth(ctx, cli); err == nil {",
	}
	for _, line := range shouldFlag {
		if !strings.Contains(line, direct) {
			t.Errorf("the detector misses a real occurrence of the banned pattern: %q", line)
		}
	}

	shouldNotFlag := []string{
		"\tw := harness.BeginCounterWindow(t, ctx, cli, \"leases_obtained\")",
		"\tbefore, after := w.End()",
		"\tharness.WaitPluginHealth(t, ctx, cli, 15*time.Second)",
		"\t// PluginHealth is what the window calls internally.",
	}
	for _, line := range shouldNotFlag {
		if strings.Contains(line, direct) {
			t.Errorf("the detector flags a line it should accept: %q", line)
		}
	}
}
