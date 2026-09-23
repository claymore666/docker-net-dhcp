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

// CommandStdout runs a command and returns its trimmed stdout, with stderr in the error. A cold module cache makes
// `go` write `go: downloading` to stderr, which CombinedOutput would fold into the value (#924).
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
