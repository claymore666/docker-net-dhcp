// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// The plugin log lives in the plugin rootfs, which Docker destroys on
// every `plugin rm` / `install` — the supported upgrade path. So every
// upgrade took production's whole plugin history with it, at exactly
// the moment an operator would want the previous version's log. A
// v1.4.0 production upgrade lost the outgoing plugin's evidence before
// anyone could read it (#420).

func TestPluginLogWriter_WritesToBothSinks(t *testing.T) {
	var stdout, file bytes.Buffer
	w := pluginLogWriter(&stdout, &file)
	if _, err := w.Write([]byte("lease bound\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(stdout.String(), "lease bound") {
		t.Error("nothing reached stdout — dockerd captures this into the daemon log, " +
			"which is the only copy that survives an upgrade")
	}
	if !strings.Contains(file.String(), "lease bound") {
		t.Error("nothing reached the log file — harness.PluginLog reads it, and it is the " +
			"input to the whole-run fault census that gates every integration run (#385)")
	}
}

// Losing either sink is a distinct regression, so each is pinned
// separately rather than by one combined assertion that could pass on
// half a fix.
func TestPluginLogWriter_ToleratesAMissingSink(t *testing.T) {
	var buf bytes.Buffer
	if w := pluginLogWriter(&buf, nil); w != &buf {
		t.Error("with no log file, the writer should be stdout alone")
	}
	if w := pluginLogWriter(nil, &buf); w != &buf {
		t.Error("with no stdout, the writer should be the file alone")
	}
}

// TestMainUsesPluginLogWriter pins the wiring. The function above can
// be perfect and unused: the log setup lives in main(), which no unit
// test can drive, so this is the only thing standing between a correct
// helper and a plugin that still writes to one sink.
func TestMainUsesPluginLogWriter(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	const call = "log.StandardLogger().SetOutput(pluginLogWriter(os.Stdout, f))"
	if !strings.Contains(string(src), call) {
		t.Errorf("main.go no longer routes logrus through pluginLogWriter.\n"+
			"Expected: %s\n"+
			"If it writes straight to the file again, every plugin upgrade resumes "+
			"destroying production's log history (#420).", call)
	}
}
