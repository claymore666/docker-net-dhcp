// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package plugin

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const testCtr = "0123456789abcdef"

func TestBuildResolvConf_SearchListPrecedence(t *testing.T) {
	dns := []string{"192.0.2.1"}

	t.Run("option_119_wins_over_option_15", func(t *testing.T) {
		got := string(buildResolvConf(dns, []string{"a.example", "b.example"}, "fallback.example", ""))
		wantSearch := "search a.example b.example\n"
		if !strings.Contains(got, wantSearch) {
			t.Errorf("expected %q in output; got:\n%s", wantSearch, got)
		}
		if strings.Contains(got, "fallback.example") {
			t.Errorf("option-15 fallback leaked into output despite option-119 being set:\n%s", got)
		}
	})

	t.Run("option_15_used_when_119_empty", func(t *testing.T) {
		got := string(buildResolvConf(dns, nil, "single.example", ""))
		wantSearch := "search single.example\n"
		if !strings.Contains(got, wantSearch) {
			t.Errorf("expected %q in output; got:\n%s", wantSearch, got)
		}
	})

	t.Run("no_search_line_when_both_empty", func(t *testing.T) {
		got := string(buildResolvConf(dns, nil, "", ""))
		if strings.Contains(got, "search ") {
			t.Errorf("unexpected search line when both options empty:\n%s", got)
		}
		if !strings.Contains(got, "nameserver 192.0.2.1") {
			t.Errorf("nameserver missing from output:\n%s", got)
		}
	})

	t.Run("multiple_nameservers_render_in_order", func(t *testing.T) {
		got := string(buildResolvConf([]string{"192.0.2.1", "192.0.2.2"}, nil, "", ""))
		idx1 := strings.Index(got, "nameserver 192.0.2.1")
		idx2 := strings.Index(got, "nameserver 192.0.2.2")
		if idx1 == -1 || idx2 == -1 || idx1 >= idx2 {
			t.Errorf("expected nameservers in DHCP-supplied order; got:\n%s", got)
		}
	})
}

// resolvReadStats counts what a reader saw during a rewrite (#1188). New content plus blank or comment filler is the
// new content. Trailing NULs are trimmed: a plain growing write was seen as the old content plus a NUL, or as the new
// content cut at the old length (tmpfs, 1 to 3 in 400000 reads); the hook test below pins the extension.
type resolvReadStats struct {
	reads, empty, torn, spliced, stale int
	tornSeen                           [][]byte
}

