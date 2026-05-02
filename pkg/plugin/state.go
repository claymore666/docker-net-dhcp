package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// stateDir is the directory where per-network options are persisted.
// Lives inside the plugin's writable filesystem; survives plugin
// disable/enable cycles but is reset on `docker plugin rm` or upgrade,
// which is fine — the disk-state read in netOptions falls back to the
// docker API for any network that hasn't been re-saved yet.
//
// Configurable via the STATE_DIR env var so test runs can point at a
// scratch directory.
var stateDir = func() string {
	if d := os.Getenv("STATE_DIR"); d != "" {
		return d
	}
	return "/var/lib/net-dhcp"
}()

// stateFilePath returns the on-disk path for a given network's options.
func stateFilePath(networkID string) string {
	return filepath.Join(stateDir, networkID+".json")
}

// saveOptions persists the decoded options for a network. The first call
// creates the state directory if it doesn't already exist (the Dockerfile
// pre-creates it, but a fresh test environment won't).
//
// We write the file directly rather than via temp+rename because a
// torn write becomes invalid JSON, which loadOptions reports as
// "missing", which sends netOptions down the docker-API fallback path
// — i.e. the failure mode is "lose the optimization", not corruption.
func saveOptions(networkID string, opts DHCPNetworkOptions) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("failed to create state dir %v: %w", stateDir, err)
	}
	data, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("failed to encode options: %w", err)
	}
	if err := os.WriteFile(stateFilePath(networkID), data, 0o644); err != nil {
		return fmt.Errorf("failed to write options file: %w", err)
	}
	return nil
}

// loadOptions reads previously-persisted options for a network. Returns
// os.ErrNotExist (wrapped) when no state file is present so callers can
// fall back to other sources (e.g. the docker API).
func loadOptions(networkID string) (DHCPNetworkOptions, error) {
	var opts DHCPNetworkOptions
	data, err := os.ReadFile(stateFilePath(networkID))
	if err != nil {
		return opts, err
	}
	if err := json.Unmarshal(data, &opts); err != nil {
		return opts, fmt.Errorf("persisted options for %v are corrupt: %w", networkID, err)
	}
	return opts, nil
}

// deleteOptions removes the persisted options for a network. Called from
// DeleteNetwork. A "not found" error is treated as success since it
// just means we never persisted state for this network in the first
// place (e.g. created before we shipped persistence).
func deleteOptions(networkID string) error {
	if err := os.Remove(stateFilePath(networkID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove options file: %w", err)
	}
	return nil
}
