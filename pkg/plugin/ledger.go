package plugin

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// ledgerFileName is the append-only lease audit log inside
	// STATE_DIR, one JSON object per line (#109).
	ledgerFileName = "leases.jsonl"
	// ledgerMaxSize / ledgerMaxAge bound the active file: whichever
	// trips first rotates it to <name>.1 (replacing the previous
	// rotation). Two generations on a 16 MB / 30 day budget keeps the
	// worst case ~32 MB — bounded even on busy networks.
	ledgerMaxSize = 16 << 20
	ledgerMaxAge  = 30 * 24 * time.Hour
)

// ledgerEntry is one lease-lifecycle event. Kind is one of "bound",
// "renew", "release", or "release_failed" — the last one is written
// when the SIGTERM-driven DHCPRELEASE didn't complete cleanly, so the
// ledger never claims a release that may not have reached the server.
type ledgerEntry struct {
	TS        string `json:"ts"`
	Kind      string `json:"kind"`
	Network   string `json:"network"`
	Endpoint  string `json:"endpoint"`
	Container string `json:"container,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	IP        string `json:"ip,omitempty"`
	MAC       string `json:"mac,omitempty"`
}

// leaseLedger appends lease events to a JSONL file with size- and
// age-based rotation. Failures are counted (ledger_write_failures on
// /Plugin.Health) and logged, never propagated — the audit trail is
// auxiliary and must not affect lease handling.
type leaseLedger struct {
	path     string
	maxSize  int64
	maxAge   time.Duration
	now      func() time.Time
	failures *atomic.Int32

	mu sync.Mutex
	// firstTS is the timestamp of the active file's first entry,
	// recovered from disk after a plugin restart so age rotation
	// doesn't reset on every enable cycle.
	firstTS time.Time
}

func newLeaseLedger(path string, failures *atomic.Int32) *leaseLedger {
	return &leaseLedger{
		path:     path,
		maxSize:  ledgerMaxSize,
		maxAge:   ledgerMaxAge,
		now:      time.Now,
		failures: failures,
	}
}

// Append writes one entry, stamping TS itself. Safe for concurrent use.
func (l *leaseLedger) Append(e ledgerEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	e.TS = now.UTC().Format(time.RFC3339)
	line, err := json.Marshal(e)
	if err != nil {
		l.fail("marshal", err)
		return
	}
	line = append(line, '\n')

	if err := l.rotateIfNeeded(now, int64(len(line))); err != nil {
		// Rotation trouble shouldn't lose the event — log and keep
		// appending to the oversized file; the next Append retries.
		log.WithError(err).Warn("Lease ledger rotation failed")
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		l.fail("open", err)
		return
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.WithError(err).Debug("lease ledger close failed")
		}
	}()
	// A single write of one line under O_APPEND keeps entries intact
	// even if another process ever appends to the same file.
	if _, err := f.Write(line); err != nil {
		l.fail("write", err)
		return
	}
	if l.firstTS.IsZero() {
		l.firstTS = now
	}
}

func (l *leaseLedger) fail(op string, err error) {
	if l.failures != nil {
		l.failures.Add(1)
	}
	log.WithError(err).WithField("op", op).Warn("Lease ledger write failed")
}

// rotateIfNeeded moves the active file to <path>.1 when appending
// `incoming` bytes would cross the size budget, or when the active
// file's first entry is older than the age budget. Caller holds l.mu.
func (l *leaseLedger) rotateIfNeeded(now time.Time, incoming int64) error {
	st, err := os.Stat(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		l.firstTS = time.Time{}
		return nil
	}
	if err != nil {
		return err
	}
	if l.firstTS.IsZero() {
		// Fresh leaseLedger over an existing file (plugin restart):
		// recover the age anchor from the first line. Unparseable
		// content falls back to mtime — age rotation stays
		// approximate rather than disabled.
		l.firstTS = readFirstTS(l.path, st.ModTime())
	}
	if st.Size()+incoming <= l.maxSize && now.Sub(l.firstTS) <= l.maxAge {
		return nil
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return err
	}
	l.firstTS = time.Time{}
	return nil
}

// readFirstTS parses the timestamp of the file's first JSONL entry,
// returning fallback when the file is empty or malformed.
func readFirstTS(path string, fallback time.Time) time.Time {
	f, err := os.Open(path)
	if err != nil {
		return fallback
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.WithError(err).Debug("lease ledger close failed")
		}
	}()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return fallback
	}
	var e ledgerEntry
	if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
		return fallback
	}
	ts, err := time.Parse(time.RFC3339, e.TS)
	if err != nil {
		return fallback
	}
	return ts
}
