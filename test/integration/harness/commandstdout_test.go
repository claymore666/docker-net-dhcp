// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"strings"
	"testing"
)

// TestCommandStdout_StderrDoesNotReachTheValue drives the case the
// health document's `library` cell used to get wrong: a command that
// writes a diagnostic to stderr and the value to stdout.
//
// The mutant is CombinedOutput. Under it the returned string is
// "go: downloading …\nv0.1.0", the cell compares that against the
// plugin's label, and the failure names a version nobody wrote.
func TestCommandStdout_StderrDoesNotReachTheValue(t *testing.T) {
	got, err := CommandStdout(context.Background(), "sh", "-c",
		`echo "go: downloading github.com/example/mod v1.2.3" >&2; echo v0.1.0`)
	if err != nil {
		t.Fatalf("CommandStdout: %v", err)
	}
	if got != "v0.1.0" {
		t.Errorf("CommandStdout returned %q; want %q. A diagnostic on stderr reached the value", got, "v0.1.0")
	}
}

// The other direction: a command that FAILS must not hand back an empty
// value with a nil error, and what it wrote to stderr has to survive
// into the error -- a failure whose only explanation went to a
// discarded stream is the one nobody can act on.
func TestCommandStdout_FailureCarriesStderr(t *testing.T) {
	_, err := CommandStdout(context.Background(), "sh", "-c",
		`echo "no required module provides package" >&2; exit 1`)
	if err == nil {
		t.Fatal("CommandStdout returned no error for a command that exited 1")
	}
	if !strings.Contains(err.Error(), "no required module provides package") {
		t.Errorf("the error is %q and does not carry what the command wrote to stderr", err)
	}
}
