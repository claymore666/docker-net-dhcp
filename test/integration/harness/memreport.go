// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// MemoryReport returns the test binary's exit-time memory line and whether Sys is above bound, a whole number of MB or "" for none (#1180).
func MemoryReport(bound string) (line string, exceeded bool) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return memoryReport(&ms, bound)
}

// memoryReport compares Sys, not HeapInuse: Sys is what the process took from the OS and it does not fall after a
// GC, so a transient spike that the heap has already given back still shows at exit. The arm64 binary was killed at
// 1.1 GB RSS by one 842 MB log read while the suite's own RSS peak was 24 MB. A malformed bound is named in the
// line and never fails the suite (#1180).
func memoryReport(ms *runtime.MemStats, bound string) (string, bool) {
	const mb = 1 << 20
	heapMB, sysMB := ms.HeapInuse/mb, ms.Sys/mb
	bound = strings.TrimSpace(bound)
	if bound == "" {
		return fmt.Sprintf("integration memory: heap_inuse_mb=%d sys_mb=%d bound_mb=off", heapMB, sysMB), false
	}
	limit, err := strconv.ParseUint(bound, 10, 64)
	if err != nil || limit == 0 {
		return fmt.Sprintf("integration memory: heap_inuse_mb=%d sys_mb=%d bound_mb=ignored (ITEST_HEAP_BOUND_MB=%q is not a positive whole number)", heapMB, sysMB, bound), false
	}
	line := fmt.Sprintf("integration memory: heap_inuse_mb=%d sys_mb=%d bound_mb=%d", heapMB, sysMB, limit)
	if sysMB > limit {
		return line + fmt.Sprintf("\nintegration memory: Sys %d MB exceeds ITEST_HEAP_BOUND_MB=%d", sysMB, limit), true
	}
	return line, false
}