// resolvSplice reports a same-length mix of valid states, each byte from some state or a NUL (#1188). A reader's read(2)
// copies while the write's copy or the Truncate's zeroing runs: about 1 in 30000 reads here (measured 2026-10-03,
// tmpfs), and no in-place write avoids it. The hook test pins the states a mix cannot explain.
// With longer, a byte may also come from a longer state (#1215): read(2) stops at the size it sampled before a copy
// that can fault and sleep, so it returns a later grow's bytes cut at the old length. Measured 2026-10-04, reader page
// unmapped before each read: 609 of 86089 reads on ext4, 6 of 116508 on tmpfs, each in a faulting read and each the
// long content cut at the short one's length. Hosted lanes had 3 unclassified reads in 161661 before this class.
func resolvSplice(got []byte, states [][]byte, longer bool) bool {
	same := false
	for _, s := range states {
		if len(s) == len(got) {
			same = true
		}
	}
	if !same {
		return false
	}
	for i, c := range got {
		if c == 0 {
			continue
		}
		hit := false
		for _, o := range states {
			if (len(o) == len(got) || longer && len(o) > len(got)) && o[i] == c {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

func resolvIsPaddedNew(got, want []byte) bool {
	if !bytes.HasPrefix(got, want) {
		return false
	}
	for _, line := range strings.Split(string(got[len(want):]), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			return false
		}
	}
	return true
}

// readOnce is one read(2), as a stdio resolver does for a small file (#1188): a loop to EOF can span two rewrites.
func readOnce(path string, buf []byte) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

func readWhileRewriting(t *testing.T, path string, contents [][]byte, rounds int, read func() ([]byte, error)) resolvReadStats {
	t.Helper()
	var st resolvReadStats
	var states [][]byte
	for _, c := range contents {
		for _, o := range contents {
			states = append(states, c, resolvPadTo(c, len(o)))
		}
	}
	// A stale-size read needs a grow inside the read; the same bytes with none are on disk (#1215).
	grows := make([]bool, rounds)
	prev, _ := os.ReadFile(path)
	for i := range grows {
		grows[i] = len(contents[i%len(contents)]) > len(prev)
		prev = contents[i%len(contents)]
	}
	var epoch atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			e0 := epoch.Load()
			got, err := read()
			e1 := epoch.Load()
			if err != nil {
				continue
			}
			st.reads++
			raw := got
			got = bytes.TrimRight(got, "\x00")
			if len(got) == 0 {
				st.empty++
				continue
			}
			ok := false
			for _, c := range contents {
				if bytes.Equal(got, c) || resolvIsPaddedNew(got, c) {
					ok = true
					break
				}
			}
			if !ok && resolvSplice(raw, states, false) {
				st.spliced++
				continue
			}
			grew := false
			for i := e0 / 2; 2*i+1 <= e1; i++ {
				grew = grew || grows[i]
			}
			if !ok && grew && resolvSplice(raw, states, true) {
				st.stale++
				continue
			}
			if !ok {
				st.torn++
				if len(st.tornSeen) < 5 {
					st.tornSeen = append(st.tornSeen, append([]byte(nil), raw...))
				}
			}
		}
	}()
	for i := 0; i < rounds; i++ {
		epoch.Store(int64(2*i + 1))
		if err := writeResolvConfFile(testCtr, path, contents[i%len(contents)]); err != nil {
			close(stop)
			<-done
			t.Fatalf("write %d: %v", i, err)
		}
		epoch.Store(int64(2*i + 2))
	}
	close(stop)
	<-done
	return st
}

func TestWriteResolvConfFile_ReaderNeverSeesEmptyOrTorn(t *testing.T) {
	long := buildResolvConf([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, []string{"a.example", "b.example"}, "", "")
	short := buildResolvConf([]string{"192.0.2.9"}, nil, "", "")

	cases := []struct {
		name     string
		contents [][]byte
	}{
		{"shrink_and_grow", [][]byte{long, short}},
		{"unchanged", [][]byte{long, long}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resolv.conf")
			if err := os.WriteFile(path, tc.contents[len(tc.contents)-1], 0644); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 4096)
			st := readWhileRewriting(t, path, tc.contents, 20000, func() ([]byte, error) { return readOnce(path, buf) })
			t.Logf("reads=%d empty=%d torn=%d spliced=%d stale=%d", st.reads, st.empty, st.torn, st.spliced, st.stale)
			if st.reads == 0 {
				t.Fatal("reader never read")
			}
			if st.empty != 0 {
				t.Errorf("reader saw %d empty reads", st.empty)
			}
			resolvReportTorn(t, st, tc.contents)
		})
	}
}

// resolvReportTorn fails t on any torn read and logs the first ones beside each content (#1215).
func resolvReportTorn(t *testing.T, st resolvReadStats, contents [][]byte) {
	t.Helper()
	if st.torn == 0 {
		return
	}
	t.Errorf("reader saw %d reads that were neither the old nor the new content", st.torn)
	for i, c := range contents {
		t.Logf("content %d: len=%d %q", i, len(c), c)
	}
	for _, got := range st.tornSeen {
		t.Logf("torn read: len=%d %q", len(got), got)
	}
}

func TestResolvSplice_StaleSizeTakesOnlyLongerStates(t *testing.T) {
	long := buildResolvConf([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, []string{"a.example", "b.example"}, "", "")
	short := buildResolvConf([]string{"192.0.2.9"}, nil, "", "")
	states := [][]byte{long, short, resolvPadTo(short, len(long))}
	mixed := append(append([]byte{}, short[:len(short)-10]...), long[len(short)-10:len(short)]...)
	foreign := append([]byte{}, short...)
	foreign[len(foreign)-3] = 'X'

	for _, tc := range []struct {
		name          string
		got           []byte
		splice, stale bool
	}{
		{"long_cut_at_short_length", long[:len(short)], false, true},
		{"short_head_long_tail_at_short_length", mixed, false, true},
		{"padded_short_over_long_at_long_length", append(append([]byte{}, short...), long[len(short):]...), true, true},
		{"nul_tail_at_long_length", append(append([]byte{}, short...), make([]byte, len(long)-len(short))...), true, true},
		{"a_byte_from_no_state", foreign, false, false},
		{"a_length_that_is_no_state", long[:len(short)+1], false, false},
		{"long_bytes_past_any_state", append(append([]byte{}, long...), '#'), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvSplice(tc.got, states, false); got != tc.splice {
				t.Errorf("same-length splice = %v, want %v for %q", got, tc.splice, tc.got)
			}
			if got := resolvSplice(tc.got, states, true); got != tc.stale {
				t.Errorf("stale-size splice = %v, want %v for %q", got, tc.stale, tc.got)
			}
		})
	}
}

