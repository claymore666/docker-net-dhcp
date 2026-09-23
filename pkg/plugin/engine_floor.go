// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"errors"
	"strconv"
	"strings"
)

// MinEngineVersion is the lowest Docker Engine line the engine matrix measured the plugin on,
// read by scripts/engine-floor.sh from this exact declaration (#670).
const MinEngineVersion = "20.10"

// ProductionEngineVersion is the engine build the maintained deployment runs, which the lane
// measures through the newest image of its line, 26.1.4 (#1014).
const ProductionEngineVersion = "26.1.5"

// errEngineTooOld is fatal in main.go, since an engine below the floor does not improve on its own (#670).
var errEngineTooOld = errors.New("refused: the Docker Engine is below the minimum this plugin is measured on")

// engineIdentity's APIVersion is the version the client negotiated, the daemon's maximum on an older engine (#670).
type engineIdentity struct {
	Version    string
	APIVersion string
}

// versionKey compares major.minor as integers, since 20.10 is above 20.9, and returns ok=false on an unreadable
// version (#670).
func versionKey(v string) (major, minor int, ok bool) {
	// Vendor builds append ("26.1.5+dfsg1") and pre-releases add a suffix ("29.0.0-rc.1"); major.minor stays in front.
	fields := strings.SplitN(v, ".", 3)
	if len(fields) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(fields[0])
	if err != nil || major < 0 {
		return 0, 0, false
	}
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

// engineBelowFloor fails open with ok=false on an unreadable version, since refusing on no evidence
// would block a working engine at install time (#670).
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
