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
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// stateFileMode is the mode every file the plugin writes under stateDir
// carries, and the lease ledger with it.
//
// 0600 because /var/lib/net-dhcp is an rbind rw HOST mount (config.json),
// so at 0644 the container MACs, IPs, hostnames and the full lease audit
// trail were readable by any user on the host. Not a privilege boundary
// -- nothing stored is a credential and the writer is root either way --
// but there is no reason for it to be world-readable, and an operator who
// reads leases.jsonl as a non-root user now needs sudo (#708).
//
// Existing files are re-chmod'd on the next WRITE: the state writes go
// through a temp file and a rename, and the ledger chmods on append.
// That is not the same as "an upgrade tightens what it finds", which
// this comment used to claim. Nothing in the upgrade path writes
// tombstones.json -- it is rewritten only when a tombstone is laid or
// consumed -- so on a host with stable containers it keeps its old mode
// indefinitely. Observed 0644 after a production upgrade to v1.8.0.
// Sweeping stateDir at startup is #804.
const stateFileMode = 0o600

// stateSchemaVersion is the version stamped on the per-network options
// file. It is NOT stamped on tombstones.json; see syncEphemeral for why
// a 60-second cache neither needs a version nor deserves one.
//
// Version 1 is what every build writes today. A file written before the
// field existed carries no "v" at all and is read AS version 1, because
// that is what it is: the field was added to give a future change
// somewhere to branch, not to describe a change that already happened.
//
// The field is additive. Options are a flat JSON object, so "v" is one
// more key an older build's encoding/json ignores, and the file stays
// readable by every build that predates this one. A nested
// {"v":1,"options":{...}} would have read as an all-zero options struct
// on an older build -- no mode, no parent -- which is the failure a
// version field exists to prevent, committed by the commit that adds
// one.
//
// It earns its place because this file is long-lived and crosses
// versions: it is written from CreateNetwork, it lives on a host bind
// mount (see stateDir), and it survives `docker plugin rm` and upgrade
// by design (#440). A downgrade or a partial upgrade WILL read a file
// some other build wrote.
//
// On read, a version this build does not understand is refused rather
// than guessed at. The caller falls back to the docker API, which is
// authoritative for everything in the struct, so the refusal costs a
// lookup -- whereas a v1 reading of a v2 file could attach a network in
// the wrong mode or on the wrong parent.
const stateSchemaVersion = 1

// syncPolicy says whether a state write must reach the disk before it is
// reported as done.
//
// It is a parameter rather than two writers because the two files want
// opposite things and the difference is one axis, not one function's
// worth of divergent code. Making it explicit at each call site is the
// point: the choice is a decision about the data, and it should be
// visible where the data is written.
type syncPolicy int

const (
	// syncDurable fsyncs the file before the rename and the directory
	// after it. For state that must survive a power cut: the options
	// file is written rarely, read on every daemon restart, and outlives
	// the build that wrote it.
	syncDurable syncPolicy = iota

	// syncEphemeral skips both fsyncs. For tombstones.json, and it is a
	// deliberate refusal rather than an omission.
	//
	// The only crash an fsync survives is power loss or a panic -- a
	// clean `systemctl restart docker` never loses the page cache. But
	// tombstoneTTL is 60 seconds, and no host boots, starts dockerd and
	// reaches this file within 60 seconds of a power cut, so every entry
	// in it prunes as stale on the first read afterwards. Durability
	// there buys nothing: the data it protects is guaranteed worthless
	// by the time anything can read it.
	//
	// And it is not free. This file is written from `add` on every
	// DeleteEndpoint and from `consume` whenever a prune changed
	// something, so an fsync here lands on the endpoint path -- the cost
	// tombstone_store.go's "I-10" note deliberately removed.
	//
	// #724 asked for fsync on both files on the grounds that "both files
	// exist specifically to survive restarts". That is true of the
	// options file and false of this one, for the TTL reason above.
	syncEphemeral
)

// syncDir fsyncs a directory so a rename into it is durable.
//
// A rename is atomic against process death without this -- a reader sees
// the old file or the new one, never a torn one -- but atomic is not
// durable. After a power cut or a hard host reset the rename can be
// absent, or present while the file it names is empty, because the
// directory entry and the file's data are separate writeback (#724).
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

