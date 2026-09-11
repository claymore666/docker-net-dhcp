// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

// MinEngineVersion is the lowest Docker Engine release this plugin is
// measured to run on (#670).
//
// IT IS A MEASUREMENT, NOT A JUDGEMENT. Every engine from this one up
// to the current release was driven through the whole baseline —
// plugin create, enable, a network in each of bridge, macvlan and
// ipvlan, a lease confirmed in the DHCP server's own log, and an
// endpoint that survives `docker restart` on the same address — by
// .github/workflows/engine-matrix.yml, one nested daemon per row.
//
// WHAT THE ROW BELOW IT DID, exactly, because the difference decides
// what this number may be called. On the cgroup v2 host the lane runs
// on, 19.03 could not start an ordinary container at all: the cell's
// control step, which runs no code of ours, failed with "cgroups:
// cgroup mountpoint does not exist". Nothing of the plugin ran on that
// row, so it is UNMEASURED and neither this file nor the documentation
// says the plugin fails on 19.03. What is measured is that 20.10 is the
// lowest engine the plugin was shown to work on.
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
