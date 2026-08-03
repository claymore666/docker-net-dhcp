// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No `//go:build integration` tag, deliberately. This guard reads
// source files and needs neither root nor a live plugin, so it belongs
// in the ordinary `go test ./...` job where it fails in seconds rather
// than after a twelve-minute suite. Same reasoning healthfloor.go
// documents for itself.

package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// floorReader is the one file allowed to call PluginHealth directly.
//
// The health floor takes a single end-of-run reading in TestMain after
// m.Run(). It is not a delta and has no window to belong to; it is the
// thing that reports what the counters ended at. Everything else in the
// suite is measuring a change and must say so through CounterWindow.
const floorReader = "healthfloor_test.go"

// TestCounterWindow_NoDirectHealthReadsInSuite is what actually holds
// the line for #405.
//
// The window type is only worth having if every measurement site uses
// it, and the natural thing to write in a new test is the pair that was
// there before:
//
//	before, _ := harness.PluginHealth(ctx, cli)
//	... exercise ...
//	after, _ := harness.PluginHealth(ctx, cli)
//	if after.X-before.X != 1 { ... }
//
// That compiles, passes, and silently subtracts two numbers from
// different plugin processes whenever a recycle lands in between —
// which is how twenty-nine sites came to exist. A reviewer cannot be
// expected to catch the thirtieth.
//
// Static rather than behavioural on purpose: reproducing the fault
// needs a plugin restart mid-measurement, which is expensive to stage
// and, being timing-dependent, would not reliably fail even then. The
// property being defended here is textual, so check it textually.
func TestCounterWindow_NoDirectHealthReadsInSuite(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "*_test.go"))
	if err != nil {
		t.Fatalf("glob suite sources: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no ../*_test.go found; this guard would pass vacuously")
	}

	// Prove the exemption still names a real file. If healthfloor_test.go
	// were renamed, a silently-unused exemption would leave the guard
	// looking fine while the floor's own call went unaccounted for.
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

// TestCounterWindow_GuardWouldCatchTheOldPattern is the negative
// control for the guard above.
//
// A guard that has never been observed rejecting anything is not known
// to work — and this repo has already shipped one that passed with the
// call it was guarding deleted, caught only by running the control.
// Rather than temporarily corrupting a real file, this feeds the
// detector the exact text it exists to reject.
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
