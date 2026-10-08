// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestDeleteNetwork_AnOptionsFileThatCannotBeRemovedIsWarnedNotReturned(t *testing.T) {
	p, _ := deferredPlugin(t, ReleaseNever)
	path, err := stateFilePath(deferredTestNetwork)
	if err != nil {
		t.Fatalf("stateFilePath: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("setup remove: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o700); err != nil {
		t.Fatalf("setup mkdir: %v", err)
	}
	if err := os.Remove(path); err == nil {
		t.Fatal("the seeded path was removable after all, so this case never exercised a failed removal")
	}

	hook := logtest.NewLocal(log.StandardLogger())
	defer hook.Reset()

	if err := p.DeleteNetwork(DeleteNetworkRequest{NetworkID: deferredTestNetwork}); err != nil {
		t.Fatalf("DeleteNetwork returned %v, want nil: a leftover options file is harmless", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("the path is gone (%v), so removal did not fail and the warn branch did not run", err)
	}
	warned := false
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "Failed to remove persisted options") {
			warned = true
		}
	}
	if !warned {
		t.Error("no \"Failed to remove persisted options\" warning was logged")
	}
}
