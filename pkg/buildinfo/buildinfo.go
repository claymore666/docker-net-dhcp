// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

// Package buildinfo carries what this binary was built from.
//
// It exists so that "which plugin is running?" is a question an
// operator can ask the plugin instead of one they answer by comparing
// image digests. Until 2.0 nothing in the process knew: the Dockerfile
// built with no -ldflags and the release workflow knew the tag and did
// not pass it, so every image, every branch build and every local
// `make` produced a binary that could say nothing about itself.
package buildinfo

// The three values, set with -ldflags -X at build time.
//
// THE DEFAULTS ARE WORDS, NOT EMPTY STRINGS, and that is the whole
// point of this file. An unset -X leaves a Go string variable empty; an
// empty Prometheus label renders as label="" and an empty JSON field
// renders as "", and both read to a human as "nothing is wrong here"
// rather than as "this build does not know". `dev` and `unknown` are
// answers; "" is a silence that looks like an answer.
//
// Version is the release tag the image was published under, or `dev`
// for anything built outside a release. Commit is the git revision the
// tree was at, in FULL: git abbreviates to a length that depends on the
// size of the clone, so an abbreviated value would let the same commit
// build to different binaries. Library is the revision of the DHCP
// library the tree carries -- the contents of internal/dhcp-golib/SOURCE,
// which is the only place that fact is written down while the library
// travels as a directory (D21).
var (
	Version = "dev"
	Commit  = "unknown"
	Library = "unknown"
)
