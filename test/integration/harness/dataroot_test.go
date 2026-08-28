// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"errors"
	"testing"
)

// The data-root chooser decides where PluginLog reads the plugin's log
// from. Both of its outcomes are quiet when wrong: the caller gets a
// read error naming a path that looks entirely plausible, which is
// exactly how the transcribed /var/lib/docker presented as a
// health-floor failure rather than as a harness bug (#841).
//
// Two cases, and both matter in opposite directions. Answer honoured:
// a second daemon with its own --data-root is the whole reason the
// value is derived instead of transcribed. Fallback taken: an Info call
// that failed, or one that answered with nothing, must not turn into an
// empty path — "/plugins/<id>/rootfs/..." would be read relative to
// nothing and fail somewhere far from the cause.
func TestChooseDataRoot(t *testing.T) {
	errNoDaemon := errors.New("Cannot connect to the Docker daemon")

	cases := []struct {
		name    string
		rootDir string
		err     error
		want    string
	}{
		{
			name:    "second daemon's own data-root is honoured",
			rootDir: "/srv/dh-itest-daemon2/data",
			want:    "/srv/dh-itest-daemon2/data",
		},
		{
			name:    "the default answered explicitly is still the answer",
			rootDir: "/var/lib/docker",
			want:    "/var/lib/docker",
		},
		{
			name:    "Info failed: fall back",
			rootDir: "",
			err:     errNoDaemon,
			want:    defaultDockerDataRoot,
		},
		{
			name:    "Info failed but answered anyway: the error wins",
			rootDir: "/srv/dh-itest-daemon2/data",
			err:     errNoDaemon,
			want:    defaultDockerDataRoot,
		},
		{
			name:    "Info succeeded with an empty data-root: fall back",
			rootDir: "",
			want:    defaultDockerDataRoot,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseDataRoot(tc.rootDir, tc.err); got != tc.want {
				t.Errorf("chooseDataRoot(%q, %v) = %q, want %q", tc.rootDir, tc.err, got, tc.want)
			}
		})
	}
}

// The fallback is a compatibility promise, not a placeholder: it is the
// value every caller assumed before #841 derived the root, so a daemon
// running on the stock layout must keep resolving exactly as it did.
func TestChooseDataRoot_FallbackIsTheStockLayout(t *testing.T) {
	if defaultDockerDataRoot != "/var/lib/docker" {
		t.Fatalf("defaultDockerDataRoot = %q, want /var/lib/docker — changing this silently "+
			"moves where every stock-layout run reads the plugin log from",
			defaultDockerDataRoot)
	}
}
