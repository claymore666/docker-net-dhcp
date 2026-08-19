// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

// Request capture (#644).
//
// # Why this exists
//
// The unit tests build their CreateEndpointRequest / JoinRequest values
// by hand, which means they assert this code against OUR MODEL of what
// libnetwork sends rather than against what it sends. When the two
// differ every unit test still passes and the difference surfaces on a
// privileged runner, or in production.
//
// #298 is the worked example: stable_lease was designed against an
// assumed CreateEndpoint payload, shipped, and was reverted from v1.3.0
// once the endpoint identity turned out to be unresolvable in the
// docker-run and compose flows. The request shape WAS the defect, and
// nothing runnable without a daemon could see it.
//
// This tees the raw request bodies the daemon actually sends into a
// directory, so an integration run can be turned into replayable
// fixtures under pkg/plugin/testdata/requests/. See
// docs/internals.md#request-fixtures for the regeneration procedure.
//
// # Why it is off unless asked
//
// captureHandler returns `next` UNCHANGED when the directory is empty,
// so the shipped plugin carries no extra allocation, no extra syscall
// and no extra failure mode on any request. The knob is declared in
// config-cover.json only — the same place GOCOVERDIR lives, and for the
// same reason: it is test instrumentation, and the operator-facing
// manifest should not grow a setting whose only use is regenerating
// this repository's fixtures.
//
// # Why a capture failure is never a request failure
//
// This runs in front of every network-driver RPC. A full disk, a
// missing bind mount or a permission error must degrade to "no
// fixtures", never to a failed `docker run` — the plugin's job does not
// depend on it. Every error path here logs and continues.

const (
	// captureMaxBodyBytes caps a single captured body. libnetwork's
	// requests are small (the largest, CreateEndpoint with full
	// interface data, is well under 4 KiB); anything approaching this
	// is not a request shape worth recording.
	captureMaxBodyBytes = 1 << 20 // 1 MiB

	// captureMaxFiles caps how many bodies one plugin process records.
	// A suite run issues a few hundred RPCs; this is generous enough to
	// cover a full run and small enough that a capture directory left
	// enabled by accident cannot fill a disk.
	captureMaxFiles = 2000
)

// captureState is the bookkeeping shared by every captured request.
type captureState struct {
	dir string

	// allowed maps a served path to the filename fragment recorded for
	// it. Built from the routing table, so the set of names this can
	// ever write is CLOSED and comes from constants in routes.go rather
	// than from the request — see methodName.
	allowed map[string]string

	mu       sync.Mutex
	seq      int
	stopped  bool
	warnOnce sync.Once
}

// captureHandler tees each request body into dir before passing the
// request on. An empty dir returns next unchanged, which is what the
// shipped plugin does.
//
// The returned handler is safe for concurrent use: the plugin serves
// RPCs concurrently, and Join for one container can overlap
// CreateEndpoint for another.
func captureHandler(next http.Handler, dir string, paths []string) http.Handler {
	if dir == "" {
		return next
	}

	allowed := make(map[string]string, len(paths))
	for _, path := range paths {
		allowed[path] = strings.TrimPrefix(path, "/")
	}
	st := &captureState{dir: dir, allowed: allowed}

	// Fail loudly at construction rather than silently at the first
	// request: a capture that was asked for and never happened is the
	// failure this whole mechanism exists to prevent, so it must not be
	// discoverable only by finding an empty directory afterwards.
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

// capture writes one request body to disk and restores r.Body so the
// downstream handler reads exactly what it would have read.
func (s *captureState) capture(r *http.Request) {
	if r.Body == nil {
		return
	}

	// Read the body under the cap. Anything longer is not recorded, and
	// the request still proceeds with its body intact.
	body, err := io.ReadAll(io.LimitReader(r.Body, captureMaxBodyBytes+1))
	closeErr := r.Body.Close()

	// The body has been consumed either way, so it must be replaced
	// before returning down ANY path below — including the error paths.
	// Forgetting this on one branch would leave the handler reading an
	// empty body and turn capture into a fault injector.
	//
	// A READ ERROR IS REPLAYED, not swallowed. Restoring only the bytes
	// read so far would hand the handler a truncated body with no error
	// on it, so a transport failure would reach the daemon as a JSON
	// decode failure instead — a different error, on a different code
	// path, for the same underlying event. Replaying the error keeps
	// the handler's behaviour byte-for-byte what it would have been
	// with no capture in front of it.
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
		// GetCapabilities and Plugin.Health carry no body; there is no
		// request shape to record.
		return
	}

	name, ok := s.nextName(r.URL.Path)
	if !ok {
		return
	}

	if err := os.WriteFile(filepath.Join(s.dir, name), body, 0o644); err != nil {
		s.warn("could not write captured request", err)
	}
}

// nextName allocates the next filename, or reports false once the file
// cap is reached.
//
// The sequence number is part of the name because ORDER IS PART OF THE
// FIXTURE: CreateEndpoint before Join before Leave is the shape a
// replay has to preserve, and a directory listing is the only record of
// it.
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

// methodName maps a served path to the fragment used in its filename.
//
// It is a LOOKUP, not a transformation, and that is the point. The path
// arrives from the socket, so deriving a filename from its characters
// means a filename that depends on request data — and proving that safe
// requires a reader (and a scanner) to follow the sanitiser and agree it
// is airtight. A closed map keyed by the routing table removes the
// question instead of answering it: every name this can write is a
// constant from routes.go, and anything else becomes "unknown".
//
// An unrouted path reaching here is not hypothetical — the daemon calls
// ProgramExternalConnectivity and RevokeExternalConnectivity on every
// container start and stop, and nothing serves them (#646). Those are
// worth recording as evidence, which is why this returns a name at all
// rather than declining to capture.
func (s *captureState) methodName(urlPath string) string {
	if name, ok := s.allowed[urlPath]; ok {
		return name
	}
	return "unknown"
}

// errReader yields one error and nothing else. It is what lets a failed
// body read be handed downstream as the same failure rather than as a
// short body.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// warn reports the first capture failure and stays quiet after it. One
// broken capture is almost always every capture broken, and this runs
// on the path of every RPC the daemon makes.
func (s *captureState) warn(msg string, err error) {
	s.warnOnce.Do(func() {
		log.WithError(err).WithField("dir", s.dir).
			Warn("Request capture failed; continuing without it (reported once). " + msg)
	})
}
