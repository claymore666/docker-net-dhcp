// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// Package buildinfo carries what this binary was built from.
package buildinfo

// The defaults are words: an unset -X leaves "", which renders as label="" or "" and reads as a real value (#910).
// Commit is the full revision, since git abbreviates to a length that depends on the clone.

// Version, Commit and Library are the release tag, git revision and DHCP library module version, set with -ldflags -X.
var (
	Version = "dev"
	Commit  = "unknown"
	Library = "unknown"
)
