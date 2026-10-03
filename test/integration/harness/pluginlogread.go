// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package harness

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// The plugin log spans the suite at debug level and reaches hundreds of MB on the arm64 lane, whose runner has 1.8 GB:
// the test binary was OOM-killed at 1.07 GB after polling it with whole-file reads (run 37084679425, #1168). Every
// reader below opens the file, seeks, and reads a bounded span; none reads the file whole to learn its length.
const (
	// PluginLogWindowMax caps the window after a mark; a test's window is kilobytes, so reaching it is a finding.
	PluginLogWindowMax = 16 << 20
	// PluginLogWholeMax caps the reads that mean the whole run (the health floor's censuses and the two whole-log
	// assertions). Those read once, at the end of a test or the suite, so the cap sits well above any log seen (#1168).
	PluginLogWholeMax = 512 << 20
	// PluginLogDumpMax is how much of the log's tail DumpPluginLog writes into a test's output.
	PluginLogDumpMax = 256 << 10
)

// errPluginLogOverCap marks a whole-log read refused for size (#1168).
var errPluginLogOverCap = errors.New("plugin log over the whole-read cap (#1168)")

// pluginLogCutLine is appended to a read cut at its cap, so an assertion that misses its evidence says why.
func pluginLogCutLine(max int64) string {
	return fmt.Sprintf("[window cut at %d bytes, #1168]\n", max)
}

// readLogRange returns the bytes of the file at path from off, at most max of them, and the file's size. A read cut at
// max ends on a line boundary when it holds one. An off past the end (rotation, reinstall) is empty, as in
// PluginLogWindow; a negative off is empty too (#1168).
func readLogRange(path string, off, max int64) (data []byte, size int64, cut bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, false, fmt.Errorf("stat %s: %w", path, err)
	}
	size = st.Size()
	if off < 0 || off > size {
		return nil, size, false, nil
	}
	n := size - off
	if n > max {
		n, cut = max, true
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, size, false, fmt.Errorf("seek %s: %w", path, err)
	}
	buf := make([]byte, n)
	got, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, size, false, fmt.Errorf("read %s: %w", path, err)
	}
	buf = buf[:got]
	if cut {
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			buf = buf[:i+1]
		}
	}
	return PluginLogWindow(buf, 0), size, cut, nil
}

// readPluginLogWindowFile returns the log after mark with the cut line appended when the window outgrew its cap.
func readPluginLogWindowFile(path string, mark int64) ([]byte, error) {
	return readCappedFrom(path, mark, PluginLogWindowMax)
}

func readCappedFrom(path string, off, max int64) ([]byte, error) {
	data, _, cut, err := readLogRange(path, off, max)
	if err != nil {
		return nil, err
	}
	if cut {
		data = append(data, pluginLogCutLine(max)...)
	}
	return data, nil
}

// readPluginLogWholeFile returns the log from byte 0 for the health floor, which judges the whole run: a cut there
// would let faults past the cap read as a clean log, so an over-cap log is an error and not a short read (#1168).
func readPluginLogWholeFile(path string) ([]byte, error) {
	return readWholeCapped(path, PluginLogWholeMax)
}

func readWholeCapped(path string, max int64) ([]byte, error) {
	data, size, cut, err := readLogRange(path, 0, max)
	if err != nil {
		return nil, err
	}
	if cut {
		return nil, fmt.Errorf("%s is %d bytes, over the %d the harness reads whole: %w",
			path, size, max, errPluginLogOverCap)
	}
	return data, nil
}

// pluginLogSizeFile is the log's length from its inode, which is all a mark needs.
func pluginLogSizeFile(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	return st.Size(), nil
}

// readPluginLogTailFile returns the last max bytes of the log, starting on a line boundary, and the log's size.
func readPluginLogTailFile(path string, max int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("stat %s: %w", path, err)
	}
	f.Close()
	size := st.Size()
	if size <= max {
		data, _, _, err := readLogRange(path, 0, max)
		return data, size, err
	}
	data, _, _, err := readLogRange(path, size-max, max)
	if err != nil {
		return nil, size, err
	}
	// The read starts mid-line: drop the fragment so the dump does not open on half a line.
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		data = data[i+1:]
	}
	return data, size, nil
}

// FormatPluginLogDump is the text DumpPluginLog logs: the log as is when it fits, else a header naming how much of
// the tail follows (#1168).
func FormatPluginLogDump(path string, tail []byte, size int64) string {
	if size <= int64(len(tail)) {
		return fmt.Sprintf("--- net-dhcp plugin log (%s) ---\n%s", path, tail)
	}
	return fmt.Sprintf("--- net-dhcp plugin log (%s): %d bytes, showing the last %d ---\n%s", path, size, len(tail), tail)
}

// countPluginLogLinesFile counts the lines containing every one of subs without holding the log: one line at a time.
func countPluginLogLinesFile(path string, subs ...string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()
	pats := make([][]byte, len(subs))
	for i, s := range subs {
		pats[i] = []byte(s)
	}
	n := 0
	var carry []byte
	buf := make([]byte, 1<<20)
	count := func(line []byte) {
		for _, p := range pats {
			if !bytes.Contains(line, p) {
				return
			}
		}
		n++
	}
	for {
		got, err := f.Read(buf)
		chunk := buf[:got]
		for len(chunk) > 0 {
			i := bytes.IndexByte(chunk, '\n')
			if i < 0 {
				carry = append(carry, chunk...)
				break
			}
			if len(carry) > 0 {
				carry = append(carry, chunk[:i]...)
				count(carry)
				carry = carry[:0]
			} else {
				count(chunk[:i])
			}
			chunk = chunk[i+1:]
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, fmt.Errorf("read %s: %w", path, err)
		}
	}
	if len(carry) > 0 {
		count(carry)
	}
	return n, nil
}
