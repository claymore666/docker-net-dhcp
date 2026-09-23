// Copyright the docker-net-dhcp contributors.
// SPDX-License-Identifier: GPL-3.0-only

package main

import (
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShouldPet(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	for _, tc := range []struct {
		name       string
		lastGood   time.Time
		staleAfter time.Duration
		want       bool
	}{
		{"never probed", time.Time{}, 45 * time.Second, false},
		{"just probed", now, 45 * time.Second, true},
		{"within the limit", now.Add(-44 * time.Second), 45 * time.Second, true},
		{"exactly at the limit", now.Add(-45 * time.Second), 45 * time.Second, true},
		{"one second over", now.Add(-46 * time.Second), 45 * time.Second, false},
		{"long gone", now.Add(-10 * time.Minute), 45 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldPet(now, tc.lastGood, tc.staleAfter); got != tc.want {
				t.Fatalf("shouldPet(last=%v, stale=%v) = %v, want %v", tc.lastGood, tc.staleAfter, got, tc.want)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	ok := config{
		petInterval:   10 * time.Second,
		probeInterval: 10 * time.Second,
		staleAfter:    45 * time.Second,
		hwTimeout:     60 * time.Second,
	}
	if err := ok.validate(); err != nil {
		t.Fatalf("the shipped defaults must validate: %v", err)
	}

	for _, tc := range []struct {
		name string
		mut  func(*config)
		want string
	}{
		{
			"stale-after at or over the hardware timeout",
			func(c *config) { c.staleAfter = 60 * time.Second },
			"shorter than the hardware timeout",
		},
		{
			"probe-interval at or over stale-after",
			func(c *config) { c.probeInterval = 45 * time.Second },
			"shorter than stale-after",
		},
		{
			"pet-interval over half the hardware timeout",
			func(c *config) { c.petInterval = 31 * time.Second },
			"under half the hardware timeout",
		},
		{"zero stale-after", func(c *config) { c.staleAfter = 0 }, "must all be positive"},
		{"zero pet-interval", func(c *config) { c.petInterval = 0 }, "must all be positive"},
		{"zero probe-interval", func(c *config) { c.probeInterval = 0 }, "must all be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			tc.mut(&c)
			err := c.validate()
			if err == nil {
				t.Fatalf("want a rejection mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %q", tc.want, err)
			}
		})
	}
}

func TestProber_BlockedProbeGoesStale(t *testing.T) {
	release := make(chan struct{})
	p := &prober{
		path:     "/irrelevant",
		interval: time.Millisecond,
		statfs: func(string) error {
			<-release
			return nil
		},
	}
	stop := make(chan struct{})
	go p.run(stop)
	defer func() { close(release); close(stop) }()

	time.Sleep(20 * time.Millisecond)
	if got := p.lastGood(); !got.IsZero() {
		t.Fatalf("a blocked probe must never publish a timestamp, got %v", got)
	}
	if shouldPet(time.Now(), p.lastGood(), time.Second) {
		t.Fatal("a blocked probe must not keep the watchdog fed")
	}
}

func TestProber_FailingProbeStopsPublishing(t *testing.T) {
	var fail atomic.Bool
	// Closed by the fake when it first returns the error: run() is one goroutine, so every earlier success is stored
	// by then and a sample taken after it is final; the 50ms below is still the window a wrong publish is caught in (#874).
	failed := make(chan struct{})
	var failedOnce sync.Once
	p := &prober{
		path:     "/irrelevant",
		interval: time.Millisecond,
		statfs: func(string) error {
			if fail.Load() {
				failedOnce.Do(func() { close(failed) })
				return errors.New("stale file handle")
			}
			return nil
		},
	}
	stop := make(chan struct{})
	go p.run(stop)
	defer close(stop)

	deadline := time.Now().Add(2 * time.Second)
	for p.lastGood().IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("a succeeding probe never published a timestamp")
		}
		time.Sleep(time.Millisecond)
	}

	fail.Store(true)
	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("the prober never ran a probe after the filesystem started failing")
	}
	frozen := p.lastGood()
	time.Sleep(50 * time.Millisecond)
	if got := p.lastGood(); !got.Equal(frozen) {
		t.Fatalf("a failing probe must not refresh the timestamp: %v -> %v", frozen, got)
	}
}

