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

// captureDirMode and captureFileMode keep captured request bodies to
// root, for stateFileMode's reason and with more force. What lands here
// is the raw libnetwork request: container IDs, endpoint IDs, the
// sandbox key, MACs and addresses -- the most identifying data the
// plugin ever holds, and more of it per record than leases.jsonl
// carries. The directory is a HOST bind mount (config-cover.json mounts
// /var/lib/dh-capture at /capture), so at 0755/0644 every one of those
// bodies was readable by any user on the host.
//
// Both are applied with an explicit chmod as well as at creation, and
// that is not belt-and-braces: neither MkdirAll nor O_CREATE changes the
// mode of something that ALREADY EXISTS, and in the normal flow both
// already exist.
//
//   - The directory is not one the plugin creates. `make
//     capture-fixtures` mkdirs CAPTURE_HOST_DIR before enabling the
//     plugin, and it has to -- a bind source that does not yet exist
//     fails `docker plugin enable` outright (#588). So the plugin is
//     always handed a directory made by someone else's umask.
//   - The filenames are not unique across processes. nextName's
//     sequence restarts at 0001 in every plugin process, so a second
//     capture into the same directory rewrites the first capture's
//     names.
//
// Measured rather than assumed: MkdirAll(0700) over an existing 0755
// directory leaves it 0755, and WriteFile(0600) over an existing 0644
// file leaves it 0644. A constant alone would have changed nothing in
// either case that actually occurs.
const (
	captureDirMode  = 0o700
	captureFileMode = 0o600
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
	// A directory whose mode could not be established counts as unusable
	// and declines to capture: "no fixtures" is this mechanism's
	// documented degradation, and it is the right one to take when the
	// alternative is writing request bodies somewhere world-readable.
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

	s.writeBody(filepath.Join(s.dir, name), body)
}

// writeBody writes one captured body at captureFileMode.
//
// It never opens a file that already exists, and that is the whole
// mechanism. os.WriteFile's mode argument is O_CREATE's, so over an
// existing file it is ignored and the old mode stands -- and these
// names DO recur: nextName's sequence restarts at 0001 in every plugin
// process, so a second capture into the same directory lands on the
// first capture's names. A file that is always created always carries
// the mode it was created with.
//
// The earlier version of this unlinked first and then wrote, which got
// the same result by a SEQUENCE rather than by a property, and left one
// silent hole: if the unlink failed and the write then SUCCEEDED over
// the existing file, there was no error anywhere and the file kept its
// old mode -- exactly the defect this exists to fix. Creating
// exclusively removes the case instead of relying on the first call
// having worked.
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

// createCaptureFile creates path at captureFileMode, replacing anything
// already at that name -- but only ever by creating, never by opening
// what is there.
//
// O_NOFOLLOW is REDUNDANT with O_EXCL, not a guard against a window
// O_EXCL leaves open. That is a correction: this comment used to say it
// covered the gap between the unlink and the retry, and re-deriving the
// claim shows there is no such gap, because the retry uses this same
// flags constant and O_CREAT|O_EXCL fails EEXIST on a symlink by
// definition. Measured:
//
//	O_CREAT|O_EXCL, no NOFOLLOW, over a symlink -> EEXIST, target untouched
//	O_CREAT alone, over a symlink               -> writes THROUGH it
//
// So O_NOFOLLOW is unreachable at both call sites, and that -- not a
// weak suite -- is why no test observes it. It is kept so the flags
// state the requirement outright rather than leaving a reader to infer
// it from O_EXCL, and it costs nothing.
func createCaptureFile(path string) (*os.File, error) {
	const flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL | unix.O_NOFOLLOW

	f, err := os.OpenFile(path, flags, captureFileMode)
	if !errors.Is(err, fs.ErrExist) {
		return f, err
	}

	// The name is in use. Unlink it and create again: Remove takes the
	// link and not its target, so a symlink at this name is removed
	// rather than written through. A Remove that fails is REPORTED
	// here, where the old shape let a successful write past it.
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, flags, captureFileMode)
}

// writeAndClose writes body and closes f, reporting the first failure.
// A close error is a write error on a file whose data may not have
// reached the filesystem, so it is not discarded.
func writeAndClose(f *os.File, body []byte) error {
	_, err := f.Write(body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ensureCaptureDir creates the capture directory and restricts it to its
// owner, returning the first thing that went wrong.
//
// The chmod is not redundant with MkdirAll's mode: MkdirAll leaves an
// existing directory exactly as it found it, and in the flow this ships
// for the directory ALWAYS already exists. `make capture-fixtures`
// mkdirs CAPTURE_HOST_DIR before enabling the plugin, because a bind
// source that does not yet exist fails `docker plugin enable` (#588) --
// so the plugin is handed a directory made by someone else's umask.
//
// It tightens the directory and never its existing contents, which is
// safe here and not in general: in the capture flow nothing writes into
// that directory before the plugin runs. Makefile:451 is a bare
// `mkdir -p`, and every other operation on it -- the `rm -rf`, the
// `find`, the `cp` out of it -- lives in capture_one_flow, which runs
// after enable-cover. A flow that did create files there first would
// leave them at their own modes, and tightening them would be a
// different change.
func ensureCaptureDir(dir string) error {
	if err := os.MkdirAll(dir, captureDirMode); err != nil {
		return err
	}
	return os.Chmod(dir, captureDirMode)
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
