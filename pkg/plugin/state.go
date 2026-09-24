// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// stateFileMode is 0600: /var/lib/net-dhcp is an rbind rw host mount, and 0644 exposed MACs, IPs and the ledger to
// every host user (#708). A file not rewritten kept 0644 after an upgrade to v1.8.0 (#804), so
// sweepStateDirModes tightens older files once per start.
const stateFileMode = 0o600

// stateSchemaVersion is stamped as an extra flat key on the options file, which crosses builds on a host mount
// (#440); a file without it reads as v1, and an unknown version is refused in favour of the docker API.
const stateSchemaVersion = 2

// stateSchemaVersionBase keeps a network with no pool binding at v1, byte-identical to older builds; v2 marks an IPAM
// binding (#110).
const stateSchemaVersionBase = 1

type syncPolicy int

const (
	syncDurable syncPolicy = iota

	// syncEphemeral skips fsync for tombstones.json: tombstoneTTL is 60 s, and no host reboots and reaches this file
	// within 60 s of a power cut, so a durable entry would prune as stale anyway (#724).
	syncEphemeral
)

// syncDir makes a rename durable: after a power cut the entry can be absent or name an empty file (#724).
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("failed to open state dir %v for sync: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("failed to sync state dir %v: %w", dir, err)
	}
	return nil
}

func writeStateFileAtomic(final, pattern string, data []byte, what string, sync syncPolicy) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("failed to create state dir %v: %w", stateDir, err)
	}
	tmp, err := os.CreateTemp(stateDir, pattern)
	if err != nil {
		return fmt.Errorf("failed to create %s temp file: %w", what, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to write %s temp file: %w", what, err)
	}
	if sync == syncDurable {
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
			return fmt.Errorf("failed to sync %s temp file: %w", what, err)
		}
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to close %s temp file: %w", what, err)
	}
	// 0600: stateDir is an rbind rw host mount (#708).
	if err := os.Chmod(tmpName, stateFileMode); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to chmod %s temp file: %w", what, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to rename %s file into place: %w", what, err)
	}
	if sync == syncDurable {
		if err := syncDir(filepath.Dir(final)); err != nil {
			return fmt.Errorf("failed to sync %s file into place: %w", what, err)
		}
	}
	return nil
}

// stateDir is the host path config.json bind-mounts rbind rw at the same path, so it survives plugin disable,
// `docker plugin rm` and upgrade (#440). STATE_DIR overrides it for tests.
var stateDir = func() string {
	if d := os.Getenv("STATE_DIR"); d != "" {
		return d
	}
	return manifestStateDir
}()

const manifestStateDir = "/var/lib/net-dhcp"

// warnIfStateDirIsNotThePersistentOne warns once when STATE_DIR is not the bind mount, which voids the durability
// above (#724).
func warnIfStateDirIsNotThePersistentOne() {
	if stateDir == manifestStateDir {
		return
	}
	log.WithFields(log.Fields{
		"state_dir": stateDir,
		"expected":  manifestStateDir,
	}).Warn("STATE_DIR is not the directory config.json bind-mounts from the host. " +
		"Network options, tombstones and the lease ledger will live inside the plugin's " +
		"own filesystem, which `docker plugin rm` and every upgrade destroy — a network's " +
		"configuration will not survive an upgrade, and the schema version stamped on it " +
		"guards nothing. Intentional for a test rig; otherwise unset STATE_DIR.")
}

// prepareStateDir creates stateDir and sweeps older file modes in one startup step (#804).
func prepareStateDir(failures intCounter) (string, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create state dir %v: %w", stateDir, err)
	}
	sweepStateDirModes(stateDir, failures)
	return stateDir, nil
}

var chmodFile = os.Chmod

// sweepStateDirModes writes perm&stateFileMode to each regular file in dir, so it only tightens and 0400 stays 0400
// (#804); no recursion, symlinks skipped, and a failed chmod is counted, never fatal.
func sweepStateDirModes(dir string, failures intCounter) {
	bump := func() {
		if failures != nil {
			failures.Add(1)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		bump()
		log.WithError(err).WithField("state_dir", dir).
			Warn("Could not read STATE_DIR to tighten files an older plugin left behind. " +
				"No file was examined; any that predate this version keep the mode they have.")
		return
	}

	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		perm := info.Mode().Perm()
		if perm&^stateFileMode == 0 {
			continue
		}
		tightened := perm & stateFileMode

		path := filepath.Join(dir, e.Name())
		if err := chmodFile(path, tightened); err != nil {
			bump()
			log.WithError(err).WithFields(log.Fields{
				"file": path,
				"mode": fmt.Sprintf("%#o", uint32(perm)),
			}).Warn("Could not tighten a state file left behind by an older plugin; it keeps the mode it has")
			continue
		}
		log.WithFields(log.Fields{
			"file": path,
			"from": fmt.Sprintf("%#o", uint32(perm)),
			"to":   fmt.Sprintf("%#o", uint32(tightened)),
		}).Info("Tightened a state file left behind by an older plugin")
	}
}

// validNetworkID accepts only a flat token, so a network ID cannot carry a path separator or traversal element.
var validNetworkID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString

func stateFilePath(networkID string) (string, error) {
	if !validNetworkID(networkID) {
		return "", fmt.Errorf("invalid network id %q", networkID)
	}
	p := filepath.Clean(filepath.Join(stateDir, networkID+".json"))
	if !strings.HasPrefix(p, filepath.Clean(stateDir)+string(os.PathSeparator)) {
		return "", fmt.Errorf("state path for network %q escapes %s", networkID, stateDir)
	}
	return p, nil
}

