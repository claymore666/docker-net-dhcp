// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/claymore666/dhcp-golib/proto"
	log "github.com/sirupsen/logrus"
)

// iidSecretFile is RFC 7217 section 5's secret_key for ipv6_iid=stable-privacy, one per plugin (#1032).
const iidSecretFile = "ipv6-iid-secret"

// iidSecretLen is 256 bits, twice the 128-bit floor RFC 7217 section 5 sets as a SHOULD (#1032).
const iidSecretLen = 32

// iidBeforePublish, when set, runs in createIIDSecret between the temporary file being complete and its publication, so
// a test can hold one creator at the moment a second one wins (#1032).
var iidBeforePublish func()

func iidSecretPath() string { return filepath.Join(stateDir, iidSecretFile) }

// loadIIDSecret reads the secret and mints it once when the file does not exist. A file under proto.MinIIDSecretLen is
// refused and never replaced: a new secret moves every stable-privacy address at its next formation (#1032).
func loadIIDSecret() ([]byte, error) {
	path := iidSecretPath()
	// Two passes: a creator that lost the race to a concurrent one reads the winner's file on the second (#1032).
	for range 2 {
		b, err := os.ReadFile(path)
		if err == nil {
			if len(b) < proto.MinIIDSecretLen {
				return nil, fmt.Errorf("the IPv6 interface identifier secret %s holds %d octets, want at least %d "+
					"(RFC 7217 section 5); it is not replaced, since a new secret changes every stable-privacy "+
					"address: restore the file, or delete it deliberately to mint a new one (#1032)",
					path, len(b), proto.MinIIDSecretLen)
			}
			return b, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("failed to read the IPv6 interface identifier secret %s: %w", path, err)
		}
		created, err := createIIDSecret(path)
		if err != nil {
			return nil, err
		}
		if created {
			log.WithField("file", path).Info("Created the secret that ipv6_iid=stable-privacy forms interface identifiers " +
				"from. Keep it with STATE_DIR: if it is lost, every stable-privacy address changes at its next formation")
		}
	}
	return nil, fmt.Errorf("the IPv6 interface identifier secret %s vanished while it was being created", path)
}

// createIIDSecret writes the secret to a temporary file and links it into place, which fails when the name exists, as
// O_EXCL does, and leaves no moment at which a reader sees a half-written file (#1032).
func createIIDSecret(path string) (created bool, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), iidSecretFile+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("failed to create the IPv6 interface identifier secret in %s: %w", filepath.Dir(path), err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	secret := make([]byte, iidSecretLen)
	if _, err := rand.Read(secret); err != nil {
		return false, fmt.Errorf("failed to draw the IPv6 interface identifier secret: %w", err)
	}
	if _, err := tmp.Write(secret); err != nil {
		return false, fmt.Errorf("failed to write the IPv6 interface identifier secret: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return false, fmt.Errorf("failed to flush the IPv6 interface identifier secret: %w", err)
	}
	if iidBeforePublish != nil {
		iidBeforePublish()
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("failed to publish the IPv6 interface identifier secret %s: %w", path, err)
	}
	return true, nil
}