func TestStatfsProbe(t *testing.T) {
	if err := statfsProbe(t.TempDir()); err != nil {
		t.Fatalf("statfs on a real directory must succeed: %v", err)
	}
	if err := statfsProbe("/definitely/not/here"); err == nil {
		t.Fatal("statfs on a missing path must fail, otherwise the probe proves nothing")
	}
}

func TestRun_PetsAHealthyDeviceOnItsOwnTicker(t *testing.T) {
	dev, err := os.CreateTemp(t.TempDir(), "watchdog")
	if err != nil {
		t.Fatal(err)
	}
	w := &watchdog{f: dev}

	p := &prober{path: "/irrelevant", interval: time.Hour, statfs: statfsProbe}
	p.last.Store(time.Now().UnixNano())

	c := config{petInterval: 5 * time.Millisecond, probeInterval: time.Millisecond,
		staleAfter: time.Hour, hwTimeout: 2 * time.Hour}

	sig := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		run(w, p, c, sig, make(chan struct{}), func(string, ...any) {})
		close(done)
	}()

	// 600 pet intervals of headroom; a ticker a thousand times slower
	// than configured misses it (#632).
	deadline := time.Now().Add(3 * time.Second)
	for size(t, dev.Name()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("run() never petted a healthy device in 3s with a %s pet interval", c.petInterval)
		}
		time.Sleep(time.Millisecond)
	}
	sig <- os.Interrupt
	<-done
}

func TestRun_StopsPettingWhenTheProbeGoesStale(t *testing.T) {
	dev, err := os.CreateTemp(t.TempDir(), "watchdog")
	if err != nil {
		t.Fatal(err)
	}
	w := &watchdog{f: dev}

	// Ticks carry synthetic times and each send on the unbuffered channel
	// returns only once the loop has handled the tick before it; sleeping
	// on a real ticker flaked under -race (#632, run 35876722101).
	last := time.Now().Add(-time.Hour)
	p := &prober{path: "/irrelevant", interval: time.Hour, statfs: statfsProbe}
	p.last.Store(last.UnixNano())

	c := config{petInterval: time.Hour, probeInterval: time.Millisecond,
		staleAfter: 40 * time.Millisecond, hwTimeout: time.Second}

	sig := make(chan os.Signal, 1)
	ticks := make(chan time.Time)
	var logged []string
	done := make(chan struct{})
	go func() {
		petLoop(w, p, c, sig, make(chan struct{}), ticks, func(f string, a ...any) { logged = append(logged, f) })
		close(done)
	}()

	ticks <- last
	ticks <- last.Add(c.staleAfter)
	ticks <- last.Add(c.staleAfter + time.Nanosecond)
	ticks <- last.Add(time.Second)
	if got := size(t, dev.Name()); got != 2 {
		t.Fatalf("want one pet per healthy tick (2), device holds %d bytes", got)
	}
	ticks <- last.Add(time.Minute)
	sig <- os.Interrupt
	<-done
	if got := size(t, dev.Name()); got != 2 {
		t.Fatalf("the watchdog was still being petted after the probe went stale: 2 -> %d", got)
	}

	// systemd stops units before it unmounts, so this SIGTERM arrives before the unmount that hangs; disarming here left
	// the board hung for 14 minutes on 2026-08-20 (#684).
	b, err := os.ReadFile(dev.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), magicClose) {
		t.Fatalf("the watchdog was disarmed on a stop taken while the filesystem was already gone; tail is %q", tail(string(b)))
	}

	var refusals int
	for _, l := range logged {
		if strings.Contains(l, "NOT petting") {
			refusals++
		}
	}
	if refusals != 1 {
		t.Fatalf("stopping to pet must be logged once, got %d; a silent reset is indistinguishable from a crash", refusals)
	}
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func tail(s string) string {
	if len(s) > 8 {
		return s[len(s)-8:]
	}
	return s
}

