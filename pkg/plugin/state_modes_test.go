// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestStateFiles_AreNotWorldReadable pins the mode of every file the
// plugin creates under stateDir.
//
// /var/lib/net-dhcp is an rbind rw HOST mount, so at 0644 the container
// MACs, IPs, hostnames and the whole lease audit trail were readable by
// any user on the host. Nothing stored is a credential, so this is not a
// privilege boundary -- it is simply free, and a mode nobody asserted is
// a mode that drifts (#708).
func TestStateFiles_AreNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	withStateDir(t, dir)

	if err := saveOptions("net1", DHCPNetworkOptions{Bridge: "br0"}); err != nil {
		t.Fatalf("saveOptions: %v", err)
	}
	if err := saveTombstones([]tombstone{{
		NetworkID: "net1", Hostname: "web1", MacAddress: "02:bb:b5:d1:0c:0a", IPAddress: "192.168.99.50", DeletedAt: time.Now(),
	}}); err != nil {
		t.Fatalf("saveTombstones: %v", err)
	}

	var failures atomic.Int32
	l := newLeaseLedger(filepath.Join(dir, ledgerFileName), &failures)
	l.Append(ledgerEntry{Kind: "bound", Network: "net1", Endpoint: "ep1", IP: "192.168.99.50"})
	if failures.Load() != 0 {
		t.Fatalf("ledger append failed %d time(s)", failures.Load())
	}

	for _, name := range []string{"net1.json", "tombstones.json", ledgerFileName} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("stat %s: %v", name, err)
			continue
		}
		// The literal, not stateFileMode: asserting a constant against
		// itself passes whatever the constant becomes.
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %#o, want 0600", name, got)
		}
	}
}

// TestLedger_TightensAnExistingWorldReadableFile is the upgrade path.
// O_CREATE's mode applies only when the file is created, and this ledger
// outlives upgrades on a host bind mount -- so without the explicit
// chmod, a file written by an older version stays 0644 forever and the
// fix would be invisible on every host that already ran the plugin.
func TestLedger_TightensAnExistingWorldReadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ledgerFileName)
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	var failures atomic.Int32
	newLeaseLedger(path, &failures).Append(ledgerEntry{Kind: "bound", Network: "n", Endpoint: "e"})

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("pre-existing ledger mode = %#o, want 0600", got)
	}
}