func TestReadWhileRewriting_CountsEachReadInItsClassAndKeepsFiveTorn(t *testing.T) {
	long := buildResolvConf([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, []string{"a.example", "b.example"}, "", "")
	short := buildResolvConf([]string{"192.0.2.9"}, nil, "", "")
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, short, 0644); err != nil {
		t.Fatal(err)
	}
	foreign := func(n int) []byte { return []byte(fmt.Sprintf("nameserver 203.0.113.%d\n", n)) }
	seq := [][]byte{long, nil, append(append([]byte{}, short...), long[len(short):]...), long[:len(short)]}
	for n := 0; n < 7; n++ {
		seq = append(seq, foreign(n))
	}
	entered, consumed := make(chan struct{}), make(chan struct{})
	var enter sync.Once
	resolvRewriteStep = func(string) { enter.Do(func() { close(entered) }); <-consumed }
	t.Cleanup(func() { resolvRewriteStep = func(string) {} })
	next := 0
	st := readWhileRewriting(t, path, [][]byte{long, short}, 1, func() ([]byte, error) {
		if next == len(seq) {
			return long, nil
		}
		<-entered
		next++
		if next == len(seq) {
			close(consumed)
		}
		return seq[next-1], nil
	})
	if st.empty != 1 || st.spliced != 1 || st.stale != 1 || st.torn != 7 {
		t.Errorf("empty=%d spliced=%d stale=%d torn=%d, want 1 1 1 7", st.empty, st.spliced, st.stale, st.torn)
	}
	if len(st.tornSeen) != 5 {
		t.Fatalf("kept %d torn reads, want the first 5", len(st.tornSeen))
	}
	for n, got := range st.tornSeen {
		if !bytes.Equal(got, foreign(n)) {
			t.Errorf("torn read %d kept as %q, want %q", n, got, foreign(n))
		}
	}
}

func TestReadWhileRewriting_StaleSizeCountsOnlyAcrossAGrow(t *testing.T) {
	long := buildResolvConf([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, []string{"a.example", "b.example"}, "", "")
	short := buildResolvConf([]string{"192.0.2.9"}, nil, "", "")
	for _, tc := range []struct {
		name        string
		from, to    []byte
		stale, torn int
	}{
		{"during_a_grow", short, long, 1, 0},
		{"during_a_shrink", long, short, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resolv.conf")
			if err := os.WriteFile(path, tc.from, 0644); err != nil {
				t.Fatal(err)
			}
			entered, consumed := make(chan struct{}), make(chan struct{})
			var enter sync.Once
			resolvRewriteStep = func(string) { enter.Do(func() { close(entered) }); <-consumed }
			t.Cleanup(func() { resolvRewriteStep = func(string) {} })
			once := false
			st := readWhileRewriting(t, path, [][]byte{tc.to, tc.from}, 1, func() ([]byte, error) {
				if once {
					return tc.to, nil
				}
				once = true
				<-entered
				close(consumed)
				return long[:len(short)], nil
			})
			if st.stale != tc.stale || st.torn != tc.torn {
				t.Errorf("stale=%d torn=%d, want %d %d", st.stale, st.torn, tc.stale, tc.torn)
			}
		})
	}
}