// writeStateFileAtomic writes data to final atomically: temp file in the
// same directory, chmod, rename. Under syncDurable it also fsyncs the
// file before the rename and the directory after it.
//
// Both persisted files go through here rather than each open-coding the
// sequence. They were already structurally identical -- CreateTemp,
// write, Chmod, Rename -- so leaving them as two copies means the next
// fix to this sequence has two places to land and misses one. What
// differs between them is the sync policy, and that is the one thing
// each caller states.
//
// Every error is returned, none is logged-and-continued. A Sync whose
// error is dropped is the same defect as the missing Sync was: it looks
// durable and is not.
//
// pattern is an os.CreateTemp pattern and never carries caller-supplied
// input -- "*" already guarantees uniqueness, and keeping request data
// out of it removes the pattern as a path-injection sink. what names the
// file in error messages.
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
	// Before the rename, not after: the rename is what publishes the
	// file, so the bytes have to have reached the disk by the time
	// anything can observe the name.
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
	// 0600, not 0644. /var/lib/net-dhcp is an rbind rw HOST mount, so
	// these files' contents -- container MACs, IPs and hostnames -- were
	// readable by any user on the host. Nothing here is a credential and
	// the plugin runs as root either way, so this is not a privilege
	// boundary; 0600 simply costs nothing (#708).
	if err := os.Chmod(tmpName, stateFileMode); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to chmod %s temp file: %w", what, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to rename %s file into place: %w", what, err)
	}
	// The rename is only durable once the directory entry is. Fatal
	// rather than logged: the data is visible either way, but "we wrote
	// it" and "it survives a power cut" are different claims, and under
	// syncDurable this function makes the second one. The caller counts
	// the failure and the write is retried on the next event.
	if sync == syncDurable {
		if err := syncDir(filepath.Dir(final)); err != nil {
			return fmt.Errorf("failed to sync %s file into place: %w", what, err)
		}
	}
	return nil
}

// stateDir is the directory where per-network options are persisted.
//
// It is a HOST path, not plugin-private storage. config.json declares
// `/var/lib/net-dhcp` as a bind mount with source and destination both
// equal to that path, rbind and rw, and this default is exactly it. So
// the contents survive plugin disable/enable, `docker plugin rm`, and
// upgrade — `plugin rm` removes the plugin's rootfs, not the bind
// source. That is deliberate (#440): a network's options outlive the
// build that wrote them.
//
// This comment used to say the opposite — that the directory lived
// inside the plugin's writable filesystem and was reset on `plugin rm`
// or upgrade, "which is fine". It was false, and it was false 14 lines
// below stateFileMode's comment saying correctly that this is an rbind
// rw host mount. It is corrected here rather than in passing because
// the whole justification for stateSchemaVersion rests on which of the
// two was right: a file that were genuinely reset on upgrade could
// never be read by a build other than the one that wrote it, and would
// need no version at all (#724).
//
// The disk-state read in netOptions still falls back to the docker API
// for any network that has not been saved yet, which is what makes
// networks predating persistence keep working.
//
// Configurable via the STATE_DIR env var so test runs can point at a
// scratch directory.
var stateDir = func() string {
	if d := os.Getenv("STATE_DIR"); d != "" {
		return d
	}
	return manifestStateDir
}()

// manifestStateDir is the path config.json declares as both source and
// destination of the rbind rw mount. It is the ONLY value of STATE_DIR
// for which everything said above about durability is true.
const manifestStateDir = "/var/lib/net-dhcp"

// warnIfStateDirIsNotThePersistentOne logs once at startup when
// STATE_DIR has been pointed somewhere other than the bind mount.
//
// A warning and not a refusal: STATE_DIR is documented as settable, the
// consequence is documented (reference.md), and an operator who means it
// -- a test rig, a host with a different layout -- should still be able
// to do it. What is not acceptable is doing it by accident and finding
// out months later.
//
// It exists because a guarantee this package leans on is conditional on
// a setting nothing checked at runtime. stateSchemaVersion justifies
// itself with "this file is long-lived and crosses versions: it lives on
// a host bind mount and survives plugin rm and upgrade by design" --
// true of manifestStateDir and false of anywhere else. Repointed, the
// options file no longer crosses versions, the version field guards
// nothing, and the tombstones and the lease ledger go with it. There was
// no signal at all until an upgrade months later took the lot.
//
// scripts/check-plugin-bind-sources.sh already holds the manifest and
// the installers to agreeing with each other. Nothing held the running
// process to agreeing with either (#724).
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

// validNetworkID accepts only a flat token — the shape of a libnetwork
// network ID (hex). It rejects path separators and traversal elements
// before networkID is ever interpolated into a filesystem path.
var validNetworkID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString

// stateFilePath returns the on-disk path for a given network's options.
// networkID originates from the driver request, so it is validated and
// the resolved path is confirmed to stay within stateDir before it
// reaches any os.* call — closing the go/path-injection vector (CWE-22).
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

