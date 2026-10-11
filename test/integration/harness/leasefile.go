// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"bytes"
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

// LeaseFileHolds reports whether dnsmasq's lease DB has a line for addr; see LeaseFileLine for the confirmation rule.
func LeaseFileHolds(path, addr string) (bool, error) {
	fields, err := LeaseFileLine(path, addr)
	return fields != nil, err
}

// LeaseFileLine returns the fields of the lease DB line for addr, taken from the read that saw it, or nil once absence
// is confirmed. The address is the third field of a v4 line (`<expiry> <mac> <addr> <hostname> <client-id>`) and of a
// v6 line (`<expiry> <iaid> <addr> <hostname> <duid>`). Presence is final, but only for a line
// with its newline: dnsmasq flushes in several write() calls, so a cut can leave five fields with a short last one.
// Absence is LeaseFileRereadTries reads of a file with complete lines but not this one, or a file with no complete
// line for the whole LeaseFileRewriteWindow; a caller that parses the line from a second read reopens #1167 (#1167, #1173).
func LeaseFileLine(path, addr string) ([]string, error) {
	deadline := time.Now().Add(LeaseFileRewriteWindow)
	absent := 0
	for read := 0; ; read++ {
		if read > 0 {
			leaseFileSleep(LeaseFileRereadGap)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		complete := string(data[:bytes.LastIndexByte(data, '\n')+1])
		for _, line := range strings.Split(complete, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 5 && strings.EqualFold(fields[2], addr) {
				return fields, nil
			}
		}
		if len(strings.TrimSpace(complete)) == 0 {
			absent = 0
		} else if absent++; absent >= LeaseFileRereadTries {
			return nil, nil
		}
		if !time.Now().Before(deadline) {
			return nil, nil
		}
	}
}