func TestWriteResolvConfFile_StalledReaderSeesOnlyModelledStates(t *testing.T) {
	long := buildResolvConf([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, []string{"a.example", "b.example"}, "", "")
	short := buildResolvConf([]string{"192.0.2.9"}, nil, "", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(path, short, 0644); err != nil {
		t.Fatal(err)
	}
	bfd, err := unix.Open(filepath.Join(dir, "buf"), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(bfd) })
	if err := unix.Ftruncate(bfd, 4096); err != nil {
		t.Fatal(err)
	}
	mbuf, err := unix.Mmap(bfd, 0, 4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Munmap(mbuf) })

	faulted := 0
	st := readWhileRewriting(t, path, [][]byte{long, short}, 20000, func() ([]byte, error) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		_ = unix.Madvise(mbuf, unix.MADV_PAGEOUT)
		_ = unix.Fadvise(bfd, 0, 0, unix.FADV_DONTNEED)
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		defer unix.Close(fd)
		var before, after unix.Rusage
		_ = unix.Getrusage(unix.RUSAGE_THREAD, &before)
		n, _, errno := unix.Syscall(unix.SYS_READ, uintptr(fd), uintptr(unsafe.Pointer(&mbuf[0])), uintptr(len(mbuf)))
		_ = unix.Getrusage(unix.RUSAGE_THREAD, &after)
		if errno != 0 {
			return nil, errno
		}
		if after.Majflt+after.Minflt > before.Majflt+before.Minflt {
			faulted++
		}
		return mbuf[:n], nil
	})
	t.Logf("reads=%d faulted=%d empty=%d torn=%d spliced=%d stale=%d", st.reads, faulted, st.empty, st.torn, st.spliced, st.stale)
	if st.empty != 0 {
		t.Errorf("reader saw %d empty reads", st.empty)
	}
	resolvReportTorn(t, st, [][]byte{long, short})
	if faulted == 0 {
		t.Errorf("no read faulted, not even the first one into the untouched buffer: RUSAGE_THREAD misses kernel-mode faults")
	}
}

func TestWriteResolvConfFile_FinalBytesAndMode(t *testing.T) {
	long := buildResolvConf([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, []string{"a.example", "b.example"}, "", "")
	short := buildResolvConf([]string{"192.0.2.9"}, nil, "", "")
	longer := append(append([]byte{}, long...), "nameserver 192.0.2.4\n"...)

	t.Run("shrink_leaves_no_stale_tail", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.WriteFile(path, long, 0644); err != nil {
			t.Fatal(err)
		}
		if err := writeResolvConfFile(testCtr, path, short); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, short) {
			t.Errorf("want %q, got %q", short, got)
		}
	})

	t.Run("grow_writes_everything", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.WriteFile(path, long, 0644); err != nil {
			t.Fatal(err)
		}
		if err := writeResolvConfFile(testCtr, path, longer); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, longer) {
			t.Errorf("want %q, got %q", longer, got)
		}
	})

	t.Run("one_byte_shrink_is_exact", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.WriteFile(path, []byte("nameserver 192.0.2.10\n"), 0644); err != nil {
			t.Fatal(err)
		}
		want := []byte("nameserver 192.0.2.1\n")
		if err := writeResolvConfFile(testCtr, path, want); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, want) {
			t.Errorf("want %q, got %q", want, got)
		}
	})

	t.Run("same_length_change_is_written", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "resolv.conf")
		if err := os.WriteFile(path, []byte("nameserver 192.0.2.1\n"), 0644); err != nil {
			t.Fatal(err)
		}
		want := []byte("nameserver 192.0.2.2\n")
		if err := writeResolvConfFile(testCtr, path, want); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, want) {
			t.Errorf("a renewal that swaps a resolver for another of the same length left %q, want %q", got, want)
		}
	})

	t.Run("missing_file_is_created_0644", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "resolv.conf")
		if err := writeResolvConfFile(testCtr, path, short); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0644 {
			t.Errorf("mode %v, want 0644", fi.Mode().Perm())
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, short) {
			t.Errorf("want %q, got %q", short, got)
		}
	})

	t.Run("unchanged_write_leaves_mtime_alone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "resolv.conf")
		if err := writeResolvConfFile(testCtr, path, long); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-time.Hour).Truncate(time.Second)
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
		if err := writeResolvConfFile(testCtr, path, long); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Stat(path)
		if !fi.ModTime().Equal(past) {
			t.Errorf("mtime moved to %v on an unchanged write, want %v", fi.ModTime(), past)
		}

		if err := writeResolvConfFile(testCtr, path, short); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, past, past); err != nil {
			t.Fatal(err)
		}
		if err := writeResolvConfFile(testCtr, path, short); err != nil {
			t.Fatal(err)
		}
		fi, _ = os.Stat(path)
		if !fi.ModTime().Equal(past) {
			t.Errorf("mtime moved to %v on an unchanged write after a shrink, want %v", fi.ModTime(), past)
		}
	})
}

