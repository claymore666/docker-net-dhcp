package gatetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// leakEnv marks the re-executed child of TestRunRejectsGateOutputCarryingTheRoot.
const leakEnv = "GATETEST_ROOT_LEAK_CHILD"

// fake writes an executable standing in for a gate. Run invokes it as
// `bin -root <root>`, so "$2" inside body is the fixture root.
func fake(t *testing.T, body string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fakegate")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing the stand-in gate: %v", err)
	}
	return bin
}

// TestRunRejectsGateOutputCarryingTheRoot drives the absence of the guard in
// Run, which is the fix for review finding 2.
//
// The finding: six self-test cases asserted a rule had fired by looking for a
// substring in the gate's output, and the gate's output contained the
// fixture's t.TempDir() path — which is named after the subtest. So a case
// named "third_party" was satisfied by the words "third_party" in the temp
// directory, not by the diagnosis. Every one of those assertions could have
// been satisfied with the rule deleted.
//
// The fix was made at the source (the gates now emit root-relative positions)
// and again here, at the one place every self-test passes through, because a
// source fix holds only until the next diagnostic is written.
//
// A guard that is never made to fire is indistinguishable from a deleted one,
// and Run reports through t.Fatalf, which cannot be observed from inside the
// same test binary. So the failing half runs in a re-executed child.
//
// What this CANNOT see: it proves the guard fires on output that is exactly
// the root, and that it says why. It does not establish that every diagnostic
// a gate could emit is root-free — that is what the gates' own cases and the
// scan.Rel change are for, and Run is the backstop under them, not a proof
// about them.
func TestRunRejectsGateOutputCarryingTheRoot(t *testing.T) {
	if os.Getenv(leakEnv) == "1" {
		// The child. Run must abort this test; if the guard is gone, it
		// returns normally, the child exits 0, and the parent below fails.
		Run(t, fake(t, `echo "$2"`), t.TempDir())
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), leakEnv+"=1")
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("Run accepted a gate whose entire output was the fixture root. "+
			"The guard that makes a leaked root a failure is not in force, so every "+
			"substring assertion in the self-tests can again be satisfied by the "+
			"subtest's own name.\nchild output:\n%s", out)
	}
	if !strings.Contains(string(out), "gate output contains the fixture root") {
		t.Fatalf("the child failed, but not with the guard's diagnosis — so this test "+
			"is not measuring the guard.\nchild output:\n%s", out)
	}
}

// TestRunAcceptsGateOutputWithoutTheRoot is the preservation control for the
// case above. A guard fails in one direction; without this, deleting the whole
// of Run's body would also turn the test above green in the child (it would
// return normally... and the parent would then fail) — but a guard rewritten
// to fire on ANY output would pass the case above and break every real gate
// test. This pins the other direction directly.
func TestRunAcceptsGateOutputWithoutTheRoot(t *testing.T) {
	root := t.TempDir()
	code, out := Run(t, fake(t, `echo "T PASS: 3 file(s) checked"`), root)
	if code != Pass {
		t.Fatalf("exit %d, want %d", code, Pass)
	}
	if !strings.Contains(out, "3 file(s) checked") {
		t.Fatalf("Run did not return the gate's output: %q", out)
	}
}

// TestRunReportsTheGateExitCode pins the mapping Run exists to carry. The
// gates report three outcomes through three exit codes and verify.sh
// distinguishes them; a Run that collapsed them would make every red-then-green
// case in this repository unable to tell a violation from a refusal.
func TestRunReportsTheGateExitCode(t *testing.T) {
	for _, want := range []int{Pass, Violate, Refuse} {
		code, _ := Run(t, fake(t, "exit "+string(rune('0'+want))), t.TempDir())
		if code != want {
			t.Errorf("Run returned %d for a gate exiting %d", code, want)
		}
	}
}
