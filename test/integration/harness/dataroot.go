// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// This file deliberately carries NO `//go:build integration` tag,
// unlike most of the package, for the reason healthfloor.go states in
// its own header: a decision that only ever runs behind a live daemon
// is a decision nothing has ever been observed making.
//
// The decision here is where the suite reads the plugin's log from.
// Getting it wrong is quiet: the read fails naming a plausible path,
// which is exactly how the transcribed /var/lib/docker presented as a
// health-floor failure rather than as a harness bug (#841). Keeping the
// chooser pure and untagged puts dataroot_test.go in the ordinary
// `go test ./...` unit job. Everything that needs a daemon — the Info
// call itself — stays in health.go.

package harness

// defaultDockerDataRoot is the daemon's compiled-in data-root on Linux,
// and the answer every caller of PluginLog assumed before the root was
// derived from the daemon.
const defaultDockerDataRoot = "/var/lib/docker"

// chooseDataRoot turns the outcome of a `docker info` call into the
// data-root to read plugin state under. Split out of dockerDataRoot so
// that both of its branches can be driven without a daemon.
//
// A failed Info call, or one that answers with no data-root at all,
// falls back to the default. That is deliberate: it keeps the error
// surfacing at the read of the file, which prints the path it tried,
// rather than here, where it would become a bare harness error with no
// path in it.
func chooseDataRoot(rootDir string, err error) string {
	if err != nil || rootDir == "" {
		return defaultDockerDataRoot
	}
	return rootDir
}