func TestWriteResolvConfFile_EveryInstantIsOldOrNew(t *testing.T) {
	long := buildResolvConf([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, []string{"a.example", "b.example"}, "", "")
	short := buildResolvConf([]string{"192.0.2.9"}, nil, "", "")

	for _, tc := range []struct {
		name     string
		old, new []byte
	}{
		{"shrink", long, short},
		{"grow", short, long},
		{"one_byte_shrink", []byte("nameserver 192.0.2.10\n"), []byte("nameserver 192.0.2.1\n")},
		{"two_byte_shrink", []byte("nameserver 192.0.2.100\n"), []byte("nameserver 192.0.2.1\n")},
		{"one_byte_grow", []byte("nameserver 192.0.2.1\n"), []byte("nameserver 192.0.2.10\n")},
		{"two_byte_grow", []byte("nameserver 192.0.2.1\n"), []byte("nameserver 192.0.2.100\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "resolv.conf")
			if err := os.WriteFile(path, tc.old, 0644); err != nil {
				t.Fatal(err)
			}
			var seen []string
			resolvRewriteStep = func(stage string) {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("%s: %v", stage, err)
					return
				}
				seen = append(seen, stage)
				switch stage {
				case "opened":
					if !bytes.Equal(got, tc.old) {
						t.Errorf("after open the file holds %q, want the old content %q", got, tc.old)
					}
				case "extended":
					if !resolvIsPaddedNew(got, tc.old) || len(got) != len(tc.new) {
						t.Errorf("after the extension the file holds %q, want the old content %q plus filler up to %d bytes", got, tc.old, len(tc.new))
					}
				case "written":
					if !resolvIsPaddedNew(got, tc.new) && !bytes.Equal(got, tc.new) {
						t.Errorf("after the write the file holds %q, want the new content %q plus filler", got, tc.new)
					}
					if len(got) < len(tc.old) || len(got) < len(tc.new) {
						t.Errorf("after the write the file is %d bytes, shorter than old (%d) or new (%d)", len(got), len(tc.old), len(tc.new))
					}
				}
			}
			t.Cleanup(func() { resolvRewriteStep = func(string) {} })
			if err := writeResolvConfFile(testCtr, path, tc.new); err != nil {
				t.Fatal(err)
			}
			want := []string{"opened", "written"}
			if len(tc.new) > len(tc.old) {
				want = []string{"opened", "extended", "written"}
			}
			if strings.Join(seen, ",") != strings.Join(want, ",") {
				t.Errorf("hook stages seen: %v, want %v", seen, want)
			}
			got, _ := os.ReadFile(path)
			if !bytes.Equal(got, tc.new) {
				t.Errorf("final content %q, want %q", got, tc.new)
			}
		})
	}
}

// Two renewals for one container can write at once; unserialized, 1.2 % of 20000 finals kept no nameserver (#1188).
func TestWriteResolvConfFile_TwoWritersLeaveOneOfTheInputs(t *testing.T) {
	a := buildResolvConf([]string{"192.0.2.1"}, nil, "", "")
	b := buildResolvConf([]string{"192.0.2.1", "192.0.2.2", "192.0.2.3"}, []string{"a.example", "b.example"}, "", "")
	start := a[:len(a)-1]
	path := filepath.Join(t.TempDir(), "resolv.conf")

	const rounds = 5000
	bad := 0
	var firstBad []byte
	for i := 0; i < rounds; i++ {
		if err := os.WriteFile(path, start, 0644); err != nil {
			t.Fatal(err)
		}
		var ready, wg sync.WaitGroup
		gate := make(chan struct{})
		for _, c := range [][]byte{a, b} {
			ready.Add(1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready.Done()
				<-gate
				if err := writeResolvConfFile(testCtr, path, c); err != nil {
					t.Errorf("write: %v", err)
				}
			}()
		}
		ready.Wait()
		close(gate)
		wg.Wait()
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
			if bad == 0 {
				firstBad = got
			}
			bad++
		}
	}
	if bad != 0 {
		t.Errorf("%d of %d finals were neither input; first: %q", bad, rounds, firstBad)
	}
}

