//go:build !linux

package runtime

import (
	"time"

	"github.com/claymore666/dhcp-golib/proto"
)

// processStart anchors the fallback monotonic clock.
var processStart = time.Now()

// monoNow is the non-Linux fallback and is NOT equivalent to the Linux one:
// Go's monotonic reading is CLOCK_MONOTONIC, which does not advance across a
// host suspend, so on this build a suspend under-counts elapsed lease time and
// the client holds an address longer than it was granted. This file exists to
// keep the package compiling, not to claim parity.
func monoNow() proto.Instant {
	return proto.Instant(time.Since(processStart))
}
