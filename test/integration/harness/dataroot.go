// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// No integration tag: the data-root chooser runs in the unit job (#841).

package harness

// defaultDockerDataRoot is the daemon's compiled-in data-root on Linux.
const defaultDockerDataRoot = "/var/lib/docker"

// chooseDataRoot turns a `docker info` outcome into the data-root; a failed or empty answer falls back to the default,
// so the error surfaces at the file read with its path (#841).
func chooseDataRoot(rootDir string, err error) string {
	if err != nil || rootDir == "" {
		return defaultDockerDataRoot
	}
	return rootDir
}
