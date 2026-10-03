// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	bigLogSize  = 64 << 20
	logLineSize = 256
)

// writeBigLog writes size bytes of log-shaped lines, each exactly logLineSize bytes with its newline, and returns the
// path. The numbered prefix says which line a read began on (#1168).
func writeBigLog(t *testing.T, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "net-dhcp.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var chunk bytes.Buffer
	for i := 0; i < size/logLineSize; i++ {
		line := fmt.Sprintf(`level=debug msg="renewed lease" line=%09d endpoint=8f21c0a4`, i)
		chunk.WriteString(line)
		chunk.WriteString(strings.Repeat(" ", logLineSize-1-len(line)))
		chunk.WriteByte('\n')
		if chunk.Len() >= 1<<20 {
			if _, err := f.Write(chunk.Bytes()); err != nil {
				t.Fatal(err)
			}
			chunk.Reset()
		}
	}
	if _, err := f.Write(chunk.Bytes()); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadPluginLogWindow_AllocationFollowsTheWindowNotTheFile(t *testing.T) {
	path := writeBigLog(t, bigLogSize)
	const window = 1 << 20
	mark := int64(bigLogSize - window)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := readPluginLogWindowFile(path, mark)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != window {
		t.Fatalf("the window holds %d bytes, want the %d written after the mark", len(got), window)
	}
	if !strings.HasPrefix(string(got), fmt.Sprintf("level=debug msg=\"renewed lease\" line=%09d", int(mark)/logLineSize)) {
		t.Errorf("the window does not begin at the mark: %.60q", got)
	}
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("window %d bytes of a %d byte log: %d bytes allocated", window, bigLogSize, alloc)
	if alloc >= 4*window {
		t.Errorf("reading a %d byte window allocated %d bytes, %d times the window and %.0f%% of the %d byte file; "+
			"a poll that does this keeps a heap the size of the log live (#1168, the arm64 test binary was "+
			"OOM-killed at 1.07 GB)", window, alloc, alloc/window, 100*float64(alloc)/bigLogSize, bigLogSize)
	}
}

func TestReadPluginLogWindow_AWindowOverTheCapIsCutAndSaysSo(t *testing.T) {
	path := writeBigLog(t, bigLogSize)

	got, err := readPluginLogWindowFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}

	marker := fmt.Sprintf("[window cut at %d bytes, #1168]\n", PluginLogWindowMax)
	if !strings.HasSuffix(string(got), marker) {
		t.Fatalf("a %d byte window over the %d cap ends without the cut line, so an assertion that misses its "+
			"evidence cannot say why; the end is %.120q", bigLogSize, PluginLogWindowMax, got[len(got)-120:])
	}
	body := got[:len(got)-len(marker)]
	if len(body) > PluginLogWindowMax {
		t.Errorf("the window body holds %d bytes, over the %d cap", len(body), PluginLogWindowMax)
	}
	if body[len(body)-1] != '\n' {
		t.Errorf("the cut left half a line at the end of the body: %.80q", body[len(body)-80:])
	}
}

func TestReadPluginLogWindow_AWindowAtTheCapIsNotCut(t *testing.T) {
	path := writeBigLog(t, bigLogSize)

	got, err := readPluginLogWindowFile(path, bigLogSize-PluginLogWindowMax)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != PluginLogWindowMax || strings.Contains(string(got[len(got)-80:]), "window cut") {
		t.Errorf("a window of exactly the cap carries a cut line or lost bytes: %d bytes", len(got))
	}
}

func TestReadPluginLogWindow_AMarkPastTheEndIsEmpty(t *testing.T) {
	path := writeBigLog(t, 1<<20)

	for _, mark := range []int64{1<<20 + 1, 1 << 40, -1} {
		got, err := readPluginLogWindowFile(path, mark)
		if err != nil {
			t.Fatalf("mark %d: %v", mark, err)
		}
		if len(got) != 0 {
			t.Errorf("mark %d is past the log (rotated or reinstalled) and must give an empty window, "+
				"as PluginLogWindow does; got %d bytes", mark, len(got))
		}
	}
}

func TestReadPluginLogWindow_AMarkAtTheEndIsEmptyAndZeroIsTheWhole(t *testing.T) {
	path := writeBigLog(t, 1<<20)

	atEnd, err := readPluginLogWindowFile(path, 1<<20)
	if err != nil || len(atEnd) != 0 {
		t.Errorf("a mark at the end gives %d bytes, err %v; want an empty window", len(atEnd), err)
	}
	whole, err := readPluginLogWindowFile(path, 0)
	if err != nil || len(whole) != 1<<20 {
		t.Errorf("a mark of zero gives %d bytes, err %v; want the whole log", len(whole), err)
	}
}

