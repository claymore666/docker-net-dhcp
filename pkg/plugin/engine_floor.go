// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"strconv"
	"strings"
)

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

// errEngineTooOld is what NewPlugin returns when the daemon answered
// and named a version below MinEngineVersion. It is fatal in main.go,
// the same shape as a lease record whose lock another process holds:
// the condition will not improve on its own, and serving through it
// means failing later at a moment nobody is watching.
var errEngineTooOld = errors.New("refused: the Docker Engine is below the minimum this plugin is measured on")

// engineIdentity is what one startup probe of the daemon learned. Both
// fields are what the DAEMON said, not what this process assumed:
// APIVersion is the version the client library NEGOTIATED, so on an
// engine older than the library's own default it is the daemon's
// maximum and not ours.
type engineIdentity struct {
	Version    string
	APIVersion string
}

// versionKey turns a Docker Engine version into a comparable pair.
//
// ENGINE VERSIONS ARE NOT DECIMALS. 20.10 is above 20.9 and below 23.0,
// so a float comparison orders them wrongly; and "9" sorts above "20"
// as a string. Comparing the two integers in order is the only reading
// that gets both right.
//
// Anything after major.minor is ignored on purpose. The floor names a
// LINE, because that is what the matrix measures: a tag that follows
// its line rather than one frozen build of it.
//
// A version this cannot read returns ok=false, and the caller must not
// turn that into a refusal: an unreadable version is no evidence about
// the engine, and refusing on no evidence would turn an unparsed
// vendor suffix into a plugin that will not start.
func versionKey(v string) (major, minor int, ok bool) {
	// A vendor build appends to the version ("20.10.24+azure",
	// "26.1.5+dfsg1"), and a pre-release prepends nothing but adds a
	// suffix ("29.0.0-rc.1"). Both keep major.minor at the front.
	fields := strings.SplitN(v, ".", 3)
	if len(fields) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(fields[0])
	if err != nil || major < 0 {
		return 0, 0, false
	}
	// The minor may carry the patch's separator already stripped, but on
	// a two-field version ("20.10") it can still carry a suffix.
	minorField := fields[1]
	for i, r := range minorField {
		if r < '0' || r > '9' {
			minorField = minorField[:i]
			break
		}
	}
	minor, err = strconv.Atoi(minorField)
	if err != nil || minor < 0 {
		return 0, 0, false
	}
	return major, minor, true
}

// engineBelowFloor reports whether engineVersion is below floor.
//
// THE UNREADABLE CASE IS NOT BELOW THE FLOOR. It returns false with
// ok=false, and every caller treats that as "no evidence" rather than
// as a refusal.
//
// WHY IT FAILS OPEN, and the reason is a cost comparison and not a
// claim about old engines. Nothing is measured about an engine whose
// version this cannot read, so neither direction is the safe one on
// evidence. What differs is what each costs when it is wrong. Failing
// closed on a vendor spelling nobody anticipated refuses an engine that
// may well work, and it refuses it at install time, where the operator
// has no way to overrule it. Failing open on an engine that really is
// below the floor leaves the plugin running unsupported, which is the
// state every release before this one shipped in, and judgeEngine says
// out loud that the minimum was not checked. This project has no
// measurement of what a below-floor engine does after an install that
// got that far: the matrix row below the floor never reached an install
// step (19.03 stops at the cell's control container), so any sentence
// here about how such an engine fails would be a guess.
func engineBelowFloor(engineVersion, floor string) (below, ok bool) {
	eMajor, eMinor, eOK := versionKey(engineVersion)
	fMajor, fMinor, fOK := versionKey(floor)
	if !eOK || !fOK {
		return false, false
	}
	if eMajor != fMajor {
		return eMajor < fMajor, true
	}
	return eMinor < fMinor, true
}
