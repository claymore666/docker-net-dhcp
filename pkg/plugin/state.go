package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
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

// tombstoneTTL bounds how long a recently-deleted endpoint's MAC is
// available for inheritance by the next CreateEndpoint on the same
// network. `docker restart` issues Delete then Create back-to-back —
// well under a second in practice — so 10s is generous headroom while
// still expiring stale entries quickly.
const tombstoneTTL = 10 * time.Second

// tombstone records the MAC of an endpoint at DeleteEndpoint time so
// the next CreateEndpoint on the same NetworkID within tombstoneTTL
// can inherit it. This is the only mechanism we have for MAC
// stability across `docker restart` on Docker 26.x: the daemon
// destroys the old endpoint and creates a new one with a fresh
// EndpointID, breaking any per-endpoint key. The "same network +
// recent" heuristic catches the sequential-restart case (which is
// the common one). Concurrent restarts of multiple containers on the
// same network within the TTL fall through to a fresh MAC because
// consumeTombstone requires exactly one match.
type tombstone struct {
	NetworkID  string    `json:"network_id"`
	MacAddress string    `json:"mac_address"`
	// IPAddress, when non-empty, is the bare IPv4 address (no /mask)
	// from the previous endpoint's lease. The next CreateEndpoint
	// passes it to udhcpc as `-r ADDR` so the upstream DHCP server
	// can ACK the same lease back to the same MAC. Empty means
	// "do an unhinted DISCOVER".
	IPAddress string    `json:"ip_address,omitempty"`
	DeletedAt time.Time `json:"deleted_at"`
}

// tombstoneFilePath returns the on-disk path for the tombstone list.
// One file holds all tombstones — there's never more than a handful
// alive at once and the prune-on-write strategy keeps it bounded.
func tombstoneFilePath() string {
	return filepath.Join(stateDir, "tombstones.json")
}

// loadTombstones reads the tombstone list from disk, returning an
// empty slice when no file exists yet. A corrupt file is treated as
// fatal-ish — we surface the parse error so a higher layer can decide
// whether to log+continue or bail.
func loadTombstones() ([]tombstone, error) {
	data, err := os.ReadFile(tombstoneFilePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ts []tombstone
	if err := json.Unmarshal(data, &ts); err != nil {
		return nil, fmt.Errorf("tombstones file is corrupt: %w", err)
	}
	return ts, nil
}

// saveTombstones atomically rewrites the tombstone list. Same
// temp-file + rename pattern as saveOptions so a crash mid-write
// leaves the previous file intact.
func saveTombstones(ts []tombstone) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("failed to create state dir %v: %w", stateDir, err)
	}
	data, err := json.Marshal(ts)
	if err != nil {
		return fmt.Errorf("failed to encode tombstones: %w", err)
	}
	final := tombstoneFilePath()
	tmp, err := os.CreateTemp(stateDir, ".tombstones.*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create tombstones temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to write tombstones temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to close tombstones temp file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to chmod tombstones temp file: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to rename tombstones file: %w", err)
	}
	return nil
}

// pruneTombstones returns ts with entries older than tombstoneTTL
// removed. A new slice is returned so the caller's view is never
// surprise-aliased.
func pruneTombstones(ts []tombstone) []tombstone {
	now := time.Now()
	out := make([]tombstone, 0, len(ts))
	for _, t := range ts {
		if now.Sub(t.DeletedAt) < tombstoneTTL {
			out = append(out, t)
		}
	}
	return out
}

// saveOptions persists the decoded options for a network. The first call
// creates the state directory if it doesn't already exist (the Dockerfile
// pre-creates it, but a fresh test environment won't).
//
// Writes are atomic via temp-file + rename so that a crash mid-write
// either leaves the previous file intact or no file at all — never a
// partial/torn JSON. (The earlier non-atomic implementation depended on
// loadOptions falling back to the docker API on parse error, which
// works but is the wrong default.)
func saveOptions(networkID string, opts DHCPNetworkOptions) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("failed to create state dir %v: %w", stateDir, err)
	}
	data, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("failed to encode options: %w", err)
	}
	final := stateFilePath(networkID)
	tmp, err := os.CreateTemp(stateDir, "."+networkID+".*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp options file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to write temp options file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to close temp options file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to chmod temp options file: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to rename options file into place: %w", err)
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
