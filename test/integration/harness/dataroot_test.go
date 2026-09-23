// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"errors"
	"testing"
)

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

func TestChooseDataRoot_FallbackIsTheStockLayout(t *testing.T) {
	if defaultDockerDataRoot != "/var/lib/docker" {
		t.Fatalf("defaultDockerDataRoot = %q, want /var/lib/docker — changing this silently "+
			"moves where every stock-layout run reads the plugin log from",
			defaultDockerDataRoot)
	}
}
