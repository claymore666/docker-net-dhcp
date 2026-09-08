// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CommandStdout runs a command and returns its trimmed STANDARD OUTPUT.
//
// THE STREAM SEPARATION IS THE POINT, and it is why this is a function
// rather than a CombinedOutput call at the one call site that needs it.
//
// A cell that derives an expected VALUE from a command must read only
// the stream the value is written on. CombinedOutput folds stderr in:
// `go` writes `go: downloading …` and toolchain notices there whenever
// the module cache is cold, and since the module import the runner
// image bakes no copy of the library, so a cold cache is the ordinary
// state rather than an exotic one. Folded in, the comparison fails
// naming a value nobody wrote, and the message accuses the plugin of a
// mismatch the derivation invented.
//
// stderr is not discarded. It goes into the error, where it explains a
// failure instead of becoming one.
func CommandStdout(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		if e := strings.TrimSpace(errb.String()); e != "" {
			return "", fmt.Errorf("%s: %w: %s", name, err, e)
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}
