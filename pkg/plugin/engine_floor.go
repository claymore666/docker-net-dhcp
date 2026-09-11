// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

// MinEngineVersion is the lowest Docker Engine release this plugin is
// measured to run on (#670).
//
// IT IS A MEASUREMENT, NOT A JUDGEMENT. Every engine from this one up
// to the current release was driven through the whole baseline —
// install, enable, network create, a lease confirmed in the DHCP
// server's own log, and an endpoint that survives `docker restart` —
// by .github/workflows/engine-matrix.yml, one nested daemon per row.
// The row below this one fails, and the row below that cannot start a
// container at all on a cgroup v2 host, 20.10 being the first release
// with cgroup v2 support.
//
// THE FORMAT IS LOAD-BEARING. major.minor, quoted, on one line, with
// this exact constant name: scripts/engine-floor.sh reads the floor out
// of this declaration rather than carrying a second copy of the number,
// and a reformatting it cannot match is a refusal there, not an empty
// answer. Patch versions are deliberately absent — the matrix drives
// tags, which follow their line, and a floor naming a patch would claim
// evidence about one build of it.
//
// CHANGING IT IS A MEASUREMENT, NOT AN EDIT. The engine matrix
// reconciles this constant against the lowest row that actually passed
// and goes red when they disagree, and this file is in that lane's
// `paths:` so the reconciliation runs on the commit that moves it.
// Raising it drops support for installs that worked before, which #674
// asks the maintainer to treat as a major-version question.
const MinEngineVersion = "20.10"
