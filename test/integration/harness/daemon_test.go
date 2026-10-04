// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	needleA = "level=info"
	needleB = "plugin=net-dhcp"
)

// writeSyntheticLog writes about 64 MiB of dockerd-shaped lines and returns the path and how many lines carry both
// needles. A line longer than logScanBuf arrives in fragments cut at multiples of logScanBuf from its start, so the
// long lines plant a needle across the first and second cut, and the file ends on such a line with no newline (#1180).
func writeSyntheticLog(t *testing.T) (string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dockerd.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	planted := 0
	filler := func(n int) string { return strings.Repeat("x", n) }
	line := func(parts ...string) {
		for _, p := range parts {
			w.WriteString(p)
		}
		w.WriteString("\n")
	}
	longLine := func(length int, at map[string]int, wantBoth bool, newline bool) {
		b := []byte(filler(length))
		for needle, off := range at {
			copy(b[off:], needle)
		}
		w.Write(b)
		if newline {
			w.WriteString("\n")
		}
		if wantBoth {
			planted++
		}
	}

	const target = 64 << 20
	written := 0
	for i := 0; written < target; i++ {
		switch i % 5 {
		case 0:
			line(`time="2026-10-03T11:23:42Z" `, needleA, " ", needleB, " msg=lease ", filler(40))
			planted++
			written += 100
		case 1:
			line(`time="2026-10-03T11:23:42Z" `, needleA, " msg=other ", filler(60))
			written += 100
		case 2:
			line(`time="2026-10-03T11:23:42Z" `, needleB, " msg=other ", filler(60))
			written += 100
		case 3:
			line()
			written++
		case 4:
			line(`time="2026-10-03T11:23:42Z" level=debug msg=plain `, filler(50))
			written += 100
		}
		if i%40000 == 0 {
			longLine(3*logScanBuf+100, map[string]int{needleA: 10, needleB: 30}, true, true)
			longLine(3*logScanBuf+100, map[string]int{needleA: logScanBuf - 3, needleB: 5}, true, true)
			longLine(3*logScanBuf+100, map[string]int{needleA: 7, needleB: 2*logScanBuf - 5}, true, true)
			longLine(3*logScanBuf+100, map[string]int{needleA: logScanBuf - 3}, false, true)
			line(needleA)
			longLine(2*logScanBuf, map[string]int{needleB: logScanBuf + 1}, false, true)
			written += 14 * logScanBuf
		}
	}
	longLine(2*logScanBuf+20, map[string]int{needleA: logScanBuf - 4, needleB: 2*logScanBuf - 6}, true, false)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path, planted
}

func TestCountLinesWithNeedles_CountsEveryLineCarryingAllNeedles(t *testing.T) {
	path, planted := writeSyntheticLog(t)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := countLinesWithNeedles(f, needleA, needleB)
	if err != nil {
		t.Fatal(err)
	}
	if got != planted {
		t.Fatalf("counted %d lines carrying both needles, planted %d", got, planted)
	}
}

// A needle planted at every offset around the first and second fragment cut, from one that ends just before the cut to
// one that starts on it, so the extremes are in: the needle whose first byte is the last before the cut, and the one
// whose last byte is the first after it. The latter is the one a carry shorter than len(needle)-1 bytes loses (#1180).
func TestCountLinesWithNeedles_NeedleAtEveryOffsetAroundACut(t *testing.T) {
	const lineLen = 3*logScanBuf + 100
	for _, straddler := range []string{needleA, needleB} {
		other := needleB
		if straddler == needleB {
			other = needleA
		}
		for _, cut := range []int{logScanBuf, 2 * logScanBuf} {
			for off := cut - len(straddler) - 1; off <= cut+1; off++ {
				for _, otherAt := range []int{3, lineLen - len(other) - 3} {
					b := []byte(strings.Repeat("x", lineLen))
					copy(b[otherAt:], other)
					copy(b[off:], straddler)
					got, err := countLinesWithNeedles(strings.NewReader(string(b)+"\n"), needleA, needleB)
					if err != nil || got != 1 {
						t.Errorf("%q at %d (cut %d, %q at %d): counted %d, %v; want 1", straddler, off, cut, other, otherAt, got, err)
					}
				}
			}
		}
	}
}

// With no needles every non-empty line counts and an empty one never does, as the whole-file read did (#1180).
func TestCountLinesWithNeedles_NoNeedlesCountsNonEmptyLines(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0}, {"\n", 0}, {"\n\n\n", 0}, {"a\n\nb\n", 2}, {"a\n\nb", 2}, {"a", 1},
	} {
		got, err := countLinesWithNeedles(strings.NewReader(tc.in))
		if err != nil || got != tc.want {
			t.Errorf("countLinesWithNeedles(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

// A scan holds one buffer, so what it allocates cannot grow with the file. The file is 64 MiB and the bound 2 MiB, 32
// scan buffers (bufio's 64 KiB, the join scratch of at most one more, the carry of a needle): the old whole-file read
// allocated 2x the file, over 128 MiB. TotalAlloc and not HeapInuse because it counts every allocation ever made: a
// whole-file read that a GC then frees would still show, where HeapInuse after the call would not (#1180).
func TestCountLinesWithNeedles_MemoryDoesNotScaleWithTheFile(t *testing.T) {
	path, _ := writeSyntheticLog(t)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() < 64<<20 {
		t.Fatalf("synthetic log is %v bytes (err %v), want at least 64 MiB", fi, err)
	}

	const bound = 2 << 20
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := countLinesWithNeedles(f, needleA, needleB); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	delta := after.TotalAlloc - before.TotalAlloc
	t.Logf("TotalAlloc delta %d bytes for a scan of the 64 MiB log (bound %d)", delta, bound)
	if delta > bound {
		t.Fatalf("scanning allocated %d bytes, over the %d bound: the memory scales with the file (#1180)", delta, bound)
	}
}
