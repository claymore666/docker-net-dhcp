// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

//go:build integration

package harness

import (
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

const leaseFileThreeLines = "1790000100 aa:bb:cc:00:00:01 192.168.99.41 host-a 01:aa:bb:cc:00:00:01\n" +
	"1790000200 aa:bb:cc:00:00:02 192.168.99.45 host-b 01:aa:bb:cc:00:00:02\n" +
	"1790000300 aa:bb:cc:00:00:03 192.168.99.47 host-c 01:aa:bb:cc:00:00:03\n"

// leaseRewriteEvery is the writer's pause between rewrites, plus the same again at random. dnsmasq rewrites once per
// lease change; a writer with no pause holds the file empty so often that no bounded re-read can pass (#1167).
const leaseRewriteEvery = 4 * time.Millisecond

func writeLeaseFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dnsmasq.leases")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write lease file: %v", err)
	}
	return path
}

// A writer doing dnsmasq's ftruncate-and-rewrite (lease.c, lease_update_file) must never make the reader say false (#1167).
func TestLeaseFileHolds_AReadInsideTheRewriteIsNotARelease(t *testing.T) {
	path := writeLeaseFile(t, leaseFileThreeLines)
	const window = 300 * time.Millisecond
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Truncate(path, 0); err != nil {
				t.Errorf("truncate: %v", err)
				return
			}
			if err := os.WriteFile(path, []byte(leaseFileThreeLines), 0o644); err != nil {
				t.Errorf("rewrite: %v", err)
				return
			}
			// A time.Sleep here entrains to the reader's timers: the reader then read inside three rewrites 20 ms apart in
			// about 1.5 % of runs. Waiting on the clock without a timer measured 0 in 600 (#1167).
			for end := time.Now().Add(leaseRewriteEvery + time.Duration(rng.Int63n(int64(leaseRewriteEvery)))); time.Now().Before(end); {
				runtime.Gosched()
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	reads := 0
	for deadline := time.Now().Add(window); time.Now().Before(deadline); reads++ {
		held, err := LeaseFileHolds(path, "192.168.99.45")
		if err != nil {
			t.Fatalf("LeaseFileHolds: %v", err)
		}
		if !held {
			t.Fatalf("read %d reported the address released while the file was only being rewritten", reads)
		}
	}
	t.Logf("%d reads inside the rewrite window, none reported a release", reads)
}

func TestLeaseFileHolds_AnEmptyFileThatFillsDuringTheGapIsHeld(t *testing.T) {
	path := writeLeaseFile(t, "")
	sleeps := 0
	defer func(old func(time.Duration)) { leaseFileSleep = old }(leaseFileSleep)
	leaseFileSleep = func(d time.Duration) {
		sleeps++
		if d != LeaseFileRereadGap {
			t.Errorf("re-read gap = %v, want %v", d, LeaseFileRereadGap)
		}
		if err := os.WriteFile(path, []byte(leaseFileThreeLines), 0o644); err != nil {
			t.Errorf("rewrite: %v", err)
		}
	}
	held, err := LeaseFileHolds(path, "192.168.99.45")
	if err != nil || !held {
		t.Fatalf("empty file refilled in the gap: held=%v err=%v, want true", held, err)
	}
	if sleeps != 1 {
		t.Fatalf("paused %d times, want 1: presence on the second read is final", sleeps)
	}
}

// A writer parked past the old 40 ms confirmation is still a rewrite in flight (#1173).
func TestLeaseFileHolds_AnEmptyFileParkedPastTheOldBudgetIsHeld(t *testing.T) {
	path := writeLeaseFile(t, "")
	const oldBudget = 40 * time.Millisecond
	sleeps := 0
	defer func(old func(time.Duration)) { leaseFileSleep = old }(leaseFileSleep)
	leaseFileSleep = func(d time.Duration) {
		time.Sleep(d)
		if sleeps++; sleeps == 6 {
			if err := os.WriteFile(path, []byte(leaseFileThreeLines), 0o644); err != nil {
				t.Errorf("rewrite: %v", err)
			}
		}
	}
	start := time.Now()
	held, err := LeaseFileHolds(path, "192.168.99.45")
	took := time.Since(start)
	if err != nil || !held {
		t.Fatalf("empty file refilled after %v: held=%v err=%v, want true", took, held, err)
	}
	if took <= oldBudget {
		t.Fatalf("returned after %v, want past the old %v budget: the test did not park the writer long enough", took, oldBudget)
	}
}

func TestLeaseFileHolds_AFileEmptyForTheWholeWindowIsReleased(t *testing.T) {
	path := writeLeaseFile(t, "")
	start := time.Now()
	held, err := LeaseFileHolds(path, "192.168.99.45")
	took := time.Since(start)
	if err != nil || held {
		t.Fatalf("held=%v err=%v, want false", held, err)
	}
	if took < LeaseFileRewriteWindow {
		t.Fatalf("an empty file was confirmed absent after %v, want at least the %v window", took, LeaseFileRewriteWindow)
	}
}

func TestLeaseFileHolds_AbsenceIsConfirmedByEveryRead(t *testing.T) {
	path := writeLeaseFile(t, leaseFileThreeLines)
	sleeps := 0
	defer func(old func(time.Duration)) { leaseFileSleep = old }(leaseFileSleep)
	leaseFileSleep = func(time.Duration) { sleeps++ }
	held, err := LeaseFileHolds(path, "192.168.99.99")
	if err != nil || held {
		t.Fatalf("held=%v err=%v, want false", held, err)
	}
	if sleeps != LeaseFileRereadTries-1 {
		t.Fatalf("paused %d times, want %d", sleeps, LeaseFileRereadTries-1)
	}
	sleeps = 0
	if held, _ := LeaseFileHolds(path, "192.168.99.45"); !held || sleeps != 0 {
		t.Fatalf("a present line: held=%v sleeps=%d, want true and no pause", held, sleeps)
	}
}

func TestLeaseFileHolds_ARemovedLineIsReleasedWithinAFewRereads(t *testing.T) {
	path := writeLeaseFile(t, leaseFileThreeLines)
	held, err := LeaseFileHolds(path, "192.168.99.45")
	if err != nil || !held {
		t.Fatalf("before the removal: held=%v err=%v, want true", held, err)
	}
	removed := "1790000100 aa:bb:cc:00:00:01 192.168.99.41 host-a 01:aa:bb:cc:00:00:01\n" +
		"1790000300 aa:bb:cc:00:00:03 192.168.99.47 host-c 01:aa:bb:cc:00:00:03\n"
	if err := os.WriteFile(path, []byte(removed), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	start := time.Now()
	held, err = LeaseFileHolds(path, "192.168.99.45")
	if err != nil {
		t.Fatalf("LeaseFileHolds: %v", err)
	}
	if held {
		t.Fatalf("a line that is gone was reported held")
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("a real removal took %v to report, want under 100ms", took)
	}
}

func TestLeaseFileHolds_AnAddressThatWasNeverThereIsNotHeld(t *testing.T) {
	path := writeLeaseFile(t, leaseFileThreeLines)
	held, err := LeaseFileHolds(path, "192.168.99.99")
	if err != nil {
		t.Fatalf("LeaseFileHolds: %v", err)
	}
	if held {
		t.Fatalf("an address no line names was reported held")
	}
	if _, err := LeaseFileHolds(filepath.Join(t.TempDir(), "absent"), "192.168.99.45"); err == nil {
		t.Fatalf("a missing lease file returned no error")
	}
}
