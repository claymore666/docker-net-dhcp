// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

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

func TestPluginLogWriter_ToleratesAMissingSink(t *testing.T) {
	var buf bytes.Buffer
	if w := pluginLogWriter(&buf, nil); w != &buf {
		t.Error("with no log file, the writer should be stdout alone")
	}
	if w := pluginLogWriter(nil, &buf); w != &buf {
		t.Error("with no stdout, the writer should be the file alone")
	}
}

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
