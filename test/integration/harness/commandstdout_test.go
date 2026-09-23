// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"context"
	"strings"
	"testing"
)

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