// parkOnFIFO waits until a writer, whose result arrives on parked, is parked on the FIFO at fifo (our write end
// finds its reader), and returns the release that lets it finish (#1188).
func parkOnFIFO(t *testing.T, fifo string, parked <-chan error) func() {
	t.Helper()
	var hold int
	deadline := time.Now().Add(5 * time.Second)
	for {
		fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if err == nil {
			hold = fd
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the blocked writer never opened the FIFO: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	return func() {
		rd, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Errorf("open reader: %v", err)
		}
		unix.Close(hold)
		select {
		case <-parked:
		case <-time.After(5 * time.Second):
			t.Error("the blocked writer did not return after release")
		}
		if err == nil {
			unix.Close(rd)
		}
	}
}

// A write parked on a blocking file holds nothing another container's write needs (#1188); a FIFO stands in for a
// hung mount over /etc/resolv.conf.
func TestWriteResolvConfFile_BlockedWriteDoesNotDelayAnotherContainer(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0644); err != nil {
		t.Fatal(err)
	}
	content := buildResolvConf([]string{"192.0.2.1"}, nil, "", "")

	parked := make(chan error, 1)
	go func() { parked <- writeResolvConfFile("ctr-blocked", fifo, content) }()
	defer parkOnFIFO(t, fifo, parked)()

	other := filepath.Join(dir, "other.conf")
	done := make(chan error, 1)
	go func() { done <- writeResolvConfFile("ctr-other", other, content) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second container write: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second container's write was still blocked after 2 s behind a parked writer")
	}
	got, err := os.ReadFile(other)
	if err != nil || !bytes.Equal(got, content) {
		t.Errorf("second container file = %q, %v; want %q", got, err, content)
	}
}

// Production shape: every container's path is the same string, which only the container's mount namespace resolves.
// A per-thread cwd gives two threads one relative name for two files, so a lock keyed on the path would stall both (#1188).
func TestWriteResolvConfFile_SamePathStringTwoContainers(t *testing.T) {
	root := t.TempDir()
	dirA, dirB := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.Mkdir(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	fifo := filepath.Join(dirA, "resolv.conf")
	if err := unix.Mkfifo(fifo, 0644); err != nil {
		t.Fatal(err)
	}
	content := buildResolvConf([]string{"192.0.2.1"}, nil, "", "")

	// The thread stays locked, so the runtime retires it with the goroutine and its cwd never reaches another test.
	write := func(id, dir string, out chan<- error) {
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			out <- err
			return
		}
		if err := os.Chdir(dir); err != nil {
			out <- err
			return
		}
		out <- writeResolvConfFile(id, "resolv.conf", content)
	}
	parked := make(chan error, 1)
	go write("ctr-a", dirA, parked)
	defer parkOnFIFO(t, fifo, parked)()

	done := make(chan error, 1)
	go write("ctr-b", dirB, done)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second container write: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second container's write to the same path string was still blocked after 2 s")
	}
	got, err := os.ReadFile(filepath.Join(dirB, "resolv.conf"))
	if err != nil || !bytes.Equal(got, content) {
		t.Errorf("second container file = %q, %v; want %q", got, err, content)
	}
}

// No entry outlives its writers, whether they succeed or fail, so a container that is gone leaves no lock (#1188).
func TestWriteResolvConfFile_LockEntryDoesNotOutliveItsWriters(t *testing.T) {
	dir := t.TempDir()
	content := buildResolvConf([]string{"192.0.2.1"}, nil, "", "")
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		id := string(rune('a' + i%3))
		path := filepath.Join(dir, id+".conf")
		if i%4 == 3 {
			path = filepath.Join(dir, "missing", id+".conf")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = writeResolvConfFile(id, path, content)
		}()
	}
	wg.Wait()
	resolvLocks.mu.Lock()
	left := len(resolvLocks.m)
	resolvLocks.mu.Unlock()
	if left != 0 {
		t.Errorf("%d lock entries left after every writer returned", left)
	}
}

// A third writer arriving while the second holds the lock waits for it; an entry dropped early would give it a
// second mutex (#1188).
func TestLockResolv_ThirdWriterWaitsBehindTheSecond(t *testing.T) {
	id := fmt.Sprintf("ctr-three-%d", time.Now().UnixNano())
	unlockA := lockResolv(id)
	gotB, gotC := make(chan func(), 1), make(chan func(), 1)
	go func() { gotB <- lockResolv(id) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resolvLocks.mu.Lock()
		l := resolvLocks.m[id]
		refs := 0
		if l != nil {
			refs = l.refs
		}
		resolvLocks.mu.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second writer never queued")
		}
		time.Sleep(time.Millisecond)
	}
	unlockA()
	unlockB := <-gotB
	go func() { gotC <- lockResolv(id) }()
	select {
	case <-gotC:
		t.Fatal("the third writer took the lock while the second held it")
	case <-time.After(200 * time.Millisecond):
	}
	unlockB()
	select {
	case unlockC := <-gotC:
		unlockC()
	case <-time.After(5 * time.Second):
		t.Fatal("the third writer never got the lock after the second released it")
	}
}