func TestFitToHardware(t *testing.T) {
	defaults := func(hw time.Duration) config {
		return config{
			petInterval:   10 * time.Second,
			probeInterval: 10 * time.Second,
			staleAfter:    45 * time.Second,
			hwTimeout:     hw,
		}
	}

	t.Run("a 60s device keeps the tuned defaults", func(t *testing.T) {
		c := defaults(60 * time.Second)
		got, changed := fitToHardware(c, nil)
		if len(changed) != 0 {
			t.Fatalf("rescaled a config that already fits: %v", changed)
		}
		if got != c {
			t.Fatalf("config changed: %+v -> %+v", c, got)
		}
	})

	t.Run("a 15s device is scaled instead of rejected", func(t *testing.T) {
		got, changed := fitToHardware(defaults(15*time.Second), nil)
		if err := got.validate(); err != nil {
			t.Fatalf("scaled config still invalid: %v", err)
		}
		if len(changed) != 3 {
			t.Fatalf("want all three timings scaled, got %v", changed)
		}
		// The values proven by hand on the board (#661).
		if got.petInterval != 3*time.Second ||
			got.probeInterval != 3*time.Second ||
			got.staleAfter != 9*time.Second {
			t.Fatalf("want 3s/3s/9s, got %s/%s/%s",
				got.petInterval, got.probeInterval, got.staleAfter)
		}
	})

	t.Run("an explicit timing is never silently overridden", func(t *testing.T) {
		explicit := map[string]bool{"stale-after": true}
		got, changed := fitToHardware(defaults(15*time.Second), explicit)
		if got.staleAfter != 45*time.Second {
			t.Fatalf("overrode an operator's own number: stale-after = %s", got.staleAfter)
		}
		for _, name := range changed {
			if name == "stale-after" {
				t.Fatal("reported scaling a value it was told not to touch")
			}
		}
		if err := got.validate(); err == nil {
			t.Fatal("an unusable explicit stale-after validated clean")
		}
	})

	t.Run("the scaled timings validate across the plausible range", func(t *testing.T) {
		for hw := 5 * time.Second; hw <= 120*time.Second; hw += time.Second {
			got, _ := fitToHardware(defaults(hw), nil)
			if err := got.validate(); err != nil {
				t.Fatalf("hw=%s scaled to %s/%s/%s which is invalid: %v",
					hw, got.petInterval, got.probeInterval, got.staleAfter, err)
			}
		}
	})

	t.Run("a hardware timeout too small to be usable stays fatal", func(t *testing.T) {
		got, _ := fitToHardware(defaults(2*time.Second), nil)
		if got.petInterval <= 0 || got.probeInterval <= 0 || got.staleAfter <= 0 {
			if err := got.validate(); err == nil {
				t.Fatal("a degenerate scaling validated clean")
			}
		}
	})
}

func TestRun_DisarmsOnlyWhileTheFilesystemAnswers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lastGood   time.Duration // age of the last successful probe
		wantDisarm bool
	}{
		{"an operator stopping the service on a healthy board disarms", 0, true},
		{"a stop taken while the share is silent stays armed", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev, err := os.CreateTemp(t.TempDir(), "watchdog")
			if err != nil {
				t.Fatal(err)
			}
			w := &watchdog{f: dev}

			p := &prober{path: "/irrelevant", interval: time.Hour, statfs: statfsProbe}
			p.last.Store(time.Now().Add(-tc.lastGood).UnixNano())

			c := config{petInterval: time.Hour, probeInterval: time.Millisecond,
				staleAfter: 40 * time.Millisecond, hwTimeout: time.Second,
				probePath: "/irrelevant"}

			sig := make(chan os.Signal, 1)
			sig <- os.Interrupt
			run(w, p, c, sig, make(chan struct{}), func(string, ...any) {})

			b, err := os.ReadFile(dev.Name())
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(b), magicClose); got != tc.wantDisarm {
				t.Fatalf("disarmed=%v, want %v (device holds %q)", got, tc.wantDisarm, string(b))
			}
		})
	}
}
