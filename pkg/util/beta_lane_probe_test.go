// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package util

import "testing"

// 2026-09-03, M8a: reverted in the commit that follows this one. Its only
// purpose is to make one run on 2.x-beta record a red, because a lane
// proven in the passing direction alone is not proven.
func TestBetaLaneProbe_FailsDeliberatelySoTheLaneRecordsARed(t *testing.T) {
	t.Fatal("deliberate failure: 2.x-beta lane observability probe")
}
