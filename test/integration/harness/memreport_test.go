// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"runtime"
	"strings"
	"testing"
)

func TestMemoryReport(t *testing.T) {
	const mb = 1 << 20
	for _, tc := range []struct {
		name     string
		heap     uint64
		sys      uint64
		bound    string
		exceeded bool
		want     []string
	}{
		{"unset bound never exceeds", 10 * mb, 5000 * mb, "", false, []string{"heap_inuse_mb=10", "sys_mb=5000", "bound_mb=off"}},
		{"Sys over the bound exceeds and the line carries both numbers", 10 * mb, 200 * mb, "100", true,
			[]string{"sys_mb=200", "bound_mb=100", "Sys 200 MB exceeds ITEST_HEAP_BOUND_MB=100"}},
		{"Sys under the bound passes", 10 * mb, 90 * mb, "100", false, []string{"sys_mb=90", "bound_mb=100"}},
		{"Sys equal to the bound passes", 10 * mb, 100 * mb, "100", false, []string{"sys_mb=100", "bound_mb=100"}},
		{"the bound is Sys, not HeapInuse: a small heap under a big Sys exceeds", 5 * mb, 300 * mb, "100", true, []string{"Sys 300 MB exceeds"}},
		{"the bound is Sys, not HeapInuse: a big heap under a small Sys does not", 300 * mb, 50 * mb, "100", false, []string{"heap_inuse_mb=300", "sys_mb=50"}},
		{"a bound with spaces is read", 1 * mb, 200 * mb, " 100 ", true, []string{"bound_mb=100"}},
		{"a malformed bound is named and never fails", 1 * mb, 500 * mb, "256MB", false, []string{"bound_mb=ignored", `"256MB"`}},
		{"a zero bound is malformed", 1 * mb, 500 * mb, "0", false, []string{"bound_mb=ignored"}},
		{"a negative bound is malformed", 1 * mb, 500 * mb, "-5", false, []string{"bound_mb=ignored"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, exceeded := memoryReport(&runtime.MemStats{HeapInuse: tc.heap, Sys: tc.sys}, tc.bound)
			if exceeded != tc.exceeded {
				t.Fatalf("exceeded = %v, want %v (line %q)", exceeded, tc.exceeded, line)
			}
			if !strings.HasPrefix(line, "integration memory: ") {
				t.Errorf("line %q lacks the fixed prefix", line)
			}
			for _, w := range tc.want {
				if !strings.Contains(line, w) {
					t.Errorf("line %q lacks %q", line, w)
				}
			}
			if !tc.exceeded && strings.Contains(line, "exceeds") {
				t.Errorf("line %q says exceeds although the bound held", line)
			}
		})
	}
}

func TestMemoryReport_ReadsTheRunningProcess(t *testing.T) {
	line, exceeded := MemoryReport("1048576")
	if exceeded || !strings.Contains(line, "bound_mb=1048576") {
		t.Fatalf("a bound of 1 TB must hold for a test binary: exceeded=%v line=%q", exceeded, line)
	}
	if line, exceeded := MemoryReport("1"); !exceeded || !strings.Contains(line, "exceeds ITEST_HEAP_BOUND_MB=1") {
		t.Fatalf("a bound of 1 MB must be exceeded by a Go test binary's Sys: exceeded=%v line=%q", exceeded, line)
	}
}
