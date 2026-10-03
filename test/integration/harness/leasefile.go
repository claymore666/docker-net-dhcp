// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"os"
	"strings"
	"time"
)

// LeaseFileRereadTries and LeaseFileRereadGap bound the confirmation of an absent lease line. dnsmasq rewrites
// its lease file on every lease change with ftruncate(fd, 0) and a full rewrite (lease.c, lease_update_file), so
// one read can land on an empty or partial file and report a release that never happened (#1167).
const (
	LeaseFileRereadTries = 3
	LeaseFileRereadGap   = 20 * time.Millisecond
)

// leaseFileSleep is the pause between confirming reads; a test swaps it to land a rewrite inside the gap (#1167).
var leaseFileSleep = time.Sleep

// LeaseFileHolds reports whether dnsmasq's lease DB has a line for addr. The address is the third field of a v4
// line (`<expiry> <mac> <addr> <hostname> <client-id>`) and of a v6 line (`<expiry> <iaid> <addr> <hostname> <duid>`).
// Presence is final, a partial rewrite cannot invent a line. Absence is confirmed: false only when every one of
// LeaseFileRereadTries reads, LeaseFileRereadGap apart, finds no line; an empty file counts as absent (#1167).
func LeaseFileHolds(path, addr string) (bool, error) {
	for try := 0; try < LeaseFileRereadTries; try++ {
		if try > 0 {
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
	}
	return false, nil
}