// tombstoneTTL covers `docker restart` and a `systemctl restart docker` of a loaded host, typically 15-30 s (#46).
const tombstoneTTL = 60 * time.Second

// tombstone keeps a deleted endpoint's MAC for the next CreateEndpoint on the network: on Docker 26.x `docker restart`
// mints a fresh EndpointID, and concurrent restarts within the TTL fall through to a fresh MAC (#46).
// It carries no schema version: every record expires before another build could read it (#724).
type tombstone struct {
	NetworkID  string `json:"network_id"`
	MacAddress string `json:"mac_address"`
	// Hostname narrows the match to one container, so a sequential `compose restart` cannot swap MACs.
	Hostname string `json:"hostname,omitempty"`
	// IPAddress is the previous bare IPv4 address, requested as option 50.
	IPAddress string `json:"ip_address,omitempty"`
	// IPv6Address is the previous bare IPv6 address, requested as the IA_NA preferred address (#152, #213).
	IPv6Address string    `json:"ipv6_address,omitempty"`
	DeletedAt   time.Time `json:"deleted_at"`
}

func tombstoneFilePath() string {
	return filepath.Join(stateDir, "tombstones.json")
}

// quarantineTombstones moves an unparseable file aside and never reaps it, since writing over it destroyed every
// entry (#724).
func quarantineTombstones() (string, error) {
	final := tombstoneFilePath()
	aside := final + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000Z")
	if err := os.Rename(final, aside); err != nil {
		return "", fmt.Errorf("failed to quarantine corrupt tombstones file: %w", err)
	}
	if err := syncDir(filepath.Dir(final)); err != nil {
		return aside, err
	}
	return aside, nil
}

// errTombstonesQuarantined separates a moved-aside file from a transient read error, which must not be read as empty
// (#693).
var errTombstonesQuarantined = errors.New("tombstones file was corrupt and has been quarantined")

// errStateSchemaTooNew is a refusal, not an absence: a newer build's file is left exactly as found (#724).
var errStateSchemaTooNew = errors.New("persisted options use a newer schema than this build understands")

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
		aside, qErr := quarantineTombstones()
		if qErr != nil {
			// Not reported as quarantined: the caller would start fresh and write over the unmoved contents (#724).
			return nil, fmt.Errorf("tombstones file is corrupt and could not be quarantined (%v): %w", err, qErr)
		}
		return nil, fmt.Errorf("%w as %s: %v", errTombstonesQuarantined, aside, err)
	}
	return ts, nil
}

func saveTombstones(ts []tombstone) error {
	data, err := json.Marshal(ts)
	if err != nil {
		return fmt.Errorf("failed to encode tombstones: %w", err)
	}
	return writeStateFileAtomic(tombstoneFilePath(), ".tombstones.*.tmp", data, "tombstones", syncEphemeral)
}

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

func saveOptions(networkID string, opts DHCPNetworkOptions) error {
	return saveNetwork(networkID, opts, nil)
}

func saveNetwork(networkID string, opts DHCPNetworkOptions, binding *ipamBinding) error {
	final, err := stateFilePath(networkID)
	if err != nil {
		return err
	}
	v := stateSchemaVersionBase
	if binding != nil {
		v = stateSchemaVersion
	}
	data, err := json.Marshal(versionedOptions{DHCPNetworkOptions: opts, V: v, IPAM: binding})
	if err != nil {
		return fmt.Errorf("failed to encode options: %w", err)
	}
	return writeStateFileAtomic(final, ".state-*.tmp", data, "options", syncDurable)
}

// versionedOptions embeds DHCPNetworkOptions so the file stays one flat JSON object, and a build older than the
// "v" key ignores it (#724).
type versionedOptions struct {
	DHCPNetworkOptions
	V int `json:"v"`
	// IPAM is the pool binding CreateNetwork learned, absent on a null-mode network.
	IPAM *ipamBinding `json:"ipam,omitempty"`
}

func loadOptions(networkID string) (DHCPNetworkOptions, error) {
	sn, err := loadNetwork(networkID)
	return sn.Options, err
}

type storedNetwork struct {
	Options DHCPNetworkOptions
	Binding *ipamBinding
}

// IPAMMode reports whether this network's addresses come from the bundled IPAM driver.
func (s storedNetwork) IPAMMode() bool { return s.Binding != nil }

func loadNetwork(networkID string) (storedNetwork, error) {
	var sn storedNetwork
	opts := &sn.Options
	path, err := stateFilePath(networkID)
	if err != nil {
		return sn, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sn, err
	}
	var vo versionedOptions
	if err := json.Unmarshal(data, &vo); err != nil {
		return sn, fmt.Errorf("persisted options for %v are corrupt: %w", networkID, err)
	}
	// A newer schema is refused, not guessed at; V == 0 predates the field and is v1. netOptions backfills only when
	// the file is absent, so a downgrade never overwrites the file it refused (#724).
	if vo.V > stateSchemaVersion {
		return sn, fmt.Errorf("%w: persisted options for %v are schema v%d, this build understands v%d", errStateSchemaTooNew, networkID, vo.V, stateSchemaVersion)
	}
	*opts = vo.DHCPNetworkOptions
	sn.Binding = vo.IPAM
	return sn, nil
}

// listStateNetworks reads the directory, one file per network. tombstones.json matches validNetworkID, so the
// plugin's own files are skipped by name (#110).
func listStateNetworks() ([]string, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if filepath.Join(stateDir, name) == tombstoneFilePath() {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !validNetworkID(id) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func deleteOptions(networkID string) error {
	path, err := stateFilePath(networkID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove options file: %w", err)
	}
	return nil
}