// tombstoneTTL bounds how long a recently-deleted endpoint's MAC is
// available for inheritance by the next CreateEndpoint on the same
// network. Two restart shapes both need to fit inside it:
//   - `docker restart <ctr>` issues Delete then Create back-to-back —
//     well under a second in practice.
//   - `systemctl restart docker` shuts containers down and (with
//     `--restart=always`) brings them back up after the daemon comes
//     ready. On a host with non-trivial container counts this gap is
//     typically 15–30s; we want the tombstone to survive it so MAC/IP
//     stay stable across daemon restarts too.
//
// 60s gives generous headroom over both. The on-disk cost is bounded
// by the prune-on-write strategy (only fresh entries land in the file)
// so a longer TTL doesn't grow disk usage.
const tombstoneTTL = 60 * time.Second

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
// Deliberately carries NO schema version, unlike the options file. A
// 60-second cache has nothing to migrate: by the time any build other
// than the writer could read this file, every record in it has expired.
// The right handling of a shape this build cannot parse is to discard
// it, which is what loadTombstones does (#724).
type tombstone struct {
	NetworkID  string `json:"network_id"`
	MacAddress string `json:"mac_address"`
	// Hostname, when non-empty, narrows tombstone matching to "same
	// container" instead of "any container on the network". Without
	// it, a sequential `compose restart` of N containers can let
	// container B inherit container A's MAC during the brief window
	// where A's tombstone is still fresh. With it, consumeTombstone
	// requires NetworkID+Hostname to match. Empty hostname falls back
	// to network-only matching (preserves the v0.5.0 contract for
	// hostname-less containers and cases where the lookup raced).
	Hostname string `json:"hostname,omitempty"`
	// IPAddress, when non-empty, is the bare IPv4 address (no /mask)
	// from the previous endpoint's lease. The next CreateEndpoint
	// passes it to dhcpcd via the `request` directive (DHCP option 50)
	// so the upstream DHCP server can ACK the same lease back to the
	// same MAC. Empty means
	// "do an unhinted DISCOVER".
	IPAddress string `json:"ip_address,omitempty"`
	// IPv6Address, when non-empty, is the bare IPv6 address from the
	// previous lease. Since #152 (dhcpcd) and #213 it is requested as
	// the DHCPv6 preferred address (IA_NA) on the next CreateEndpoint,
	// so a restarting container keeps its v6 lease the same way the
	// IPAddress hint keeps its v4 lease.
	IPv6Address string    `json:"ipv6_address,omitempty"`
	DeletedAt   time.Time `json:"deleted_at"`
}

// tombstoneFilePath returns the on-disk path for the tombstone list.
// One file holds all tombstones — there's never more than a handful
// alive at once and the prune-on-write strategy keeps it bounded.
func tombstoneFilePath() string {
	return filepath.Join(stateDir, "tombstones.json")
}

// quarantineTombstones moves an unreadable tombstone file aside and
// returns the path it was moved to.
//
// It exists because the alternative is destruction. loadTombstones'
// callers treat a load error as "start from nothing" and then write:
// the store's add path used to log a warning, set the slice to nil, and
// save one entry OVER the unreadable file, deleting every other
// tombstone in it. That is silent, unrecoverable, and happens at
// precisely the moment an operator would want the bytes -- a corrupt
// tombstone file means every container restarting in the next 60s picks
// a new MAC and a new address, and the file is the only evidence of why
// (#724).
//
// The quarantined file is never reaped. It is small, corruption is
// rare, and something that cleans up the evidence of a fault is the
// wrong instinct; an operator deletes it after reading it.
func quarantineTombstones() (string, error) {
	final := tombstoneFilePath()
	// Millisecond resolution so two quarantines in the same second do
	// not overwrite each other -- the whole point is not to lose bytes.
	aside := final + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000Z")
	if err := os.Rename(final, aside); err != nil {
		return "", fmt.Errorf("failed to quarantine corrupt tombstones file: %w", err)
	}
	// Durable for the same reason the write path is: the operator reads
	// this file after a crash, which is when a lost rename costs most.
	if err := syncDir(filepath.Dir(final)); err != nil {
		return aside, err
	}
	return aside, nil
}

// errTombstonesQuarantined marks the one load failure that is safe to
// continue past: the file's contents were unparseable, so they have been
// moved aside and there is definitively nothing to recover.
//
// It exists so callers can tell a REFUSAL from an ABSENCE. Every other
// load failure -- EIO, EMFILE, a read losing a race with a writer -- is
// transient and says nothing about the file's contents, which may be
// perfectly good. Treating those the same way means overwriting live
// data because a file descriptor was briefly unavailable. That is the
// #693 lesson in a different file: "I could not read it" must not be
// handled as "there was nothing there".
var errTombstonesQuarantined = errors.New("tombstones file was corrupt and has been quarantined")