func TestPluginLogSizeFile_IsTheFilesLengthWithoutReadingIt(t *testing.T) {
	path := writeBigLog(t, bigLogSize)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	size, err := pluginLogSizeFile(path)
	runtime.ReadMemStats(&after)

	if err != nil || size != bigLogSize {
		t.Fatalf("size %d, err %v; want %d", size, err, bigLogSize)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 {
		t.Errorf("taking a mark allocated %d bytes of a %d byte log; it needs the length and no content (#1168)",
			alloc, bigLogSize)
	}
	if _, err := pluginLogSizeFile(path + ".gone"); err == nil {
		t.Error("a missing log gave no error, so MarkPluginLog would hand out a mark of 0 for it")
	}
}

func TestReadWholeCapped_OverTheCapIsAnErrorNotAShortLog(t *testing.T) {
	path := writeBigLog(t, 1<<20)

	_, err := readWholeCapped(path, 1<<19)
	if !errors.Is(err, errPluginLogOverCap) {
		t.Errorf("a log over the whole-read cap gave err %v; the health floor would count faults over a log cut "+
			"short and report it clean", err)
	}
	data, err := readWholeCapped(path, 1<<20)
	if err != nil || len(data) != 1<<20 {
		t.Errorf("a log of exactly the cap gave %d bytes, err %v; want it whole", len(data), err)
	}
}

func TestReadPluginLogTailFile_StartsOnALineAndNamesTheSize(t *testing.T) {
	path := writeBigLog(t, 1<<20)

	tail, size, err := readPluginLogTailFile(path, 4*logLineSize+100)
	if err != nil {
		t.Fatal(err)
	}

	if size != 1<<20 {
		t.Errorf("size %d, want the file's %d", size, 1<<20)
	}
	if len(tail) != 4*logLineSize {
		t.Errorf("the tail holds %d bytes, want the four whole lines inside the bound (%d)", len(tail), 4*logLineSize)
	}
	last := fmt.Sprintf("line=%09d", (1<<20)/logLineSize-1)
	if !strings.Contains(string(tail), last) || !strings.HasPrefix(string(tail), "level=debug") {
		t.Errorf("the tail is not the end of the log on a line boundary: %.80q", tail)
	}
	text := FormatPluginLogDump("p.log", tail, size)
	if !strings.Contains(text, fmt.Sprintf("%d bytes, showing the last %d", size, len(tail))) {
		t.Errorf("a cut dump does not say how much it shows: %.140q", text)
	}
	whole, wsize, err := readPluginLogTailFile(path, 1<<20)
	if err != nil || len(whole) != 1<<20 || wsize != 1<<20 {
		t.Errorf("a bound equal to the size gave %d bytes, err %v; want the whole log", len(whole), err)
	}
	if got := FormatPluginLogDump("p.log", whole, wsize); got != "--- net-dhcp plugin log (p.log) ---\n"+string(whole) {
		t.Errorf("an uncut dump differs from the text DumpPluginLog always logged: %.100q", got)
	}
}

func TestCountPluginLogLinesFile_MatchesTheSplitCountAcrossChunkEdges(t *testing.T) {
	// A 1 MiB read buffer cuts the file mid-line; the last line has no newline.
	var b strings.Builder
	for i := 0; b.Len() < 3<<20+17; i++ {
		fmt.Fprintf(&b, "line %d ep=A%d%s\n", i, i%3, strings.Repeat("x", i%301))
	}
	text := strings.TrimSuffix(b.String(), "\n") + " ep=A1 tail"
	path := filepath.Join(t.TempDir(), "net-dhcp.log")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, subs := range [][]string{{"ep=A1"}, {"ep=A2", "xx"}, {"tail"}, {"nothing"}} {
		want := 0
		for _, line := range strings.Split(text, "\n") {
			ok := true
			for _, s := range subs {
				ok = ok && strings.Contains(line, s)
			}
			if ok {
				want++
			}
		}
		got, err := countPluginLogLinesFile(path, subs...)
		if err != nil || got != want {
			t.Errorf("subs %q: counted %d, err %v; the split count is %d", subs, got, err, want)
		}
	}
}

func TestReadCappedFrom_ACutNeverEndsInHalfALine(t *testing.T) {
	// 70-byte lines against a 1000-byte cap: the cap falls inside the 15th line.
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "line=%03d %s\n", i, strings.Repeat("y", 70-9-1))
	}
	path := filepath.Join(t.TempDir(), "net-dhcp.log")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readCappedFrom(path, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	marker := pluginLogCutLine(1000)
	body := strings.TrimSuffix(string(got), marker)
	if body == string(got) {
		t.Fatalf("no cut line at the end: %.100q", got[len(got)-100:])
	}
	if len(body)%70 != 0 || len(body) == 0 || len(body) > 1000 {
		t.Errorf("the body holds %d bytes; want whole 70-byte lines inside the cap, with no half line before the "+
			"cut line", len(body))
	}
}
