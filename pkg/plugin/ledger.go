// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// ledgerFileName is the append-only lease audit log inside STATE_DIR, one JSON object per line (#109).
	ledgerFileName = "leases.jsonl"
	// Two generations on a 16 MB / 30 day budget bound the log to about 32 MB (#109).
	ledgerMaxSize = 16 << 20
	ledgerMaxAge  = 30 * 24 * time.Hour
)

// ledgerEntry is one lease event; "stopped" and "stop_failed" record the client's shutdown, not
// the lease, whose release the releases_sent and release_failures counters report (#800, #962).
type ledgerEntry struct {
	TS        string `json:"ts"`
	Kind      string `json:"kind"`
	Network   string `json:"network"`
	Endpoint  string `json:"endpoint"`
	Container string `json:"container,omitempty"`
	Hostname  string `json:"hostname,omitempty"`
	IP        string `json:"ip,omitempty"`
	// Source is `slaac` for an address formed from a router's prefix (RFC 4862 section 5.5.3), absent for a lease.
	Source string `json:"source,omitempty"`
	MAC    string `json:"mac,omitempty"`
}

// leaseLedger appends lease events to a rotated JSONL file; a write failure is counted and logged, never propagated
// (#109).
type leaseLedger struct {
	path     string
	maxSize  int64
	maxAge   time.Duration
	now      func() time.Time
	failures intCounter

	mu      sync.Mutex
	firstTS time.Time
}

func newLeaseLedger(path string, failures intCounter) *leaseLedger {
	return &leaseLedger{
		path:     path,
		maxSize:  ledgerMaxSize,
		maxAge:   ledgerMaxAge,
		now:      time.Now,
		failures: failures,
	}
}

// Append writes one entry, stamping TS itself.
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
		log.WithError(err).Warn("Lease ledger rotation failed")
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, stateFileMode)
	if err != nil {
		l.fail("open", err)
		return
	}
	// O_CREATE's mode applies only on creation, so a file an older version created is tightened here (#708).
	if err := f.Chmod(stateFileMode); err != nil {
		log.WithError(err).Debug("lease ledger chmod failed")
	}
	// os.Rename keeps the inode's mode, so the rotated generation is tightened too; ENOENT means
	// the host never rotated (#724).
	if err := os.Chmod(l.path+".1", stateFileMode); err != nil && !os.IsNotExist(err) {
		log.WithError(err).Debug("rotated lease ledger chmod failed")
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.WithError(err).Debug("lease ledger close failed")
		}
	}()
	// One write under O_APPEND keeps a line intact against another appender.
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