// errStateSchemaTooNew reports an options file written by a plugin newer
// than this one. It is a REFUSAL, not an absence: the file is intact and
// authoritative for a build that understands it, and this build must
// leave it exactly as it found it. Callers distinguish it from
// os.IsNotExist so that "I will not read this" can never be mistaken for
// "there is nothing here" by anything that then writes -- the same split
// errTombstonesQuarantined draws one file over.
var errStateSchemaTooNew = errors.New("persisted options use a newer schema than this build understands")

// loadTombstones reads the tombstone list from disk, returning an
// empty slice when no file exists yet.
//
// An unparseable file is moved aside (see quarantineTombstones) and the
// error wraps errTombstonesQuarantined. Moving it aside is what makes a
// caller's "start fresh" safe: the next write lands on a name nothing
// occupies instead of on top of the evidence.
//
// A file this build cannot parse is discarded rather than migrated, and
// that is the whole forward-compatibility story for tombstones -- see
// the type comment. Sixty seconds of address stability is the entire
// value at stake.
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
			// The file could not even be moved. Do NOT report this as
			// quarantined: the caller would start fresh and write over
			// contents that are still sitting there.
			return nil, fmt.Errorf("tombstones file is corrupt and could not be quarantined (%v): %w", err, qErr)
		}
		return nil, fmt.Errorf("%w as %s: %v", errTombstonesQuarantined, aside, err)
	}
	return ts, nil
}

// saveTombstones atomically rewrites the tombstone list. Atomic, not
// durable: see syncEphemeral for why an fsync on this path would buy
// nothing and cost something.
func saveTombstones(ts []tombstone) error {
	data, err := json.Marshal(ts)
	if err != nil {
		return fmt.Errorf("failed to encode tombstones: %w", err)
	}
	return writeStateFileAtomic(tombstoneFilePath(), ".tombstones.*.tmp", data, "tombstones", syncEphemeral)
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
	final, err := stateFilePath(networkID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(versionedOptions{DHCPNetworkOptions: opts, V: stateSchemaVersion})
	if err != nil {
		return fmt.Errorf("failed to encode options: %w", err)
	}
	return writeStateFileAtomic(final, ".state-*.tmp", data, "options", syncDurable)
}

// versionedOptions is the on-disk shape of a network's options: the
// options themselves plus the schema version.
//
// DHCPNetworkOptions is embedded rather than nested because
// encoding/json flattens an untagged embedded struct. The file therefore
// stays the same flat object it has always been, with one extra "v" key
// that any older build's decoder ignores -- so this change is readable
// by builds that predate it, which a nested {"v":1,"options":{...}}
// would not be. DHCPNetworkOptions itself stays untouched: it is also
// the mapstructure target for user-supplied `docker network create`
// options, and a storage concern has no business appearing there.
type versionedOptions struct {
	DHCPNetworkOptions
	V int `json:"v"`
}

// loadOptions reads previously-persisted options for a network. Returns
// os.ErrNotExist (wrapped) when no state file is present so callers can
// fall back to other sources (e.g. the docker API).
func loadOptions(networkID string) (DHCPNetworkOptions, error) {
	var opts DHCPNetworkOptions
	path, err := stateFilePath(networkID)
	if err != nil {
		return opts, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return opts, err
	}
	var vo versionedOptions
	if err := json.Unmarshal(data, &vo); err != nil {
		return opts, fmt.Errorf("persisted options for %v are corrupt: %w", networkID, err)
	}
	// A version we do not understand is refused, not guessed at:
	// decoding a v2 file with v1 semantics could silently attach a
	// network in the wrong mode or on the wrong parent.
	// V == 0 is a file written before the field existed; that is v1.
	//
	// REFUSING IS ONLY CHEAP IF NOBODY WRITES AFTERWARDS. The caller
	// falls back to the docker API, which is authoritative for
	// everything in this struct, so a refusal costs one lookup -- but
	// netOptions used to follow that fallback with a backfill save,
	// which would have replaced the v2 file it had just refused with a
	// v1 one. A downgrade would then have DESTROYED the newer file
	// rather than declining to read it, which is the exact failure a
	// version field exists to prevent. netOptions now backfills only
	// when the file was genuinely absent; see the comment there.
	if vo.V > stateSchemaVersion {
		return opts, fmt.Errorf("%w: persisted options for %v are schema v%d, this build understands v%d", errStateSchemaTooNew, networkID, vo.V, stateSchemaVersion)
	}
	return vo.DHCPNetworkOptions, nil
}

// deleteOptions removes the persisted options for a network. Called from
// DeleteNetwork. A "not found" error is treated as success since it
// just means we never persisted state for this network in the first
// place (e.g. created before we shipped persistence).
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
