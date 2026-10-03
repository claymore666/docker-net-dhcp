// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"os"
	"strings"
	"time"
)

// dnsmasq truncates its lease file and rewrites it in full on every lease change (lease.c, lease_update_file), so an
// empty file is a rewrite in flight, never an absence: re-read it until a line appears or LeaseFileRewriteWindow has
// passed (#1167, #1173). Saturated box (32 busy loops, 86751 rewrites): longest park 14.6 ms, yet the old three
// reads in 40 ms still flipped in 3 of 2000 runs. 500 ms is 34 times that park; the pollers allow seconds.
const (
	LeaseFileRereadTries   = 3
	LeaseFileRereadGap     = 20 * time.Millisecond
	LeaseFileRewriteWindow = 500 * time.Millisecond
)

// leaseFileSleep is the pause between confirming reads; a test swaps it to land a rewrite inside the gap (#1167).
var leaseFileSleep = time.Sleep

// LeaseFileHolds reports whether dnsmasq's lease DB has a line for addr. The address is the third field of a v4
// line (`<expiry> <mac> <addr> <hostname> <client-id>`) and of a v6 line (`<expiry> <iaid> <addr> <hostname> <duid>`).
// Presence is final, a partial rewrite cannot invent a line. Absence is confirmed by LeaseFileRereadTries reads of
// a non-empty file without the line, or by an empty file for the whole LeaseFileRewriteWindow (#1167, #1173).
func LeaseFileHolds(path, addr string) (bool, error) {
	deadline := time.Now().Add(LeaseFileRewriteWindow)
	absent := 0
	for read := 0; ; read++ {
		if read > 0 {
			leaseFileSleep(LeaseFileRereadGap)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 5 && strings.EqualFold(fields[2], addr) {
				return true, nil
			}
		}
		if len(strings.TrimSpace(string(data))) == 0 {
			absent = 0
		} else if absent++; absent >= LeaseFileRereadTries {
			return false, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
	}
}
