// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// Request capture (#644) tees the bodies the daemon sends into a directory, so fixtures reflect libnetwork's real
// payloads (#298 shipped against an assumed one). With no directory, captureHandler returns next unchanged; the knob
// lives in config-cover.json only. A capture failure never fails a request: every error path logs and continues.

const (
	// captureMaxBodyBytes: the largest libnetwork request, CreateEndpoint, is well under 4 KiB (#644).
	captureMaxBodyBytes = 1 << 20

	captureMaxFiles = 2000
)

// captureDirMode and captureFileMode keep raw libnetwork requests on a host bind mount to root (#785). Both are
// also chmod-ed, since the directory and names already exist in the normal flow; measured: MkdirAll(0700) over a
// 0755 directory leaves it 0755, and WriteFile(0600) over a 0644 file leaves it 0644.
const (
	captureDirMode  = 0o700
	captureFileMode = 0o600
)

type captureState struct {
	dir string

	allowed map[string]string

	mu       sync.Mutex
	seq      int
	stopped  bool
	warnOnce sync.Once
}

func captureHandler(next http.Handler, dir string, paths []string) http.Handler {
	if dir == "" {
		return next
	}

	allowed := make(map[string]string, len(paths))
	for _, path := range paths {
		allowed[path] = strings.TrimPrefix(path, "/")
	}
	st := &captureState{dir: dir, allowed: allowed}

	// An unusable directory, including one whose mode could not be set, declines to capture (#785).
	if err := ensureCaptureDir(dir); err != nil {
		log.WithError(err).WithField("dir", dir).
			Error("Request capture was requested but its directory is unusable; capturing nothing")
		return next
	}

	log.WithField("dir", dir).
		Warn("Request capture is ENABLED — this is test instrumentation and should not be set in production")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.capture(r)
		next.ServeHTTP(w, r)
	})
}

func (s *captureState) capture(r *http.Request) {
	if r.Body == nil {
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, captureMaxBodyBytes+1))
	closeErr := r.Body.Close()

	// The body is replaced on every path below, and a read error is replayed, not truncated into a decode error (#644).
	if err != nil {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), errReader{err}))
		s.warn("could not read request body for capture", err)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	if closeErr != nil {
		s.warn("could not close request body during capture", closeErr)
	}
	if len(body) == 0 || len(body) > captureMaxBodyBytes {
		return
	}

	name, ok := s.nextName(r.URL.Path)
	if !ok {
		return
	}

	s.writeBody(filepath.Join(s.dir, name), body)
}

// writeBody only ever creates a file, so it carries captureFileMode: names recur because nextName restarts at 0001
// in every process (#786).
func (s *captureState) writeBody(path string, body []byte) {
	f, err := createCaptureFile(path)
	if err != nil {
		s.warn("could not write captured request", err)
		return
	}
	if err := writeAndClose(f, body); err != nil {
		s.warn("could not write captured request", err)
	}
}

// Measured: O_CREAT|O_EXCL over a symlink is EEXIST with the target untouched, and O_CREAT alone writes through it.
// O_NOFOLLOW is therefore redundant with O_EXCL and kept only to state the requirement (#786).
func createCaptureFile(path string) (*os.File, error) {
	const flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL | unix.O_NOFOLLOW

	f, err := os.OpenFile(path, flags, captureFileMode)
	if !errors.Is(err, fs.ErrExist) {
		return f, err
	}

	// Remove takes a symlink and not its target, and a failed Remove is reported (#786).
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, flags, captureFileMode)
}

func writeAndClose(f *os.File, body []byte) error {
	_, err := f.Write(body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ensureCaptureDir chmods the directory because `make capture-fixtures` creates it before enable (#588); it
// tightens the directory, not its contents, since nothing writes there before the plugin runs (#785).
func ensureCaptureDir(dir string) error {
	if err := os.MkdirAll(dir, captureDirMode); err != nil {
		return err
	}
	return os.Chmod(dir, captureDirMode)
}

func (s *captureState) nextName(urlPath string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stopped {
		return "", false
	}
	if s.seq >= captureMaxFiles {
		s.stopped = true
		log.WithFields(log.Fields{"dir": s.dir, "files": s.seq}).
			Warn("Request capture reached its file cap; recording nothing further")
		return "", false
	}

	s.seq++
	return fmt.Sprintf("%04d-%s.json", s.seq, s.methodName(urlPath)), true
}

// methodName is a lookup from the routing table, so no filename derives from request data; unrouted RPCs the daemon
// sends on every start are recorded as evidence (#646).
func (s *captureState) methodName(urlPath string) string {
	if name, ok := s.allowed[urlPath]; ok {
		return name
	}
	return "unknown"
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func (s *captureState) warn(msg string, err error) {
	s.warnOnce.Do(func() {
		log.WithError(err).WithField("dir", s.dir).
			Warn("Request capture failed; continuing without it (reported once). " + msg)
	})
}
